package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/report"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// BuildVersion 는 백엔드 버전입니다. cmd/artex 가 켤 때 넣고, 그 값은
// -ldflags "-X main.version=<tag>" 에서 옵니다. 로컬 빌드의 기본은 "dev" 입니다.
// 화면은 GET /api/health 로 이 값을 봅니다.
var BuildVersion = "dev"

// Server 는 ARTEX 백엔드를 JSON HTTP API 로 엽니다. shadcn/ui 화면이 이 API 를 부릅니다.
// 초보용: 작업·자산 그래프·탐색 그래프의 주인(Manager)과 실행 루프(Engine)를 화면과 잇는 계층입니다.
type Server struct {
	m      *Manager
	engine *Engine
	ctx    context.Context

	skillDir string // 스킬 하위 디렉터리의 루트
	jwtKey   []byte // HS256 서명 키. dataDir/jwt.key 에서 읽거나 없으면 만들어 둡니다.

	// concMu 는 동시 실행 상한 결정(입장과 재조정)을 한 줄로 세웁니다. 스케줄 틱과
	// 설정 변경·작업 생성이 같은 스냅샷의 빈 자리를 같이 세면 상한을 넘겨 승격할 수 있습니다.
	concMu sync.Mutex

	cfgMu     sync.Mutex
	chatAgent *agent.ChatAgent // 채팅 화면의 대화 실행기. LLM 이 없으면 nil
	// llmProv 는 기록기와 장애 조치 사슬까지 붙인 전역 공급자입니다. applyLLM 이 넣습니다.
	// 작업이 자기 사슬을 비우면 작업 라우터가 이것을 씁니다.
	llmProv   llm.Provider
	llmDirect llm.Provider // 장애 조치 풀을 붙이기 전의 전역 공급자
	llmCfg    agent.Config // 현재 LLM 설정. 키는 밖으로 내보이지 않습니다.
	llmOn     bool
	llmProf   string // 활성 LLM 프로필 이름. llmrec 기록에 태그로 붙습니다.

	// chatBusy 는 작업마다 메인 에이전트 실행을 지킵니다. 채팅 처리기는 요청 문맥이 아니라
	// 서버 배경 문맥에서 에이전트를 띄우고 바로 돌아옵니다. 그래서 새로고침이나 프록시
	// 시간 초과가 돌고 있는 실행을 끊지 못합니다. 이 맵은 작업마다 차례를 하나씩만 허용해
	// 동시에 온 메시지가 exp<id>-main 기록을 망가뜨리지 않게 합니다.
	chatMu   sync.Mutex
	chatBusy map[string]bool
	// chatCancel 는 진행 중인 대화 실행의 취소 함수입니다. 키는 convBusyKey 입니다.
	// 사람이 멈추면 그 세션의 에이전트 실행만 끊습니다. chatMu 아래에서 chatBusy 와 같이
	// 넣고 뺍니다. 실행을 끊어도 P3 트리거 대기열은 건드리지 않습니다. 비우는 고루틴이
	// 다음 대기 항목으로 갑니다.
	chatCancel map[string]context.CancelCauseFunc

	// triggerQ 는 에이전트마다 P3 트리거 발화를 쌓습니다. 에이전트별 펌프가 전략에 따른
	// 동시 상한까지 실행을 띄웁니다. 직렬이면 상한 1(합치기는 선택), 병렬이면
	// trigger_max_parallel(0 은 무제한)이고 합치지 않습니다. triggerActive 는 에이전트별
	// 진행 수를 셉니다. 실행이 끝나면 하나를 빼고 빈 자리를 다시 채웁니다. triggerCfg 는
	// 마지막으로 읽은 전략을 기억해, 펌프가 queueMu 를 쥔 채 DB 를 묻지 않게 합니다.
	// 서로 다른 에이전트는 항상 동시에 돕니다. 대기열은 메모리에만 있습니다(chatBusy 와 같음).
	// 다시 켜면 대기 중인 발화는 버려지고, 스케줄러가 다음 틱의 워터마크부터 다시 쏩니다.
	queueMu       sync.Mutex
	triggerQ      map[string][]triggeredRun
	triggerActive map[string]int
	triggerCfg    map[string]triggerBehavior

	// profChatAgents 는 프로필 id 마다 채팅 화면용 ChatAgent 를 기억합니다.
	// 처음 쓸 때 만들고, 프로필을 저장·활성화·지우면 버려서 수정이 바로 반영됩니다.
	profMu         sync.Mutex
	profChatAgents map[int64]*agent.ChatAgent // 프로필별 ChatAgent 캐시(채팅 화면)

	// provByProfile 는 LLM 프로필 id 마다 공급자를 하나만 기억합니다. 같은 프로필에 묶인
	// 에이전트는 공급자 하나를 나눠 쓰므로 속도 제한도 하나입니다. applyLLM 도 저장된
	// 전역 활성 프로필을 이 캐시로 풀어서, 전역 폴백과 작업 사슬이 요청 속도를 두 배로
	// 세지 않습니다. 프로필을 고치면 비우고, 활성 프로필을 다시 적용할 때 채웁니다.
	provCacheMu   sync.Mutex
	provByProfile map[int64]*provEntry
	provCacheGen  uint64

	// llmHealth 는 프로세스 전체의 LLM 장애 조치 서킷 브레이커입니다(라운드 로빈).
	// 일부러 공급자 캐시 밖에 둡니다. 사슬을 다시 만들어도(다른 프로필 저장, 설정 변경)
	// 어느 백엔드가 잔액 부족인지, 속도 제한인지 배운 내용을 지우면 안 됩니다.
	llmHealth *llmpool.Registry

	// 작업이 사슬을 직접 정하면, 목표·플래너·워커·메인 에이전트가 그 작업의 동적 공급자
	// 하나를 나눕니다. 묶음은 그대로 두고, 호출마다 저장된 현재 프로필을 읽습니다.
	taskAgentMu sync.Mutex
	taskAgents  map[string]*taskAgentBundle

	// 식은 작업 보관은 상주 FIFO 워커 하나가 처리합니다. 버퍼된 깨움 채널이 몰린 대기열을
	// 모으고, 어디까지 했는지의 기준은 데이터베이스입니다.
	archiveWake chan struct{}
	archiveWG   sync.WaitGroup
	side        *sideQuestionState
}

// provEntry 는 LLM 프로필 id 하나의 캐시된 공급자와 그 설정입니다.
type provEntry struct {
	prov llm.Provider
	cfg  agent.Config
}

// triggeredRun 은 에이전트 차례를 기다리는 P3 트리거 발화 하나입니다.
// taskID 와 mergeable 로, 비우기가 같은 작업의 발견·목표 이벤트를 시작 전에 대화 하나로
// 합칩니다. 간격 발화는 합치지 않습니다.
type triggeredRun struct {
	agentKey  string
	title     string
	message   string // 이벤트 본문(트리거 문구 + 도구/인자/반환 등); 작업 설명/목표 머리글은 포함하지 않음
	taskID    int64  // 발견·목표 트리거의 원본 작업. 간격이거나 없으면 0
	taskDesc  string // 작업 설명(작업 수준, 같은 작업면 동일); 합칠 때 한 번만 렌더링
	taskGoal  string // 작업 목표(작업 수준, 같은 작업면 동일); 합칠 때 한 번만 렌더링
	mergeable bool   // 발견·목표 이벤트면 true. 같은 taskID 로 합칩니다.
}

func New(ctx context.Context, m *Manager, skillDir string, dataDir string, keyDir string) *Server {
	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil {
		log.Fatalf("[auth] JWT key: %v", err)
	}
	s := &Server{m: m, engine: NewEngine(m), ctx: ctx, skillDir: skillDir, jwtKey: key, chatBusy: map[string]bool{},
		chatCancel: map[string]context.CancelCauseFunc{}, triggerQ: map[string][]triggeredRun{},
		triggerActive: map[string]int{}, triggerCfg: map[string]triggerBehavior{},
		profChatAgents: map[int64]*agent.ChatAgent{},
		provByProfile:  map[int64]*provEntry{}, llmHealth: newLLMHealthRegistry(m.pg),
		taskAgents: map[string]*taskAgentBundle{}, archiveWake: make(chan struct{}, 1)}
	s.initSideQuestions()
	// 서킷 브레이커 임계값/쿨다운은 실패 경로의 핫 파라미터이며, 시작 시 전역 재시도 전략을 Registry에 한 번 밀어 넣습니다.
	// 이후 전략을 저장할 때마다 한 번 더 밀어 넣습니다(saveLLMRetryPolicy).
	s.applyRetryPolicy()
	// 모든 작업은 안정된 작업 라우터를 씁니다. 명시적 사슬이 비어 있으면 그 라우터가
	// 에이전트 바인딩을 보고, 그다음 전역 공급자를 봅니다. 그래서 돌고 있는 작업에
	// 첫 사슬을 더하면 바로 다음 LLM 호출부터 적용됩니다.
	s.engine.SetAuthoritativeAgentResolver(func(t *Task) (*agent.Planner, *agent.Worker) {
		if !s.taskRuntimeAvailable(t, "planner", "worker") {
			return nil, nil
		}
		b := s.agentsForTask(t)
		return b.pl, b.wk
	})
	// 전역 준비(Ready()/llm_configured)는 전역 활성 LLM 공급자가 깔려 있는지를 말합니다.
	// 작업이 실제로 돌 수 있는지는 별개입니다(ReadyFor 가 위의 해석기를 봅니다).
	s.engine.SetReadiness(func() bool {
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		return s.llmOn
	})
	// DB에 저장된 프롬프트 템플릿을 에이전트에 연결합니다(신규 방안 §3.3 / §5a). 없을 때는
	// 덮어쓰기 행이 없고, 에이전트는 내장 기본값을 유지합니다. 동작은 그대로입니다.
	if m.pg != nil {
		agent.PromptOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil {
				return "", false
			}
			t, err := m.pg.CurrentPrompt(a.ID)
			if err != nil || t == "" {
				return "", false
			}
			return t, true
		}
		// DB에 저장된 마무리(정산) 프롬프트를 연결합니다. 열이 비면 내장 기본값입니다.
		agent.WrapupOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupPrompt == "" {
				return "", false
			}
			return a.WrapupPrompt, true
		}
		// DB에 저장된 마무리 턴 예산을 연결합니다. 0 이거나 없으면 에이전트별 내장 기본값입니다.
		agent.WrapupMaxTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.WrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.WrapupMaxTurns, true
		}
		// DB에 저장된 작업 시간 초과 마무리 프롬프트와 턴(워커·플래너)을 연결합니다. 비었거나 0 이면 기본값입니다.
		agent.WrapupTaskTimeoutOverride = func(key string) (string, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupPrompt == "" {
				return "", false
			}
			return a.TaskTimeoutWrapupPrompt, true
		}
		agent.WrapupTaskTimeoutTurnsOverride = func(key string) (int, bool) {
			a, err := m.pg.GetAgentByKey(key)
			if err != nil || a == nil || a.TaskTimeoutWrapupMaxTurns <= 0 {
				return 0, false
			}
			return a.TaskTimeoutWrapupMaxTurns, true
		}
		wireAgentAugment(m.pg, s.skillDir, s.hostTools) // 보이는 skills/MCP와 트래픽/오케스트레이션 host 도구를 agent 도구 집합에 넣습니다. 워커(의도 하나를 실행한 뒤 정지)가 의도 하나를 실행할 때 이 집합을 씁니다.
		domainReg := buildDomainReg(m.Assets())
		wireTools(m.pg, domainReg) // 내장 도구 표: agent별로 거르고, 설명/schema를 덮어쓰며, 기본값을 주입합니다. 워커가 부르는 도구 정의이며, 자산 그래프와는 별개입니다.
		seedPrompts(m.pg)          // 내장 agent 기본 프롬프트 본문을 agent_prompts에 시드합니다(비어 있을 때만)
		s.seedOrchestrationTools() // P2 작업을 가로지르는 오케스트레이션 도구를 tools 표에 시드합니다(agent별로 묶을 수 있음)
		if err := s.seedFindingRetester(); err != nil {
			log.Printf("[retester] seed: %v", err)
		}
		go s.evidenceStore().RunGC(s.ctx)
		s.seedPythonInterpreter()     // 사용자 정의 스크립트 도구: 부팅 시 python 인터프리터를 검사해 넣습니다(비어 있을 때만)
		go newScheduler(s).Run(s.ctx) // P3 트리거 스케줄(타이머/finding/목표 이벤트)이며, 사용자 정의 agent만 해당합니다. 엔진이 작업을 깨우는 시계이며, 자산 그래프와 탐색 그래프를 직접 수정하지 않습니다.
		// 발견(finding) IM 푸시 전달 엔진입니다. Scheduler와 나란히 있으나 독립이고, 푸시는 실시간(3초)을 요구합니다. 발견(finding)을 밖으로 보내는 경로이며, 자산 그래프·탐색 그래프와는 분리됩니다.
		// 트리거의 업무 리듬과 다르고, 둘의 실패는 서로 엮이지 않습니다. 푸시가 멈춰도 agent 트리거에 영향을 주면 안 됩니다.
		go newNotifier(s).Run(s.ctx)
		// 켰는데 도구 캐시가 아직 없는 MCP 를 채웁니다. 첫 실행의 브라우저 MCP 가 대표적입니다.
		// 비동기로 돌려 부팅을 막지 않습니다.
		go s.discoverEmptyMCPsOnStartup()
		logSink.SetDB(ctx, m.pg) // 최근 로그 100줄을 되돌리고 비동기 저장을 켭니다.
	}
	// 우선순위: 저장된 DB 설정이 환경 변수보다 앞섭니다.
	if cfg, ok := s.loadLLMConfig(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] saved LLM config init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from DB: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else if cfg, ok := agent.FromEnv(); ok {
		if err := s.applyLLM(cfg); err != nil {
			log.Printf("[engine] env provider init failed — engine idle: %v", err)
		} else {
			log.Printf("[engine] LLM configured from env: %s / %s", cfg.Provider(), cfg.Model)
		}
	} else {
		log.Printf("[engine] no LLM provider configured — engine idle until set via /api/llm or env")
	}
	s.restoreTaskRuntimes()
	go s.reconcileConcurrency()
	s.startTaskArchiveWorker()
	s.wireInterceptReviewer() // LLM 최후 승인: 가로채기 규칙에 맞지 않은 명령을 모델이 판정합니다. 가드(guard) 뒤의 안전망이며, 자산 그래프에는 쓰지 않습니다.
	return s
}

// 복구한 마감·워커 루프는 새 작업과 같은 문맥을 물려받아야 합니다.
// 부팅 때 붙인 곁질문 체크포인트 발행자도 그 문맥에 있습니다.
func (s *Server) restoreTaskRuntimes() {
	m := s.m
	// 디스크에 남은 작업을 다시 읽어 재시작 뒤에도 목록이 남게 하고,
	// 저장된 일시정지 상태도 되돌립니다. 재시작 전에 멈춘 작업은 계속 멈춥니다.
	for _, t := range m.LoadExisting() {
		lifecycle := t.lifecycleSnapshot()
		// 이전 충돌·재시작 때문에 'running' 으로 남은 의도를 치웁니다. 살아있는 워커가
		// 없으므로, 화면에서 영원히 돌지 않고 다시 집어 갈 수 있게 합니다.
		if n, _ := t.Store.ResetRunningIntents(); n > 0 {
			log.Printf("[engine] task %s 잔여 running 의도 %d개를 open으로 재설정", t.ID, n)
		}
		if lifecycle.Paused {
			s.engine.Pause(t.ID, agent.AbortPausedOnReload)
		}
		// 작업 단위 시간 제한: 아직 종료 상태가 아니고 timeout이 있는 작업마다 deadline 조정기를 띄웁니다. 플래너(의도만 생성)/워커(의도 하나를 실행한 뒤 정지)와는 독립입니다
		// loop. 비활성 작업도 재시작 후 시각이 되면 마무리할 수 있습니다(deadline이 이미 지났으면 즉시 마무리 순서로 갑니다). 이 조정기는 엔진의 마감 처리이며, 자산 그래프와 탐색 그래프를 직접 고치지 않습니다.
		if !isTerminalStatus(lifecycle.Status) {
			s.engine.startDeadlineCoordinator(s.ctx, t)
		}
	}
	// 끄기 전에 이미 입장한 작업을 모두 되돌립니다. 화면의 활성 작업만 켜면 다른
	// 비대기 작업이 루프 없이 동시 실행 자리를 차지해, 상주 FIFO 가 영원히 막힐 수 있습니다.
	// 일시정지 루프는 쉬고, 대기 작업은 아래에서 자리가 날 때 입장합니다.
	for _, t := range m.List() {
		lifecycle := t.lifecycleSnapshot()
		if !lifecycle.Queued && !isTerminalStatus(lifecycle.Status) {
			s.engine.Run(s.ctx, t)
		}
	}
}

