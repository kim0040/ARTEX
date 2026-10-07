package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	pgdb "github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/enrich"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

// Task는 작업 하나입니다. 설명과 목표, 자기 탐색 그래프 저장소를 가지고,
// 프로세스 전역 자산 저장소를 같이 씁니다. ID는 PG 작업 id를 문자열로 둔 것이고, ExpID는
// 이 작업이 가진 탐색입니다.
// 초보용: 작업마다 탐색 그래프가 따로 있고, 자산 그래프는 모든 작업이 공유합니다.
type Task struct {
	ID           string `json:"id"`
	ExpID        int64  `json:"exploration_id"`
	Name         string `json:"name"` // 선택적 작업 이름; 빈 값=이름 없음
	CategoryID   *int64 `json:"category_id,omitempty"`
	CategoryName string `json:"category_name,omitempty"`
	PinnedAt     int64  `json:"pinned_at,omitempty"`
	Description  string `json:"description"`
	Goal         string `json:"goal"`
	CreatedAt    int64  `json:"created_at"`
	CompletedAt  int64  `json:"completed_at,omitempty"` // 종료 상태에 들어간 unix 초; 0=미완료
	Paused       bool   `json:"paused"`
	Queued       bool   `json:"queued"` // 동시 실행 상한 때문에 보류되어 빈 자리가 나면 자동 시작을 기다림; true=아직 실행 전
	// QueuedAt은 안의 순서 키로, 유닉스 나노초입니다. 일부러
	// CreatedAt보다 잘게 두어, 같은 초에 대기열에 넣은 작업 여러 개도
	// 진짜 FIFO 순서를 유지합니다.
	QueuedAt           int64   `json:"queued_at,omitempty"`
	QueueMode          string  `json:"queue_mode,omitempty"`
	ParentRef          string  `json:"parent_ref,omitempty"`     // 부모 작업 id(오케스트레이션 spawn 기록)
	LLMProfileID       *int64  `json:"llm_profile_id,omitempty"` // 이 작업의 플래너(의도만 생성)/워커(의도 하나를 실행한 뒤 정지)를 돌릴 LLM 설정; nil=전역 활성 설정 사용
	LLMProfileIDs      []int64 `json:"llm_profile_ids,omitempty"`
	ActiveLLMProfileID *int64  `json:"active_llm_profile_id,omitempty"`
	LLMChainRevision   int64   `json:"-"`
	LLMFailoverState   string  `json:"llm_failover_state,omitempty"`
	LLMFailoverReason  string  `json:"llm_failover_reason,omitempty"`
	SourceTaskIDs      []int64 `json:"source_task_ids,omitempty"`
	CompanyIDs         []int64 `json:"company_ids,omitempty"`
	Status             string  `json:"status"` // DB에 적어 둔 작업 생애주기(done/failed/timeout은 종료 상태; 비었거나 그 외는 실행 상태에서 도출)
	// 작업 단위 시간 제한(docs/작업-단위-시간-제한과-마무리-설계.md 참고). DeadlineAt/FirstRunAt은 unix 초이며, 0=미설정/미실행.
	TimeoutSeconds       int                    `json:"timeout_seconds"`
	PlanHeartbeatSeconds int                    `json:"plan_heartbeat_seconds"` // 플래너(의도만 생성) 하트비트 트리거 간격(초)
	CoverageEnabled      bool                   `json:"coverage_enabled"`       // 자산 커버리지 기능 스위치(생성 시 결정, 기본 켜짐)
	FirstRunAt           int64                  `json:"first_run_at,omitempty"`
	DeadlineAt           int64                  `json:"deadline_at,omitempty"`
	Store                *pgdb.ExplorationStore `json:"-"`
	Guard                *guard.Guard           `json:"-"`
	notify               chan struct{}
	lifecycleMu          sync.RWMutex
	llmMu                sync.RWMutex

	// pendingTriggers는 지난 라운드가 꺼낸 뒤 쌓인 구체적 변화를 모읍니다(워커 완료 / 발견).
	// 그 변화가 계획 라운드를 깨웠습니다. 디바운스가 몰림을
	// 라운드 하나로 모으므로, drainTriggers()가 비우기 전에 여러 개가 쌓일 수 있습니다.
	trigMu          sync.Mutex
	pendingTriggers []agent.TriggerEvent
}

// taskLifecycleState는 바뀌는 작업 수명과 물려받은 범위의, 서로 맞는 모습입니다.
// 호출자는 lifecycleSnapshot과
// updateLifecycle을 써야 합니다. Manager가 작업을 공개한 뒤에 대응하는 Task 필드를
// 직접 읽거나 쓰면 안 됩니다.
type taskLifecycleState struct {
	Name          string
	PinnedAt      int64
	Status        string
	Paused        bool
	Queued        bool
	QueuedAt      int64
	QueueMode     string
	CompletedAt   int64
	FirstRunAt    int64
	DeadlineAt    int64
	SourceTaskIDs []int64
	CompanyIDs    []int64
	CategoryID    *int64
	CategoryName  string
}

func (t *Task) lifecycleSnapshot() taskLifecycleState {
	if t == nil {
		return taskLifecycleState{}
	}
	t.lifecycleMu.RLock()
	defer t.lifecycleMu.RUnlock()
	return t.lifecycleSnapshotLocked()
}

func (t *Task) lifecycleSnapshotLocked() taskLifecycleState {
	return taskLifecycleState{
		Name:          t.Name,
		PinnedAt:      t.PinnedAt,
		Status:        t.Status,
		Paused:        t.Paused,
		Queued:        t.Queued,
		QueuedAt:      t.QueuedAt,
		QueueMode:     t.QueueMode,
		CompletedAt:   t.CompletedAt,
		FirstRunAt:    t.FirstRunAt,
		DeadlineAt:    t.DeadlineAt,
		SourceTaskIDs: append([]int64(nil), t.SourceTaskIDs...),
		CompanyIDs:    append([]int64(nil), t.CompanyIDs...),
		CategoryID:    cloneInt64Ptr(t.CategoryID),
		CategoryName:  t.CategoryName,
	}
}

func (t *Task) updateLifecycle(update func(*taskLifecycleState)) {
	if t == nil || update == nil {
		return
	}
	t.lifecycleMu.Lock()
	state := t.lifecycleSnapshotLocked()
	update(&state)
	t.Name = state.Name
	t.PinnedAt = state.PinnedAt
	t.Status = state.Status
	t.Paused = state.Paused
	t.Queued = state.Queued
	t.QueuedAt = state.QueuedAt
	t.QueueMode = state.QueueMode
	t.CompletedAt = state.CompletedAt
	t.FirstRunAt = state.FirstRunAt
	t.DeadlineAt = state.DeadlineAt
	t.SourceTaskIDs = append(t.SourceTaskIDs[:0], state.SourceTaskIDs...)
	t.CompanyIDs = append(t.CompanyIDs[:0], state.CompanyIDs...)
	t.CategoryID = cloneInt64Ptr(state.CategoryID)
	t.CategoryName = state.CategoryName
	t.lifecycleMu.Unlock()
}

type taskLLMState struct {
	ProfileID      *int64
	ProfileIDs     []int64
	ActiveID       *int64
	ChainRevision  int64
	FailoverState  string
	FailoverReason string
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (t *Task) llmStateSnapshot() taskLLMState {
	t.llmMu.RLock()
	defer t.llmMu.RUnlock()
	return taskLLMState{
		ProfileID:      cloneInt64Ptr(t.LLMProfileID),
		ProfileIDs:     append(make([]int64, 0, len(t.LLMProfileIDs)), t.LLMProfileIDs...),
		ActiveID:       cloneInt64Ptr(t.ActiveLLMProfileID),
		ChainRevision:  t.LLMChainRevision,
		FailoverState:  t.LLMFailoverState,
		FailoverReason: t.LLMFailoverReason,
	}
}

func (t *Task) setLLMState(profileID, activeID *int64, profileIDs []int64, revision int64, state, reason string) bool {
	t.llmMu.Lock()
	defer t.llmMu.Unlock()
	if revision < t.LLMChainRevision {
		return false
	}
	t.LLMProfileID = cloneInt64Ptr(profileID)
	t.ActiveLLMProfileID = cloneInt64Ptr(activeID)
	t.LLMProfileIDs = append(t.LLMProfileIDs[:0], profileIDs...)
	t.LLMChainRevision = revision
	t.LLMFailoverState = state
	t.LLMFailoverReason = reason
	return true
}

// DeleteTaskOptions는 작업 자신의 탐색 그래프 밖에 있는 데이터 정리를 고릅니다.
// 모든 옵션의 기본은 false입니다. 예전 동작과 맞추려고요.
type DeleteTaskOptions struct {
	DeleteAssets     bool `json:"delete_assets"`
	DeleteTraffic    bool `json:"delete_traffic"`
	DeleteFiles      bool `json:"delete_files"`
	DeleteFindings   bool `json:"delete_findings"`
	DeleteLLMRecords bool `json:"delete_llm_records"`
}

// DeleteTaskResult는 지우는 정리를 API 호출자가 감사할 수 있게 합니다.
type DeleteTaskResult struct {
	Deleted           string `json:"deleted"`
	AssetsDeleted     int64  `json:"assets_deleted"`
	AssetsDetached    int64  `json:"assets_detached"`
	TrafficDeleted    int64  `json:"traffic_deleted"`
	FilesDeleted      bool   `json:"files_deleted"`
	FindingsDeleted   int64  `json:"findings_deleted"`
	LLMRecordsDeleted int64  `json:"llm_records_deleted"`
	CleanupWarning    string `json:"cleanup_warning,omitempty"`
}

// Manager는 PostgreSQL 데이터 원본(자산 그래프, 작업마다의 탐색
// 그래프, 설정)과 메모리 속 작업 손잡이 묶음을 가집니다.
// 초보용: 자산 그래프와 작업마다의 탐색 그래프, 그리고 메모리 속 작업 목록의 주인이 여기입니다.
type Manager struct {
	dir         string
	pg          *pgdb.DB
	assets      *pgdb.AssetStore
	traffic     *traffic.Traffic       // 프로세스 전역 기록 프록시(없을 수 있음)
	enrich      *enrich.Engine         // 엔진 쪽 자산 자동 완성(DNS/HTTP)
	interceptor *intercept.Interceptor // 사용자가 정한 도구 호출 가로채기 규칙

	companyMu sync.Mutex // 작업과 기업 범위 확정을, 살아 있는 손잡이 등록과 순서를 맞춥니다
	// taskStateMu는 PostgreSQL 수명 쓰기와
	// 메모리 속 거울의 확정 순서를 지킵니다. lifecycleMu는 스냅샷의 경주를 없애지만, 이
	// 바깥 쓰기 잠금이 없으면 더 옛 요청이 먼저 확정하고 나중에 공개할 수 있습니다.
	taskStateMu sync.Mutex
	mu          sync.RWMutex
	tasks       map[string]*Task
	active      string
	trafficOn   bool // 트래픽 캡처 스위치(기본 꺼짐; settings.traffic_capture)
	llmRecOn    bool // LLM 기록 스위치(기본 꺼짐; settings.llm_record)
	// 웹 검색 스위치와 출처(기본 꺼짐; settings.web_search_*). brave-free는 braveKey가 필요하고, tavily는 tavilyKey가 필요하다.
	// webSearchProxy는 독립 출구 프록시(http/https/socks5)이며, 트래픽을 기록하는 기록 프록시(127.0.0.1:8788)와는 무관하다.
	webSearchOn      bool
	webSearchBackend string
	braveKey         string
	tavilyKey        string
	webSearchProxy   string
	// globalProxy는 목표 트래픽이 모두 지나는 출구 프록시입니다
	// (http/https/socks5, user:pass는 선택). 비어 있으면 직접 연결입니다. 트래픽
	// 캡처가 켜지면 MITM의 상위가 되고, 캡처가 꺼지면
	// 에이전트 bash 환경이나 WebFetch에 직접 넣습니다. ProxyAddr를 보세요.
	globalProxy string
}

// 화면이 실행 중에 켜고 끄는 설정 키입니다.
const (
	settingTrafficCapture      = "traffic_capture"
	settingAgentTrafficBinding = "agent_traffic_binding"
	settingWebSearchOn         = "web_search_enabled"
	settingWebSearchBackend    = "web_search_backend"
	settingBraveKey            = "brave_search_api_key"
	settingTavilyKey           = "tavily_search_api_key"
	settingWebSearchProxy      = "web_search_proxy"
	// settingGlobalProxy는 목표 트래픽 전체의 전역 출구 프록시입니다
	// (http/https/socks5). 비어 있으면 직접 연결입니다. web_search_proxy와는 다릅니다(그것은
	// 검색 백엔드만 보냅니다). 설정별 LLM 프록시와도 다릅니다.
	settingGlobalProxy = "global_proxy"
	settingWorkers     = "workers"
	settingLLMRecord   = "llm_record"
	// LLM 풀링(장애 조치). 기본은 꺼짐——켜면 「전역 활성 설정」의 agent가 현재 설정에서
	// 사용할 수 없을 때(잔액 부족/key 무효/속도 제한/서비스 이상) 다음 설정으로 자동 전환한다.
	// settingLLMPoolBindFallback은 풀링이 켜져 있을 때만 의미가 있다: 기본은 꺼짐, 즉 agent/작업이 명시적으로
	// 어떤 설정에 묶으면 그것만 쓰고 실패하면 실패로 끝난다. 켜면 묶인 설정이 실패해도 풀링 체인으로 폴백한다.
	settingLLMPoolOn           = "llm_pool_enabled"
	settingLLMPoolBindFallback = "llm_pool_bind_fallback"
	// 작업 동시 실행 상한: 스위치 + 상한 수. 기본은 꺼짐; 켜면 기본 상한 5(defaultConcurrencyLimit 참고).
	settingConcurrencyOn    = "task_concurrency_enabled"
	settingConcurrencyLimit = "task_concurrency_limit"
	// 실험 기능: noa 모델이 이끄는 컨텍스트 압축(norma v0.4.0). 기본은 꺼짐——켜면 플랫폼에 붙은 네 종류의
	// agent(플래너(의도만 생성)/워커(의도 하나를 실행한 뒤 정지)/메인 에이전트/대화)의 컨텍스트 압축을 noa가 맡고, 내장 compaction을 대체한다.
	// run마다 한 번 읽으며, 전환은 이후에 시작하는 run에만 영향을 준다.
	settingNoaCompaction = "noa_compaction"
	// defaultWebSearchBackend는 웹 검색은 켜졌는데 백엔드를 고르지 않았을 때 씁니다.
	defaultWebSearchBackend = "ddgs"
	// deepSeekWebSearchBackend는 자기 키 대신 활성 LLM 설정을 빌립니다.
	// 그래서 DeepSeek를 가리키는 anthropic 형식 설정에서만 됩니다.
	deepSeekWebSearchBackend = "deepseek"
	// defaultWorkers는 설정이 없을 때 동시에 도는 워커 수입니다.
	defaultWorkers = 3
	// defaultConcurrencyLimit은 기능을 켰는데 명시적 상한을 저장하지 않았을 때의
	// 동시 실행 작업 상한입니다.
	defaultConcurrencyLimit = 5
)

// ConcurrencyLimit은 동시 실행 작업 상한이 켜져 있는지와
// 그 상한을 돌려줍니다(켰는데 없으면 기본 5). 켜져 있으면 limit은 항상 1 이상입니다.
func (m *Manager) ConcurrencyLimit() (enabled bool, limit int) {
	on, _, _ := m.pg.GetSetting(settingConcurrencyOn)
	if strings.TrimSpace(on) != "true" {
		return false, 0
	}
	limit = defaultConcurrencyLimit
	if v, ok, _ := m.pg.GetSetting(settingConcurrencyLimit); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 {
			limit = n
		}
	}
	return true, limit
}

// SetConcurrency는 실행 중 작업 동시성 상한을 저장합니다. limit이 1 미만이면 1로 올립니다.
func (m *Manager) SetConcurrency(enabled bool, limit int) error {
	if limit < 1 {
		limit = defaultConcurrencyLimit
	}
	if err := m.pg.SetSetting(settingConcurrencyLimit, strconv.Itoa(limit)); err != nil {
		return err
	}
	return m.pg.SetSetting(settingConcurrencyOn, strconv.FormatBool(enabled))
}

// Workers는 설정된 동시 워커 수를 돌려줍니다(기본 3). 엔진 Run이
// 작업마다 읽으므로, 바꾼 값은 그 뒤에 시작한 작업에 적용됩니다.
func (m *Manager) Workers() int {
	v, ok, err := m.pg.GetSetting(settingWorkers)
	if err != nil || !ok {
		return defaultWorkers
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultWorkers
	}
	return n
}

// SetWorkers는 동시 워커 수를 저장합니다. 0 이하는 거절합니다.
func (m *Manager) SetWorkers(n int) error {
	if n <= 0 {
		return fmt.Errorf("workers는 0보다 커야 합니다")
	}
	return m.pg.SetSetting(settingWorkers, strconv.Itoa(n))
}

// Enrich는 자산 자동 완성 엔진을 돌려줍니다(초기화가 실패하면 nil일 수 있음).
func (m *Manager) Enrich() *enrich.Engine { return m.enrich }