// agentMaxTurns 는 에이전트 키의 max_turns 입니다. 0 은 무제한이고, DB 나 행이 없을 때의 기본도 0 입니다.
func (s *Server) agentMaxTurns(key string) int {
	if s.m.pg == nil {
		return 0
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil {
		return 0
	}
	return a.MaxTurns
}

// agentRunSeconds 는 에이전트 키의 벽시계 실행 예산(초)입니다. 0 은 무제한이고,
// DB 나 행이 없으면 스키마와 같이 1200 입니다.
func (s *Server) agentRunSeconds(key string) int {
	if s.m.pg == nil {
		return 1200
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil || a == nil {
		return 1200
	}
	return a.RunSecs
}

// loadLLMConfig 는 PostgreSQL 의 llm_profiles 에서 활성 LLM 프로필을 읽습니다.
func (s *Server) loadLLMConfig() (agent.Config, bool) {
	p, err := s.m.pg.ActiveProfile()
	if err != nil || p == nil {
		return agent.Config{}, false
	}
	cfg := agent.ConfigFrom(p.Format, p.Model, p.BaseURL, p.APIKey, p.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = p.RatePerSecond, p.RatePerMinute
	cfg.ContextWindowK = p.ContextWindowK
	cfg.ThinkingType = p.ThinkingType
	cfg.ReasoningEffort = p.ReasoningEffort
	cfg.Stream = p.Streaming
	cfg.MaxTokens, cfg.MaxTokensField = p.MaxTokens, p.MaxTokensField
	cfg.SessionHeaderKey = p.SessionHeaderKey
	s.applyProfileRetry(&cfg, p)
	if cfg.APIKey == "" {
		return cfg, false
	}
	s.cfgMu.Lock()
	s.llmProf = p.Name
	s.cfgMu.Unlock()
	return cfg, true
}

// saveLLMConfig 는 LLM 설정을 PostgreSQL 의 활성 "default" 프로필로 저장합니다.
func (s *Server) saveLLMConfig(cfg agent.Config) error {
	// cfg.Provider() 는 이미 세 형식 문자열 중 하나를 돌려줍니다
	// (anthropic / openai / openai-responses). DB CHECK 제약과 같습니다.
	format := cfg.Provider()
	var id int64
	// 이 legacy 엔드포인트의 요청 본문에는 폴링/송수신/출력 상한 파라미터가 없으므로, DB에 이미 저장된 값을 그대로 가져옵니다 —
	// 그렇지 않으면 저장할 때마다 profile의 priority, pool_exclude, streaming과 출력 상한을
	// (max_tokens / max_tokens_field) 조용히 영값으로 되돌립니다.
	var priority int
	var poolExclude bool
	streaming := true // 기존 DB/신규 생성은 기본 스트리밍
	var maxTokens int
	var maxTokensField string
	var sessionHeaderKey string
	var retry db.RetryOverride
	if profs, _ := s.m.pg.ListProfiles(); profs != nil {
		for _, p := range profs {
			if p.Name == "default" {
				id, priority, poolExclude, streaming = p.ID, p.Priority, p.PoolExclude, p.Streaming
				maxTokens, maxTokensField = p.MaxTokens, p.MaxTokensField
				sessionHeaderKey = p.SessionHeaderKey
				retry = p.Retry
				break
			}
		}
	}
	newID, err := s.m.pg.SaveProfile(&db.LLMProfile{
		ID: id, Name: "default", Format: format, Model: cfg.Model, BaseURL: cfg.BaseURL, Proxy: cfg.Proxy,
		APIKey: cfg.APIKey, RatePerSecond: cfg.RatePerSecond, RatePerMinute: cfg.RatePerMinute,
		ContextWindowK: cfg.ContextWindowK, ThinkingType: cfg.ThinkingType, ReasoningEffort: cfg.ReasoningEffort, IsDefault: true,
		Priority: priority, PoolExclude: poolExclude, Streaming: streaming,
		MaxTokens: maxTokens, MaxTokensField: maxTokensField, SessionHeaderKey: sessionHeaderKey,
		Retry: retry,
	})
	if err != nil {
		return err
	}
	return s.m.pg.SetActiveProfile(newID)
}

// reapplyActiveProfile 는 활성 DB 프로필로 엔진을 다시 읽습니다. 프로필을 저장하거나
// 활성화하면 재시작 없이 반영됩니다. 실패하면 로그만 남기고 돌고 있는 엔진은 그대로 둡니다.
func (s *Server) reapplyActiveProfile() {
	cfg, ok := s.loadLLMConfig()
	if !ok {
		return
	}
	if err := s.applyLLM(cfg); err != nil {
		log.Printf("[engine] reapply active profile failed: %v", err)
		return
	}
	log.Printf("[engine] LLM reapplied from active profile: %s / %s", cfg.Provider(), cfg.Model)
}

// webSearchFor 는 전역 웹 검색 설정(백엔드·키)을 에이전트 자신의 web_search 스위치로 가립니다.
// 백엔드와 키는 전역 설정에서 오고, 켜고 끄는 것은 에이전트마다 정합니다.
func (s *Server) webSearchFor(key string) agent.WebSearchOpts {
	o := s.m.WebSearchOpts()
	if a, err := s.m.pg.GetAgentByKey(key); err != nil || a == nil || !a.WebSearch {
		o.Enabled = false
	}
	return o
}

// nonStreamingResolver 는 프로필의 스트리밍 선택을 잡아 둔 함수를 돌려줍니다.
// 에이전트는 실행마다 이것을 읽습니다. 프로필이 바뀌면 applyLLM 이 에이전트를 다시 만들어
// 이 빌드에 적용 중인 값만 남습니다.
func nonStreamingResolver(cfg agent.Config) func() bool {
	nonStreaming := !cfg.Stream
	return func() bool { return nonStreaming }
}

// maxTokensResolver 는 답변 하나당 출력 상한에 대해 nonStreamingResolver 와 같습니다.
func maxTokensResolver(cfg agent.Config) func() int {
	maxTokens := cfg.MaxTokens
	return func() int { return maxTokens }
}

// applyLLM 은 cfg 로 플래너·워커·메인 에이전트를 다시 만들어 돌고 있는 엔진에
// 전역 활성 쌍으로 넣습니다. 실행 중에 불러도 됩니다. 화면이 LLM 을 설정할 때 씁니다.
func (s *Server) applyLLM(cfg agent.Config) error {
	var prov llm.Provider
	// 저장된 활성 프로필은 작업 사슬·에이전트 바인딩과 같은 캐시된 공급자를 써야 합니다.
	// 그렇지 않으면 경로마다 속도 제한이 따로 생깁니다.
	if active, _ := s.m.pg.ActiveProfile(); active != nil {
		if activeCfg, ok := s.loadProfileConfig(active.ID); ok && activeCfg == cfg {
			prov, _, _ = s.providerForProfile(active.ID)
		}
	}
	if prov == nil {
		var err error
		prov, err = cfg.NewProvider()
		if err != nil {
			return err
		}
		// 저장되지 않은 환경 변수 설정에는 프로필 캐시 키가 없습니다.
		s.cfgMu.Lock()
		profName := s.llmProf
		s.cfgMu.Unlock()
		prov = llmrec.Wrap(prov, s.m.PG(), cfg.Model, profName, cfg.ThinkingType, cfg.ReasoningEffort, s.m.LLMRecordEnabled)
		prov = bindSideProvider(prov, cfg, 0, profName)
	}
	s.cfgMu.Lock()
	s.llmDirect = prov
	s.cfgMu.Unlock()
	// LLM 폴링(기본 꺼짐): 활성 설정을 장애 조치 체인에 넣고, 현재 설정을 쓸 수 없으면 다음으로 자동 전환합니다.
	// 「전역 활성 설정」을 타는 이 경로에만 영향을 줍니다. agent 바인딩 / 작업 pin은 providerForProfile로 갑니다.
	// 기본은 여전히 그 설정을 단독으로 씁니다(poolForBinding 참고). 끄거나 대체 설정이 없으면 원래 provider를 반환하며, 동작은 그대로입니다.
	if act, err := s.m.pg.ActiveProfile(); err == nil && act != nil {
		prov = s.poolForActive(act.ID, prov, cfg)
	}
	// 전역 플래너·워커 쌍은 없습니다. 작업마다 agentsForTask 가 만든 쌍으로 돌고,
	// 엔진의 권한 있는 해석기가 그것을 고릅니다. 전역 준비(Ready()/llm_configured)는
	// 아래에서 켜는 s.llmOn 으로 알립니다.

	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	win := cfg.CompactionWindow()
	// 채팅은 전역 폴백으로 남습니다. ChatAgent 하나가 여러 에이전트 키를 맡고,
	// 키별 바인딩은 대화 때 정합니다(runConversationSync → chatAgentForProfile).
	// 메인 에이전트의 전역 인스턴스는 없습니다. 작업마다 agentsForTask 가 만듭니다.
	// 그래서 여기서는 메인 에이전트를 만들지 않습니다.
	s.cfgMu.Lock()
	// 채팅 에이전트는 키로 여러 사용자 정의 에이전트를 맡으므로 전역 옵션
	// (backend/key)이고, Chat 시점에 대화별 agent의 Enabled를 게이트합니다. 대화는 항상 활성 설정을 씁니다.
	s.chatAgent = agent.NewChatAgent(prov, cfg.Model, s.m.dir, tx, win) // 채팅 화면 실행기
	s.chatAgent.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	s.chatAgent.SetWebSearch(s.m.WebSearchOpts())
	s.chatAgent.SetGuard(s.chatGuard())
	s.chatAgent.SetNonStreaming(nonStreamingResolver(cfg))
	s.chatAgent.SetMaxTokens(maxTokensResolver(cfg))
	s.chatAgent.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(run마다 읽음)
	s.llmProv = prov
	s.llmCfg = cfg
	s.llmOn = true
	s.cfgMu.Unlock()
	s.invalidateTaskAgents()

	// 활성 작업을 깨웁니다. 엔진이 쉬고 있을 때 만든 작업이 탐색을 시작하게 합니다.
	if t := s.m.ActiveTask(); t != nil {
		t.Notify()
	}
	return nil
}

// loadProfileConfig 는 프로필 id 하나에서 키가 포함된 agent.Config 를 만듭니다.
// 프로필이 없거나 API 키가 없으면 ok 는 false 입니다.
func (s *Server) loadProfileConfig(id int64) (agent.Config, bool) {
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return agent.Config{}, false
	}
	cfg := agent.ConfigFrom(p.Format, p.Model, p.BaseURL, p.APIKey, p.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = p.RatePerSecond, p.RatePerMinute
	cfg.ContextWindowK = p.ContextWindowK
	cfg.ThinkingType = p.ThinkingType
	cfg.ReasoningEffort = p.ReasoningEffort
	cfg.Stream = p.Streaming
	cfg.MaxTokens, cfg.MaxTokensField = p.MaxTokens, p.MaxTokensField
	cfg.SessionHeaderKey = p.SessionHeaderKey
	s.applyProfileRetry(&cfg, p)
	if cfg.APIKey == "" {
		return cfg, false
	}
	return cfg, true
}

// effectiveProfileForAgent 는 에이전트가 쓸 LLM 프로필 id 를 고릅니다. 우선순위는
// 에이전트 바인딩(agents.llm_profile_id) → 고정(작업·대화) → nil 입니다. nil 이면
// 호출자가 전역 활성 프로필로 돌아갑니다. 지워진 프로필에 묶이는 일은 없습니다
// (FK ON DELETE SET NULL). 그 밖의 잘못된 값은 loadProfileConfig 가 버려서 호출자가 폴백합니다.
func (s *Server) effectiveProfileForAgent(agentKey string, pinID *int64) *int64 {
	if s.m.pg != nil && agentKey != "" {
		if a, _ := s.m.pg.GetAgentByKey(agentKey); a != nil && a.LLMProfileID != nil {
			return a.LLMProfileID
		}
	}
	return pinID
}

// resolveChatAgent 는 대화 하나의 ChatAgent 를 고릅니다. 에이전트 자신의 바인딩이나
// 이 대화가 고른 프로필이 먼저이고, 전역 활성 설정은 폴백입니다. 보내기 전 검사와
// 배경 실행기가 반드시 이것을 같이 써야 합니다. 두 경로가 다르게 고르면, 유효한
// 프로필을 골랐는데도 전역 설정이 없을 때 "LLM 미설정"으로 거절되던 경우가 다시 납니다.
func (s *Server) resolveChatAgent(c *db.Conversation) *agent.ChatAgent {
	ca := s.chatAgentRef()
	if eff := s.effectiveProfileForAgent(c.AgentKey, c.LLMProfileID); eff != nil {
		if pa := s.chatAgentForProfile(*eff); pa != nil {
			ca = pa
		}
	}
	return ca
}

// chatUnavailableReason 는 ChatAgent 를 못 고른 이유를 설명합니다. 설정을 추가할지,
// 하나를 켤지, 이 대화에 지정할지를 사람이 알 수 있습니다. 어느 경우인지 숨기는
// "설정 안 됨" 한 줄이 아닙니다.
func (s *Server) chatUnavailableReason() string {
	if s.m.pg != nil {
		if profiles, err := s.m.pg.ListProfiles(); err == nil && len(profiles) == 0 {
			return "LLM이 아직 없습니다. 시스템 → LLM 설정에서 설정을 추가하세요"
		}
		if active, err := s.m.pg.ActiveProfile(); err == nil && active == nil {
			return "활성화된 LLM 설정이 없습니다. 시스템 → LLM 설정에서 하나를 켜거나, 이 대화에 설정을 지정하세요"
		}
	}
	return "LLM이 준비되지 않아 대화할 수 없습니다. 시스템 → LLM 설정에 쓸 수 있고 켜진 설정이 있는지 확인하세요"
}

// providerForProfile 는 프로필 id 의 캐시된 공급자와 설정을 돌려줍니다. 같은 프로필에
// 묶이거나 고정된 에이전트는 공급자 하나(속도 제한 하나)를 나눕니다. 프로필이 없거나
// 잘못되면 ok 는 false 이고, 호출자는 전역 쌍으로 돌아갑니다.
func (s *Server) providerForProfile(id int64) (llm.Provider, agent.Config, bool) {
	s.provCacheMu.Lock()
	if e := s.provByProfile[id]; e != nil {
		s.provCacheMu.Unlock()
		return e.prov, e.cfg, true
	}
	generation := s.provCacheGen
	s.provCacheMu.Unlock()
	cfg, ok := s.loadProfileConfig(id)
	if !ok {
		return nil, agent.Config{}, false
	}
	prov, err := cfg.NewProvider()
	if err != nil {
		log.Printf("[engine] build provider for LLM profile %d failed: %v", id, err)
		return nil, agent.Config{}, false
	}
	// 기록기로 감싸고 이 프로필 이름을 태그로 붙입니다. llmrec 가 화면의 LLM 기록에 씁니다.
	if p, _ := s.m.pg.ProfileByID(id); p != nil {
		prov = llmrec.Wrap(prov, s.m.PG(), cfg.Model, p.Name, cfg.ThinkingType, cfg.ReasoningEffort, s.m.LLMRecordEnabled)
		prov = bindSideProvider(prov, cfg, id, p.Name)
	}
	s.provCacheMu.Lock()
	if generation != s.provCacheGen {
		s.provCacheMu.Unlock()
		return s.providerForProfile(id)
	}
	if e := s.provByProfile[id]; e != nil { // 경합에서 졌으면 먼저 넣은 쪽을 유지합니다.
		prov, cfg = e.prov, e.cfg
	} else {
		s.provByProfile[id] = &provEntry{prov: prov, cfg: cfg}
		log.Printf("[engine] built provider for LLM profile %d (%s / %s)", id, cfg.Provider(), cfg.Model)
	}
	s.provCacheMu.Unlock()
	return prov, cfg, true
}

// chatAgentForProfile 는 특정 LLM 프로필로 만든 ChatAgent 를 프로필 id 마다 기억합니다.
// 프로필이 없거나 API 키가 없으면 nil 입니다.
func (s *Server) chatAgentForProfile(id int64) *agent.ChatAgent {
	s.profMu.Lock()
	cached := s.profChatAgents[id]
	s.profMu.Unlock()
	if cached != nil {
		return cached
	}
	prov, cfg, ok := s.providerForProfile(id)
	if !ok {
		return nil
	}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	ca := agent.NewChatAgent(s.poolForBinding(id, prov, cfg), cfg.Model, s.m.dir, tx, cfg.CompactionWindow())
	ca.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	ca.SetWebSearch(s.m.WebSearchOpts())
	ca.SetGuard(s.chatGuard())
	ca.SetNonStreaming(nonStreamingResolver(cfg))
	ca.SetMaxTokens(maxTokensResolver(cfg))
	ca.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(run마다 읽음)
	s.profMu.Lock()
	if ex := s.profChatAgents[id]; ex != nil { // 경합에서 졌으면 먼저 넣은 쪽을 유지합니다.
		ca = ex
	} else {
		s.profChatAgents[id] = ca
	}
	s.profMu.Unlock()
	return ca
}

// invalidateProfileAgents 는 프로필별 ChatAgent 와 공급자 캐시를 버립니다. 프로필을
// 저장·활성화·지우거나 에이전트 바인딩이 바뀌면, 고정된 작업의 에이전트와 묶인 모델이
// 다음 라운드에 다시 만들어집니다.
func (s *Server) invalidateProfileAgents() {
	s.profMu.Lock()
	s.profChatAgents = map[int64]*agent.ChatAgent{}
	s.profMu.Unlock()
	s.provCacheMu.Lock()
	s.provCacheGen++
	s.provByProfile = map[int64]*provEntry{}
	s.provCacheMu.Unlock()
	s.invalidateTaskAgents()
}

func (s *Server) chatAgentRef() *agent.ChatAgent {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.chatAgent
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerSideRoutes(mux)

	// 인증 경로. JWT 검사에서 빠집니다(requireAuth 가 처리).
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/init", s.authInit)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/change-password", s.authChangePassword)

	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/logs", s.getLogs)
	mux.HandleFunc("GET /api/logs/history", s.getLogsHistory)
	mux.HandleFunc("GET /api/logs/stream", s.streamLogs)

	// 페이지에서 한 번에 업데이트합니다. 기본 JWT 인증을 탑니다(auth.go는 /api/auth/* 와
	// /api/health만 통과시키므로), 프로그램 자신을 바꾸는 이 인터페이스들은 원래 로그인이 필요합니다.
	mux.HandleFunc("GET /api/update/check", s.updateCheck)
	mux.HandleFunc("POST /api/update/apply", s.updateApply)
	mux.HandleFunc("POST /api/update/rollback", s.updateRollback)
	mux.HandleFunc("GET /api/update/stream", s.updateStream)

	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("GET /api/task-categories", s.pgListTaskCategories)
	mux.HandleFunc("POST /api/task-categories", s.pgCreateTaskCategory)
	mux.HandleFunc("PATCH /api/task-categories/{id}", s.pgRenameTaskCategory)
	mux.HandleFunc("DELETE /api/task-categories/{id}", s.pgDeleteTaskCategory)
	mux.HandleFunc("POST /api/tasks/category/batch", s.updateTasksCategoryBatch)
	mux.HandleFunc("GET /api/task-templates", s.pgListTaskTemplates)
	mux.HandleFunc("POST /api/task-templates", s.pgCreateTaskTemplate)
	mux.HandleFunc("PATCH /api/task-templates/{id}", s.pgUpdateTaskTemplate)
	mux.HandleFunc("DELETE /api/task-templates/{id}", s.pgDeleteTaskTemplate)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.updateTaskMetadata)
	mux.HandleFunc("PATCH /api/tasks/{id}/category", s.updateTaskCategory)
	// 작업 단위 자산 가로채기/allow 규칙. 엔진이 이 작업의 자산을 가로채거나 allow할 때 쓰며, 탐색 그래프의 의도와는 별개입니다.
	mux.HandleFunc("GET /api/tasks/{id}/intercept-rules", s.taskInterceptListRules)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules", s.taskInterceptCreateRule)
	mux.HandleFunc("PUT /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptDeleteRule)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules/{rid}/toggle", s.taskInterceptToggleRule)
	mux.HandleFunc("POST /api/tasks/control/batch", s.controlTasksBatch)
	mux.HandleFunc("GET /api/task-archives", s.listTaskArchives)
	mux.HandleFunc("GET /api/task-archives/{id}", s.getTaskArchive)
	mux.HandleFunc("POST /api/tasks/{id}/archive", s.queueTaskArchive)
	mux.HandleFunc("POST /api/tasks/archive/batch", s.queueTaskArchivesBatch)
	mux.HandleFunc("POST /api/task-archives/{id}/restore", s.queueTaskArchiveRestore)
	mux.HandleFunc("POST /api/task-archives/restore/batch", s.restoreTaskArchivesBatch)
	mux.HandleFunc("DELETE /api/task-archives/{id}", s.queueTaskArchiveDelete)
	mux.HandleFunc("POST /api/task-archives/delete/batch", s.deleteTaskArchivesBatch)
	mux.HandleFunc("GET /api/tasks/{id}/coverage", s.taskCoverage)
	mux.HandleFunc("GET /api/tasks/{id}/coverage-graph", s.taskCoverageGraph)
	mux.HandleFunc("GET /api/tasks/{id}/asset-refs", s.taskAssetRefs)
	mux.HandleFunc("POST /api/tasks/{id}/assets", s.attachTaskAssets)
	mux.HandleFunc("DELETE /api/tasks/{id}/assets/{assetID}", s.detachTaskAsset)
	mux.HandleFunc("GET /api/tasks/{id}/intent-assets", s.taskIntentAssets)

	// 작업 공간 파일 관리자(workDir 대상). UI에서 작업 작업 파일을 다루는 화면이며, 자산 그래프·탐색 그래프와는 별개입니다.
	mux.HandleFunc("GET /api/workspace/list", s.wsList)
	mux.HandleFunc("GET /api/workspace/read", s.wsRead)
	mux.HandleFunc("POST /api/workspace/write", s.wsWrite)
	mux.HandleFunc("POST /api/workspace/mkdir", s.wsMkdir)
	mux.HandleFunc("DELETE /api/workspace/delete", s.wsDelete)
	mux.HandleFunc("GET /api/workspace/download", s.wsDownload)
	mux.HandleFunc("POST /api/workspace/upload", s.wsUpload)
	mux.HandleFunc("GET /api/tasks/{id}/scope", s.taskScopeList)
	mux.HandleFunc("POST /api/tasks/{id}/scope", s.taskScopeAdd)
	mux.HandleFunc("DELETE /api/tasks/{id}/scope/{sid}", s.taskScopeDelete)
	mux.HandleFunc("GET /api/tasks/{id}/goals", s.listGoals)                       // 목표 관리: 이 작업의 목표를 모두 나열합니다
	mux.HandleFunc("POST /api/tasks/{id}/goals", s.addGoal)                        // 목표 관리: 사람이 목표를 추가합니다(작업을 되살림)
	mux.HandleFunc("PATCH /api/tasks/{id}/goals/{gid}", s.editGoal)                // 목표 관리: 목표를 수정합니다(작업을 되살림)
	mux.HandleFunc("DELETE /api/tasks/{id}/goals/{gid}", s.deleteGoal)             // 목표 관리: 목표를 하드 삭제합니다(되살리지 않음)
	mux.HandleFunc("GET /api/tasks/{id}/constraints", s.listConstraints)           // 제약 관리: 이 작업의 조작 제약을 나열합니다
	mux.HandleFunc("POST /api/tasks/{id}/constraints", s.addConstraint)            // 제약 관리: 제약을 추가합니다(플래너(의도만 생성)에 알리지 않음)
	mux.HandleFunc("PATCH /api/tasks/{id}/constraints/{cid}", s.editConstraint)    // 제약 관리: 제약을 수정합니다
	mux.HandleFunc("DELETE /api/tasks/{id}/constraints/{cid}", s.deleteConstraint) // 제약 관리: 제약을 삭제합니다
	mux.HandleFunc("POST /api/tasks/{id}/control", s.control)
	mux.HandleFunc("PUT /api/tasks/{id}/llm", s.updateTaskLLMProfiles)
	mux.HandleFunc("GET /api/tasks/{id}/llm/resolution", s.taskLLMResolutionHandler)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/control", s.controlIntent)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/messages", s.sendWorkerMessage)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/rerun", s.rerunIntent)    // blocked/exhausted/stopped인 의도 하나를 다시 실행합니다
	mux.HandleFunc("POST /api/tasks/{id}/intents/rerun-blocked", s.rerunBlocked) // 이 작업의 blocked 의도를 모두 일괄 재실행합니다
	mux.HandleFunc("POST /api/active", s.setActive)

	mux.HandleFunc("GET /api/llm", s.getLLM)
	mux.HandleFunc("POST /api/llm", s.setLLM)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)

	// 자산 그래프. 모든 작업이 공유하는 자산(도메인, IP, 서비스, 엔드포인트)입니다.
	mux.HandleFunc("GET /api/assets", s.listAssets)
	mux.HandleFunc("GET /api/assets/counts", s.assetCounts)
	mux.HandleFunc("POST /api/assets", s.insertAssets)
	mux.HandleFunc("DELETE /api/assets", s.deleteAssets)

	// 기업. 자산 그래프의 회사를 묶고 범위를 붙입니다.
	mux.HandleFunc("GET /api/companies", s.listCompanies)
	mux.HandleFunc("POST /api/companies", s.createCompany)
	mux.HandleFunc("GET /api/companies/{id}", s.getCompany)
	mux.HandleFunc("DELETE /api/companies/{id}", s.deleteCompany)
	mux.HandleFunc("POST /api/companies/{id}/scope", s.addCompanyScope)
	mux.HandleFunc("POST /api/companies/reattribute", s.reattribute)

	mux.HandleFunc("GET /api/exploration/frontier", s.frontier)
	mux.HandleFunc("GET /api/exploration/findings", s.findings)
	mux.HandleFunc("GET /api/exploration/findings/groups", s.findingGroups)
	mux.HandleFunc("GET /api/exploration/findings/asset-tree", s.findingAssetTree)
	mux.HandleFunc("GET /api/exploration/findings/stats", s.findingStats)
	mux.HandleFunc("GET /api/exploration/findings/export", s.findingsExport)
	s.registerFindingTraffic(mux)
	mux.HandleFunc("GET /api/exploration/findings/{id}", s.getFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/lineage", s.findingLineage)
	mux.HandleFunc("POST /api/exploration/findings/{id}/deepen", s.deepenFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/retests", s.listFindingRetests)
	mux.HandleFunc("GET /api/exploration/findings/retests/active", s.listActiveFindingRetests)
	mux.HandleFunc("POST /api/exploration/findings/{id}/retests", s.startFindingRetest)
	mux.HandleFunc("PATCH /api/exploration/findings/{id}", s.patchFinding)
	mux.HandleFunc("DELETE /api/exploration/findings/{id}", s.deleteFinding)
	mux.HandleFunc("GET /api/exploration/intents", s.intents)
	mux.HandleFunc("GET /api/exploration/graph", s.explorationGraph)
	mux.HandleFunc("GET /api/exploration/nodes", s.explorationNodes)
	mux.HandleFunc("GET /api/exploration/activity", s.activity)
	mux.HandleFunc("GET /api/exploration/activity/history", s.activityHistory)
	mux.HandleFunc("GET /api/exploration/main-sessions", s.mainSessions)
	mux.HandleFunc("POST /api/exploration/main-session/new", s.newMainSession)
	mux.HandleFunc("GET /api/exploration/activity/stream", s.streamActivity)
	mux.HandleFunc("GET /api/exploration/activity/{seq}", s.activityDetail)
	mux.HandleFunc("GET /api/exploration/tokens", s.tokenStats)
	mux.HandleFunc("GET /api/tokens/daily", s.tokenDailyStats)
	mux.HandleFunc("GET /api/tokens/conversations", s.conversationTokens)
	mux.HandleFunc("GET /api/tokens/usage", s.pgUsageStats) // 전역 llm_usage 집계(대시보드 신규 뷰)

	mux.HandleFunc("GET /api/audit", s.getAudit)
	mux.HandleFunc("POST /api/gc", s.gc)
	mux.HandleFunc("GET /api/traffic", s.getTraffic)
	mux.HandleFunc("GET /api/traffic/hosts", s.getTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic", s.deleteTraffic)
	mux.HandleFunc("DELETE /api/traffic/hosts", s.deleteTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic/all", s.deleteAllTraffic)
	mux.HandleFunc("GET /api/traffic/exchange", s.getTrafficExchange)
	mux.HandleFunc("GET /api/traffic/blob", s.getTrafficBlob)
	mux.HandleFunc("GET /api/commands", s.pgListCommands)
	mux.HandleFunc("GET /api/commands/stats", s.pgToolStats) // 도구별 호출 횟수 집계
	mux.HandleFunc("GET /api/llm/records", s.pgListLLMRecords)
	mux.HandleFunc("DELETE /api/llm/records", s.pgDeleteLLMRecords)
	mux.HandleFunc("GET /api/llm/records/tasks", s.pgLLMTasks)
	mux.HandleFunc("GET /api/llm/records/by-model", s.pgTokenByModel) // 모델별 이 작업의 token 사용량 집계
	mux.HandleFunc("GET /api/llm/records/{id}", s.pgGetLLMRecord)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	// 발견(finding) IM 푸시. 채널은 「다중 인스턴스 + 각자 필터 규칙」인 자원이라, 독립된 한 그룹의
	// REST 인터페이스로 두고, 평탄한 /api/settings 키-값에는 넣지 않습니다.
	mux.HandleFunc("GET /api/notify/meta", s.notifyMeta)
	mux.HandleFunc("GET /api/notify/channels", s.notifyListChannels)
	mux.HandleFunc("POST /api/notify/channels", s.notifyCreateChannel)
	mux.HandleFunc("PATCH /api/notify/channels/{id}", s.notifyUpdateChannel)
	mux.HandleFunc("DELETE /api/notify/channels/{id}", s.notifyDeleteChannel)
	mux.HandleFunc("POST /api/notify/channels/{id}/test", s.notifyTestChannel)
	mux.HandleFunc("GET /api/notify/deliveries", s.notifyListDeliveries)
	mux.HandleFunc("POST /api/notify/deliveries/{id}/retry", s.notifyRetryDelivery)
	mux.HandleFunc("POST /api/settings/web-search/test", s.testWebSearch)
	mux.HandleFunc("GET /api/report", s.getReport)
	mux.HandleFunc("GET /api/chat/mentions", s.searchChatMentions)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/chat/upload", s.chatUpload) // 방식 1 파일 업로드: 세션/작업 작업 디렉터리 uploads/에 저장됩니다
	mux.HandleFunc("GET /api/tasks/{id}/chat/status", s.taskChatStatus)
	mux.HandleFunc("POST /api/tasks/{id}/chat/stop", s.stopChat)

	// --- 관리 백엔드 API (PostgreSQL 데이터 소스; 신규 데이터베이스와 관리 백엔드 방안) --- 관리 UI가 PostgreSQL의 자산 그래프와 탐색 그래프를 읽고 쓰는 입구입니다.
	mux.HandleFunc("DELETE /api/tasks/{id}", s.pgDeleteTask)
	// 에이전트 정의와 채팅 화면의 대화. 메인 에이전트와 별도로, 사람이 고른 에이전트와의 대화입니다.
	mux.HandleFunc("GET /api/conversations", s.pgListConversations)
	mux.HandleFunc("POST /api/conversations", s.pgCreateConversation)
	mux.HandleFunc("POST /api/conversations/delete/batch", s.pgDeleteConversationsBatch)
	mux.HandleFunc("PATCH /api/conversations/{id}", s.pgRenameConversation)
	mux.HandleFunc("PATCH /api/conversations/{id}/profile", s.pgUpdateConversation)
	mux.HandleFunc("DELETE /api/conversations/{id}", s.pgDeleteConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.pgConversationMessages)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.pgSendConversationMessage)
	mux.HandleFunc("POST /api/conversations/{id}/stop", s.pgStopConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages/{seq}", s.pgConversationMsgDetail)

	mux.HandleFunc("GET /api/agents", s.pgListAgents)
	mux.HandleFunc("POST /api/agents", s.pgCreateAgent)
	mux.HandleFunc("GET /api/agents/{key}", s.pgGetAgent)
	mux.HandleFunc("PATCH /api/agents/{key}", s.pgUpdateAgent)
	mux.HandleFunc("DELETE /api/agents/{key}", s.pgDeleteAgent)
	mux.HandleFunc("PUT /api/agents/{key}/config", s.pgSaveAgentConfig)
	mux.HandleFunc("PUT /api/agents/{key}/prompt", s.pgSavePrompt)
	mux.HandleFunc("POST /api/agents/{key}/prompt/reset", s.pgResetPrompt)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup", s.pgSaveWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/reset", s.pgResetWrapup)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup/task-timeout", s.pgSaveTaskTimeoutWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/task-timeout/reset", s.pgResetTaskTimeoutWrapup)
	mux.HandleFunc("GET /api/agents/{key}/triggers", s.pgListTriggers)
	mux.HandleFunc("POST /api/agents/{key}/triggers", s.pgCreateTrigger)
	mux.HandleFunc("PATCH /api/triggers/{id}", s.pgUpdateTrigger)
	mux.HandleFunc("DELETE /api/triggers/{id}", s.pgDeleteTrigger)
	mux.HandleFunc("GET /api/agents/{key}/prompts", s.pgListPromptVersions)
	mux.HandleFunc("GET /api/agents/{key}/variables", s.pgPromptVars)
	mux.HandleFunc("POST /api/agents/{key}/prompt/preview", s.pgPreviewPrompt)
	mux.HandleFunc("GET /api/agents/{key}/visibility", s.pgGetAgentVisibility)
	mux.HandleFunc("PUT /api/agents/{key}/visibility", s.pgSetAgentVisibility)
	// 내장 도구 목록(설명/파라미터 기본값은 수정 가능, agent별 바인딩. key와 handler는 코드 층). 워커 도구의 카탈로그이며, 두 그래프의 데이터와는 별개입니다.
	mux.HandleFunc("GET /api/tools", s.pgListTools)
	mux.HandleFunc("PUT /api/tools/{key}", s.pgUpdateTool)
	mux.HandleFunc("POST /api/tools/custom", s.pgCreateCustomTool)
	mux.HandleFunc("POST /api/tools/custom/test", s.pgTestCustomTool)
	mux.HandleFunc("PUT /api/tools/custom/{key}", s.pgUpdateCustomTool)
	mux.HandleFunc("DELETE /api/tools/custom/{key}", s.pgDeleteCustomTool)
	mux.HandleFunc("POST /api/settings/python/detect", s.pgDetectPython)
	mux.HandleFunc("POST /api/tools/{key}/reset", s.pgResetTool)
	// MCP 등록. 외부 도구 서버를 붙이고 지웁니다. 워커가 부르는 도구 목록에 들어갑니다.
	mux.HandleFunc("GET /api/mcp", s.pgListMCP)
	mux.HandleFunc("POST /api/mcp", s.pgSaveMCP)
	mux.HandleFunc("DELETE /api/mcp/{id}", s.pgDeleteMCP)
	mux.HandleFunc("GET /api/mcp/{id}/tools", s.pgMCPTools)
	mux.HandleFunc("POST /api/mcp/{id}/refresh", s.pgRefreshMCP)
	// 자산 동기화 — ScopeSentry 데이터 소스. 외부 수집분을 자산 그래프에 반영하는 경로이며, 탐색 그래프와는 별개입니다.
	mux.HandleFunc("GET /api/sync/scopesentry/status", s.syncSSStatus)
	mux.HandleFunc("POST /api/sync/scopesentry/datasource", s.syncSSDatasource)
	mux.HandleFunc("GET /api/sync/scopesentry/projects", s.syncSSProjects)
	mux.HandleFunc("GET /api/sync/scopesentry/tasks", s.syncSSTasks)
	mux.HandleFunc("POST /api/sync/scopesentry/sync", s.syncSSRun)
	// Skill CRUD (파일 시스템). 에이전트에 보이는 절차를 파일로 두는 관리이며, 자산 그래프·탐색 그래프와는 별개입니다.
	mux.HandleFunc("GET /api/skills", s.fsListSkills)
	mux.HandleFunc("POST /api/skills", s.fsCreateSkill)
	mux.HandleFunc("POST /api/skills/upload", s.fsUploadSkill)
	mux.HandleFunc("DELETE /api/skills/{name}", s.fsDeleteSkill)
	mux.HandleFunc("GET /api/skills/missing", s.fsMissingSkills)   // 빗나감(호출하려 했으나 없는) skill 이름
	mux.HandleFunc("GET /api/skills/{name}/usage", s.fsSkillUsage) // 단일 skill의 최근 호출
	mux.HandleFunc("PUT /api/skills/{name}/meta", s.fsUpdateSkillMeta)
	mux.HandleFunc("POST /api/skills/{name}/dirs", s.fsCreateDir)
	mux.HandleFunc("GET /api/skills/{name}/files", s.fsListFiles)
	// {file...} 는 슬래시를 포함한 경로 조각을 받습니다. 예: scripts/extract.py
	mux.HandleFunc("GET /api/skills/{name}/files/{file...}", s.fsReadFile)
	mux.HandleFunc("PUT /api/skills/{name}/files/{file...}", s.fsWriteFile)
	mux.HandleFunc("DELETE /api/skills/{name}/files/{file...}", s.fsDeletePath)
	// MCP 자원 쪽 가시성(더 구체적인 skill 라우트가 우선 매칭됩니다)
	mux.HandleFunc("GET /api/visibility/{kind}/{id}", s.pgResourceVisibility)
	mux.HandleFunc("POST /api/visibility/toggle", s.pgToggleVisibility)
	// Skill 가시성(이름 기준, 더 구체적이며 위의 와일드카드 라우트보다 우선)
	mux.HandleFunc("GET /api/visibility/skill/{name}", s.pgSkillVisibility)
	mux.HandleFunc("POST /api/visibility/skill/toggle", s.pgToggleSkillVisibility)
	// LLM 다중 profile
	mux.HandleFunc("GET /api/llm/profiles", s.pgListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{id}", s.pgDeleteProfile)
	mux.HandleFunc("POST /api/llm/profiles/active", s.pgActivateProfile)
	mux.HandleFunc("GET /api/llm/retry-policy", s.pgGetLLMRetryPolicy)
	mux.HandleFunc("POST /api/llm/retry-policy", s.pgSaveLLMRetryPolicy)
	mux.HandleFunc("GET /api/llm/pool", s.pgLLMPoolStatus)
	mux.HandleFunc("POST /api/llm/pool/reset", s.pgLLMPoolReset)
	mux.HandleFunc("POST /api/llm/models", s.pgListModels)

	// 가로채기 규칙 관리. 가드(guard)가 명령을 거르는 규칙이며, 엔진 실행 전에 적용됩니다.
	mux.HandleFunc("GET /api/intercept/rules", s.interceptListRules)
	mux.HandleFunc("POST /api/intercept/rules", s.interceptCreateRule)
	mux.HandleFunc("PUT /api/intercept/rules/{id}", s.interceptUpdateRule)
	mux.HandleFunc("DELETE /api/intercept/rules/{id}", s.interceptDeleteRule)
	mux.HandleFunc("POST /api/intercept/rules/{id}/toggle", s.interceptToggleRule)

	// 자산 가로채기 규칙 관리(전역 차단 목록: 도메인/IP/URL/CIDR). 자산 그래프에 들어오기 전에 전역으로 막는 목록입니다.
	mux.HandleFunc("GET /api/asset-intercept/rules", s.assetInterceptListRules)
	mux.HandleFunc("POST /api/asset-intercept/rules", s.assetInterceptCreateRule)
	mux.HandleFunc("PUT /api/asset-intercept/rules/{id}", s.assetInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/asset-intercept/rules/{id}", s.assetInterceptDeleteRule)
	mux.HandleFunc("POST /api/asset-intercept/rules/{id}/toggle", s.assetInterceptToggleRule)

	mux.HandleFunc("GET /api/intercept/pending", s.interceptListPending)
	mux.HandleFunc("GET /api/intercept/pending/{id}", s.interceptGetOne)
	mux.HandleFunc("POST /api/intercept/pending/{id}/decide", s.interceptDecide)
	mux.HandleFunc("GET /api/intercept/history", s.interceptHistory)
	mux.HandleFunc("GET /api/intercept/history/{id}", s.interceptDetail)
	mux.HandleFunc("GET /api/intercept/history/{id}/execution", s.interceptExecution)
	mux.HandleFunc("GET /api/intercept/task/{taskID}", s.interceptListTaskItems)
	mux.HandleFunc("GET /api/intercept/tool-config", s.interceptGetToolConfig)
	mux.HandleFunc("PUT /api/intercept/tool-config", s.interceptSetToolConfig)
	mux.HandleFunc("GET /api/intercept/judge", s.interceptGetJudgeConfig)
	mux.HandleFunc("PUT /api/intercept/judge", s.interceptSetJudgeConfig)
	mux.HandleFunc("GET /api/intercept/judge/usage", s.interceptJudgeUsage) // 최후 승인에 누적된 token 사용량

	// /api/* 는 CORS 와 JWT 를 탑니다. 그 밖은 바이너리에 넣은 화면이 줍니다.
	// 화면 자체는 공개이고, 인증은 브라우저와 API 에서 합니다. 화면을 넣지 않은 빌드에서는
	// 이 처리기가 404 만 돌려줍니다. 그때는 next dev 를 따로 띄웁니다.
	api := cors(s.requireAuth(mux))
	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/", s.webuiHandler())
	return root
}

// --- 처리기. HTTP 요청을 Manager 와 Engine 에 연결합니다. ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "service": "artex", "version": BuildVersion})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	out["engine_mode"] = "idle"              // 명세의 열거값. 활성 작업이 있으면 아래에서 바꿉니다.
	out["llm_configured"] = s.engine.Ready() // LLM 공급자가 깔려 있는지. 작업이 돌 수 있는지는 별개입니다.
	if tr := s.m.Traffic(); tr != nil {
		c, _ := tr.Count()
		out["traffic"] = c
		out["traffic_enabled"] = true
	}
	if counts, err := s.m.Assets().CountsByType(); err == nil {
		total := 0
		for _, n := range counts {
			total += n
		}
		out["assets"] = total
		out["asset_counts"] = counts
	}

	// 작업을 고릅니다. ?task=<id> 가 있으면 그 작업에 고정합니다. 상세 화면이
	// 전역 활성 작업이 바뀌었다고 조용히 따라가면 안 됩니다. 비어 있으면 활성 작업입니다.
	taskParam := r.URL.Query().Get("task")
	t := s.m.ResolveTask(taskParam)
	if t == nil {
		if taskParam != "" && taskParam != "active" {
			writeErr(w, 404, "작업을 찾을 수 없습니다")
			return
		}
		writeJSON(w, 200, out) // 고른 작업이 없으면 전역 필드만 돌려줍니다.
		return
	}
	out["llm_configured"] = s.engine.ReadyFor(t)

	st, _ := t.Store.Stats()
	out["exploration"] = st

	// 작업마다의 실행 상태와 심장박동. "LLM 이 설정됨"과는 다릅니다.
	intents, _ := t.Store.ListByKind(db.KindIntent, 100000)
	inFlight := 0
	for _, in := range intents {
		if in.State == "running" {
			inFlight++
		}
	}
	goals, _ := t.Store.ListByKind(db.KindGoal, 10000)
	goalsMet := 0
	for _, g := range goals {
		if g.State == "met" {
			goalsMet++
		}
	}
	last := s.engine.LastActivity(t.ID)
	paused := s.engine.IsPaused(t.ID)
	activeCalls := s.engine.ActiveLLMCalls(t.ID)
	running := s.engine.ReadyFor(t) && s.engine.Started(t.ID) && !paused
	// 일이 없는 이벤트 엔진은 건강한 쉼입니다. 저장된 running 의도가 있는데
	// 그에 해당하는 LLM 호출이 없을 때만 멈춘 것으로 봅니다.
	stalled := running && activeCalls == 0 && inFlight > 0 && last > 0 && time.Now().Unix()-last > 120

	// engine_mode 는 명세의 열거값입니다. exploring, paused, stalled, idle.
	engineMode := "idle"
	switch {
	case paused:
		engineMode = "paused"
	case stalled:
		engineMode = "stalled"
	case running && activeCalls > 0:
		engineMode = "exploring"
	}
	out["engine_mode"] = engineMode

	out["active_task"] = map[string]any{
		"id": t.ID, "description": t.Description, "goal": t.Goal,
		"running":       running,
		"paused":        paused,
		"engine_mode":   engineMode,
		"in_flight":     inFlight,
		"llm_in_flight": activeCalls,
		"last_activity": last,
		"stalled":       stalled,
		"goals_total":   len(goals),
		"goals_met":     goalsMet,
	}
	writeJSON(w, 200, out)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	active := ""
	if t := s.m.ActiveTask(); t != nil {
		active = t.ID
	}
	list := s.m.List()
	metrics, _ := s.m.PG().TaskListMetricsAll()
	archiveBlockers, _ := s.m.PG().TaskArchiveBlockers()
	dtos := make([]TaskDTO, 0, len(list))
	for _, t := range list {
		dto := taskDTO(t, s.resolvedTaskStatus(t))
		applyTaskArchiveBlocker(&dto, archiveBlockers)
		metric := metrics[t.ExpID]
		dto.Tokens = tokenTotalDTO(metric.Tokens)
		// 실행 시간 표시는 메모리의 심장박동을 우선합니다. 없으면 재시작 뒤에도 남는
		// 저장된 마지막 활동 시각을 씁니다.
		dto.LastActivity = metric.LastActivity
		if live := s.engine.LastActivity(t.ID); live > dto.LastActivity {
			dto.LastActivity = live
		}
		dto.GoalsTotal = metric.Goals.Total
		dto.GoalsMet = metric.Goals.Met
		dto.InFlight = metric.RunningIntents
		dto.Findings = FindingSeverityDTO{
			Critical: metric.Findings.Critical,
			High:     metric.Findings.High,
			Medium:   metric.Findings.Medium,
			Low:      metric.Findings.Low,
		}
		dtos = append(dtos, dto)
	}
	writeJSON(w, 200, map[string]any{"tasks": dtos, "active": active})
}

func (s *Server) resolvedTaskStatus(t *Task) string {
	if t == nil {
		return "created"
	}
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Queued:
		return "queued"
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case t.llmStateSnapshot().FailoverState == "chain_exhausted" && s.engine.Started(t.ID):
		// LLM 준비는 실행 조건이지 수명 상태가 아닙니다. 사슬이 소진된 작업을
		// running 으로 두면 사람이 사슬을 바로 고치거나 리셋할 수 있습니다.
		return "running"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	default:
		return "created"
	}
}

func (s *Server) setActive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.m.SetActive(req.ID) {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	// 연 작업의 엔진을 다시 돌립니다. 이미 돌고 있으면 아무 일도 하지 않습니다.
	// 대기 중인 작업: 활성으로만 바꿔 볼 수 있고, 엔진은 시작하지 않습니다(동시 상한을 유지하고, reconcile이 빈자리를 채웁니다).
	if t, ok := s.m.Task(req.ID); ok && !t.lifecycleSnapshot().Queued {
		s.engine.Run(s.ctx, t)
	}
	writeJSON(w, 200, map[string]any{"active": req.ID})
}

// control 은 작업의 자율 실행(플래너와 워커)을 멈추거나 다시 시작합니다.
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" {
		writeErr(w, 400, "action 은 pause 또는 resume 이어야 합니다")
		return
	}
	result, err := s.applyTaskControl(t, req.Action)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// controlIntent 는 워커 의도 하나를 멈추거나, 다시 시작하거나, 취소합니다.
// 돌고 있는 워커는 상태를 고치거나 치우기 전에 항상 멈춥니다. 사람이 지운 뒤
// 도착한 도구 출력이 블랙보드 기록을 다시 만들지 않게 합니다.
func (s *Server) controlIntent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 의도를 제어할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil || iid <= 0 {
		writeErr(w, 400, "의도 id가 올바르지 않습니다")
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"` // cancel(삭제) 때 필수: 삭제 이유
		Mode   string `json:"mode"`   // cancel 전용: soft(기본, 거짓 삭제)| hard(진짜 삭제, 독점 자손을 연쇄로 제거)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "JSON 이 올바르지 않습니다: "+err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" && req.Action != "cancel" {
		writeErr(w, 400, "action 은 pause, resume, cancel 중 하나여야 합니다")
		return
	}

	result, err := s.applyIntentControl(r.Context(), t, iid, req.Action, req.Reason, req.Mode)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

// rerunIntent는 성공하지 못한 의도 하나(blocked/exhausted/stopped)를 다시 실행한다: open으로 되돌리고, 워커
// 다시 클레임하고 이전 대화 기록이 있으면 이어서 실행한다(그래프에 이미 기록된 fact/finding/asset은 유지). 작업이 이미 종료 상태이거나 일시정지라면 함께 되살린다.
// 「오류가 난 work에서 계속 실행을 클릭」할 때 쓴다. 네트워크/LLM 흔들림으로 blocked가 된 뒤 한 번에 재시도할 수 있다.
func restoreRerunIntent(t *Task, before *db.Node) error {
	if t == nil || before == nil {
		return fmt.Errorf("missing intent rollback snapshot")
	}
	if before.State == "blocked" && before.BlockedReason != "" {
		return t.Store.SetIntentBlockedReason(before.ID, before.BlockedReason)
	}
	return t.Store.SetIntentState(before.ID, before.State)
}