// NewManager는 PostgreSQL에 연결하고, proxyAddr가 비어 있지 않으면
// 트래픽 기록 프록시를 시작합니다. PostgreSQL은 필수입니다(유일한 데이터 원본).
// 초보용: 자산 그래프와 탐색 그래프가 있는 DB에 붙고, 주소가 있으면 기록 프록시를 켭니다.
func NewManager(dir, proxyAddr string) (*Manager, error) {
	// 데이터 디렉터리를 처음부터 절대 경로로 풉니다. 모든 데이터 경로가
	// 여기서 나옵니다. 특히 MITM CA 인증서는 워커 셸에 경로가 들어가고
	// (SSL_CERT_FILE/CURL_CA_BUNDLE) WebFetch가 읽습니다. 상대 경로(기본은
	// `go run` 아래의 ./data)는 현재 작업 디렉터리가 맞을 때만 풀립니다. 그래서 다른
	// 디렉터리의 curl이나 WebFetch는 CA를 못 읽습니다. 프록시로 가는 TLS가 깨집니다(curl 000 / EOF).
	// 절대 경로면 작업 디렉터리와 무관합니다.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dsn, source, err := pgdb.DSN()
	if err != nil {
		return nil, err
	}
	log.Printf("[pg] 데이터베이스 설정 출처: %s", source)
	pg, err := pgdb.Open(dsn)
	if err != nil {
		return nil, err
	}
	if err := pg.RecoverFindingRetests(); err != nil {
		pg.Close()
		return nil, fmt.Errorf("recover finding retests: %w", err)
	}
	if err := pg.EnsureLLMRecordsTable(); err != nil {
		log.Printf("[llmrec] create table: %v", err)
	}
	if err := pg.EnsureLLMUsageTable(); err != nil {
		log.Printf("[llmusage] create table: %v", err)
	}
	m := &Manager{dir: dir, pg: pg, assets: pg.Assets(), tasks: map[string]*Task{}, interceptor: intercept.New(pg)}
	if proxyAddr != "" {
		tr, err := traffic.Open(filepath.Join(dir, "traffic"), proxyAddr)
		if err != nil {
			log.Printf("[traffic] disabled: %v", err)
		} else {
			err = tr.RecoverHostDeleteStages(func(_ int64, taskID int64) (bool, error) {
				if taskID <= 0 {
					return false, errors.New("보관 트래픽 임시 로그에 작업 ID가 없습니다")
				}
				task, taskErr := pg.GetTask(taskID)
				if taskErr != nil {
					return false, taskErr
				}
				// 보관됐거나 영원히 지운 작업은 GetTask에서 숨깁니다.
				// 복원된 작업은 보이고, 잠시 치워 둔 트래픽을 되돌려야 합니다.
				return task == nil, nil
			})
			if err != nil {
				_ = tr.Close()
				_ = pg.Close()
				return nil, fmt.Errorf("recover traffic delete staging: %w", err)
			}
		}
		if tr != nil {
			m.traffic = tr
			go func() {
				log.Printf("[traffic] recording proxy on %s (set HTTP_PROXY=%s + trust _ca CA)", proxyAddr, tr.ProxyAddr())
				if err := tr.Start(); err != nil {
					log.Printf("[traffic] proxy stopped: %v", err)
				}
			}()
		}
	}
	// 자산 자동 완성 엔진(§5)입니다. HTTP 프로브는 기록
	// 프록시를 통합니다(m.ProxyAddr. 트래픽 캡처 스위치를 따름).
	m.trafficOn = pg.GetBool(settingTrafficCapture, false)
	// LLM 기록 스위치(기본 꺼짐). 기록기는 호출할 때마다 이 플래그를 읽는다.
	m.llmRecOn = pg.GetBool(settingLLMRecord, false)
	// 저장된 웹 검색 설정을 읽습니다(기본: 꺼짐, ddgs).
	m.webSearchOn = pg.GetBool(settingWebSearchOn, false)
	if v, ok, _ := pg.GetSetting(settingWebSearchBackend); ok && v != "" {
		m.webSearchBackend = v
	} else {
		m.webSearchBackend = defaultWebSearchBackend
	}
	if v, ok, _ := pg.GetSetting(settingBraveKey); ok {
		m.braveKey = v
	}
	if v, ok, _ := pg.GetSetting(settingTavilyKey); ok {
		m.tavilyKey = v
	}
	if v, ok, _ := pg.GetSetting(settingWebSearchProxy); ok {
		m.webSearchProxy = v
	}
	// 전역 출구 프록시입니다(기본: 직접 연결). 캡처가 켜지면 MITM의
	// 상위로 넣어, 기록된 트래픽이 그쪽으로 나갑니다. 캡처가
	// 꺼지면 ProxyAddr가 에이전트에 직접 넘깁니다(bash 환경 / WebFetch).
	if v, ok, _ := pg.GetSetting(settingGlobalProxy); ok {
		m.globalProxy = strings.TrimSpace(v)
	}
	if m.traffic != nil {
		if err := m.traffic.SetUpstreamProxy(m.globalProxy); err != nil {
			log.Printf("[proxy] 전역 프록시 %q이(가) 유효하지 않아 무시했습니다: %v", m.globalProxy, err)
		}
	}
	m.enrich = enrich.New(m.assets, m.ProxyAddr, 4)
	// 심어 둔 브라우저 MCP를 저장된 캡처 상태와 맞춥니다. 그래서
	// 캡처가 이미 켜진 채 재시작해도 Playwright가 프록시를 탑니다.
	m.syncBrowserMCPProxy()
	return m, nil
}

// TrafficEnabled는 트래픽 캡처가 켜져 있는지 알립니다(기본 꺼짐). 꺼지면
// 에이전트에 프록시, 트래픽 도구, 프롬프트를 넣지 않습니다(아무것도 기록하지 않음).
func (m *Manager) TrafficEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.trafficOn
}

// SetTrafficEnabled는 트래픽 캡처 스위치를 저장하고 적용합니다. 호출자는 그 뒤
// 에이전트를 다시 만들어야 합니다(applyLLM). 새 프록시, 도구, 프롬프트가 먹게 하려고요.
func (m *Manager) SetTrafficEnabled(on bool) error {
	if err := m.pg.SetBool(settingTrafficCapture, on); err != nil {
		return err
	}
	m.mu.Lock()
	m.trafficOn = on
	m.mu.Unlock()
	// 브라우저 MCP에 기록 프록시와 CA를 넣거나(켬) 뺍니다(끔). 그래서
	// Playwright가 MITM을 탑니다. 위 플래그를 뒤집은 뒤에 돌려야 합니다.
	// ProxyAddr와 ProxyCACert가 그 플래그를 따르기 때문입니다. putSettings가 다음에 에이전트를 다시 만들고(applyLLM),
	// 그 과정에서 MCP를 새 인자/환경으로 다시 띄웁니다.
	m.syncBrowserMCPProxy()
	return nil
}

// LLMRecordEnabled는 LLM 요청/응답 기록이 켜져 있는지 알립니다
// (기본 꺼짐; settings.llm_record). 기록기는 호출마다 이 값을 보므로
// 토글은 에이전트를 다시 만들지 않고 바로 적용됩니다.
func (m *Manager) LLMRecordEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.llmRecOn
}

// SetLLMRecordEnabled는 LLM 기록 스위치를 저장하고 적용합니다. 바로
// 먹습니다. applyLLM은 필요 없습니다. 기록기가 호출마다 플래그를 읽기 때문입니다.
func (m *Manager) SetLLMRecordEnabled(on bool) error {
	if err := m.pg.SetBool(settingLLMRecord, on); err != nil {
		return err
	}
	m.mu.Lock()
	m.llmRecOn = on
	m.mu.Unlock()
	return nil
}

// NoaCompactionEnabled는 실험용 noa 맥락 압축이 켜져 있는지 알립니다
// (기본 꺼짐; settings.noa_compaction). 주입된 결정 함수가 에이전트 실행마다
// 읽으므로, 토글은 다시 만들지 않고 다음 실행부터 적용됩니다.
func (m *Manager) NoaCompactionEnabled() bool {
	return m.pg.GetBool(settingNoaCompaction, false)
}

// SetNoaCompaction은 noa 스위치를 저장합니다. 다음 에이전트 실행부터 먹습니다.
// 결정 함수가 실행마다 읽으므로, 다시 만들 필요가 없습니다.
func (m *Manager) SetNoaCompaction(on bool) error {
	return m.pg.SetBool(settingNoaCompaction, on)
}

// LLMPoolEnabled는 LLM 장애 조치("풀링")가 켜져 있는지 알립니다 (기본 꺼짐;
// settings.llm_pool_enabled). 제공자 사슬을 만들 때 읽습니다(applyLLM).
// 그래서 바꾸면 다시 만들어야 합니다. putSettings가 그렇게 합니다.
func (m *Manager) LLMPoolEnabled() bool {
	if m.pg == nil {
		return false
	}
	return m.pg.GetBool(settingLLMPoolOn, false)
}

// SetLLMPoolEnabled는 장애 조치 스위치를 저장합니다. 호출자는 그 뒤 에이전트를 다시 만듭니다
// (applyLLM). 그때 먹습니다.
func (m *Manager) SetLLMPoolEnabled(on bool) error { return m.pg.SetBool(settingLLMPoolOn, on) }

// LLMPoolBindFallback은 특정 설정에 묶인 에이전트나 작업이, 그 설정이 실패해도 사슬로 물러설지 알립니다.
// 켜면 그 설정이 실패해도 사슬로 물러섭니다 (기본 꺼짐: 바인딩되면
// 독점, 실패하면 곧 실패). LLMPoolEnabled가 켜져 있을 때만 의미가 있습니다.
func (m *Manager) LLMPoolBindFallback() bool {
	if m.pg == nil {
		return false
	}
	return m.pg.GetBool(settingLLMPoolBindFallback, false)
}

// SetLLMPoolBindFallback은 묶인 설정의 폴백 스위치를 저장합니다. 호출자는
// 그 뒤 에이전트를 다시 만듭니다(applyLLM).
func (m *Manager) SetLLMPoolBindFallback(on bool) error {
	return m.pg.SetBool(settingLLMPoolBindFallback, on)
}

// WebSearch는 현재 웹 검색 설정을 돌려줍니다. 켜짐 여부,
// 백엔드(ddgs | brave-free | tavily), Brave API 키, Tavily API
// 키(설정된 것만 값이 있음), 전용 출구 프록시(비어 있으면 직접 연결)입니다.
func (m *Manager) WebSearch() (on bool, backend, braveKey, tavilyKey, proxy string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	backend = m.webSearchBackend
	if backend == "" {
		backend = defaultWebSearchBackend
	}
	return m.webSearchOn, backend, m.braveKey, m.tavilyKey, m.webSearchProxy
}