func (s *Server) rerunIntent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "의도 id가 올바르지 않습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 의도를 다시 실행할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)
	before, err := t.Store.GetNode(iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	reopened, err := t.Store.ReopenIntent(iid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !reopened {
		writeErr(w, 409, "이 의도는 재실행 가능한 상태가 아닙니다(blocked/exhausted/stopped만 재실행 가능)")
		return
	}
	queued, err := s.admitTask(t, "resume")
	if err != nil {
		if rollbackErr := restoreRerunIntent(t, before); rollbackErr != nil {
			err = fmt.Errorf("%w; restore intent %d after admission failure: %v", err, iid, rollbackErr)
		}
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[task] #%s 의도 #%d 다시 열림(재실행)", t.ID, iid)
	writeJSON(w, 200, map[string]any{"id": t.ID, "reopened": iid, "queued": queued})
}

// rerunBlocked는 이 작업의 blocked 의도 전부를 한꺼번에 다시 실행한다(한 번의 네트워크/LLM 끊김으로 여러 개가 blocked된 뒤
// 한 번에 전부 재시도하기에 맞다). open으로 되돌리고 작업을 되살린다. 다시 연 개수를 반환한다.
func (s *Server) rerunBlocked(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 의도를 다시 실행할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)
	intents, err := t.Store.ListByKind(db.KindIntent, 1000000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	before := make([]*db.Node, 0)
	for _, intent := range intents {
		if intent.State == "blocked" {
			copy := *intent
			before = append(before, &copy)
		}
	}
	n, err := t.Store.ReopenBlockedIntents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	queued := false
	if n > 0 {
		queued, err = s.admitTask(t, "resume")
		if err != nil {
			rollbackErrors := make([]string, 0)
			for _, intent := range before {
				if rollbackErr := restoreRerunIntent(t, intent); rollbackErr != nil {
					rollbackErrors = append(rollbackErrors, fmt.Sprintf("intent %d: %v", intent.ID, rollbackErr))
				}
			}
			if len(rollbackErrors) > 0 {
				err = fmt.Errorf("%w; restore blocked intents after admission failure: %s", err, strings.Join(rollbackErrors, "; "))
			}
			writeErr(w, 500, err.Error())
			return
		}
		log.Printf("[task] #%s blocked 의도 %d건을 일괄 재개", t.ID, n)
	}
	writeJSON(w, 200, map[string]any{"id": t.ID, "reopened": n, "queued": queued})
}

// getLLM 은 현재 LLM 설정을 돌려줍니다. 키는 절대 내보이지 않습니다.
func (s *Server) getLLM(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"configured":       s.llmOn,
		"provider":         s.llmCfg.Provider(),
		"model":            s.llmCfg.Model,
		"base_url":         s.llmCfg.BaseURL,
		"proxy":            s.llmCfg.Proxy,
		"key_set":          s.llmCfg.APIKey != "",
		"rate_per_second":  s.llmCfg.RatePerSecond,
		"rate_per_minute":  s.llmCfg.RatePerMinute,
		"context_window_k": s.llmCfg.ContextWindowK,
		"thinking_type":    s.llmCfg.ThinkingType,
		"reasoning_effort": s.llmCfg.ReasoningEffort,
	})
}

// setLLM 은 실행 중에 LLM 을 설정합니다. api_key 가 비어 있으면 기존 키를 유지합니다.
func (s *Server) setLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		BaseURL         string  `json:"base_url"`
		Proxy           string  `json:"proxy"`
		APIKey          string  `json:"api_key"`
		RatePerSecond   float64 `json:"rate_per_second"`
		RatePerMinute   float64 `json:"rate_per_minute"`
		ContextWindowK  int     `json:"context_window_k"`
		ThinkingType    string  `json:"thinking_type"`
		ReasoningEffort string  `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = req.RatePerSecond, req.RatePerMinute
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	if k := req.ContextWindowK; k > 0 { // 0 이면 기본 200K 를 유지합니다. 상한은 1M 입니다.
		if k > 1000 {
			k = 1000
		}
		cfg.ContextWindowK = k
	}
	if cfg.APIKey == "" {
		s.cfgMu.Lock()
		cfg.APIKey = s.llmCfg.APIKey // 다시 입력하지 않으면 기존 키를 유지합니다.
		s.cfgMu.Unlock()
	}
	if cfg.APIKey == "" {
		writeErr(w, 400, "api_key 는 필수입니다")
		return
	}
	// 활성 프로필로 저장하기 전에 공급자가 만들어지는지 확인합니다.
	if _, err := cfg.NewProvider(); err != nil {
		writeErr(w, 400, "공급자 초기화에 실패했습니다: "+err.Error())
		return
	}
	if err := s.saveLLMConfig(cfg); err != nil {
		writeErr(w, 500, "공급자 저장에 실패했습니다: "+err.Error())
		return
	}
	s.invalidateProfileAgents()
	if _, ok := s.loadLLMConfig(); !ok {
		writeErr(w, 500, "저장된 공급자를 쓸 수 없습니다")
		return
	}
	if err := s.applyLLM(cfg); err != nil {
		writeErr(w, 400, "공급자 초기화에 실패했습니다: "+err.Error())
		return
	}
	log.Printf("[engine] LLM configured via UI: %s / %s", cfg.Provider(), cfg.Model)
	s.getLLM(w, r)
}

// testLLM 은 아주 짧은 완성을 실제로 호출해 설정이 되는지 확인합니다.
func (s *Server) testLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		BaseURL          string `json:"base_url"`
		Proxy            string `json:"proxy"`
		APIKey           string `json:"api_key"`
		ThinkingType     string `json:"thinking_type"`
		ReasoningEffort  string `json:"reasoning_effort"`
		ProfileID        *int64 `json:"profile_id"`         // 저장된 profile을 시험할 때 넘긴다: api_key가 비어 있으면 거기에 저장된 key를 쓴다
		Streaming        *bool  `json:"streaming"`          // 생략=스트리밍, profile을 저장할 때와 같은 기본값
		SessionHeaderKey string `json:"session_header_key"` // 비어 있지 않으면=테스트 때에도 이 사용자 지정 세션 헤더를 붙인다. 값은 일회용 무작위 session id
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	// 실제 실행과 같게, 같은 생각 매개변수를 보냅니다. reasoning_effort 나 thinking 필드를
	// 거절하는 공급자는 테스트도 실패합니다. "테스트는 되고 실행은 400" 이 나오지 않게 합니다.
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	// 마찬가지로 송수신 모드도 그 profile의 선택을 따른다. 그중 한 채널만 지원하는 엔드포인트는 여기서
	// 드러나야 한다. 세션에 들어가서야 「테스트는 통과한 설정이 아예 돌지 않는다」를 알게 되면 안 된다.
	if req.Streaming != nil {
		cfg.Stream = *req.Streaming
	}
	// 사용자 지정 세션 헤더 이름은 이 설정을 따른다: 비어 있지 않으면 테스트 요청도 이 헤더를 보낸다(값은 일회용 무작위 session id,
	// TestConnection 참고). opencode zen 등 x-opencode-session을 강제하는 엔드포인트는 이것이 없으면
	// 바로 400이 된다. 테스트 경로에도 반드시 붙여야 한다. 그렇지 않으면 「대화는 되고 테스트는 400」이 된다.
	cfg.SessionHeaderKey = req.SessionHeaderKey
	// API Key 해석 우선순위: 폼 입력 > 지정한 profile에 저장된 key > 전역 설정의 key.
	// 저장된 profile의 key는 브라우저로 돌려주지 않는다. 그래서 저장된 설정을 테스트할 때 폼이 비어 있고, DB에서 가져와야 한다.
	// 세션 헤더 이름도 같다: 폼에 없으면 저장된 profile 값으로 폴백한다.
	if req.ProfileID != nil && (cfg.APIKey == "" || cfg.SessionHeaderKey == "") {
		if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
			if cfg.APIKey == "" {
				cfg.APIKey = p.APIKey
			}
			if cfg.SessionHeaderKey == "" {
				cfg.SessionHeaderKey = p.SessionHeaderKey
			}
		}
	}
	if cfg.APIKey == "" {
		s.cfgMu.Lock()
		cfg.APIKey = s.llmCfg.APIKey
		s.cfgMu.Unlock()
	}
	if cfg.APIKey == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "API Key가 제공되지 않음"})
		return
	}
	// 재시도 매개변수는 연결 테스트에 넣지 【않는다】: 테스트는 30초 하드 타임아웃이 있고, 설정의 재시도 횟수/긴 간격을 겹치면
	// 원래 쓸 수 있는 엔드포인트가 「타임아웃 실패」로만 나온다. 테스트가 보는 것은 「이 엔드포인트가 통하는지」이고, 재시도 리듬은
	// 실제로 돌기 시작한 뒤의 일이다.
	lat, reply, err := agent.TestConnection(r.Context(), cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 모델의 실제 응답을 돌려주어 「테스트 통과」에 근거를 남긴다. HTTP 200만이 아니라 정말로 말했는지 볼 수 있다.
	writeJSON(w, 200, map[string]any{
		"ok": true, "latency_ms": lat.Milliseconds(), "model": cfg.Model, "reply": truncateReply(reply),
	})
}

// truncateReply 는 연결 테스트 답을 화면에 맞게 자릅니다. "OK" 만 하라고 해도
// 모델이 길게 말하거나 생각을 쏟을 수 있습니다. 화면은 정말로 말했는지만 보여 주면 됩니다.
// 글자(룬) 단위로 잘라 여러 바이트 문자가 중간에서 깨지지 않습니다.
func truncateReply(s string) string {
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

type createTaskReq struct {
	Name                 string   `json:"name,omitempty"` // 선택적 작업 이름; 생략/빈 값=이름 없음
	CategoryID           *int64   `json:"category_id,omitempty"`
	Description          string   `json:"description"`
	Goal                 string   `json:"goal"`
	LLMProfileID         *int64   `json:"llm_profile_id,omitempty"`    // 이 작업을 돌릴 LLM 설정을 지정한다; 생략/null=활성화된 설정
	LLMProfileIDs        []int64  `json:"llm_profile_ids,omitempty"`   // 순서가 있는 작업 수준 설정 체인; 첫 항목이 처음에 적용된다
	SourceTaskIDs        []string `json:"source_task_ids,omitempty"`   // 직접적이고 읽기 전용으로 상속하는 원본 작업만
	CompanyIDs           []int64  `json:"company_ids,omitempty"`       // 기업 범위를 연결하고 현재 기업 자산의 연관 스냅샷을 남긴다; 자산을 복사하거나 의도를 강제 생성하지 않는다
	TimeoutSeconds       int      `json:"timeout_seconds"`             // 작업 수준 타임아웃(초); 0/생략=시간 제한 없음
	PlanHeartbeatSeconds int      `json:"plan_heartbeat_seconds"`      // 플래너 하트비트 트리거 간격(초); 0/생략=기본 600(10분); 하한=기본=600, 그보다 낮으면 자동으로 600까지 올린다
	SeedFirstIntent      *bool    `json:"seed_first_intent,omitempty"` // 생성할 때 시드 의도 하나를 바로 내려보낸다(내용=설명+목표). 워커가 첫 플래너 라운드를 기다리지 않고 바로 시작한다; 생략/null=기본은 꺼짐, 표준대로 먼저 계획한 뒤 실행한다. 명시적으로 true를 넘겨야 켜진다(CTF에서 work 하나로 해결되는 경우가 많아, 시작 전 플래너 라운드를 생략할 수 있다).
	CoverageEnabled      *bool    `json:"coverage_enabled,omitempty"`  // 자산 커버리지 기능; 생략/null=기본 켜짐(true). false=커버리지 계산/표시/자동 범위 누적을 끄고 add_task_scope/list_untested_assets를 숨긴다. company 연결은 영향받지 않는다.
	// InterceptRules는 작업 수준의 자산 가로채기/allow 규칙이다(생성 시 입력하고, task_intercept_rules에 저장하며, 전역 테이블에는 넣지 않는다). 이 규칙은 엔진이 자산 그래프의 자산을 탐색 그래프의 의도로 넘길 때 거르는 기준이다.
	InterceptRules []taskInterceptRuleReq `json:"intercept_rules,omitempty"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "JSON 이 올바르지 않습니다: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		req.Description = "이름 없는 작업"
	}
	if len(req.LLMProfileIDs) == 0 && req.LLMProfileID != nil {
		req.LLMProfileIDs = []int64{*req.LLMProfileID}
	}
	if err := s.validateTaskProfileIDs(req.LLMProfileIDs); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.TimeoutSeconds < 0 {
		req.TimeoutSeconds = 0
	}
	if len(req.SourceTaskIDs) > db.MaxTaskSourceCount {
		writeErr(w, 400, fmt.Sprintf("연관 작업은 최대 %d개까지 선택할 수 있습니다", db.MaxTaskSourceCount))
		return
	}
	sourceIDs := make([]int64, 0, len(req.SourceTaskIDs))
	seenSources := map[int64]bool{}
	for _, raw := range req.SourceTaskIDs {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 || seenSources[id] {
			writeErr(w, 400, "연관 작업 id가 유효하지 않거나 중복됨")
			return
		}
		if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
			writeErr(w, 400, fmt.Sprintf("연관 작업 #%d이(가) 없습니다", id))
			return
		}
		seenSources[id] = true
		sourceIDs = append(sourceIDs, id)
	}
	companyIDs, err := db.NormalizeTaskCompanyIDs(req.CompanyIDs)
	if err != nil {
		writeErr(w, 400, fmt.Sprintf("연관 기업이 유효하지 않습니다: 유효한 기업은 최대 %d개까지 선택할 수 있습니다", db.MaxTaskCompanyCount))
		return
	}
	req.CompanyIDs = companyIDs
	interceptRules, err := buildTaskInterceptRules(req.InterceptRules)
	if err != nil {
		writeErr(w, 400, "작업 수준 가로채기 규칙이 유효하지 않습니다: "+err.Error())
		return
	}
	t, err := s.m.CreateTaskWithOptions(req.Description, req.Goal, db.TaskCreateOptions{
		Name: strings.TrimSpace(req.Name), CategoryID: req.CategoryID,
		SourceTaskIDs: sourceIDs, CompanyIDs: req.CompanyIDs, LLMProfileIDs: req.LLMProfileIDs,
		TimeoutSeconds: req.TimeoutSeconds, PlanHeartbeatSeconds: req.PlanHeartbeatSeconds,
		CoverageEnabled: req.CoverageEnabled,
		InterceptRules:  interceptRules,
	})
	if err != nil {
		if errors.Is(err, db.ErrTaskCategoryInvalid) || errors.Is(err, db.ErrTaskCategoryNotFound) {
			writeErr(w, 400, "작업 분류가 없거나 유효하지 않습니다")
			return
		}
		if errors.Is(err, db.ErrTaskCompanyIDsInvalid) || errors.Is(err, db.ErrTaskCompanyNotFound) {
			writeErr(w, 400, "연관 기업이 없거나 유효하지 않습니다")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[task] 새 작업 #%s «%s» 대상: %s", t.ID, req.Description, req.Goal)
	// 공유하는 생성 후 흐름(seed + 시드 의도 + 백그라운드 목표 분해 + engine.Run). spawn_task와 같은 구간을 재사용한다.
	// launchTask 내부는 비동기라 UI를 막지 않는다. 목표 분해는 백그라운드에서 보이게 진행된다.
	s.launchTask(t, req.Description+" "+req.Goal, req.SeedFirstIntent != nil && *req.SeedFirstIntent)
	writeJSON(w, 201, taskDTO(t, s.resolvedTaskStatus(t)))
}

func (s *Server) validateTaskProfileIDs(ids []int64) error {
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return fmt.Errorf("LLM 설정 id가 유효하지 않거나 중복됨")
		}
		seen[id] = true
		if _, ok := s.loadProfileConfig(id); !ok {
			return fmt.Errorf("LLM 설정 #%d이(가) 없거나 API Key가 설정되지 않음", id)
		}
	}
	return nil
}