// WebSearchOpts는 그 설정을 에이전트 패키지 구조체로 돌려줍니다. 서버가 각
// 에이전트에 넣습니다. 꺼져 있거나, 키가 필요한 백엔드를 골랐는데
// 키가 없으면 비활성입니다. 반만 설정된 백엔드가 세션을 만들 때 도구를 조용히 빼지 않게 하려고요.
func (m *Manager) WebSearchOpts() agent.WebSearchOpts {
	on, backend, braveKey, tavilyKey, proxy := m.WebSearch()
	if on && backend == "brave-free" && strings.TrimSpace(braveKey) == "" {
		on = false
	}
	if on && backend == "tavily" && strings.TrimSpace(tavilyKey) == "" {
		on = false
	}
	o := agent.WebSearchOpts{Enabled: on, Backend: backend, BraveKey: braveKey, TavilyKey: tavilyKey, Proxy: proxy}
	if backend == deepSeekWebSearchBackend {
		o.DeepSeekBaseURL, o.DeepSeekAPIKey, o.DeepSeekModel = m.deepSeekSearchCreds()
	}
	return o
}

// deepSeekSearchCreds는 deepseek 검색 백엔드가 빌리는 자격 증명을 찾습니다.
// 활성 LLM 설정에서 빌립니다(자기 키는 없음). 그
// 설정이 서버 쪽 검색을 실제로 돌릴 수 있는지는 여기서 일부러 검사하지 않습니다. DeepSeek는
// Anthropic 형식 끝점에서만 그것을 엽니다. 화면이
// 요구를 적고 사용자가 정합니다.
// 검색할 때(또는 설정 화면의 테스트 버튼에서) 그냥 실패하며, 그 반응은
// 다른 백엔드가 나쁜 키에 주는 것과 같습니다.
func (m *Manager) deepSeekSearchCreds() (baseURL, apiKey, model string) {
	p, err := m.pg.ActiveProfile()
	if err != nil || p == nil {
		return "", "", ""
	}
	return p.BaseURL, p.APIKey, p.Model
}

// SetWebSearch는 웹 검색 설정을 저장하고 적용합니다. braveKey, tavilyKey,
// proxy는 nil이면 각각 그대로 둡니다(스위치를 켠다고 저장한
// 키나 프록시를 지우지 않음. 빈 문자열 포인터를 넘기면 지움). 호출자는 그 뒤 에이전트를 다시 만들어야 합니다(applyLLM).
// 설정이 먹게 하려고요.
func (m *Manager) SetWebSearch(on bool, backend string, braveKey, tavilyKey, proxy *string) error {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		backend = defaultWebSearchBackend
	}
	if err := m.pg.SetBool(settingWebSearchOn, on); err != nil {
		return err
	}
	if err := m.pg.SetSetting(settingWebSearchBackend, backend); err != nil {
		return err
	}
	m.mu.Lock()
	m.webSearchOn = on
	m.webSearchBackend = backend
	m.mu.Unlock()
	if braveKey != nil {
		if err := m.pg.SetSetting(settingBraveKey, *braveKey); err != nil {
			return err
		}
		m.mu.Lock()
		m.braveKey = *braveKey
		m.mu.Unlock()
	}
	if tavilyKey != nil {
		if err := m.pg.SetSetting(settingTavilyKey, *tavilyKey); err != nil {
			return err
		}
		m.mu.Lock()
		m.tavilyKey = *tavilyKey
		m.mu.Unlock()
	}
	if proxy != nil {
		p := strings.TrimSpace(*proxy)
		if err := m.pg.SetSetting(settingWebSearchProxy, p); err != nil {
			return err
		}
		m.mu.Lock()
		m.webSearchProxy = p
		m.mu.Unlock()
	}
	return nil
}

// browserMCPName은 심어 둔 Playwright MCP입니다. 프록시 인자와 CA 환경을
// 트래픽 캡처 스위치와 맞춰 둡니다.
const browserMCPName = "browser"

// syncBrowserMCPProxy는 심어 둔 브라우저 MCP의 프록시 인자와 CA 환경을
// 현재 트래픽 캡처 상태와 맞춥니다. 캡처가 켜지면 Playwright를
// 기록 프록시로 보내고(--proxy-server) 그 MITM CA를 믿게 합니다(NODE_EXTRA_CA_CERTS).
// 캡처가 꺼지면 둘 다 뺍니다. 여러 번 불러도 같고, 사용자가 MCP를 지우거나 이름을 바꾸면
// 아무 일도 안 합니다. m.mu를 잡지 않은 채 불러야 합니다(ProxyAddr/ProxyCACert가 잠금을 잡음).
func (m *Manager) syncBrowserMCPProxy() {
	servers, err := m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] browser 프록시 동기화: MCP 목록 읽기 실패: %v", err)
		return
	}
	var srv *pgdb.MCPServer
	for _, s := range servers {
		if s.Name == browserMCPName {
			srv = s
			break
		}
	}
	if srv == nil {
		return // 사용자가 지우거나 이름을 바꿨습니다. 그대로 둡니다.
	}

	proxy := m.ProxyAddr()  // 캡처가 꺼지면 빈 문자열
	cert := m.ProxyCACert() // 캡처가 꺼지면 빈 문자열

	args := stripProxyArgs(decodeStrSlice(srv.Args))
	env := decodeStrMap(srv.Env)
	delete(env, "NODE_EXTRA_CA_CERTS")
	if proxy != "" {
		args = append(args, "--proxy-server", proxy)
		if cert != "" {
			env["NODE_EXTRA_CA_CERTS"] = cert
		}
	}
	srv.Args = encodeJSON(args)
	srv.Env = encodeJSON(env)
	if _, err := m.pg.SaveMCP(srv); err != nil {
		log.Printf("[mcp] browser 프록시 동기화 실패: %v", err)
		return
	}
	if proxy != "" {
		log.Printf("[mcp] browser MCP에 캡처 프록시 %s를 연결했습니다 (CA %s)", proxy, cert)
	} else {
		log.Printf("[mcp] browser MCP에서 캡처 프록시 설정을 제거했습니다")
	}
}

// stripProxyArgs는 --proxy-server/--proxy-bypass 플래그를 뺍니다("--flag val"과
// "--flag=val" 둘 다). 현재 상태에서 깨끗이 다시 넣으려고요.
// 입력 슬라이스는 바꾸지 않습니다.
func stripProxyArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--proxy-server" || a == "--proxy-bypass" {
			i++ // 다음 값도 건너뜁니다.
			continue
		}
		if strings.HasPrefix(a, "--proxy-server=") || strings.HasPrefix(a, "--proxy-bypass=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func decodeStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func decodeStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func encodeJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// HostTools는 실행 중 host가 주는 도구입니다. 모든 에이전트의 기본 목록에 넣습니다
// (ToolAugment). 그다음 tools 표가 에이전트별 바인딩으로 거릅니다. 지금은
// 트래픽 도구이고, 전역 캡처 스위치로 막습니다. 캡처가 꺼지면 비어서,
// 바인딩과 관계없이 어떤 에이전트도 traffic_search/traffic_get을 못 받습니다.
func (m *Manager) HostTools() []actool.CoreTool {
	if m.traffic == nil || !m.TrafficEnabled() {
		return nil
	}
	return m.traffic.Tools()
}

func (m *Manager) Assets() *pgdb.AssetStore  { return m.assets }
func (m *Manager) PG() *pgdb.DB              { return m.pg }
func (m *Manager) Traffic() *traffic.Traffic { return m.traffic }

// ProxyAddr는 에이전트가 목표 트래픽을 보내는 출구 프록시 주소를 돌려줍니다.
//   - 캡처 켜짐 → 기록 MITM 프록시(전역 프록시가 있으면 그쪽으로
//     나감). 에이전트는 그 CA도 받습니다(ProxyCACert를 보세요).
//   - 캡처 꺼짐 → 전역 출구 프록시 자체(CA는 비움. 진짜 목표
//     인증서). 전역 프록시가 없으면 빈 문자열(직접 연결, 기록 없음).
//
// 그래서 전역 프록시는 두 모드에서 모두 먹습니다. 캡처 중에는 MITM의 상위로,
// 캡처가 아니면 에이전트 자신의 bash 환경이나 WebFetch로 들어갑니다.
func (m *Manager) ProxyAddr() string {
	if m.traffic != nil && m.TrafficEnabled() {
		return m.traffic.ProxyAddr()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.globalProxy
}

// ProxyCACert는 에이전트가 출구 프록시의 HTTPS를 확인하려면 믿어야 하는 CA 인증서 경로를 돌려줍니다.
// 트래픽 캡처가 켜져 있을 때만 비어 있지 않습니다(MITM이 인증서를 다시 서명).
// 캡처가 꺼진 채 직접 쓰는 전역 프록시는 그냥 전달이라
// 진짜 목표 인증서를 유지합니다. 거기서는 전용 CA가 필요 없습니다. 이 값이 비어 있는 것은
// 워커에게 "기록 꺼짐" 신호이기도 합니다(workerSystem을 보세요).
func (m *Manager) ProxyCACert() string {
	if m.traffic == nil || !m.TrafficEnabled() {
		return ""
	}
	return m.traffic.CACertPath()
}

// GlobalProxy는 설정된 전역 출구 프록시 URL을 돌려줍니다(비어 있으면 직접 연결).
func (m *Manager) GlobalProxy() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.globalProxy
}

// SetGlobalProxy는 전역 출구 프록시를 검사하고, 저장하고, 적용합니다
// (http/https/socks5, user:pass는 선택. 비어 있으면 직접 연결). MITM의
// 상위는 바로 바꿉니다. 호출자는 그 뒤 에이전트를 다시 만들어야 합니다(applyLLM).
// 캡처가 꺼진 경로(bash 환경 / WebFetch)도 변화를 받게 하려고요.
func (m *Manager) SetGlobalProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if _, err := traffic.ValidateProxyURL(raw); err != nil {
			return err
		}
	}
	if err := m.pg.SetSetting(settingGlobalProxy, raw); err != nil {
		return err
	}
	m.mu.Lock()
	m.globalProxy = raw
	m.mu.Unlock()
	if m.traffic != nil {
		if err := m.traffic.SetUpstreamProxy(raw); err != nil {
			return err
		}
	}
	// 브라우저 MCP의 출구도 새 전역 프록시와 맞춥니다.
	m.syncBrowserMCPProxy()
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.traffic != nil {
		m.traffic.Close()
	}
	return m.pg.Close()
}