func (s *Server) updateTaskLLMProfiles(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	// 어떤 생명주기 상태(종료 상태 포함)에서도 체인을 바꿀 수 있다: 작업이 끝난 뒤에도 메인 에이전트 대화는 이 체인을 타고,
	// 모델을 쓸 수 없을 때 체인을 바꾸지 않으면 이미 끝난 작업의 상호작용까지 같이 잠긴다.
	before := t.llmStateSnapshot()
	var req struct {
		LLMProfileIDs      []int64 `json:"llm_profile_ids"`
		ActiveLLMProfileID *int64  `json:"active_llm_profile_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "JSON 이 올바르지 않습니다: "+err.Error())
		return
	}
	if err := s.validateTaskProfileIDs(req.LLMProfileIDs); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	active := int64(0)
	if req.ActiveLLMProfileID != nil {
		active = *req.ActiveLLMProfileID
	}
	reopened, err := s.m.ReplaceTaskLLMProfiles(t.ID, req.LLMProfileIDs, active)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 프로필 수정은 그 다음 LLM 호출부터 적용됩니다. 이미 나간 호출은 자기 공급자를 유지합니다.
	// 동시 실행 재조정은 ActiveLLMCalls 를 살아있는 자리로 보고, 쓸 수 없는 작업의
	// 일시정지·대기 전환을 호출이 돌아올 때까지 미룹니다. 깨우면 쉬고 있는 running 작업이 바로 이어집니다.
	t.Notify()
	go s.reconcileConcurrency()
	llmState := t.llmStateSnapshot()
	result := map[string]any{
		"id": t.ID, "llm_profile_ids": llmState.ProfileIDs,
		"active_llm_profile_id": llmState.ActiveID,
		"llm_failover_state":    llmState.FailoverState, "reopened_intents": reopened,
	}
	if !sameOptionalID(before.ActiveID, llmState.ActiveID) {
		event := s.emitManualTaskLLMSwitch(t, before.ActiveID, llmState.ActiveID)
		result["switch_event"] = activityDTO(event)
	}
	writeJSON(w, 200, result)
}

var (
	reURL    = regexp.MustCompile(`https?://[^\s'"]+`)
	reIPPort = regexp.MustCompile(`\b((?:\d{1,3}\.){3}\d{1,3})(?::(\d{1,5}))?`)
	reDomain = regexp.MustCompile(`\b((?:[a-zA-Z0-9-]+\.)+[a-zA-Z]{2,})(?::(\d{1,5}))?`)
)

// parseTarget 은 자유 글에서 대상(스킴, 호스트, 포트)을 뽑습니다. 전체 URL,
// IP[:port], 도메인[:port] 를 받습니다. 파싱할 것이 없으면 ok 는 false 입니다.
func parseTarget(text string) (scheme, host string, port int, ok bool) {
	text = strings.TrimSpace(text)
	if m := reURL.FindString(text); m != "" {
		if u, err := url.Parse(m); err == nil && u.Hostname() != "" {
			scheme = strings.ToLower(u.Scheme)
			host = strings.ToLower(u.Hostname())
			port = portOr(u.Port(), defaultPort(scheme))
			return scheme, host, port, true
		}
	}
	if m := reIPPort.FindStringSubmatch(text); m != nil {
		host = m[1]
		port = portOr(m[2], 80)
		return schemeForPort(port), host, port, true
	}
	if m := reDomain.FindStringSubmatch(text); m != nil {
		host = strings.ToLower(m[1])
		if m[2] == "" {
			return "https", host, 443, true
		}
		port = portOr(m[2], 443)
		return schemeForPort(port), host, port, true
	}
	return "", "", 0, false
}

func portOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return d
}
func defaultPort(scheme string) int {
	if scheme == "http" {
		return 80
	}
	return 443
}
func schemeForPort(p int) string {
	if p == 443 || p == 8443 {
		return "https"
	}
	return "http"
}

// llmHost 는 설정된 LLM 끝점의 호스트입니다. 그 호스트를 작업 범위에 넣지 않으려고 씁니다.
func (s *Server) llmHost() string {
	s.cfgMu.Lock()
	base := s.llmCfg.BaseURL
	s.cfgMu.Unlock()
	if base == "" {
		return ""
	}
	if u, err := url.Parse(base); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

func (s *Server) seed(t *Task, text string) {
	scheme, host, port, ok := parseTarget(text)
	if !ok {
		log.Printf("[seed] task %s: %q에서 대상 host/IP를 파싱하지 못해 사이트를 만들지 않습니다(scope를 수동으로 설정하세요)", t.ID, text)
		return
	}
	// 가드: 설정된 LLM 게이트웨이를 대상으로 삼지 않습니다.
	if gw := s.llmHost(); gw != "" && host == gw {
		log.Printf("[seed] task %s: 대상 %q은(는) LLM 게이트웨이이므로 침투 대상으로 거부", t.ID, host)
		return
	}

	u := scheme + "://" + host
	if !(scheme == "https" && port == 443) && !(scheme == "http" && port == 80) {
		u += ":" + strconv.Itoa(port)
	}
	var rootID int64
	if as := s.m.Assets(); as != nil {
		taskID, _ := strconv.ParseInt(t.ID, 10, 64)
		if net.ParseIP(host) != nil {
			rootID, _ = as.UpsertIP(db.UpsertIPReq{IP: host, TaskID: taskID})
		} else if scheme == "https" || scheme == "http" {
			rootID, _ = as.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: u, TaskID: taskID})
		} else {
			rootID, _ = as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: host, TaskID: taskID})
		}
		if rootID > 0 {
			_ = as.SetTaskAssetSource(taskID, rootID, "task", "작업 설명 또는 대상에서 초기화", nil)
		}
	}
	// 심은 자산을 이 작업의 시작 뿌리에 앵커로 묶습니다. 계보를 남기는 용도입니다.
	// 자산 그래프는 전역으로 공유되고, 앵커는 이제 읽기를 막지 않습니다.
	if rootID > 0 {
		if begin, _ := t.Store.OriginFactID(); begin > 0 {
			_ = t.Store.Anchor(begin, rootID)
		}
	}
	log.Printf("[seed] task %s: 대상 사이트 %s", t.ID, u)
	// 여기서는 Notify하지 않는다: 첫 라운드 트리거 여부는 engine.Run의 HasActiveIntent가 통일해서 정한다(시드 의도 작업은
	// 첫 라운드를 건너뛴다). seed는 Run보다 먼저 실행된다. 여기서 Notify하면 채널에 버퍼되고, plannerLoop가 시작될 때
	// 소비되어 Run의 게이트를 우회한다 → 시드 작업이 여전히 첫 라운드를 잘못 트리거한다.
}

// seedFirstIntent 는 만들 때 프론티어에 open 의도 하나(summary = 설명+목표)를 넣습니다.
// 워커가 플래너 라운드를 기다리지 않고 바로 집어 실행합니다. 플래너의 최상위 의도와 같이
// 기원 사실에서 RelDerivedFrom 으로 이어져 사실 노드까지 거슬러 갑니다. 실패해도
// 평범한 플래너 흐름으로 돌아갑니다.
func (s *Server) seedFirstIntent(t *Task) {
	summary := fmt.Sprintf("작업 목표 완료: %s(작업: %s)", t.Goal, t.Description)
	id, err := t.Store.AddIntent(map[string]any{"summary": summary}, 8, nil, "seed")
	if err != nil {
		log.Printf("[seed] task %s: 시드 의도 전달 실패: %v", t.ID, err)
		return
	}
	if origin, _ := t.Store.OriginFactID(); origin > 0 {
		_ = t.Store.Link(origin, db.RelDerivedFrom, id)
	}
	log.Printf("[seed] task %s: 시드 의도 #%d을(를) 전달함", t.ID, id)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	dto := taskDTO(t, s.resolvedTaskStatus(t))
	archiveBlockers, _ := s.m.PG().TaskArchiveBlockers()
	applyTaskArchiveBlocker(&dto, archiveBlockers)
	writeJSON(w, 200, dto)
}