// isTerminalStatus는 작업 상태가 끝났는지 알립니다(done/failed/timeout).
// db.IsTerminal을 감싼 패키지 안 함수입니다. 서버 파일이 정의를 하나로 같이 씁니다.
func isTerminalStatus(status string) bool { return pgdb.IsTerminal(status) }

// unixOrZero는 t의 유닉스 초를 돌려줍니다. 시각이 nil이면 0입니다.
func unixOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func unixNanoOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixNano()
}

func taskFromPG(pt *pgdb.Task, store *pgdb.ExplorationStore, ic *intercept.Interceptor) *Task {
	return &Task{
		ID: strconv.FormatInt(pt.ID, 10), ExpID: pt.ExplorationID,
		Name:       pt.Name,
		CategoryID: cloneInt64Ptr(pt.CategoryID), CategoryName: pt.CategoryName,
		PinnedAt:    unixOrZero(pt.PinnedAt),
		Description: pt.Description, Goal: pt.Goal, CreatedAt: pt.CreatedAt.Unix(), Paused: pt.Paused, Queued: pt.Queued,
		QueuedAt: unixNanoOrZero(pt.QueuedAt), QueueMode: pt.QueueMode,
		CompletedAt: unixOrZero(pt.CompletedAt), Status: pt.Status, ParentRef: pt.ParentRef,
		LLMProfileID:  pt.LLMProfileID,
		LLMProfileIDs: append([]int64(nil), pt.LLMProfileIDs...), ActiveLLMProfileID: pt.ActiveLLMProfileID,
		LLMChainRevision: pt.LLMChainRevision,
		LLMFailoverState: pt.LLMFailoverState, LLMFailoverReason: pt.LLMFailoverReason,
		SourceTaskIDs:  append([]int64(nil), pt.SourceTaskIDs...),
		CompanyIDs:     append([]int64(nil), pt.CompanyIDs...),
		TimeoutSeconds: pt.TimeoutSeconds, PlanHeartbeatSeconds: pt.PlanHeartbeatSeconds,
		CoverageEnabled: pt.CoverageEnabled,
		FirstRunAt:      unixOrZero(pt.FirstRunAt), DeadlineAt: unixOrZero(pt.DeadlineAt),
		Store: store, Guard: guard.NewWithInterceptor(ic), notify: make(chan struct{}, 1),
	}
}

// UpdateTaskMetadata는 목록에만 쓰는 작업 메타데이터를 바꿉니다. 플래너,
// 메인 에이전트, 워커 호출은 끊지 않습니다.
func (m *Manager) UpdateTaskMetadata(taskID string, patch pgdb.TaskPatch) (*Task, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		return nil, nil
	}
	updated, err := m.pg.UpdateTask(id, patch)
	if err != nil || updated == nil {
		return nil, err
	}
	m.mu.RLock()
	task := m.tasks[taskID]
	m.mu.RUnlock()
	if task == nil {
		return nil, nil
	}
	task.updateLifecycle(func(state *taskLifecycleState) {
		state.Name = updated.Name
		state.PinnedAt = unixOrZero(updated.PinnedAt)
	})
	return task, nil
}

// CreateTask는 작업과 그 탐색을 만들고 활성으로 둡니다.
// timeoutSeconds 는 작업 단위의 벽시계 시간 예산이다 (0 = 시간 제한 없음).
func (m *Manager) CreateTask(description, goal string, llmProfileID *int64, timeoutSeconds, planHeartbeatSeconds int) (*Task, error) {
	var ids []int64
	if llmProfileID != nil {
		ids = []int64{*llmProfileID}
	}
	return m.CreateTaskWithOptions(description, goal, pgdb.TaskCreateOptions{
		LLMProfileIDs: ids, TimeoutSeconds: timeoutSeconds, PlanHeartbeatSeconds: planHeartbeatSeconds,
	})
}

func (m *Manager) CreateTaskWithOptions(description, goal string, opts pgdb.TaskCreateOptions) (*Task, error) {
	if len(opts.CompanyIDs) > 0 {
		m.companyMu.Lock()
		defer m.companyMu.Unlock()
	}
	pt, err := m.pg.CreateTaskWithOptions(description, goal, opts)
	if err != nil {
		return nil, err
	}
	t := taskFromPG(pt, m.pg.Exploration(pt.ExplorationID), m.interceptor)
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.active = t.ID
	m.mu.Unlock()
	return t, nil
}

// RenameTaskCategory는 분류 이름을 저장하고, 그것을 가리키는 살아 있는 작업 DTO를
// 모두 고칩니다. taskStateMu가 작업 재배치와 순서를 맞춥니다.
func (m *Manager) RenameTaskCategory(id int64, name string) (*pgdb.TaskCategory, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	category, err := m.pg.RenameTaskCategory(id, name)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range m.tasks {
		task.updateLifecycle(func(state *taskLifecycleState) {
			if state.CategoryID != nil && *state.CategoryID == id {
				state.CategoryName = category.Name
			}
		})
	}
	return category, nil
}

// DeleteTaskCategory는 영향받는 살아 있는 작업을 모두 미분류로 옮깁니다.
func (m *Manager) DeleteTaskCategory(id int64) (bool, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	deleted, err := m.pg.DeleteTaskCategory(id)
	if err != nil || !deleted {
		return deleted, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, task := range m.tasks {
		task.updateLifecycle(func(state *taskLifecycleState) {
			if state.CategoryID != nil && *state.CategoryID == id {
				state.CategoryID = nil
				state.CategoryName = ""
			}
		})
	}
	return true, nil
}

// SetTaskCategory는 살아 있는 작업 하나의 분류를 바꿉니다. 실행은 끊지 않습니다.
func (m *Manager) SetTaskCategory(taskID string, categoryID *int64) (*pgdb.TaskCategory, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		return nil, pgdb.ErrTaskCategoryTaskNotFound
	}
	category, err := m.pg.SetTaskCategory(id, categoryID)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	task := m.tasks[taskID]
	m.mu.RUnlock()
	if task != nil {
		task.updateLifecycle(func(state *taskLifecycleState) {
			state.CategoryID = cloneInt64Ptr(categoryID)
			state.CategoryName = ""
			if category != nil {
				state.CategoryName = category.Name
			}
		})
	}
	return category, nil
}

// SetTasksCategory는 분류 변경 하나를 작업 여러 개에 한 번에 적용합니다.
// DB 쓰기와 메모리 속 갱신이 taskStateMu를 같이 잡으므로, 동시에 온
// 작업 하나 변경이 끼어들어 살아 있는 DTO를 낡게 두지 않습니다. 돌려주는
// 집합은 실제로 옮긴 id입니다. 나머지는 없어진 것으로 호출자가 알립니다.
func (m *Manager) SetTasksCategory(taskIDs []string, categoryID *int64) (map[string]bool, *pgdb.TaskCategory, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	numeric := make([]int64, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		id, err := strconv.ParseInt(taskID, 10, 64)
		if err != nil || id <= 0 {
			return nil, nil, pgdb.ErrTaskCategoryTaskNotFound
		}
		numeric = append(numeric, id)
	}
	updatedIDs, category, err := m.pg.SetTasksCategory(numeric, categoryID)
	if err != nil {
		return nil, nil, err
	}
	categoryName := ""
	if category != nil {
		categoryName = category.Name
	}
	updated := make(map[string]bool, len(updatedIDs))
	m.mu.RLock()
	tasks := make([]*Task, 0, len(updatedIDs))
	for _, id := range updatedIDs {
		taskID := strconv.FormatInt(id, 10)
		updated[taskID] = true
		if task := m.tasks[taskID]; task != nil {
			tasks = append(tasks, task)
		}
	}
	m.mu.RUnlock()
	for _, task := range tasks {
		task.updateLifecycle(func(state *taskLifecycleState) {
			state.CategoryID = cloneInt64Ptr(categoryID)
			state.CategoryName = categoryName
		})
	}
	return updated, category, nil
}

// DeleteCompanyWithAssets는 DB 연쇄 삭제와 살아 있는 작업 손잡이를
// 매니저 수준의 한 임계 구역에 둡니다. 작업이 등록 직전에
// 기업 범위를 확정하고, 삭제 뒤 메모리 청소를
// 놓치는 틈을 닫습니다.
func (m *Manager) DeleteCompanyWithAssets(id int64, deleteAssets bool) (int64, error) {
	m.companyMu.Lock()
	defer m.companyMu.Unlock()
	assetsDeleted, err := m.pg.Companies().DeleteCompanyWithAssets(id, deleteAssets)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	for _, task := range m.tasks {
		task.updateLifecycle(func(state *taskLifecycleState) {
			companyIDs := make([]int64, 0, len(state.CompanyIDs))
			for _, companyID := range state.CompanyIDs {
				if companyID != id {
					companyIDs = append(companyIDs, companyID)
				}
			}
			state.CompanyIDs = companyIDs
		})
	}
	m.mu.Unlock()
	return assetsDeleted, nil
}

// ReplaceTaskLLMProfiles는 작업의 순서 있는 제공자 사슬을 다시 정하고, 확정된
// 상태를 살아 있는 작업 손잡이에 비춥니다. 끝난 작업도 고칠 수 있습니다 —
// 작업이 끝난 뒤에도 그들의 메인 에이전트 대화는 체인 위에서 계속 이어진다.
func (m *Manager) ReplaceTaskLLMProfiles(id string, profileIDs []int64, activeProfileID int64) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, err
	}
	if err := m.pg.ReplaceTaskLLMProfiles(n, profileIDs, activeProfileID); err != nil {
		return 0, err
	}
	pt, err := m.pg.GetTask(n)
	if err != nil || pt == nil {
		return 0, err
	}
	m.mu.Lock()
	if task := m.tasks[id]; task != nil {
		task.setLLMState(pt.LLMProfileID, pt.ActiveLLMProfileID, pt.LLMProfileIDs, pt.LLMChainRevision, pt.LLMFailoverState, pt.LLMFailoverReason)
	}
	m.mu.Unlock()
	// 종료 상태 작업은 할당량으로 막힌 의도를 다시 열지 않는다: 작업에는 이미 실행 중인 워커가 없고, 다시 열면 그것들을
	// blocked 에서 open 으로 옮길 뿐이다——그곳에는 실행할 주체가 없고, 「의도 재실행」의 재실행 조건도 더 이상 만족하지 않아,
	// 오히려 죽은 상태가 된다. 종료 상태 작업을 이어서 실행하려면 의도 재실행/목표 추가로 가고, 그 길은 작업을 다시
	// admit 하여 실행 상태로 되돌린다.
	if pgdb.IsTerminal(pt.Status) {
		return 0, nil
	}
	if task, ok := m.Task(id); ok {
		reopened, reopenErr := task.Store.ReopenIntentsByBlockedReason(pgdb.IntentBlockedLLMQuota)
		if reopenErr != nil {
			return reopened, reopenErr
		}
		return reopened, nil
	}
	return 0, nil
}

// LoadExisting은 PG 작업 목록에서 메모리 속 작업 손잡이를 다시 만듭니다.
func (m *Manager) LoadExisting() []*Task {
	pts, err := m.pg.ListTasks()
	if err != nil {
		log.Printf("[manager] reload: %v", err)
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var loaded []*Task
	for _, pt := range pts {
		id := strconv.FormatInt(pt.ID, 10)
		if _, ok := m.tasks[id]; ok {
			continue
		}
		t := taskFromPG(pt, m.pg.Exploration(pt.ExplorationID), m.interceptor)
		m.tasks[id] = t
		loaded = append(loaded, t)
	}
	if m.active == "" {
		var newest *Task
		for _, t := range m.tasks {
			if newest == nil || t.CreatedAt > newest.CreatedAt {
				newest = t
			}
		}
		if newest != nil {
			m.active = newest.ID
		}
	}
	if len(loaded) > 0 {
		log.Printf("[manager] reloaded %d task(s) from PG", len(loaded))
	}
	return loaded
}

// SetTaskPaused는 작업의 일시정지 상태를 저장합니다.
func (m *Manager) SetTaskPaused(id string, paused bool) error {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return err
	}
	if err := m.pg.SetPaused(n, paused); err != nil {
		return err
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.Paused = paused
		})
	}
	m.mu.Unlock()
	return nil
}

// ApplyTaskAdmission은 동시성 스케줄러가 다루는 수명 필드를 원자적으로 확정합니다.
// 상태, 일시정지, 대기열 메타데이터를 UPDATE 하나에 두어,
// 실패한 재개가 작업을 반만 살리지 않게 합니다(예를 들어 실행 중인데
// 여전히 사용자 일시정지, 또는 대기열에서 뺐는데 엔진은 안 시작).
//
// preservePosition은 그 행이 이미 대기 중일 때만 적용됩니다. 같은
// 입장을 반복하면 FIFO 시각을 유지합니다. 명시적으로 일시정지했다가
// 다시 대기열에 넣는 작업은 새 꼬리 위치를 받습니다.
// 초보용: 동시에 몇 작업을 돌릴지 정하는 스케줄러가, 상태와 대기열을 한 번에 확정합니다.
func (m *Manager) ApplyTaskAdmission(id, expectedStatus, status string, queued bool, mode string, preservePosition bool) error {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return err
	}
	if queued {
		if mode != "bootstrap" && mode != "resume" {
			return fmt.Errorf("invalid queue mode %q", mode)
		}
	} else {
		mode = ""
	}

	var queuedAt, completedAt, firstRunAt, deadlineAt sql.NullTime
	var committedMode string
	err = m.pg.QueryRow(`UPDATE tasks
	SET status=$2,
	    completed_at=CASE
	        WHEN $2 IN ('done','failed','timeout') THEN COALESCE(completed_at, now())
	        ELSE NULL
	    END,
	    paused=false,
	    queued=$3,
	    queued_at=CASE
	        WHEN NOT $3 THEN NULL
	        WHEN $5 AND queued THEN COALESCE(queued_at, now())
	        ELSE now()
	    END,
	    queue_mode=CASE
	        WHEN NOT $3 THEN ''
	        WHEN ($5 AND queued AND queue_mode='bootstrap') OR $4='bootstrap' THEN 'bootstrap'
	        ELSE 'resume'
	    END,
	    first_run_at=CASE
	        WHEN $6='timeout' AND $2 NOT IN ('done','failed','timeout') THEN NULL
	        ELSE first_run_at
	    END,
	    deadline_at=CASE
	        WHEN $6='timeout' AND $2 NOT IN ('done','failed','timeout') THEN NULL
	        ELSE deadline_at
	    END
	WHERE id=$1 AND deleted_at IS NULL AND status=$6
	RETURNING queued_at, queue_mode, completed_at, first_run_at, deadline_at`, n, status, queued, mode, preservePosition, expectedStatus).
		Scan(&queuedAt, &committedMode, &completedAt, &firstRunAt, &deadlineAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task %s lifecycle changed before admission (expected status %q)", id, expectedStatus)
	}
	if err != nil {
		return err
	}

	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.Status = status
			state.Paused = false
			state.Queued = queued
			state.QueueMode = committedMode
			state.QueuedAt = 0
			if queuedAt.Valid {
				state.QueuedAt = queuedAt.Time.UnixNano()
			}
			state.CompletedAt = 0
			if completedAt.Valid {
				state.CompletedAt = completedAt.Time.Unix()
			}
			state.FirstRunAt = 0
			if firstRunAt.Valid {
				state.FirstRunAt = firstRunAt.Time.Unix()
			}
			state.DeadlineAt = 0
			if deadlineAt.Valid {
				state.DeadlineAt = deadlineAt.Time.Unix()
			}
		})
	}
	m.mu.Unlock()
	return nil
}

// ApplyTaskPause는 작업을 입장 대기열에서 원자적으로 빼고
// 사용자 일시정지를 기록합니다. queue_mode는 일부러 남깁니다. 한 번도 안 돈
// 부트스트랩 작업을 재개해도 목표 분해를 하게 하려고요. 다만 다음 대기열 넣기는
// 새 queued_at 시각을 받아 FIFO 꼬리로 갑니다.
func (m *Manager) ApplyTaskPause(id string) error {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return err
	}
	var mode string
	err = m.pg.QueryRow(`UPDATE tasks
		SET paused=true, queued=false, queued_at=NULL
		WHERE id=$1 AND deleted_at IS NULL AND paused=false
		  AND status NOT IN ('done','failed','timeout')
		RETURNING COALESCE(queue_mode,'')`, n).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task %s is unavailable for pause", id)
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.Paused = true
			state.Queued = false
			state.QueuedAt = 0
			state.QueueMode = mode
		})
	}
	m.mu.Unlock()
	return nil
}

// EnqueueTask는 동시성 보류를 저장하고 메모리 속 손잡이를 맞춥니다.
func (m *Manager) EnqueueTask(id, mode string) error {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return err
	}
	if mode != "bootstrap" && mode != "resume" {
		return fmt.Errorf("invalid queue mode %q", mode)
	}
	var queuedAt time.Time
	var committedMode string
	err = m.pg.QueryRow(`UPDATE tasks
		SET queued=true,
		    queued_at=CASE WHEN queued THEN COALESCE(queued_at, now()) ELSE now() END,
		    queue_mode=CASE
		        WHEN queue_mode='bootstrap' OR $2='bootstrap' THEN 'bootstrap'
		        ELSE 'resume'
		    END
		WHERE id=$1 AND deleted_at IS NULL
		RETURNING queued_at, queue_mode`, n, mode).Scan(&queuedAt, &committedMode)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("task %s is unavailable for enqueue", id)
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.QueuedAt = queuedAt.UnixNano()
			state.Queued = true
			state.QueueMode = committedMode
		})
	}
	m.mu.Unlock()
	return nil
}

// DequeueTask는 동시성 보류를 뺍니다. clearMode=false는 사용자가
// 대기 중인 작업을 일시정지할 때 씁니다. 나중 재개가 부트스트랩이 필요한지 알게 하려고요.
func (m *Manager) DequeueTask(id string, clearMode bool) error {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return err
	}
	if err := m.pg.Dequeue(n, clearMode); err != nil {
		return err
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.Queued = false
			state.QueuedAt = 0
			if clearMode {
				state.QueueMode = ""
			}
		})
	}
	m.mu.Unlock()
	return nil
}

// TaskStatus는 작업의 현재 메모리 속 상태를 돌려줍니다(모르면 빈 문자열).
func (m *Manager) TaskStatus(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t := m.tasks[id]; t != nil {
		return t.lifecycleSnapshot().Status
	}
	return ""
}