// taskCoverage 는 작업의 거친 자산 시험 범위입니다. 분모, 시험한 수, 남은 수입니다.
func (s *Server) taskCoverage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소가 활성화되지 않았습니다")
		return
	}
	// 자산 커버리지 기능이 꺼지면 → 즉시 {enabled:false}를 반환하고, 프론트엔드는 이에 따라 커버리지 카드/진행을 숨긴다.
	if !t.CoverageEnabled {
		writeJSON(w, 200, &db.Coverage{Enabled: false, ByType: []db.CoverageByType{}})
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	cov, err := as.TaskCoverageWithSources(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	cov.Enabled = true
	writeJSON(w, 200, cov)
}

// taskCoverageGraph 는 작업의 힘 기반 자산 커버리지 그래프입니다.
// 범위 안 자산 전부(유형마다) + 연결용 루트 도메인/회사 노드, 각각 tested/in_scope를 담는다.
func (s *Server) taskCoverageGraph(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소가 활성화되지 않았습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	g, err := as.BuildCoverageGraph(taskID, t.ExpID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, g)
}

// taskAssetRefs 는 이 작업에서 주어진 자산 id 에 앵커된 의도·사실·발견을 돌려줍니다.
// 커버리지 그래프 노드 서랍의 「연관 의도 / 연관 사실」이 이 목록을 씁니다.
func (s *Server) taskAssetRefs(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	assetID, _ := strconv.ParseInt(r.URL.Query().Get("asset_id"), 10, 64)
	if assetID <= 0 {
		writeErr(w, 400, "asset_id가 필요합니다")
		return
	}
	refs, err := t.Store.AssetRefsWithSources(assetID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	intents := []CoverageAssetRefDTO{}
	facts := []CoverageAssetRefDTO{}
	findings := []CoverageAssetRefDTO{}
	for _, ref := range refs {
		dto := coverageAssetRefDTO(ref)
		switch ref.Kind {
		case "intent":
			intents = append(intents, dto)
		case "fact":
			facts = append(facts, dto)
		case "finding":
			findings = append(findings, dto)
		}
	}
	writeJSON(w, 200, map[string]any{"intents": intents, "facts": facts, "findings": findings})
}

// taskScopeList 는 작업의 범위 행입니다. 커버리지 분모가 어디서 왔는지입니다.
func (s *Server) taskScopeList(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소가 활성화되지 않았습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	rows, err := as.ListTaskScopeWithSources(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"scope": rows})
}

func (s *Server) taskScopeAdd(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소가 활성화되지 않았습니다")
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Value  string `json:"value"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	ts, err := as.AddAgentScope(taskID, body.Kind, body.Value, body.Reason, "manual")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, ts)
}

func (s *Server) taskScopeDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	as := s.m.Assets()
	if as == nil {
		writeErr(w, 503, "자산 저장소가 활성화되지 않았습니다")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	scopeID, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "범위 id가 올바르지 않습니다")
		return
	}
	deleted, err := as.DeleteTaskScope(taskID, scopeID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !deleted {
		writeErr(w, 404, "범위 항목을 찾을 수 없습니다")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) frontier(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	fr, _ := t.Store.Frontier(atoiDefault(r.URL.Query().Get("limit"), 100))
	writeJSON(w, 200, taskNodeDTOs(fr))
}

func (s *Server) findings(w http.ResponseWriter, r *http.Request) {
	// task 매개변수가 없으면 → 전역 「발견」 페이지: 독립 findings 테이블에서 읽는다(작업을 지운 뒤에도 finding은 남는다).
	// task 매개변수가 있으면 → 그 작업만(작업 개요/발견 탭에서 사용). exploration_nodes에서 읽는다(작업이 있으면 노드도 있다).
	q := r.URL.Query()
	taskParam := q.Get("task")
	if taskParam == "" {
		// page/limit가 있으면 → 서버 페이지네이션 {items,total,...}; 없으면 → 순수 배열(dashboard 집계용,
		// intents 엔드포인트의 호환 전략과 같다). 필터/정렬은 통일해서 SQL로 내린다.
		if q.Get("page") == "" && q.Get("limit") == "" {
			fs, _ := s.m.pg.ListFindings(500)
			assets := s.resolveFindingAssets(fs)
			out := make([]FindingDTO, 0, len(fs))
			for _, f := range fs {
				out = append(out, findingFromDB(f, assets))
			}
			writeJSON(w, 200, out)
			return
		}
		page := findingPaginationParam(q.Get("page"), 1, 0)
		limit := findingPaginationParam(q.Get("limit"), 20, 200)
		fs, total, err := s.m.pg.ListFindingsPage(findingFilterFromQuery(q), page, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		assets := s.resolveFindingAssets(fs)
		out := make([]FindingDTO, 0, len(fs))
		for _, f := range fs {
			out = append(out, findingFromDB(f, assets))
		}
		writeJSON(w, 200, map[string]any{"items": out, "total": total, "page": page, "page_size": limit})
		return
	}
	t := s.m.ResolveTask(taskParam)
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	f, err := t.Store.ListByKind(db.KindFinding, 200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tid, _ := strconv.ParseInt(t.ID, 10, 64)
	meta, err := s.m.pg.FindingMetaByNodeID(tid)
	if err != nil {
		log.Printf("[findings] task=%s meta: %v", t.ID, err)
	}
	var aidSet []int64
	for _, m := range meta {
		aidSet = append(aidSet, m.AssetIDs...)
	}
	assets := s.resolveAssetIDs(aidSet)
	out := findingDTOsForTask(t, f, meta, assets)

	// 연관 작업의 발견은 살아있는 읽기 전용 보기입니다. 출처 메타데이터가 있어서
	// 작업 화면은 안정된 id 는 남기고 수정 버튼은 숨깁니다.
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		nodes, listErr := source.Store.ListByKind(db.KindFinding, 200)
		if listErr != nil {
			writeErr(w, 500, listErr.Error())
			return
		}
		for _, node := range nodes {
			node.SourceTaskID = source.Task.TaskID
			node.Inherited = true
		}
		sourceMeta, metaErr := s.m.pg.FindingMetaByNodeID(source.Task.TaskID)
		if metaErr != nil {
			log.Printf("[findings] source_task=%d meta: %v", source.Task.TaskID, metaErr)
		}
		var sourceAssetIDs []int64
		for _, item := range sourceMeta {
			sourceAssetIDs = append(sourceAssetIDs, item.AssetIDs...)
		}
		out = append(out, findingDTOsForOwner(
			i64s(source.Task.TaskID),
			source.Task.Description,
			nodes,
			sourceMeta,
			s.resolveAssetIDs(sourceAssetIDs),
		)...)
	}
	writeJSON(w, 200, out)
}

// normFilter 는 화면의 "all" 과 빈 값을 "" 로 바꿉니다. DB 는 그것을 필터 없음으로 봅니다.
func normFilter(v string) string {
	if v == "all" {
		return ""
	}
	return v
}

// resolveFindingAssets 는 주어진 발견이 앵커한 자산을 id 로 읽습니다.
// 발견 DTO 가 자산 이름을 그릴 때 씁니다.
func (s *Server) resolveFindingAssets(fs []*db.DBFinding) map[int64]*db.Asset {
	var ids []int64
	for _, f := range fs {
		ids = append(ids, f.AssetIDs...)
	}
	return s.resolveAssetIDs(ids)
}

// resolveAssetIDs 는 id 중복을 빼고 자산 행을 id→자산 맵으로 읽습니다.
func (s *Server) resolveAssetIDs(ids []int64) map[int64]*db.Asset {
	assets := map[int64]*db.Asset{}
	seen := map[int64]bool{}
	var uniq []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return assets
	}
	list, err := s.m.pg.Assets().GetByIDs(uniq)
	if err != nil {
		log.Printf("[findings] resolve assets: %v", err)
	}
	for _, a := range list {
		assets[a.ID] = a
	}
	return assets
}

// findingStats 는 표 전체의 집계입니다. 통계 카드와 취약 유형 필터가 씁니다.
// 페이지가 나뉜 발견 페이지용.
func (s *Server) findingStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.m.pg.FindingStats()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

// getFinding 은 독립 표 id(DTO 의 finding_id)로 발견 하나를 돌려줍니다.
// 화면에 쓸 앵커된 자산도 같이 풀립니다.
func (s *Server) getFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "발견 id가 올바르지 않습니다")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "발견을 찾을 수 없습니다")
		return
	}
	assets := s.resolveAssetIDs(f.AssetIDs)
	dto := findingFromDB(f, assets)
	if contextTaskID := strings.TrimSpace(r.URL.Query().Get("context_task")); contextTaskID != "" {
		contextTask := s.m.ResolveTask(contextTaskID)
		if contextTask == nil {
			writeErr(w, 404, "맥락의 작업을 찾을 수 없습니다")
			return
		}
		sourceTaskID, inherited, allowed := findingProvenanceInTask(contextTask, f.TaskID)
		if !allowed {
			writeErr(w, 404, "작업 맥락에서 발견을 쓸 수 없습니다")
			return
		}
		dto.SourceTaskID = sourceTaskID
		dto.Inherited = inherited
	}
	writeJSON(w, 200, dto)
}

// findingsExport는 발견 페이지의 발견(finding)을 내보낸다.
//
//	scope   = filtered（페이지 필터를 그대로 사용）| all（전체）| selected（선택한 ids）
//	format  = md-single（하나의 .md로 합침）| md-zip（발견(finding)마다 .md 하나, zip으로 묶음）
//	          | csv | json  표 또는 JSON 파일
//	ids     = 쉼표로 구분한 finding id（scope=selected일 때 필수）
//	필터 매개변수 severity/status/vulnclass/task_id/q/sort 는 목록 인터페이스와 같다(scope=filtered 용).
func (s *Server) findingsExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := q.Get("scope")
	format := q.Get("format")

	var ids []int64
	var filter db.FindingFilter
	switch scope {
	case "selected":
		for _, part := range strings.Split(q.Get("ids"), ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				writeErr(w, 400, "발견 id가 올바르지 않습니다: "+part)
				return
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			writeErr(w, 400, "선택한 발견이 없습니다")
			return
		}
	case "all":
		// 빈 filter = 조건을 하나도 넣지 않는다.
	case "filtered", "":
		filter = findingFilterFromQuery(q)
	default:
		writeErr(w, 400, "scope가 올바르지 않습니다: "+scope)
		return
	}

	fs, err := s.m.pg.ListFindingsForExport(filter, ids)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	stage, err := os.MkdirTemp("", "artex-finding-export-")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(stage)
	if err = s.evidenceStore().StageFindingsExport(r.Context(), fs, stage, format == "md-zip"); err != nil {
		evidenceError(w, err)
		return
	}
	now := time.Now()
	stamp := now.Format("20060102-150405")
	setDownload := func(contentType, filename string) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	}

	switch format {
	case "md-single":
		setDownload("text/markdown; charset=utf-8", "findings-"+stamp+".md")
		_, _ = w.Write([]byte(report.FindingsMarkdown(fs, now)))
	case "md-zip":
		path := filepath.Join(stage, "findings.zip")
		if err := buildFindingsEvidenceZip(path, fs, stage, now); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		file, err := os.Open(path)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer file.Close()
		setDownload("application/zip", "findings-"+stamp+".zip")
		http.ServeContent(w, r, "findings.zip", now, file)
	case "csv":
		setDownload("text/csv; charset=utf-8", "findings-"+stamp+".csv")
		_, _ = w.Write(report.FindingsCSV(fs))
	case "json":
		assets := s.resolveFindingAssets(fs)
		out := make([]FindingDTO, 0, len(fs))
		for _, f := range fs {
			for i := range f.TrafficBindings {
				f.TrafficBindings[i].Snapshot.ReqHead = ""
				f.TrafficBindings[i].Snapshot.RespHead = ""
			}
			out = append(out, findingFromDB(f, assets))
		}
		setDownload("application/json; charset=utf-8", "findings-"+stamp+".json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	default:
		writeErr(w, 400, "format이 올바르지 않습니다: "+format)
	}
}

func findingProvenanceInTask(contextTask *Task, findingTaskID *int64) (sourceTaskID string, inherited, allowed bool) {
	if contextTask == nil || findingTaskID == nil {
		return "", false, false
	}
	contextID, err := strconv.ParseInt(contextTask.ID, 10, 64)
	if err != nil {
		return "", false, false
	}
	if *findingTaskID == contextID {
		return "", false, true
	}
	for _, sourceID := range contextTask.lifecycleSnapshot().SourceTaskIDs {
		if sourceID == *findingTaskID {
			return i64s(sourceID), true, true
		}
	}
	return "", false, false
}

// findingLineage 는 작업 뿌리에서 이 발견 노드까지의 탐색 부분 그래프입니다.
// 발견 노드, 그 조상, 그들 사이의 간선입니다. 상세 화면이 "어떻게 도달했는지"를 보여 줍니다.
// 노드나 작업이 없으면(원래 작업이 지워진 경우) {nodes,edges} 는 빕니다.
func (s *Server) findingLineage(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "발견 id가 올바르지 않습니다")
		return
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "발견을 찾을 수 없습니다")
		return
	}
	empty := map[string]any{"nodes": []any{}, "edges": []any{}}
	if f.NodeID == nil || f.TaskID == nil {
		writeJSON(w, 200, empty)
		return
	}
	t := s.m.ResolveTask(i64s(*f.TaskID))
	if t == nil {
		writeJSON(w, 200, empty)
		return
	}
	nodes, edges, err := t.Store.FindingLineage(*f.NodeID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"nodes": taskNodeDTOs(nodes), "edges": edgeDTOs(edges)})
}

// patchFinding 은 발견의 일부만 고칩니다. {status, severity} 의 부분집합입니다.
// id 는 독립 발견 표 id(DTO finding_id)입니다. 심각도 수정은 원래 탐색 노드에도
// 비쳐 작업별 보기가 맞게 남습니다. 고친 발견 DTO 를 돌려줍니다.
func (s *Server) patchFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "발견 id가 올바르지 않습니다")
		return
	}
	var body struct {
		Status    *string `json:"status"`
		Severity  *string `json:"severity"`
		Name      *string `json:"name"`
		VulnClass *string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "JSON 이 올바르지 않습니다: "+err.Error())
		return
	}
	if body.Status == nil && body.Severity == nil && body.Name == nil && body.VulnClass == nil {
		writeErr(w, 400, "바꿀 항목이 없습니다. status, severity, name, vulnclass 중 하나를 주세요")
		return
	}
	if body.Status != nil {
		if !db.ValidFindingStatus(*body.Status) {
			writeErr(w, 400, "status가 올바르지 않습니다: "+*body.Status)
			return
		}
		// 알림이 있는 경로를 탄다. 상태 갱신과 「상태 변경 푸시 이벤트」가 같은 트랜잭션에 저장되고,
		// 상태는 바뀌었는데 푸시 이벤트가 유실되는 창을 피한다. 이벤트 등록 실패는 상태 갱신에 영향을 주지 않으므로,
		// 로그만 남기고 호출자에게는 오류를 반환하지 않는다.
		from, found, notified, err := s.m.pg.SetFindingStatusWithNotify(r.Context(), id, *body.Status)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !found {
			writeErr(w, 404, "발견을 찾을 수 없습니다")
			return
		}
		if !notified && from != *body.Status {
			log.Printf("[notify] 상태 변경 이벤트가 등록되지 않음 finding=%d %s→%s(상태는 이미 갱신됨)", id, from, *body.Status)
		}
	}
	if body.Severity != nil {
		if !db.ValidSeverity(*body.Severity) {
			writeErr(w, 400, "severity가 올바르지 않습니다: "+*body.Severity)
			return
		}
		n, err := s.m.pg.SetFindingSeverity(id, *body.Severity)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeErr(w, 404, "발견을 찾을 수 없습니다")
			return
		}
	}
	if body.Name != nil {
		n, err := s.m.pg.SetFindingName(id, strings.TrimSpace(*body.Name))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeErr(w, 404, "발견을 찾을 수 없습니다")
			return
		}
	}
	if body.VulnClass != nil {
		n, err := s.m.pg.SetFindingVulnClass(id, strings.TrimSpace(*body.VulnClass))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeErr(w, 404, "발견을 찾을 수 없습니다")
			return
		}
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "발견을 찾을 수 없습니다")
		return
	}
	writeJSON(w, 200, findingFromDB(f, s.resolveAssetIDs(f.AssetIDs)))
}

// deleteFinding 은 발견을 지웁니다. 발견 행과 원래 탐색 노드입니다.
// id 는 독립 발견 표 id(DTO finding_id)입니다.
func (s *Server) deleteFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "발견 id가 올바르지 않습니다")
		return
	}
	n, err := s.m.pg.DeleteFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if n == 0 {
		writeErr(w, 404, "발견을 찾을 수 없습니다")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
}

func (s *Server) intents(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		// 예전 호환: 작업을 못 찾으면 맨 목록 모양을 그대로 돌려줍니다.
		writeJSON(w, 200, []any{})
		return
	}
	q := r.URL.Query()
	limit := min(atoiDefault(q.Get("limit"), 300), 500)
	// 페이지 매개변수가 없으면 예전 맨 배열 응답을 유지합니다. 기존 호출과 폴링이 그대로 됩니다.
	if q.Get("before") == "" && q.Get("page") == "" {
		in, err := t.Store.ListByKind(db.KindIntent, limit)
		if err != nil {
			log.Printf("[intents] task=%s limit=%d: %v", t.ID, limit, err)
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, taskNodeDTOs(in))
		return
	}
	// 페이지 형식: ?before=<id> (또는 표시용 ?page) 는 {items, has_more} 입니다.
	// 워커 세션 목록이 스크롤로 예전 고정 300 경계를 넘습니다. 직접 연관된 작업의
	// 불변 의도 결과도 포함하지만, 그 행은 이 작업의 프론티어나 집어 가기 경로에 들어가지 않습니다.
	before := int64(atoiDefault(q.Get("before"), 0))
	in, hasMore, err := taskIntentHistoryPage(t.Store, before, limit, false, 0)
	if err != nil {
		log.Printf("[intents] task=%s before=%d limit=%d: %v", t.ID, before, limit, err)
		writeErr(w, 500, err.Error())
		return
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		items, sourceMore, sourceErr := taskIntentHistoryPage(source.Store, before, limit, true, source.Task.TaskID)
		if sourceErr != nil {
			writeErr(w, 500, sourceErr.Error())
			return
		}
		in = append(in, items...)
		hasMore = hasMore || sourceMore
	}
	sort.Slice(in, func(i, j int) bool { return in[i].ID > in[j].ID })
	if len(in) > limit {
		in = in[:limit]
		hasMore = true
	}
	writeJSON(w, 200, map[string]any{"items": taskNodeDTOs(in), "has_more": hasMore})
}

func inheritedIntentResult(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// inheritedGraphSnapshot 은 원본 작업의 불변 기록만 보여 줍니다. 아직 열려 있거나
// 돌고 있는 일은 새지 않습니다. 간선도 노드와 같이 걸러, 숨은 살아있는 의도 구조를
// 드러내는 끊긴 id 가 응답에 남지 않습니다.
func inheritedGraphSnapshot(nodes []*db.Node, edges []db.Edge, sourceTaskID int64) ([]*db.Node, []db.Edge) {
	visible := make(map[int64]struct{}, len(nodes))
	filteredNodes := make([]*db.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || (node.Kind == db.KindIntent && !inheritedIntentResult(node.State)) {
			continue
		}
		node.SourceTaskID = sourceTaskID
		node.Inherited = true
		visible[node.ID] = struct{}{}
		filteredNodes = append(filteredNodes, node)
	}
	filteredEdges := make([]db.Edge, 0, len(edges))
	for _, edge := range edges {
		if _, ok := visible[edge.From]; !ok {
			continue
		}
		if _, ok := visible[edge.To]; !ok {
			continue
		}
		filteredEdges = append(filteredEdges, edge)
	}
	return filteredNodes, filteredEdges
}

// taskIntentHistoryPage 는 탐색 하나의 최신순 페이지입니다. 원본에 열린 의도나
// 돌고 있는 의도가 많아도, 프론티어를 화면에 흘리지 않고 과거 결과가 충분해질 때까지 페이지를 넘깁니다.
func taskIntentHistoryPage(store *db.ExplorationStore, before int64, limit int, inherited bool, sourceTaskID int64) ([]*db.Node, bool, error) {
	if !inherited {
		return store.ListByKindPage(db.KindIntent, before, limit)
	}
	batch := max(limit, 300)
	cursor := before
	out := make([]*db.Node, 0, limit+1)
	for {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil {
			return nil, false, err
		}
		for _, node := range page {
			if !inheritedIntentResult(node.State) {
				continue
			}
			node.SourceTaskID = sourceTaskID
			node.Inherited = true
			out = append(out, node)
			if len(out) > limit {
				return out[:limit], true, nil
			}
		}
		if !more || len(page) == 0 {
			return out, false, nil
		}
		cursor = page[len(page)-1].ID
	}
}

// explorationGraph 는 탐색 사슬 전체(작업 그래프)를 노드와 간선으로 돌려줍니다.
func (s *Server) explorationGraph(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{"nodes": []any{}, "edges": []any{}})
		return
	}
	nodes, err := t.Store.Nodes(2000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	edges, err := t.Store.Edges(5000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, source := range sources {
		sourceNodes, nodeErr := source.Store.Nodes(2000)
		if nodeErr != nil {
			writeErr(w, 500, nodeErr.Error())
			return
		}
		sourceEdges, edgeErr := source.Store.Edges(5000)
		if edgeErr != nil {
			writeErr(w, 500, edgeErr.Error())
			return
		}
		sourceNodes, sourceEdges = inheritedGraphSnapshot(sourceNodes, sourceEdges, source.Task.TaskID)
		nodes = append(nodes, sourceNodes...)
		edges = append(edges, sourceEdges...)
	}
	writeJSON(w, 200, map[string]any{"nodes": taskNodeDTOs(nodes), "edges": edgeDTOs(edges)})
}

// explorationNodes 는 방송판을 위해, 이 작업 자신의 탐색 노드를
// 페이지로 나눈 시간순입니다. ?order=asc 가 아니면 최신이 먼저입니다. kind, state,
// 페이로드 부분 문자열로 거릅니다. 물려받은 노드는 일부러 뺍니다. 이 보드는 이 작업이
// 지금 하는 일만 말하고, 원본 작업 저장소를 가로질러 페이지를 넘기면 커서가 의미를 잃습니다.
// 응답에는 이 페이지에 닿는 간선과 이웃 노드도 담깁니다.
// 각 행이 가리키는 곳입니다. 어디서 왔고 무엇을 만들었는지 말할 수 있습니다.
func (s *Server) explorationNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 1)
	size := atoiDefault(q.Get("size"), 20)
	t := s.m.ResolveTask(q.Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{
			"items": []any{}, "total": 0, "page": page, "size": size,
			"edges": []any{}, "refs": map[string]any{},
		})
		return
	}
	filter := db.NodeFilter{
		Kinds:  csvValues(q.Get("kind")),
		States: csvValues(q.Get("state")),
		Query:  q.Get("q"),
		Asc:    q.Get("order") == "asc",
	}
	nodes, total, err := t.Store.NodesPage(filter, page, size)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := make([]int64, 0, len(nodes))
	onPage := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
		onPage[n.ID] = true
	}
	edges, err := t.Store.EdgesTouching(ids)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	neighbourSet := map[int64]bool{}
	for _, e := range edges {
		if !onPage[e.From] {
			neighbourSet[e.From] = true
		}
		if !onPage[e.To] {
			neighbourSet[e.To] = true
		}
	}
	neighbourIDs := make([]int64, 0, len(neighbourSet))
	for id := range neighbourSet {
		neighbourIDs = append(neighbourIDs, id)
	}
	neighbours, err := t.Store.NodesByIDs(neighbourIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	refs := make(map[string]TaskNodeDTO, len(neighbours))
	for _, n := range neighbours {
		refs[i64s(n.ID)] = taskNodeDTO(n)
	}
	writeJSON(w, 200, map[string]any{
		"items":  taskNodeDTOs(nodes),
		"total":  total,
		"page":   page,
		"size":   size,
		"edges":  edgeDTOs(edges),
		"refs":   refs,
		"assets": s.nodeAnchoredAssets(t, append(ids, neighbourIDs...)),
	})
}

// nodeAnchoredAssets 는 주어진 노드의 앵커를 화면에 쓸 자산 이름으로 풉니다. 키는 노드 id 입니다.
// 앵커는 출처를 보여주는 장식입니다. 방송판용으로 제공한다. 여기서 실패해도 호출자의
// 페이지를 잃게 해서는 안 되므로, 오류는 로그만 남기고 "자산 없음"으로 낮춥니다.
func (s *Server) nodeAnchoredAssets(t *Task, nodeIDs []int64) map[string][]FindingAssetDTO {
	out := map[string][]FindingAssetDTO{}
	anchors, err := t.Store.NodeAssets(nodeIDs)
	if err != nil {
		log.Printf("[broadcast] node assets: %v", err)
		return out
	}
	var flat []int64
	for _, ids := range anchors {
		flat = append(flat, ids...)
	}
	assets := s.resolveAssetIDs(flat)
	for nodeID, ids := range anchors {
		if dtos := findingAssetDTOs(ids, assets); len(dtos) > 0 {
			out[i64s(nodeID)] = dtos
		}
	}
	return out
}

// csvValues 는 쉼표로 나눈 질의 값을 쪼개고 빈 항목은 버립니다.
func csvValues(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// activity 는 워커 실행 단계 기록입니다. ?since=seq 로 그 이후만 받습니다.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "cursor": 0})
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 300)
	var intentPtr *int64
	if iv := r.URL.Query().Get("intent"); iv != "" {
		if n, err := strconv.ParseInt(iv, 10, 64); err == nil {
			intentPtr = &n
		}
	}
	var (
		items  []db.Activity
		cursor int64
		err    error
	)
	if intentPtr != nil {
		items, cursor, err = t.Store.ActivityListWithSources(*intentPtr, since, limit)
	} else {
		items, cursor, err = t.Store.ActivityList(nil, since, limit)
	}
	if err != nil {
		log.Printf("[activity] task=%s since=%d limit=%d intent=%v: %v", t.ID, since, limit, intentPtr, err)
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": activityDTOs(items), "cursor": cursor})
}

// parseActivitySession 은 안정된 세션 키(main, plan, intent:<ID>)를 DB 세션 필터로 바꿉니다.
// 목표 에이전트와 플래너는 둘 다 worker="planner" 인 계획 세션 하나입니다.
// 워커 세션은 의도 하나이고, 키는 그 노드 id 입니다.
func parseActivitySession(sess string) (db.ActivitySessionFilter, bool) {
	switch {
	case sess == "" || sess == "main":
		// 맨 "main" 은 현재 구간입니다. 호출자가 저장소로 MainSeg 를 정합니다.
		return db.ActivitySessionFilter{Main: true}, true
	case strings.HasPrefix(sess, "main:"):
		seg, err := strconv.Atoi(strings.TrimPrefix(sess, "main:"))
		if err != nil || seg < 0 {
			return db.ActivitySessionFilter{}, false
		}
		return db.ActivitySessionFilter{Main: true, MainSeg: &seg}, true
	case sess == "plan":
		return db.ActivitySessionFilter{Worker: "planner"}, true
	case strings.HasPrefix(sess, "intent:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(sess, "intent:"), 10, 64)
		if err != nil {
			return db.ActivitySessionFilter{}, false
		}
		return db.ActivitySessionFilter{NodeID: &id}, true
	}
	return db.ActivitySessionFilter{}, false
}

// activitySessionStore 는 워커 세션을 현재 작업과 직접 원본에 맞춥니다.
// 플래너와 메인은 항상 이 작업에 있습니다. 물려받은 의도 세션은 불변 기록이라
// 읽을 때만 원본 탐색을 씁니다.
func activitySessionStore(t *Task, filter db.ActivitySessionFilter) (*db.ExplorationStore, int64, error) {
	if filter.NodeID == nil {
		return t.Store, 0, nil
	}
	node, err := t.Store.GetNodeWithSources(*filter.NodeID)
	if err != nil {
		return nil, 0, err
	}
	if node == nil || node.Kind != db.KindIntent {
		return nil, 0, nil
	}
	if !node.Inherited {
		return t.Store, 0, nil
	}
	if !inheritedIntentResult(node.State) {
		return nil, 0, nil
	}
	sources, err := t.Store.DirectSourceStores()
	if err != nil {
		return nil, 0, err
	}
	for _, source := range sources {
		if source.Task.TaskID == node.SourceTaskID {
			return source.Store, source.Task.TaskID, nil
		}
	}
	return nil, 0, nil
}

// activityHistory 는 세션 활동의 역순 페이지 하나입니다. ?before 가 없으면 최신 페이지로
// 세션을 엽니다. ?before=<id> 는 위로 스크롤할 때 더 오래된 페이지입니다.
// snapshot_cursor 는 조회 시점의 작업 단위 최대 id 입니다. 클라이언트가 작업 SSE 를
// since=snapshot_cursor 로 열면 기록(id<=커서)과 실시간 꼬리(id>커서)가 빈틈과 겹침 없이 만납니다.
func (s *Server) activityHistory(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	q := r.URL.Query()
	sess := q.Get("session")
	filter, ok := parseActivitySession(sess)
	if !ok {
		writeErr(w, 400, "세션이 올바르지 않습니다")
		return
	}
	if filter.Main && filter.MainSeg == nil { // 그냥 main이면 현재 구간입니다.
		seg, err := t.Store.CurrentMainSeg()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		filter.MainSeg = &seg
	}
	before := int64(atoiDefault(q.Get("before"), 0))
	limit := min(atoiDefault(q.Get("limit"), 200), 500) // 상한입니다. 요청 하나가 끝없는 조각을 가져가지 못하게 합니다.
	store, sourceTaskID, err := activitySessionStore(t, filter)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if store == nil {
		writeErr(w, 404, "세션을 찾을 수 없습니다")
		return
	}
	// 브라우저는 이 작업의 방송만 따라갑니다. 연 워커 기록이 연관 작업에서 왔어도
	// SSE 를 잇는 커서는 이 작업의 것으로 둡니다.
	snapshot, err := t.Store.ActivityMaxID()
	if err != nil {
		log.Printf("[activity/history] task=%s session=%s snapshot: %v", t.ID, sess, err)
		writeErr(w, 500, err.Error())
		return
	}
	var items []db.Activity
	var hasMore bool
	if sourceTaskID > 0 {
		// 원본 세션은 의도가 종료 상태일 때만 읽을 수 있습니다. DB 질의는 물려받은
		// 데이터에서 모델의 생각·정산 행도 뺍니다.
		items, hasMore, err = store.ActivityPageForTerminalIntent(*filter.NodeID, before, limit)
	} else {
		items, hasMore, err = store.ActivityPage(filter, before, limit)
	}
	if err != nil {
		log.Printf("[activity/history] task=%s session=%s before=%d limit=%d: %v", t.ID, sess, before, limit, err)
		writeErr(w, 500, err.Error())
		return
	}
	if sourceTaskID > 0 {
		for i := range items {
			items[i].SourceTaskID = sourceTaskID
			items[i].Inherited = true
		}
	}
	earliest := before
	if len(items) > 0 {
		earliest = items[0].ID
	}
	writeJSON(w, 200, map[string]any{
		"items":           activityDTOs(items),
		"snapshot_cursor": snapshot,
		"earliest_cursor": earliest,
		"has_more":        hasMore,
	})
}

// tokenStats 는 작업의 워커별 토큰 사용량입니다. 입력, 출력, 캐시 읽기·쓰기.
// 메인 에이전트, 플래너, work#N 각각입니다.
func (s *Server) tokenStats(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, map[string]any{
			"workers":  []db.TokenUsage{},
			"sessions": []db.SessionTokenUsage{},
			"total":    tokenTotalDTO(db.TokenUsage{}),
		})
		return
	}
	stats, err := t.Store.TokenStatsByWorker()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sessions, err := t.Store.TokenStatsBySession()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	total, err := t.Store.TokenTotal() // 작업 전체 합계(모든 에이전트)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"workers": stats, "sessions": sessions, "total": tokenTotalDTO(total)})
}

// tokenDailyStats 는 모든 작업의 토큰을 달력 날(UTC)로 모읍니다.
// 지난 ?days=N 일, 기본 30일입니다.
func (s *Server) tokenDailyStats(w http.ResponseWriter, r *http.Request) {
	days := atoiDefault(r.URL.Query().Get("days"), 30)
	if s.m.pg == nil {
		writeJSON(w, 200, []any{})
		return
	}
	buckets, err := s.m.pg.TokenDailyAll(days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if buckets == nil {
		buckets = []db.DailyTokenBucket{}
	}
	writeJSON(w, 200, buckets)
}

// conversationTokens 는 대화별 토큰 요약입니다. 대시보드가 채팅 사용량을
// 프로필별·일별 통계에 합칩니다. 그렇지 않으면 작업(탐색) 사용량만 셉니다.
func (s *Server) conversationTokens(w http.ResponseWriter, r *http.Request) {
	if s.m.pg == nil {
		writeJSON(w, 200, []any{})
		return
	}
	rows, err := s.m.pg.ConversationTokenSummaries()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}

// streamActivity 는 실시간 SSE 꼬리입니다. ?since=<seq> 이후 기록을 다시 보낸 뒤
// 새로 붙는 활동을 밀어 줍니다. seq 커서가 기록과 실시간을 빈틈 없이 잇습니다.
// 다시 붙을 때 클라이언트가 마지막 seq 를 넘겨 빠진 이벤트를 받습니다.
// ?intent=<id> 가 있으면 워커 세션 하나로 좁힙니다.
// getLogs 는 최근 백엔드 로그입니다. Seq 가 since 보다 큰 것, 오래된 것이 먼저입니다.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 500)
	lines, cursor := logSink.recent(since, limit)
	writeJSON(w, 200, map[string]any{"items": lines, "cursor": cursor})
}

// getLogsHistory 는 DB 에서 더 오래된 로그를 돌려줍니다. 주어진 db_id 보다 앞입니다.
// 예: GET /api/logs/history?before=<db_id>&limit=200
// {items:[LogLine], has_more: bool}을 돌려줍니다.
func (s *Server) getLogsHistory(w http.ResponseWriter, r *http.Request) {
	if s.m.pg == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "has_more": false})
		return
	}
	before := int64(atoiDefault(r.URL.Query().Get("before"), 0))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	if limit > 500 {
		limit = 500
	}
	// before가 없으면 가장 최근 DB 행을 돌려줍니다(링 복원과 같음).
	var (
		rows []*db.DBLog
		err  error
	)
	if before <= 0 {
		rows, err = s.m.pg.RecentLogs(limit)
	} else {
		rows, err = s.m.pg.ListLogsBefore(before, limit)
	}
	if err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	items := make([]LogLine, 0, len(rows))
	for _, r := range rows {
		items = append(items, LogLine{
			DBID:  r.ID,
			TS:    r.CreatedAt.Format(time.RFC3339),
			Level: r.Level,
			Tag:   r.Tag,
			Text:  r.Text,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items, "has_more": len(rows) == limit})
}

// streamLogs는 백엔드 로그의 실시간 SSE 꼬리입니다. ?since=seq 뒤의 기록을
// 다시 재생한 다음 새 줄을 밀어 줍니다.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "스트리밍을 지원하지 않습니다")
		return
	}
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := logSink.subscribe()
	defer unsub()
	send := func(l LogLine) {
		b, _ := json.Marshal(l)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	lines, cursor := logSink.recent(since, 1000)
	for _, l := range lines {
		send(l)
	}
	if cursor > since {
		since = cursor
	}
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			if l.Seq <= since {
				continue
			}
			since = l.Seq
			send(l)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) streamActivity(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "스트리밍을 지원하지 않습니다")
		return
	}
	var intentPtr *int64
	if iv := r.URL.Query().Get("intent"); iv != "" {
		if n, err := strconv.ParseInt(iv, 10, 64); err == nil {
			intentPtr = &n
		}
	}
	// 커서 우선순위: 브라우저의 자동 재연결은 Last-Event-ID를 보냅니다(받은
	// 마지막 id). 질의보다 이것을 믿어, 자동 재연결이 끊긴
	// 자리에서 정확히 이어지게 합니다. 새 연결이나 수동 연결에는 헤더가 없고,
	// 기록 페이지의 since=snapshot_cursor를 넘깁니다.
	since := int64(atoiDefault(r.URL.Query().Get("since"), 0))
	if le := r.Header.Get("Last-Event-ID"); le != "" {
		if n, err := strconv.ParseInt(le, 10, 64); err == nil {
			since = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 프록시 버퍼링을 끕니다.

	// 기록을 다시 재생하기 전에 구독합니다. 그 사이 이벤트를 잃지 않으려고요. 겹친 것은
	// 이미 재생한 id의 채널 이벤트를 건너뛰어 중복을 뺍니다.
	ch, unsub := s.engine.Broadcaster().Subscribe(t.ID)
	defer unsub()

	// 표준 SSE id 줄을 냅니다. 브라우저가 자동 재연결 때 Last-Event-ID로
	// 되울리게 합니다(위의 커서 우선순위를 보세요).
	sendSSE := func(a db.Activity) {
		b, _ := json.Marshal(activityDTO(a))
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", a.ID, b)
		flusher.Flush()
	}

	// since 뒤의 DB 밀린 분을 따라잡을 때까지 묶음으로 메웁니다. 이것은
	// 기록 스냅샷과 실시간 꼬리 사이의 빈틈입니다. 첫 페이지 기록이
	// 아닙니다(그것은 /activity/history). 긴 작업은 묶음 하나보다 훨씬 많을 수 있어,
	// 한 번만 읽지 않고 반복합니다. 질의 오류면 로그를 남기고 닫아,
	// 클라이언트가 마지막 id부터 다시 연결해 재시도하게 합니다(방송은 손실이 있습니다.
	// 기준은 DB입니다). 그 동안 Broadcaster는 실시간 이벤트를 버퍼합니다.
	// 아래의 id가 since 이하인 건너뛰기가, 이 재생이 이미 덮은 것을 버립니다.
	const replayBatch = 500
	for {
		items, cursor, err := t.Store.ActivityList(intentPtr, since, replayBatch)
		if err != nil {
			log.Printf("[activity/stream] task=%s replay since=%d: %v", t.ID, since, err)
			return
		}
		for _, a := range items {
			sendSSE(a)
		}
		if cursor > since {
			since = cursor
		}
		if len(items) < replayBatch {
			break
		}
	}
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-ch:
			if !ok {
				return
			}
			if a.ID <= since {
				continue // 이미 재생함
			}
			if intentPtr != nil && (a.NodeID == nil || *a.NodeID != *intentPtr) {
				continue // 좁힌 세션: 이 의도의 단계만
			}
			since = a.ID
			sendSSE(a)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// activityDetail은 단계 하나의 상세 본문 전체를 필요할 때 돌려줍니다.
func (s *Server) activityDetail(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "작업이 없습니다")
		return
	}
	seq, _ := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	d, err := taskActivityDetail(t, seq)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"detail": d})
}

// taskActivityDetail은 예전 로컬 작업 동작을 유지합니다(생각
// 행 포함). 물려받은 상세는 종료된 워커 의도로 제한합니다.
// 그래서 짐작한 전역 활동 id로 원본 플래너나 메인의
// 대화 기록, 또는 진행 중 기록을 보지 못하게 합니다.
func taskActivityDetail(t *Task, seq int64) (string, error) {
	return t.Store.ActivityDetailWithSources(seq)
}

func (s *Server) getTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "exchanges": []any{}})
		return
	}
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 0)
	size := atoiDefault(q.Get("size"), 100)
	ex, matched, _ := tr.Page(traffic.PageQuery{
		Host:    q.Get("host"),
		Method:  q.Get("method"),
		Query:   q.Get("q"),
		Body:    q.Get("body"),
		Path:    q.Get("path"),
		Status:  q.Get("status"),
		RespMin: int64(atoiDefault(q.Get("resp_min"), -1)),
		RespMax: int64(atoiDefault(q.Get("resp_max"), -1)),
		Sort:    q.Get("sort"),
		Order:   q.Get("order"),
	}, page, size)
	count, _ := tr.Count() // 통계 카드용 전역 합계
	writeJSON(w, 200, map[string]any{
		"enabled":   s.m.TrafficEnabled(), // 캡처 스위치를 반영합니다
		"proxy":     s.m.ProxyAddr(),
		"count":     count,   // 기록된 전체(필터 없음)
		"total":     matched, // 현재 필터에 맞는 행 수(페이지용)
		"page":      page,
		"size":      size,
		"exchanges": trafficDTOs(ex),
	})
}

// getTrafficHosts는 기록된 호스트를 개수와 함께 중복 없이 돌려줍니다. 화면의
// 대상 고르기에 씁니다(호스트를 고르면 목록을 거르고, 그다음 지움).
func (s *Server) getTrafficHosts(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeJSON(w, 200, map[string]any{"hosts": []any{}})
		return
	}
	hosts, err := tr.Hosts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"hosts": hosts})
}

// deleteTraffic은 질의의 호스트 부분 문자열을 포함하는 모든 호스트의 기록 트래픽을 지웁니다.
// 화면의 호스트 필터가 부분 문자열이라, 거른 것이
// 지워지는 것입니다. 색인 행과 호스트마다의 파일 트리를 지운 뒤,
// 남은 교환이 가리키지 않는 블롭을 거둡니다. 호스트가 비면 400입니다.
// 지운 교환 수를 돌려줍니다.
func (s *Server) deleteTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "트래픽 기록이 꺼져 있습니다")
		return
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		writeErr(w, 400, "host 가 없습니다")
		return
	}
	n, err := tr.DeleteHost(host)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// deleteTrafficHosts는 정확한 호스트 묶음의 트래픽을 지웁니다(JSON 본문
// hosts 배열). 화면의 여러 개 선택 삭제 경로입니다. 정확히
// 맞으므로, api.example.com을 골라도 api.example.com.cn은 안 지웁니다.
// 지운 교환 수를 돌려줍니다.
func (s *Server) deleteTrafficHosts(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "트래픽 기록이 꺼져 있습니다")
		return
	}
	var req struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "본문이 올바르지 않습니다")
		return
	}
	hosts := make([]string, 0, len(req.Hosts))
	for _, h := range req.Hosts {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		writeErr(w, 400, "hosts 항목이 필요합니다")
		return
	}
	n, err := tr.DeleteHostsExact(hosts)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// deleteAllTraffic은 기록된 교환을 모두 지운 뒤 색인을 압축해,
// 공간을 실제로 파일시스템에 돌려줍니다. 비운 색인은
// 전체를 다시 쓰기가 싼 유일한 때입니다. 발견에 이미 묶인 증거는
// 증거 저장소에 있고, 일부러 그대로 둡니다. 지운
// 교환 수와 되돌린 색인 바이트를 돌려줍니다.
func (s *Server) deleteAllTraffic(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "트래픽 기록이 꺼져 있습니다")
		return
	}
	n, reclaimed, err := tr.DeleteAll()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n, "reclaimed": reclaimed})
}

// getTrafficExchange는 교환 하나의 날것 요청과 응답 전체를 돌려줍니다.
// 필요할 때 트래픽 트리에서 읽습니다(본문은 페이지 목록에 없음).
func (s *Server) getTrafficExchange(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "트래픽 기록이 꺼져 있습니다")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "id 가 없습니다")
		return
	}
	req, resp, err := tr.Get(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"req": req, "resp": resp})
}

// getTrafficBlob은 너무 큰 본문 하나를 sha256으로 흘려 보냅니다. 인라인
// 상한을 넘은 본문은 교환 끝점이 안 실습니다. 미리보기와
// @blob sha256:<hash> 포인터만 줍니다. 화면이 전체를 가져오는 길이 이것입니다.
// 버퍼하지 않고 흘립니다. 메모리에 담기엔 너무 큰 본문이기 때문입니다.
func (s *Server) getTrafficBlob(w http.ResponseWriter, r *http.Request) {
	tr := s.m.Traffic()
	if tr == nil {
		writeErr(w, 404, "트래픽 기록이 꺼져 있습니다")
		return
	}
	hash := strings.TrimSpace(r.URL.Query().Get("hash"))
	if hash == "" {
		writeErr(w, 400, "hash 가 없습니다")
		return
	}
	f, size, err := tr.Blob(hash)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", hash+".bin"))
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("[traffic] blob %s 다운로드 중단: %v", hash, err)
	}
}

// getSettings는 화면이 켜고 끄는 실행 중 앱 설정을 돌려줍니다. Brave API 키는
// 값이 아니라 있는지 여부(brave_key_set)로 돌려줍니다.
// 화면이 설정됨을 보여도 비밀을 다시 울리지 않게 하려고요.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.settingsPayload())
}

func (s *Server) settingsPayload() map[string]any {
	on, backend, braveKey, tavilyKey, proxy := s.m.WebSearch()
	pyStored, _, _ := s.m.pg.GetSetting(settingPythonInterp)
	concOn, concLimit := s.m.ConcurrencyLimit()
	if concLimit == 0 {
		concLimit = defaultConcurrencyLimit // 꺼져 있을 때도 UI에 합리적인 기본값을 그대로 돌려준다
	}
	return map[string]any{
		"traffic_capture":          s.m.TrafficEnabled(),
		"agent_traffic_binding":    s.m.pg.GetBool(settingAgentTrafficBinding, false),
		"llm_record":               s.m.LLMRecordEnabled(),
		"web_search_enabled":       on,
		"web_search_backend":       backend,
		"brave_key_set":            strings.TrimSpace(braveKey) != "",
		"tavily_key_set":           strings.TrimSpace(tavilyKey) != "",
		"web_search_proxy":         proxy,                       // 독립 출구 프록시(http/https/socks5), 빈 값=직접 연결
		"global_proxy":             s.m.GlobalProxy(),           // 전역 출구 프록시(http/https/socks5), 모든 대상 트래픽이 여기를 타고, 빈 값=직접 연결
		"python_interpreter":       strings.TrimSpace(pyStored), // 사용자/자동으로 설정한 값(빈 값=런타임 감지 사용)
		"workers":                  s.m.Workers(),               // 동시 작업 agent 수(기본 3). 이후에 시작하는 작업에 적용된다
		"task_concurrency_enabled": concOn,                      // 작업 동시 실행 상한 스위치(기본 꺼짐)
		"task_concurrency_limit":   concLimit,                   // 동시에 실행 중인 작업 상한(켜면 기본 5)
		// LLM 라운드로빈(장애 전환). 기본 꺼짐. 켜면 전역 활성 설정을 쓰는 agent 가 현재 설정을 쓸 수 없을 때
		// 다음 설정으로 자동 전환한다. bind_fallback 은 라운드로빈이 켜져 있을 때만 의미가 있다(기본 꺼짐).
		"llm_pool_enabled":       s.m.LLMPoolEnabled(),
		"llm_pool_bind_fallback": s.m.LLMPoolBindFallback(),
		// 조작 제약 주입 범위(기본 모두 켜짐): 이 작업의 allow/deny 제약을 해당 agent 의 시스템 프롬프트에 붙인다.
		"constraints_inject_planner": s.constraintInjectPlanner(),
		"constraints_inject_worker":  s.constraintInjectWorker(),
		// 실험 기능: noa 모델 기반 컨텍스트 압축(기본 꺼짐). 켜면 플랫폼에 붙은 네 종류 agent 의 컨텍스트 압축을 noa 가
		// 맡아 내장 compaction 을 대체한다. run 마다 한 번 읽고, 이후에 시작하는 run 에 적용된다.
		"noa_compaction": s.m.NoaCompactionEnabled(),
		// 발견(finding) IM 푸시의 전역 항목. 채널 자체는 독립 리소스이며 /api/notify/* 로 관리한다.
		// 여기에는 「모든 채널에 작용하는」 세 항목만 둔다.
		"notify_enabled":             s.m.pg.GetBool(settingNotifyEnabled, true),
		"notify_public_base_url":     notifyPublicBaseURL(s.m.pg),
		"notify_digest_interval_min": notifyDigestIntervalMin(s.m.pg),
	}
}

// notifyPublicBaseURL 은 푸시 되돌아가기 링크에 쓰는 외부 주소를 읽는다.
func notifyPublicBaseURL(pg *db.DB) string {
	v, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	return v
}

// notifyDigestIntervalMin 은 요약 주기(분)를 읽고, 잘못되었거나 설정되지 않으면 기본값으로 돌아간다.
// 빈 문자열이 아니라 기본값을 되돌려야 UI 가 현재 적용 값을 입력란에 채울 수 있다.
func notifyDigestIntervalMin(pg *db.DB) int {
	v, ok, _ := pg.GetSetting(settingNotifyDigestMinutes)
	if !ok {
		return notifyDefaultDigestMinutes
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return notifyDefaultDigestMinutes
	}
	return n
}

// pgDetectPython은 인터프리터 찾기를 다시 돌리고, 저장한 뒤 돌려줍니다.
func (s *Server) pgDetectPython(w http.ResponseWriter, r *http.Request) {
	p := detectPython()
	if p == "" {
		writeErr(w, 404, "python을 찾지 못했습니다(python3/python 모두 PATH에 없음)")
		return
	}
	if err := s.m.pg.SetSetting(settingPythonInterp, p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"python_interpreter": p})
}

// putSettings는 설정 변경을 적용합니다. traffic_capture를 바꾸면
// 에이전트를 다시 만듭니다(applyLLM). 새 프록시, 트래픽 도구, 프롬프트가 먹게 하려고요.
// 꺼지면 에이전트는 프록시 설정, 트래픽 도구, 프록시 프롬프트 내용이 없습니다.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TrafficCapture      *bool `json:"traffic_capture"`
		AgentTrafficBinding *bool `json:"agent_traffic_binding"`
		LLMRecord           *bool `json:"llm_record"` // LLM 녹화 스위치(기본 꺼짐). 즉시 적용되며 agent 를 다시 만들 필요가 없다
		// 웹 검색입니다. WebSearchEnabled와 Backend가 도구와 백엔드를 바꿉니다. BraveKey와 TavilyKey는
		// 선택입니다. 빼면(null) 저장된 키를 그대로 두고, 빈 문자열을 보내면 지웁니다.
		WebSearchEnabled *bool   `json:"web_search_enabled"`
		WebSearchBackend *string `json:"web_search_backend"`
		BraveKey         *string `json:"brave_search_api_key"`
		TavilyKey        *string `json:"tavily_search_api_key"`
		WebSearchProxy   *string `json:"web_search_proxy"`   // 독립 출구 프록시(http/https/socks5). null=변경 없음, ""=비움
		GlobalProxy      *string `json:"global_proxy"`       // 전역 출구 프록시(http/https/socks5). null=변경 없음, ""=비움(직접 연결)
		PythonInterp     *string `json:"python_interpreter"` // 사용자 정의 스크립트 도구의 python 인터프리터 경로
		Workers          *int    `json:"workers"`            // 동시 작업 agent 수(>0). 이후에 시작하는 작업에 적용된다
		// 작업 동시 실행 상한: 동시에 「실행 중」인 작업 수 상한. 꺼짐=무제한. 켜면 새 작업이 상한을 넘을 때 대기하고, 자리가 나면 자동으로 시작한다.
		ConcurrencyEnabled *bool `json:"task_concurrency_enabled"`
		ConcurrencyLimit   *int  `json:"task_concurrency_limit"`
		// LLM 라운드로빈(장애 전환) 스위치 + 「바인딩된 설정이 실패해도 라운드로빈 체인으로 폴백」 스위치. 둘 다
		// provider 체인을 다시 만들어야 적용되며, 아래의 changed → applyLLM 경로를 탄다.
		LLMPoolEnabled      *bool `json:"llm_pool_enabled"`
		LLMPoolBindFallback *bool `json:"llm_pool_bind_fallback"`
		// 조작 제약 주입 범위 스위치(기본 모두 켜짐). 즉시 적용된다(플래너/워커가 매 라운드 읽음). agent 를 다시 만들 필요가 없다.
		ConstraintsInjectPlanner *bool `json:"constraints_inject_planner"`
		ConstraintsInjectWorker  *bool `json:"constraints_inject_worker"`
		// 실험 기능: noa 컨텍스트 압축 스위치(기본 꺼짐). run 마다 읽고, 이후에 시작하는 run 에 적용되며, agent 를 다시 만들 필요가 없다.
		NoaCompaction *bool `json:"noa_compaction"`
		// 발견(finding) IM 푸시의 전역 항목. 세 항목 모두 전달 엔진이 매 라운드 한 번씩 읽으므로, 바꾸면 즉시 적용되고,
		// agent 를 다시 만들거나 재시작할 필요가 없다.
		NotifyEnabled    *bool   `json:"notify_enabled"`
		NotifyBaseURL    *string `json:"notify_public_base_url"`
		NotifyDigestMins *int    `json:"notify_digest_interval_min"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.ConstraintsInjectPlanner != nil {
		if err := s.m.pg.SetBool(settingConstraintsInjectPlanner, *req.ConstraintsInjectPlanner); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.ConstraintsInjectWorker != nil {
		if err := s.m.pg.SetBool(settingConstraintsInjectWorker, *req.ConstraintsInjectWorker); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.NoaCompaction != nil {
		// run 마다 읽는 해석기. 전환은 이후에 시작하는 run 에 즉시 적용되며, applyLLM 으로 다시 만들 필요가 없다.
		if err := s.m.SetNoaCompaction(*req.NoaCompaction); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// 푸시 전역 항목: 전달 엔진이 매 라운드 다시 읽으므로 즉시 적용되고, 재시작할 필요가 없다.
	if req.NotifyEnabled != nil {
		if err := s.m.pg.SetBool(settingNotifyEnabled, *req.NotifyEnabled); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.NotifyBaseURL != nil {
		// 끝의 슬래시를 통일해서 자른다. 되돌아가기 링크는 fmt.Sprintf("%s/function/...") 로 붙이므로,
		// 끝 슬래시를 남기면 "//function/..." 같은 이중 슬래시 경로가 나온다.
		base := trimTrailingSlash(strings.TrimSpace(*req.NotifyBaseURL))
		if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			writeErr(w, 400, "콜백 주소는 http:// 또는 https://로 시작해야 합니다")
			return
		}
		if err := s.m.pg.SetSetting(settingNotifyPublicBaseURL, base); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.NotifyDigestMins != nil {
		// 하한 1분. 더 짧은 주기는 실시간 푸시와 같으므로, 그때는 채널을 realtime 모드로 바꾸는 편이 맞다.
		if *req.NotifyDigestMins < 1 || *req.NotifyDigestMins > 24*60 {
			writeErr(w, 400, "집계 주기는 1분에서 1440분 사이여야 합니다")
			return
		}
		if err := s.m.pg.SetSetting(settingNotifyDigestMinutes, strconv.Itoa(*req.NotifyDigestMins)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.Workers != nil {
		if err := s.m.SetWorkers(*req.Workers); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if req.ConcurrencyEnabled != nil || req.ConcurrencyLimit != nil {
		// 부분 PUT: 주지 않은 필드는 현재 값으로 메운다. 하나만 고치려다 다른 값이 초기화되는 일을 피한다.
		curOn, curLimit := s.m.ConcurrencyLimit()
		if curLimit == 0 {
			curLimit = defaultConcurrencyLimit
		}
		on, limit := curOn, curLimit
		if req.ConcurrencyEnabled != nil {
			on = *req.ConcurrencyEnabled
		}
		if req.ConcurrencyLimit != nil {
			limit = *req.ConcurrencyLimit
		}
		if err := s.m.SetConcurrency(on, limit); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// 즉시 한 번 조정한다. 끌 때는 대기 중인 것을 모두 통과시키고, 상한을 올리면 빈 자리를 채워 시작하며, 다음 tick 을 기다릴 필요가 없다.
		go s.reconcileConcurrency()
	}
	if req.PythonInterp != nil {
		if err := s.m.pg.SetSetting(settingPythonInterp, strings.TrimSpace(*req.PythonInterp)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.LLMRecord != nil {
		// 녹화기는 호출할 때마다 이 플래그를 읽는다. 전환은 즉시 적용되며 applyLLM 으로 다시 만들 필요가 없다.
		if err := s.m.SetLLMRecordEnabled(*req.LLMRecord); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	changed := false
	if req.LLMPoolEnabled != nil {
		if err := s.m.SetLLMPoolEnabled(*req.LLMPoolEnabled); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true // 제공자 사슬 모양 자체가 바뀜 → 다시 만듭니다.
	}
	if req.LLMPoolBindFallback != nil {
		if err := s.m.SetLLMPoolBindFallback(*req.LLMPoolBindFallback); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	if req.LLMPoolEnabled != nil || req.LLMPoolBindFallback != nil {
		// 고정된 작업의 플래너/워커와 설정별 채팅 에이전트는
		// 옛 스위치 상태로 만든 제공자를 가집니다. 버려서 새 것을 받게 합니다.
		s.invalidateProfileAgents()
	}
	if req.AgentTrafficBinding != nil {
		if err := s.m.pg.SetBool(settingAgentTrafficBinding, *req.AgentTrafficBinding); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.TrafficCapture != nil {
		if err := s.m.SetTrafficEnabled(*req.TrafficCapture); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	if req.GlobalProxy != nil {
		// 검사 실패(나쁜 스킴이나 호스트)는 클라이언트 오류입니다. 500이 아닙니다.
		if err := s.m.SetGlobalProxy(*req.GlobalProxy); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		changed = true // 캡처가 꺼진 출구는 만들 때 에이전트에 구워집니다 → 다시 만듭니다.
	}
	if req.WebSearchEnabled != nil || req.WebSearchBackend != nil || req.BraveKey != nil || req.TavilyKey != nil || req.WebSearchProxy != nil {
		// 안 준 필드는 현재 상태로 채웁니다. 일부만 PUT해도 나머지가 리셋되지 않게 합니다.
		on, backend, _, _, _ := s.m.WebSearch()
		if req.WebSearchEnabled != nil {
			on = *req.WebSearchEnabled
		}
		if req.WebSearchBackend != nil {
			backend = *req.WebSearchBackend
		}
		if err := s.m.SetWebSearch(on, backend, req.BraveKey, req.TavilyKey, req.WebSearchProxy); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	if changed {
		// 에이전트를 다시 만듭니다. 새 프록시, 도구, 프롬프트, 웹 검색이 먹게 합니다(LLM이 설정된 경우만).
		s.cfgMu.Lock()
		cfg, on := s.llmCfg, s.llmOn
		s.cfgMu.Unlock()
		if on {
			if err := s.applyLLM(cfg); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
		}
	}
	writeJSON(w, 200, s.settingsPayload())
}

// testWebSearch는 준(또는 지금 저장된)
// 백엔드, 프록시, 키로 시험 검색을 실제로 돌려, 설정이 검색 백엔드에 닿는지 확인합니다.
// testLLM과 같습니다. 백엔드와 프록시는 요청에서 옵니다(그래서 폼의 아직 안 저장한
// 수정을 시험함). API 키가 비면 저장된 값으로 물러섭니다. 사용자가
// 다시 치지 않아도 됩니다. 항상 200이고 {ok, error?, count?, backend?}입니다.
func (s *Server) testWebSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend   string `json:"web_search_backend"`
		Proxy     string `json:"web_search_proxy"`
		BraveKey  string `json:"brave_search_api_key"`
		TavilyKey string `json:"tavily_search_api_key"`
	}
	// 본문이 비어 있어도 됩니다. 아래의 저장된 설정으로 전부 물러섭니다.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, 400, err.Error())
		return
	}
	_, backend, storedBraveKey, storedTavilyKey, _ := s.m.WebSearch()
	if strings.TrimSpace(req.Backend) != "" {
		backend = req.Backend
	}
	// 프록시는 폼에 있는 그대로 씁니다(비어 있으면 직접 연결). 그래서 시험은 화면에
	// 보인 것과 같습니다. 저장 전에 프록시를 비워 직접 연결을 시험하는 것도 포함합니다.
	proxy := strings.TrimSpace(req.Proxy)
	// API 키는 비밀이라, 이미 저장했으면 폼이 빼 둡니다. 그래서 저장된 값으로 물러섭니다.
	braveKey := storedBraveKey
	if strings.TrimSpace(req.BraveKey) != "" {
		braveKey = req.BraveKey
	}
	tavilyKey := storedTavilyKey
	if strings.TrimSpace(req.TavilyKey) != "" {
		tavilyKey = req.TavilyKey
	}
	cfg := actool.WebSearchConfig{Backend: backend, BraveAPIKey: braveKey, TavilyAPIKey: tavilyKey, Proxy: proxy}
	// 단단한 상한입니다. 느리거나 막힌 프록시가 요청을 붙잡지 못하게 합니다.
	wall := 30 * time.Second
	// deepseek 자격 증명은 폼에 없고, 현재 활성화된 LLM 설정에서 온다. 또한 검색할 때마다 한 번씩
	// 모델 추론을 돌리므로, 30초 공통 상한은 빡빡해서 따로 늘린다. 여기서 설정이 쓸 수 있는지는 미리 판단하지 않는다. 이 한 번을 재는 것
	// 자체가 사용자가 직접 확인하는 수단이고, 정말 안 될 때는 아래 오류가 사전 판단보다 정보가 많다.
	probeQuery := "test"
	if strings.TrimSpace(backend) == deepSeekWebSearchBackend {
		cfg.DeepSeekBaseURL, cfg.DeepSeekAPIKey, cfg.DeepSeekModel = s.m.deepSeekSearchCreds()
		wall = 120 * time.Second
		// 검색어는 DeepSeek 쪽 모델이 스스로 정한다. "test" 는 너무 막연해서 검색을 건너뛰고 바로 답하게 만든다.
		probeQuery = "DeepSeek company official website"
	}
	ctx, cancel := context.WithTimeout(r.Context(), wall)
	defer cancel()
	results, err := actool.WebSearchProbe(ctx, cfg, probeQuery, 3)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "backend": backend})
		return
	}
	if len(results) == 0 {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "검색 결과가 0건입니다(속도 제한되었거나 프록시가 연결되지 않았을 수 있음)", "backend": backend})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(results), "backend": backend})
}

// mainSessions는 작업의 메인 에이전트 대화 구간을 나열합니다(최신 먼저). 그리고
// 현재 것. 프론트엔드는 이들을 메인 에이전트 아래 전환 가능한 세션으로 그린다.
func (s *Server) mainSessions(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	list, err := t.Store.ListMainSessions()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	current := 0
	if len(list) > 0 {
		current = list[0].Seq // 최신 먼저
	}
	writeJSON(w, 200, map[string]any{"sessions": list, "current": current})
}

// newMainSession은 새 메인 에이전트 대화 구간을 시작합니다. 구간
// 카운터만 올라갑니다. 작업의 탐색 그래프, 자산, 목표는 그대로라,
// 메인 에이전트는 같은 작업을 깨끗한 대화 기록과 맥락으로 이어 갑니다.
// 초보용: 새 대화 구간만 열고, 탐색 그래프와 자산, 목표는 그대로 둡니다.
func (s *Server) newMainSession(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 새 세션을 만들 수 없습니다")
		return
	}
	m, err := t.Store.NewMainSession()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"seq": m.Seq, "created_at": rfc3339(m.CreatedAt), "current": m.Seq})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "활성화된 작업이 없습니다")
		return
	}
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 새 메시지를 보낼 수 없습니다")
		return
	}
	// 주의: 작업 일시정지(paused)는 메인 에이전트 대화를 가로채지 않는다. 메인 에이전트 오케스트레이션 세션은 플래너/
	// 워커의 일시정지와 별개이며, 일시정지 중에도 대화는 이어갈 수 있다(일시정지는 진행 중인 그 한 라운드만 끝내며, control() 참고).
	var req struct {
		Message     string           `json:"message"`
		Attachments []chatAttachment `json:"attachments,omitempty"` // 방식 1로 올린 파일(경로는 작업 작업 디렉터리 기준)
		Seg         *int             `json:"seg,omitempty"`         // 대상 메인 세션 구간. 생략=최신 구간
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	agentMessage, ok := s.prepareChatMentionMessage(w, req.Message)
	if !ok {
		return
	}
	// 입장은 삭제의 채팅 취소와 순서를 맞춥니다. 모든 채팅
	// 턴은 첫 활동이나 파일 쓰기 전에 바쁨으로 표시합니다. 규칙 모드 턴도 포함합니다.
	// 삭제가 경주에서 이기면, 두 번째 장벽 검사가 이 요청을 거절합니다.
	s.chatMu.Lock()
	if s.engine.IsDeleting(t.ID) {
		s.chatMu.Unlock()
		writeErr(w, 409, "작업을 삭제하는 중이라 새 메시지를 보낼 수 없습니다")
		return
	}
	if s.chatBusy[t.ID] {
		s.chatMu.Unlock()
		writeErr(w, 409, "메인 에이전트가 이전 메시지를 처리 중입니다. 잠시 기다려 주세요")
		return
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: req.Message})
	s.chatBusy[t.ID] = true
	s.chatCancel[t.ID] = cancel
	s.chatMu.Unlock()

	// 이 턴은 사용자가 대화 중인 메인 에이전트 구간의 것입니다(어느
	// 구간이든 맨 위 채팅 대화처럼 주고받을 수 있음). 모든
	// mainagent 행에 그것을 찍어, 이 턴의 대화 기록과 활동이 그 구간에 들어가게 합니다.
	// seg가 없으면(옛 클라이언트) 가장 새 구간으로 물러섭니다.
	mainSeg := 0
	if req.Seg != nil && *req.Seg >= 0 {
		mainSeg = *req.Seg
	} else {
		mainSeg, _ = t.Store.CurrentMainSeg()
	}
	segPtr := &mainSeg

	// 사람의 턴을 저장하고 브로드캐스트해서, 메인 에이전트 오케스트레이션 세션이 페이지
	// 를 새로고침해도 실시간으로 갱신됩니다. 대화는 활동 스트림에 있습니다.
	// worker는 mainagent입니다(작업별 활동 표, SSE로 다시 재생). 첨부가
	// 있으면 활동의 Detail에 {text, attachments}가 들어가 대화 기록이
	// 첨부 카드를 그립니다.
	humanTurn := userActivityWithAttachments("mainagent", req.Message, req.Attachments)
	humanTurn.MainSeg = segPtr
	s.engine.emitActivity(t, humanTurn)
	var ma *agent.MainAgent
	if s.taskRuntimeAvailable(t, "mainagent") {
		ma = s.agentsForTask(t).main
	}
	if ma != nil {
		// 에이전트는 턴마다 취소 가능한 ctx에서 돕니다(r.Context()가 아님). 턴은
		// 몇 분이 걸릴 수 있습니다(도구를 여러 번 도는 루프). 요청 수명에 묶으면
		// 페이지 새로고침이나 프록시 시간 초과가 도중에 취소했습니다(context canceled).
		// 단계와 마지막 답은 SSE로 실시간으로 돌아옵니다(worker는 mainagent). 그래서
		// 처리기는 바로 돌아가고, 브라우저가 요청을 붙들고 있을 필요가 없습니다.
		// 초보용: 화면의 메인 에이전트 턴은 요청이 아니라 서버 배경에서 돌고, 단계는 SSE로 흐릅니다.
		go func() {
			defer func() {
				s.finishTaskChat(t.ID, cancel)
			}()
			// 모든 단계를 내보냅니다(생각, tool_use, tool_result, 글, result). 그래서 메인
			// 에이전트 세션이 워커나 플래너처럼 일을 실시간으로 보여 줍니다. 마지막 답은
			// 잡은 result 단계입니다. 답을 따로 내보내면 중복됩니다.
			emit := func(rec db.Activity) {
				rec.MainSeg = segPtr
				s.engine.emitActivity(t, rec)
			}
			maTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
			resume := func() { s.reviveTask(t) } // set_goals 가 목표를 추가하면 → 작업을 running 으로 되돌린다
			// 업로드한 첨부파일의 【절대 경로】 목록을 agent에게 보내는 메시지에 이어 붙인다. agent는 이를 보고 Read/Bash로 파일을 연다.
			// taskDir = agent의 작업 디렉터리(CWD)이며, chatUpload이 디스크에 쓰는 위치, ensureRunDir와 같다.
			taskDir := filepath.Join(s.m.dir, "tasks", t.ID)
			agentMsg := composeAgentMessage(agentMessage, req.Attachments, taskDir)
			s.engine.BeginLLMCall(t.ID)
			_, err := ma.Chat(ctx, maTaskID, mainSeg, s.m.Assets(), t.Store, t.Goal, agentMsg, emit, t.Notify, resume, t.NotifyGoal, t.NotifyHint)
			s.engine.EndLLMCall(t.ID)
			if err != nil && ctx.Err() == nil {
				s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", IsError: true, Summary: "(메인 에이전트 오류: " + err.Error() + "）", MainSeg: segPtr})
			}
		}()
		writeJSON(w, 202, map[string]any{"status": "accepted", "mode": "llm"})
		return
	}
	reply := s.fallbackChat(t, req.Message)
	s.engine.emitActivity(t, db.Activity{Worker: "mainagent", Kind: "text", Summary: reply, MainSeg: segPtr})
	s.finishTaskChat(t.ID, cancel)
	writeJSON(w, 200, map[string]any{"reply": reply, "mode": "rule"})
}

func (s *Server) cancelTaskChat(taskID string, cause error) bool {
	s.chatMu.Lock()
	cancel := s.chatCancel[taskID]
	if cancel != nil {
		cancel(cause)
	}
	s.chatMu.Unlock()
	return cancel != nil
}

func (s *Server) finishTaskChat(taskID string, cancel context.CancelCauseFunc) {
	cancel(agent.AbortChatTurnFinished)
	s.chatMu.Lock()
	delete(s.chatBusy, taskID)
	delete(s.chatCancel, taskID)
	s.chatMu.Unlock()
}

// taskChatStatus는 작업의 메인 에이전트 턴 상태를 권위 있게 알립니다.
// 활동 시각은 믿을 만한 대리가 아닙니다. 도구 호출이나 LLM 호출이
// 중간 프레임 없이 몇 분 돌 수 있기 때문입니다.
func (s *Server) taskChatStatus(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, http.StatusNotFound, "작업을 찾을 수 없습니다")
		return
	}
	s.chatMu.Lock()
	running := s.chatBusy[t.ID]
	s.chatMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"running": running})
}

// stopChat은 작업의 진행 중 메인 에이전트 턴을 끊습니다(수동 정지 버튼).
// pgStopConversation과 같습니다. 현재 턴만 취소합니다. 플래너와 워커는
// 영향 없이 계속 돕니다. 사용자는 바로 새 메시지를 보낼 수 있습니다.
func (s *Server) stopChat(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.PathValue("id"))
	if t == nil {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.cancelTaskChat(t.ID, agent.AbortChatStoppedByUser) {
		writeJSON(w, 200, map[string]any{"status": "idle"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "stopping"})
}

// fallbackChat is the no-LLM human-steering handler: 단순 명령 + 상황 요약.
func (s *Server) fallbackChat(t *Task, msg string) string {
	m := strings.TrimSpace(msg)
	lower := strings.ToLower(m)
	switch {
	case strings.HasPrefix(m, "의도") || strings.HasPrefix(lower, "intent"):
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(m, "의도"), "intent"))
		_, _ = t.Store.AddIntent(map[string]any{"summary": text}, 9, nil, "human")
		return "높은 우선순위 의도 하나를 주입했습니다: " + text
	case strings.HasPrefix(m, "힌트") || strings.HasPrefix(lower, "hint"):
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(m, "힌트"), "hint"))
		_, _ = t.Store.AddNode(db.KindHint, map[string]any{"text": text}, 0, "active", "human", nil)
		return "힌트를 기록했습니다. 플래너가 다음에 읽습니다: " + text
	default:
		assetCounts, _ := s.m.Assets().CountsByType()
		assets := 0
		for _, c := range assetCounts {
			assets += c
		}
		fnd, _ := t.Store.ListByKind(db.KindFinding, 1000)
		fr, _ := t.Store.Frontier(1000)
		return fmt.Sprintf("(규칙 모드, LLM 미설정) 현재 상황: 자산 %d, 수령 대기 의도 %d, 확인된 발견 %d.\n사용 가능한 명령: \"의도 ...\"로 의도를 주입하고, \"힌트 ...\"로 플래너에게 힌트를 줍니다.", assets, len(fr), len(fnd))
	}
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeErr(w, 404, "활성화된 작업이 없습니다")
		return
	}
	findings, _ := t.Store.ListByKind(db.KindFinding, 1000) // 순수 발견(finding)(사실은 독립된 KindFact이며, 보고서에는 들어가지 않는다)
	counts := map[string]int{}
	for _, ty := range []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"} {
		ns, _ := s.m.Assets().QueryByType(ty, 100000, 0)
		if len(ns) > 0 {
			counts[ty] = len(ns)
		}
	}
	md := report.Markdown(report.Input{
		Title: t.Description, Goal: t.Goal, GeneratedAt: time.Now(),
		AssetCounts: counts, Findings: findings,
	})
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(md))
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	t := s.m.ResolveTask(r.URL.Query().Get("task"))
	if t == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, map[string]any{"entries": t.Guard.Audit(), "attributions": t.Guard.Attributions()})
}

// gc는 아직 아무 일도 안 하는 자리입니다(새 자산 저장소에는 GC가 아직 없음).
func (s *Server) gc(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"removed": 0})
}

// --- 도우미 ---

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func atoiDefault(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}