// StampTaskFirstRun은 첫 진짜 실행 때 first_run_at과 deadline_at을 찍습니다(한 번만, 이미
// DB에) 있고, 라이브 핸들의 deadline_at 에 그대로 비춘다. deadline 의 unix 시각을 반환한다 (0 = 제한 없음).
func (m *Manager) StampTaskFirstRun(id string) (int64, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, err
	}
	m.mu.RLock()
	timeout := 0
	if t := m.tasks[id]; t != nil {
		timeout = t.TimeoutSeconds
	}
	m.mu.RUnlock()
	dl, err := m.pg.StampFirstRun(n, timeout)
	if err != nil {
		return 0, err
	}
	var dlUnix int64
	if dl != nil {
		dlUnix = dl.Unix()
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			if state.FirstRunAt == 0 {
				state.FirstRunAt = time.Now().Unix()
			}
			state.DeadlineAt = dlUnix
		})
	}
	m.mu.Unlock()
	return dlUnix, nil
}

// SetTaskStatusGuarded는 작업이 이미 끝나지 않았을 때만 종료 상태를 넣습니다
// (완료와 시간 초과의 경주를 풉니다. 먼저 종료를 쓴 쪽이 이김). 이긴
// 상태를 살아 있는 손잡이에 비춥니다. won=false면 다른 종료가 이미 붙었습니다.
func (m *Manager) SetTaskStatusGuarded(id, status string) (won bool, err error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return false, err
	}
	won, err = m.pg.SetTerminalStatusGuarded(n, status)
	if err != nil || !won {
		return won, err
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.Status = status
			if state.CompletedAt == 0 {
				state.CompletedAt = time.Now().Unix()
			}
		})
	}
	m.mu.Unlock()
	return true, nil
}

// SetTaskStatus는 작업의 수명 상태를 저장합니다(예: done). 그리고
// 메모리 속 손잡이에 비춰, 다시 읽지 않아도 파생 DTO 상태에 나오게 합니다.
func (m *Manager) SetTaskStatus(id, status string) error {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return err
	}
	if err := m.pg.SetStatus(n, status); err != nil {
		return err
	}
	m.mu.Lock()
	if t := m.tasks[id]; t != nil {
		t.updateLifecycle(func(state *taskLifecycleState) {
			state.Status = status
			// DB의 completed_at 도장을 살아 있는 손잡이에도 비춥니다. 그래서 DTO가
			// 다시 읽지 않고 끝난 시각을 보여 줍니다(종료면 한 번 찍고, 아니면 지움).
			if pgdb.IsTerminal(status) {
				if state.CompletedAt == 0 {
					state.CompletedAt = time.Now().Unix()
				}
			} else {
				state.CompletedAt = 0
			}
		})
	}
	m.mu.Unlock()
	return nil
}

// DeleteTask는 작업과, 고르면 관련된 전역 데이터도 지웁니다. 트래픽에는
// 작업 id 열이 없어서, 관련된 교환은 그 작업의 자산 행에 있는 호스트를
// 정확히 맞춰 찾습니다. 파일은 DB 작업 전에 치워 둡니다. 트래픽은
// PostgreSQL이 자산/앵커 쓰는 쪽을 막는 동안 치워 둡니다. 둘 다 DB
// 커밋 전 DB 실패 때 되돌리고, DB 커밋 뒤 외부 파일·트래픽 정리를 확정합니다.
// 초보용: DB 커밋 전 실패는 옮겨 둔 파일과 트래픽을 복원합니다. 커밋 후 정리가 실패하면
// 작업 삭제는 이미 확정되어 있고 정리 오류를 돌려줍니다. 모든 실패가 전체 복원을 뜻하지 않습니다.
func (m *Manager) DeleteTask(id string, opts DeleteTaskOptions) (DeleteTaskResult, error) {
	result := DeleteTaskResult{Deleted: id}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return result, err
	}
	registered, err := m.pg.GetTask(n)
	if err != nil {
		return result, err
	}
	if registered == nil {
		m.forgetTask(id, n)
		return result, nil
	}

	var fileStage *taskFileDeleteStage
	if opts.DeleteFiles {
		fileStage, err = stageTaskFiles(m.dir, id, registered.ExplorationID)
		if err != nil {
			return result, err
		}
		result.FilesDeleted = fileStage.deleted
	}

	var trafficStage *traffic.HostDeleteStage
	var prepare func(pgdb.TaskDeletePreparation) error
	if opts.DeleteTraffic && m.traffic != nil {
		prepare = func(p pgdb.TaskDeletePreparation) error {
			if len(p.TrafficHosts) == 0 {
				return nil
			}
			trafficStage, err = m.traffic.StageDeleteHostsExact(p.TrafficHosts)
			if err != nil {
				return err
			}
			result.TrafficDeleted = trafficStage.Deleted()
			return nil
		}
	}

	dbResult, err := m.pg.DeleteTaskCascadePrepared(
		n, opts.DeleteAssets, opts.DeleteFindings, opts.DeleteLLMRecords, prepare,
	)
	if err != nil {
		return result, rollbackTaskDelete(err, trafficStage, fileStage)
	}
	result.AssetsDeleted = dbResult.AssetsDeleted
	result.AssetsDetached = dbResult.AssetsDetached
	result.FindingsDeleted = dbResult.FindingsDeleted
	result.LLMRecordsDeleted = dbResult.LLMRecordsDeleted

	// 이제 PostgreSQL이 기준입니다. 치워 둔 바깥 삭제를 끝내고,
	// 마지막 제거가 오류를 내도 살아 있는 작업은 잊습니다. 그런 오류는
	// 형이 있어서, HTTP 층이 작업 런타임을 내릴 수 있습니다.
	// DB 행이 이미 없는 작업을 잘못 되살리지 않으려고요.
	var finalizeErrs []error
	if trafficStage != nil {
		if err := trafficStage.Commit(); err != nil {
			finalizeErrs = append(finalizeErrs, fmt.Errorf("finalize traffic deletion: %w", err))
		}
	}
	if fileStage != nil {
		if err := fileStage.commit(); err != nil {
			finalizeErrs = append(finalizeErrs, fmt.Errorf("finalize task file deletion: %w", err))
		}
	}
	m.forgetTask(id, n)
	if err := errors.Join(finalizeErrs...); err != nil {
		return result, &taskDeleteCommittedError{err: err}
	}
	return result, nil
}

// taskDeleteCommittedError는 PostgreSQL 삭제는 됐는데, 되돌릴 수 있는
// 임시 디렉터리 중 하나를 없애는 데 실패한 경우입니다. 작업은 지운 채로 둬야 합니다.
type taskDeleteCommittedError struct{ err error }

func (e *taskDeleteCommittedError) Error() string {
	return "task deletion committed; external cleanup incomplete: " + e.err.Error()
}

func (e *taskDeleteCommittedError) Unwrap() error { return e.err }

func rollbackTaskDelete(cause error, trafficStage *traffic.HostDeleteStage, fileStage *taskFileDeleteStage) error {
	errs := []error{cause}
	// 준비 순서를 거꾸로 합니다. 첫 복원이 실패해도 둘 다 시도하고,
	// errors.Join이 원래 PostgreSQL 오류를 유지합니다.
	if trafficStage != nil {
		if err := trafficStage.Rollback(); err != nil {
			errs = append(errs, fmt.Errorf("restore traffic after task delete failure: %w", err))
		}
	}
	if fileStage != nil {
		if err := fileStage.rollback(); err != nil {
			errs = append(errs, fmt.Errorf("restore task files after task delete failure: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) forgetTask(id string, numericID int64) {
	m.mu.Lock()
	delete(m.tasks, id)
	for _, task := range m.tasks {
		task.updateLifecycle(func(state *taskLifecycleState) {
			kept := make([]int64, 0, len(state.SourceTaskIDs))
			for _, sourceID := range state.SourceTaskIDs {
				if sourceID != numericID {
					kept = append(kept, sourceID)
				}
			}
			state.SourceTaskIDs = kept
		})
	}
	if m.active == id {
		m.active = ""
		for _, t := range m.tasks {
			m.active = t.ID
			break
		}
	}
	m.mu.Unlock()
}

type stagedTaskPath struct {
	source string
	staged string
}

type taskFileDeleteStage struct {
	stageDir string
	moves    []stagedTaskPath
	deleted  bool
	done     bool
}

// stageTaskFiles는 그 작업의 작업 공간과 그 탐색의 대화 기록을 같은 파일시스템의
// 임시 디렉터리로 원자적으로 바꿉니다. 대화 기록
// 접두사 끝의 대시는 중요합니다. 탐색 12가 탐색 123과 겹치면 안 됩니다.
func stageTaskFiles(dataDir, taskID string, explorationID int64) (*taskFileDeleteStage, error) {
	stage := &taskFileDeleteStage{}
	var targets []string
	taskDir := filepath.Join(dataDir, "tasks", taskID)
	if _, err := os.Lstat(taskDir); err == nil {
		targets = append(targets, taskDir)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	transcriptDir := filepath.Join(dataDir, "transcripts")
	entries, err := os.ReadDir(transcriptDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		entries = nil
	}
	prefix := fmt.Sprintf("exp%d-", explorationID)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || (!entry.IsDir() && !strings.HasSuffix(name, ".jsonl")) {
			continue
		}
		targets = append(targets, filepath.Join(transcriptDir, name))
	}
	if len(targets) == 0 {
		stage.done = true
		return stage, nil
	}

	parent := filepath.Join(dataDir, ".delete-staging")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	stage.stageDir, err = os.MkdirTemp(parent, "task-"+taskID+"-")
	if err != nil {
		return nil, err
	}
	for _, source := range targets {
		staged := filepath.Join(stage.stageDir, fmt.Sprintf("%d-%s", len(stage.moves), filepath.Base(source)))
		if err := os.Rename(source, staged); err != nil {
			cause := fmt.Errorf("stage task file %s: %w", source, err)
			if restoreErr := stage.rollback(); restoreErr != nil {
				return nil, errors.Join(cause, fmt.Errorf("restore partially staged task files: %w", restoreErr))
			}
			return nil, cause
		}
		stage.moves = append(stage.moves, stagedTaskPath{source: source, staged: staged})
	}
	stage.deleted = true
	return stage, nil
}

func (s *taskFileDeleteStage) commit() error {
	if s == nil || s.done {
		return nil
	}
	err := os.RemoveAll(s.stageDir)
	s.done = true
	return err
}

func (s *taskFileDeleteStage) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.moves) - 1; i >= 0; i-- {
		move := s.moves[i]
		if _, err := os.Lstat(move.source); err == nil {
			errs = append(errs, fmt.Errorf("restore destination already exists: %s", move.source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("inspect restore destination %s: %w", move.source, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.source), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("create restore parent for %s: %w", move.source, err))
			continue
		}
		if err := os.Rename(move.staged, move.source); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", move.source, err))
		}
	}
	if len(errs) == 0 && s.stageDir != "" {
		if err := os.RemoveAll(s.stageDir); err != nil {
			errs = append(errs, fmt.Errorf("remove task file stage: %w", err))
		}
	}
	s.done = true
	return errors.Join(errs...)
}

// deleteTaskFiles는 좁은 테스트가 쓰는, 따로 선 도우미의 약속을 유지합니다.
func deleteTaskFiles(dataDir, taskID string, explorationID int64) (bool, error) {
	stage, err := stageTaskFiles(dataDir, taskID, explorationID)
	if err != nil {
		return false, err
	}
	deleted := stage.deleted
	if err := stage.commit(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (m *Manager) Task(id string) (*Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	return t, ok
}

// ActiveTask는 지금 활성인 작업을 돌려줍니다(없으면 nil).
func (m *Manager) ActiveTask() *Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == "" {
		return nil
	}
	return m.tasks[m.active]
}

// SetActive는 활성 작업을 바꿉니다. id를 모르면 false입니다.
func (m *Manager) SetActive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return false
	}
	m.active = id
	return true
}

func (m *Manager) ResolveTask(id string) *Task {
	if id == "" || id == "active" {
		return m.ActiveTask()
	}
	t, _ := m.Task(id)
	return t
}

func (m *Manager) List() []*Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	// 상단 고정 작업이 우선이고, 그룹 안에서는 고정 시각의 내림차순이다. 일반 작업은 id 내림차순이다. m.tasks 는 map 이라,
	// 폴링할 때마다 다시 정렬해야 한다. id 는 같은 시각에 만든 작업에 안정적이고 유일한 마지막 순서를 준다.
	sort.Slice(out, func(i, j int) bool {
		iState := out[i].lifecycleSnapshot()
		jState := out[j].lifecycleSnapshot()
		if (iState.PinnedAt > 0) != (jState.PinnedAt > 0) {
			return iState.PinnedAt > 0
		}
		if iState.PinnedAt != jState.PinnedAt {
			return iState.PinnedAt > jState.PinnedAt
		}
		ai, _ := strconv.ParseInt(out[i].ID, 10, 64)
		aj, _ := strconv.ParseInt(out[j].ID, 10, 64)
		return ai > aj
	})
	return out
}

// Notify는 자산 그래프나 탐색 그래프가 바뀌었다고 알립니다(모아서 받는 쪽이
// 플래너를 깨움). 막지 않습니다.
// 초보용: 그래프가 바뀌면 플래너 루프에 다음 계획 라운드 신호를 보냅니다.
func (t *Task) Notify() {
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// NotifyDone은 Notify에 힌트를 더합니다. 워커가 intentID를 막 끝냈고, 그것이
// 이번 깨움의 이유입니다. 플래너는 다음 라운드에서 쌓인 트리거를 읽어
// 어떤 의도가 끝났는지(그리고 그 출력)를 말할 수 있습니다. 이벤트는
// 라운드가 drainTriggers로 비울 때까지 쌓입니다(디바운스).
func (t *Task) NotifyDone(intentID int64) {
	if intentID > 0 {
		t.trigMu.Lock()
		t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "done", IntentID: intentID})
		t.trigMu.Unlock()
	}
	t.Notify()
}

// NotifyFinding은 워커가 intentID에서 발견을 보고했음을 기록하고(요약),
// 플래너를 깨웁니다. 그래서 라운드가 어떤 의도가 무엇을 찾았는지 말합니다.
func (t *Task) NotifyFinding(intentID int64, summary string) {
	t.trigMu.Lock()
	t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "finding", IntentID: intentID, Detail: summary})
	t.trigMu.Unlock()
	t.Notify()
}

// NotifyGoal은 set_goals 호출 한 번으로 목표가 하나 이상 추가됐음을 기록합니다.
// 사람이 메인 에이전트를 통해 넣었습니다. 그다음 플래너를 깨워, 다음 라운드가 다음을 말하게 합니다.
// "사람이 목표 N개를 추가했다: …"를 출력한다. 플래너가 새로 열린 목표를
// 개요에서 스스로 찾게 하는 대신이다. 호출 한 번 → 트리거 이벤트 하나 (set_goals 의 한 번 배치는 한 건으로 치고, 건마다 화면을 도배하지 않는다).
// 이 이벤트는 일찍 돌아오는 종료 라운드에서도 남습니다(비우기는 문 뒤에서 일어남).
// 그래서 끝난 작업을 되살리는 set_goals도, 작업이 다시 돌면 한 번은 드러납니다.
func (t *Task) NotifyGoal(texts []string) {
	if len(texts) == 0 {
		return
	}
	t.trigMu.Lock()
	t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "goal", Goals: texts})
	t.trigMu.Unlock()
	t.Notify()
}

// NotifyHint는 add_hint 호출 한 번으로 힌트가 하나 이상 추가됐음을 기록합니다.
// 사람이 메인 에이전트를 통하거나, 작업 사이 오케스트레이션이 넣습니다. 그다음
// 플래너에 알리므로, 다음 라운드는 "사람이 전략 힌트 N개를 추가했다: …"라는 안내를 받고 그것들을 본다
// 탐색 그래프 개요에 접힌 새 힌트를 스스로 찾지 않고 바로 봅니다.
// 호출 한 번이 트리거 이벤트 하나입니다(묶인 add_hint는 힌트마다 하나가 아니라 한 건).
func (t *Task) NotifyHint(texts []string) {
	if len(texts) == 0 {
		return
	}
	t.trigMu.Lock()
	t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "hint", Hints: texts})
	t.trigMu.Unlock()
	t.Notify()
}

// NotifyGoalDeleted 는 사람이 목표를 삭제했음을 기록하고(개요의 목표 관리를 통해), 그런 다음
// 플래너를 깨워, 다음 라운드가 어떤 목표가 빠졌는지 말하게 합니다. 이 이벤트는
// 일찍 돌아오는 종료 라운드에서도 남습니다(비우기는 문 뒤에서 일어남).
func (t *Task) NotifyGoalDeleted(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	t.trigMu.Lock()
	t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "goal_deleted", Detail: text})
	t.trigMu.Unlock()
	t.Notify()
}

// NotifyGoalEdited 는 사람이 목표를 수정했음을 기록하고(개요의 목표 관리를 통해), 그런 다음 깨운다
// 플래너를, 다음 라운드가 옛 글에서 새 글로의 변화를 말하게 합니다. 이 이벤트는
// 일찍 돌아오는 종료 라운드에서도 남습니다(비우기는 문 뒤에서 일어남).
func (t *Task) NotifyGoalEdited(oldText, newText string) {
	oldText, newText = strings.TrimSpace(oldText), strings.TrimSpace(newText)
	if newText == "" {
		return
	}
	t.trigMu.Lock()
	t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "goal_edited", OldGoal: oldText, NewGoal: newText})
	t.trigMu.Unlock()
	t.Notify()
}

// NotifyCancelled 는 사람이 intentID 를 삭제했음을 기록하고(reason = 삭제 이유), 그런 다음
// 플래너를 깨워, 다음 라운드가 어떤 의도가 왜 빠졌는지 말하게 합니다.
// summary는 삭제 전에 잡아 둔 의도의 글입니다. 완전 삭제에 필요합니다.
// 플래너가 트리거를 읽을 때쯤 노드는 이미 없기 때문입니다. 둘 다에 적용됩니다.
// 소프트 삭제(state가 deleted)와 하드 삭제(물리 연쇄)입니다.
func (t *Task) NotifyCancelled(intentID int64, summary, reason string) {
	if intentID > 0 {
		t.trigMu.Lock()
		t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "cancelled", IntentID: intentID, Summary: summary, Detail: reason})
		t.trigMu.Unlock()
	}
	t.Notify()
}

// drainTriggers는 지난 라운드 이후 쌓인 트리거 이벤트를 돌려주고 비웁니다.
func (t *Task) drainTriggers() []agent.TriggerEvent {
	t.trigMu.Lock()
	defer t.trigMu.Unlock()
	ev := t.pendingTriggers
	t.pendingTriggers = nil
	return ev
}
