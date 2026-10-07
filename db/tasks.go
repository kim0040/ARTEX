package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Task는 작업 등록부의 한 줄이다(탐색과 1:1). 탐색 그래프는 이 작업 안에 있다.
type Task struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"` // 선택적 작업 이름; 비어 있음=이름 없음
	CategoryID    *int64     `json:"category_id,omitempty"`
	CategoryName  string     `json:"category_name,omitempty"`
	Pinned        bool       `json:"pinned"`
	PinnedAt      *time.Time `json:"pinned_at,omitempty"`
	Description   string     `json:"description"`
	Goal          string     `json:"goal"`
	ExplorationID int64      `json:"exploration_id"`
	Status        string     `json:"status"`
	Paused        bool       `json:"paused"`
	Queued        bool       `json:"queued"`
	QueuedAt      *time.Time `json:"queued_at,omitempty"`
	QueueMode     string     `json:"queue_mode,omitempty"`
	LLMProfileID  *int64     `json:"llm_profile_id,omitempty"`
	// 작업 단위로 순서가 있는 LLM 사슬. 옛 API 클라이언트가 프로파일 id 하나만
	// 보내는 동안 LLMProfileID는 ActiveLLMProfileID의 호환 별칭으로 남는다.
	LLMProfileIDs      []int64    `json:"llm_profile_ids,omitempty"`
	ActiveLLMProfileID *int64     `json:"active_llm_profile_id,omitempty"`
	LLMChainRevision   int64      `json:"-"`
	LLMFailoverState   string     `json:"llm_failover_state,omitempty"`
	LLMFailoverReason  string     `json:"llm_failover_reason,omitempty"`
	SourceTaskIDs      []int64    `json:"source_task_ids,omitempty"`
	CompanyIDs         []int64    `json:"company_ids,omitempty"`
	ParentRef          string     `json:"parent_ref,omitempty"` // 부모 작업 id(오케스트레이션 spawn 기록; 비어 있음=최상위)
	CreatedAt          time.Time  `json:"created_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"` // 종료 상태(done/failed/timeout)에 들어간 시각; 종료 상태가 아니면 nil
	// 태스크 수준 타임아웃(docs/태스크-수준-타임아웃과-마무리-설계.md 참고).
	TimeoutSeconds int        `json:"timeout_seconds"`        // 0=시간 제한 없음
	FirstRunAt     *time.Time `json:"first_run_at,omitempty"` // 실제로 처음 실행을 시작한 시각(created_at이 아님); nil=아직 실행 전
	DeadlineAt     *time.Time `json:"deadline_at,omitempty"`  // = first_run_at + timeout_seconds; nil=제한 없음 또는 아직 실행 전
	// planner 하트비트 트리거 간격(초): 이전 plan 종료 또는 작업 시작부터 이 값이 차고 그 동안 트리거가 없으면 → 한 라운드를 트리거한다.
	// 하한=기본값=300(5min). 이보다 낮으면 모두 300으로 올린다(CreateTask에서 정규화). docs/planner-trigger-impl-plan.md 참고
	PlanHeartbeatSeconds int `json:"plan_heartbeat_seconds"`
	// CoverageEnabled는 「자산 커버리지 기능」의 총스위치이다(기본값 true). false이면: 테스트
	// 커버리지를 계산하거나 보여 주지 않고, task_scope(source=auto)를 자동 누적하지 않으며, agent에게 add_task_scope/
	// list_untested_assets를 열지 않고, 상황판에 coverage 블록을 넣지 않는다(scope 필드는 유지). company 연관
	// (task_scope kind=company)은 이 스위치와 무관하며 언제나 영향을 받지 않는다. db/task_scope.go 참고. 이 스위치는 자산 그래프의 테스트 범위가 UI에 보이는 방식을 정한다.
	CoverageEnabled bool `json:"coverage_enabled"`
}

// TaskDeleteResult는 작업·탐색 삭제와 같은 트랜잭션에서 한
// 선택적 관련 데이터 정리를 보고한다.
type TaskDeleteResult struct {
	AssetsDeleted     int64
	AssetsDetached    int64
	FindingsDeleted   int64
	LLMRecordsDeleted int64
}

// TaskDeletePreparation은 PostgreSQL 삭제 트랜잭션 안에서,
// 자산·앵커를 쓰는 쪽을 막은 뒤에 만든다. Prepare 콜백은
// PostgreSQL이 커밋하기 전에 TrafficHosts로 바깥 트래픽 삭제를 준비할 수 있다.
type TaskDeletePreparation struct {
	ExplorationID int64
	TrafficHosts  []string
}

// IsTerminal은 작업 상태가 끝난 상태인지 알려 준다.
// 유일한 기준이며, 곳곳에 흩어진 done/failed 하드코딩 판정을 대체한다.
func IsTerminal(status string) bool {
	return status == "done" || status == "failed" || status == "timeout"
}

// CreateTask는 탐색과 작업을 한 트랜잭션에 만들고 작업을 돌려준다.
// timeoutSeconds는 작업 단위 벽시계 예산이다(0 = 시간 제한 없음). deadline_at은
// 여기서가 아니라 처음 실제 실행 때 찍힌다(엔진을 본다).
// MinPlanHeartbeatSeconds는 planner 하트비트 간격의 하한 = 기본값 = 10min이다.
// 그보다 낮으면(기본 누락 0 / 음수 / 잘못 설정된 작은 값 포함) 모두 10min으로 올려 planner가 과부하되지 않게 한다.
const MinPlanHeartbeatSeconds = 600

// MaxTaskSourceCount는 작업 하나가 플래너·메인 에이전트 프롬프트마다 끌어오는
// 살아있는 상속 맥락의 상한이다. 상속은 일부러 직접 연결만 한다.
// 들어오는 수를 묶어, 생성 요청 하나가 탐색 그래프와 자산 맥락 조회를 끝없이 곱하지 않게 한다.
const MaxTaskSourceCount = 8

// MaxTaskCompanyCount는 작업에 붙는 기업 자산 범위 수의 상한이다.
// 기업 범위는 프롬프트 맥락이고, 지금 귀속된
// 자산은 작업을 만들 때 스냅샷으로 작업에 넣는다.
const MaxTaskCompanyCount = 32

const taskCompanyAssetSource = "company"

var (
	ErrTaskCompanyIDsInvalid = errors.New("invalid task company ids")
	ErrTaskCompanyNotFound   = errors.New("작업의 기업을 찾을 수 없습니다")
)

// NormalizeTaskCompanyIDs는 ID를 검사하고 중복을 빼되, 사용자가 처음 본 순서는 남긴다.
func NormalizeTaskCompanyIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, min(len(ids), MaxTaskCompanyCount))
	normalized := make([]int64, 0, min(len(ids), MaxTaskCompanyCount))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: company id must be positive", ErrTaskCompanyIDsInvalid)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
		if len(normalized) > MaxTaskCompanyCount {
			return nil, fmt.Errorf("%w: got more than %d unique companies", ErrTaskCompanyIDsInvalid, MaxTaskCompanyCount)
		}
	}
	return normalized, nil
}

func normalizeHeartbeat(sec int) int {
	if sec < MinPlanHeartbeatSeconds {
		return MinPlanHeartbeatSeconds
	}
	return sec
}

func (d *DB) CreateTask(description, goal string, llmProfileID *int64, timeoutSeconds, planHeartbeatSeconds int) (*Task, error) {
	var ids []int64
	if llmProfileID != nil {
		ids = []int64{*llmProfileID}
	}
	return d.CreateTaskWithOptions(description, goal, TaskCreateOptions{
		LLMProfileIDs: ids, TimeoutSeconds: timeoutSeconds, PlanHeartbeatSeconds: planHeartbeatSeconds,
	})
}

// TaskCreateOptions는 작업·탐색 행과 한 번에 커밋해야 하는 작업 데이터다.
type TaskCreateOptions struct {
	Name                 string // 선택적 작업 이름; 비어 있음=이름 없음
	CategoryID           *int64
	SourceTaskIDs        []int64
	CompanyIDs           []int64
	LLMProfileIDs        []int64
	TimeoutSeconds       int
	PlanHeartbeatSeconds int
	// CoverageEnabled는 「자산 커버리지 기능」 스위치이다; nil=기본 켜짐(true). 이 스위치를 신경 쓰지 않는 생성
	// 경로(오케스트레이션 spawn, 기존 API)는 원래 동작을 따른다. 웹에서 작업을 만들 때만 false를 명시해 끌 수 있다.
	CoverageEnabled *bool
	// InterceptRules는 작업 수준의 자산 가로채기 규칙이며, 생성 시 작업과 같은 트랜잭션에서 task_intercept_rules에 기록된다. 이 규칙은 워커가 자산 그래프의 자산에 닿는 도구 호출을 가로챌지 정한다.
	InterceptRules []TaskInterceptRuleInput
}

// CreateTaskWithOptions는 탐색, 작업, 직접 원본 관계,
// 순서가 있는 작업 LLM 사슬을 한 트랜잭션에 만든다.
func (d *DB) CreateTaskWithOptions(description, goal string, opts TaskCreateOptions) (*Task, error) {
	if len(opts.SourceTaskIDs) > MaxTaskSourceCount {
		return nil, fmt.Errorf("원본 작업이 너무 많습니다. %d개인데 최대는 %d개입니다", len(opts.SourceTaskIDs), MaxTaskSourceCount)
	}
	companyIDs, err := NormalizeTaskCompanyIDs(opts.CompanyIDs)
	if err != nil {
		return nil, err
	}
	opts.CompanyIDs = companyIDs
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var expID int64
	if err := tx.QueryRow(`INSERT INTO explorations(description, goal) VALUES ($1,$2) RETURNING id`, description, goal).Scan(&expID); err != nil {
		return nil, err
	}
	// 기원 사실: 탐색 그래프의 뿌리다. KindFact 노드(state='origin')이고
	// 원래 작업 설명을 담는다. 목표·의도·발견은 여기에서 내려오고,
	// 심은 대상 자산은 계보로 여기에 앵커된다(자산 그래프는
	// 전역으로 공유되고, 작업마다 갈라지지 않는다). 특별한 'begin' 종류가 아니라 사실이라, 첫 의도를 포함해
	// 모든 의도가 사실 노드에 같이 연결된다.
	originPayload, _ := json.Marshal(map[string]any{
		"summary":     "작업 시작점:" + description + "；목표:" + goal,
		"description": description,
		"goal":        goal,
	})
	if _, err := tx.Exec(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, 'fact', $2, 0, 'origin', 'system')`, expID, string(originPayload)); err != nil {
		return nil, err
	}
	if opts.TimeoutSeconds < 0 {
		opts.TimeoutSeconds = 0
	}
	opts.PlanHeartbeatSeconds = normalizeHeartbeat(opts.PlanHeartbeatSeconds)
	coverageEnabled := opts.CoverageEnabled == nil || *opts.CoverageEnabled
	var categoryName string
	if opts.CategoryID != nil {
		if *opts.CategoryID <= 0 {
			return nil, fmt.Errorf("%w: category id must be positive", ErrTaskCategoryInvalid)
		}
		if err := tx.QueryRow(`SELECT name FROM task_categories WHERE id=$1`, *opts.CategoryID).Scan(&categoryName); err != nil {
			if err == sql.ErrNoRows {
				return nil, ErrTaskCategoryNotFound
			}
			return nil, err
		}
	}
	var active *int64
	if len(opts.LLMProfileIDs) > 0 {
		id := opts.LLMProfileIDs[0]
		active = &id
	}
	t := &Task{
		Name: opts.Name, CategoryID: opts.CategoryID, CategoryName: categoryName,
		Description: description, Goal: goal, ExplorationID: expID,
		LLMProfileID: active, ActiveLLMProfileID: active,
		LLMProfileIDs:  append([]int64(nil), opts.LLMProfileIDs...),
		SourceTaskIDs:  append([]int64(nil), opts.SourceTaskIDs...),
		CompanyIDs:     append([]int64(nil), opts.CompanyIDs...),
		TimeoutSeconds: opts.TimeoutSeconds, PlanHeartbeatSeconds: opts.PlanHeartbeatSeconds,
		CoverageEnabled: coverageEnabled,
	}
	if err := tx.QueryRow(`
INSERT INTO tasks(name, category_id, description, goal, exploration_id, llm_profile_id, active_llm_profile_id, timeout_seconds, plan_heartbeat_seconds, coverage_enabled)
VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$8,$9)
RETURNING id, status, paused, created_at`, opts.Name, opts.CategoryID, description, goal, expID, active, opts.TimeoutSeconds, opts.PlanHeartbeatSeconds, coverageEnabled).Scan(&t.ID, &t.Status, &t.Paused, &t.CreatedAt); err != nil {
		return nil, err
	}
	if err := insertTaskRelations(tx, t.ID, opts.SourceTaskIDs); err != nil {
		return nil, err
	}
	if err := insertTaskCompanies(tx, t.ID, opts.CompanyIDs); err != nil {
		return nil, err
	}
	if err := insertTaskLLMProfiles(tx, t.ID, opts.LLMProfileIDs); err != nil {
		return nil, err
	}
	if err := insertTaskInterceptRules(tx, t.ID, opts.InterceptRules); err != nil {
		return nil, err
	}
	if len(opts.LLMProfileIDs) == 0 {
		t.LLMFailoverState = "default"
	} else {
		t.LLMFailoverState = "ready"
	}
	return t, tx.Commit()
}

func insertTaskCompanies(tx *sql.Tx, taskID int64, companyIDs []int64) error {
	if len(companyIDs) == 0 {
		return nil
	}
	// 기업 범위를 고치면 assets.company_id를 다시 계산한다. 만들 때의
	// 스냅샷을 그 수정과 직렬화해서, 작업이 커밋된 귀속 상태 하나만 보게 한다.
	if err := lockCompanyScopeMutation(tx); err != nil {
		return err
	}
	var inserted int
	err := tx.QueryRow(`
WITH requested(company_id, position) AS (
    SELECT company_id, position
    FROM unnest($2::bigint[]) WITH ORDINALITY AS requested(company_id, position)
), inserted AS (
    INSERT INTO task_scope(task_id, kind, company_id, source, reason)
    SELECT $1, 'company', companies.id, 'manual', '작업을 만들 때 연결한 기업'
    FROM requested
    JOIN companies ON companies.id=requested.company_id
    ORDER BY requested.position
    RETURNING company_id
)
SELECT count(*) FROM inserted`, taskID, companyIDs).Scan(&inserted)
	if err != nil {
		return err
	}
	if inserted != len(companyIDs) {
		return fmt.Errorf("%w: one or more companies do not exist", ErrTaskCompanyNotFound)
	}

	// task_ids를 고치면 sync_task_asset_links가 먼저 일반
	// 출처 행을 만든다. 그래서 출처 upsert는 그 뒤에 돌아야
	// 기업 이름과 생성 당시 이유가 운영자에게 남는다.
	if _, err := tx.Exec(`
UPDATE assets
SET task_ids=CASE
    WHEN $1=ANY(task_ids) THEN task_ids
    ELSE array_append(task_ids, $1)
END
WHERE company_id=ANY($2::bigint[])`, taskID, companyIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT $1, asset.id, $3, '작업을 만들 때 연결한 기업: ' || company.name
FROM assets asset
JOIN companies company ON company.id=asset.company_id
WHERE asset.company_id=ANY($2::bigint[])
  AND $1=ANY(asset.task_ids)
ON CONFLICT (task_id, asset_id) DO UPDATE
SET source=EXCLUDED.source,
    source_summary=EXCLUDED.source_summary,
    source_node_id=NULL`, taskID, companyIDs, taskCompanyAssetSource); err != nil {
		return err
	}
	return nil
}

func insertTaskRelations(tx *sql.Tx, taskID int64, sourceIDs []int64) error {
	seen := map[int64]bool{}
	for _, sourceID := range sourceIDs {
		if sourceID <= 0 || sourceID == taskID || seen[sourceID] {
			return fmt.Errorf("invalid or duplicate source task id %d", sourceID)
		}
		seen[sourceID] = true
		res, err := tx.Exec(`INSERT INTO task_relations(task_id, source_task_id)
SELECT $1, id FROM tasks WHERE id=$2 AND deleted_at IS NULL`, taskID, sourceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("원본 작업 %d 을(를) 찾을 수 없습니다", sourceID)
		}
	}
	return nil
}

func insertTaskLLMProfiles(tx *sql.Tx, taskID int64, profileIDs []int64) error {
	seen := map[int64]bool{}
	for position, profileID := range profileIDs {
		if profileID <= 0 || seen[profileID] {
			return fmt.Errorf("invalid or duplicate LLM profile id %d", profileID)
		}
		seen[profileID] = true
		if _, err := tx.Exec(`INSERT INTO task_llm_profiles(task_id, profile_id, position) VALUES ($1,$2,$3)`, taskID, profileID, position); err != nil {
			return err
		}
	}
	return nil
}

const taskCols = `id, COALESCE(name,''), category_id,
COALESCE((SELECT category.name FROM task_categories category WHERE category.id=tasks.category_id),''),
description, goal, exploration_id, status, paused, queued, queued_at, COALESCE(queue_mode,''), llm_profile_id, active_llm_profile_id, COALESCE(parent_ref,''), pinned_at, created_at, completed_at, COALESCE(timeout_seconds,0), COALESCE(plan_heartbeat_seconds,300), COALESCE(coverage_enabled,true), first_run_at, deadline_at`

func scanTask(sc interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	if err := sc.Scan(&t.ID, &t.Name, &t.CategoryID, &t.CategoryName, &t.Description, &t.Goal, &t.ExplorationID, &t.Status, &t.Paused, &t.Queued, &t.QueuedAt, &t.QueueMode, &t.LLMProfileID, &t.ActiveLLMProfileID, &t.ParentRef, &t.PinnedAt, &t.CreatedAt, &t.CompletedAt, &t.TimeoutSeconds, &t.PlanHeartbeatSeconds, &t.CoverageEnabled, &t.FirstRunAt, &t.DeadlineAt); err != nil {
		return nil, err
	}
	t.Pinned = t.PinnedAt != nil
	return &t, nil
}

// SetParentRef records the parent task id for a task (오케스트레이션 agent의 spawn_task 연관).
func (d *DB) SetParentRef(id int64, parentRef string) error {
	_, err := d.Exec(`UPDATE tasks SET parent_ref=NULLIF($2,'') WHERE id=$1`, id, parentRef)
	return err
}

// ListTasks는 살아있는 작업을 돌려준다. 고정한 작업이 먼저, 그다음 최신 id다.
func (d *DB) ListTasks() ([]*Task, error) {
	// id는 BIGSERIAL이며, 같은 시각에 생성된 작업도 안정적이고 유일한 순서를 가집니다.
	rows, err := d.Query(`SELECT ` + taskCols + ` FROM tasks WHERE deleted_at IS NULL
ORDER BY (pinned_at IS NOT NULL) DESC, pinned_at DESC NULLS LAST, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := d.hydrateTasksContext(out); err != nil {
		return nil, err
	}
	return out, nil
}

// TaskPatch는 실행 상태를 바꾸지 않고 작업 목록 메타데이터만 고친다.
type TaskPatch struct {
	Name   *string
	Pinned *bool
}

// UpdateTask는 작업 이름·고정을 일부만 고치고, 고친 행을 돌려준다.
// 이미 고정한 작업을 다시 고정해도 원래 위치는 남긴다.
func (d *DB) UpdateTask(id int64, patch TaskPatch) (*Task, error) {
	task, err := scanTask(d.QueryRow(`UPDATE tasks SET
	name = CASE WHEN $2::boolean THEN $3 ELSE name END,
	pinned_at = CASE
		WHEN $4::boolean IS NULL THEN pinned_at
		WHEN $4::boolean THEN COALESCE(pinned_at, now())
		ELSE NULL
	END
WHERE id=$1 AND deleted_at IS NULL
RETURNING `+taskCols, id, patch.Name != nil, patch.Name, patch.Pinned))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return task, nil
}

// GetTask는 살아있는 작업 하나를 돌려준다. 없거나 지워졌으면 nil이다.
func (d *DB) GetTask(id int64) (*Task, error) {
	t, err := scanTask(d.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=$1 AND deleted_at IS NULL`, id))
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	if err := d.hydrateTaskContext(t); err != nil {
		return nil, err
	}
	return t, nil
}

// SetPaused는 작업의 일시 정지 표시를 저장한다.
func (d *DB) SetPaused(id int64, paused bool) error {
	_, err := d.Exec(`UPDATE tasks SET paused=$1 WHERE id=$2`, paused, id)
	return err
}

// Enqueue는 작업을 영구 입장 대기열의 끝에 넣는다. 이미 대기 중일 때
// 다시 해도 원래 FIFO 위치는 유지한다.
func (d *DB) Enqueue(id int64, mode string) error {
	if mode != "bootstrap" && mode != "resume" {
		return fmt.Errorf("invalid queue mode %q", mode)
	}
	_, err := d.Exec(`UPDATE tasks
SET queued=true,
    queued_at=CASE WHEN queued THEN COALESCE(queued_at, now()) ELSE now() END,
    queue_mode=CASE
        WHEN queue_mode='bootstrap' OR $2='bootstrap' THEN 'bootstrap'
        ELSE 'resume'
    END
WHERE id=$1`, id, mode)
	return err
}

// Dequeue는 동시 실행 보류를 푼다. 사용자가 대기 중인 작업을 일시 정지하면
// clearMode는 false다. 다음 입장이 첫 기동인지 이어 하기인지는 남긴다.
func (d *DB) Dequeue(id int64, clearMode bool) error {
	_, err := d.Exec(`UPDATE tasks
SET queued=false,
    queued_at=NULL,
    queue_mode=CASE WHEN $2 THEN '' ELSE queue_mode END
WHERE id=$1`, id, clearMode)
	return err
}

// SetQueued는 옛 호출자와 테스트가 쓰는 호환 도우미다. 새
// 스케줄 코드는 Enqueue/Dequeue를 써서 FIFO 메타데이터를 분명히 한다.
func (d *DB) SetQueued(id int64, queued bool) error {
	if queued {
		return d.Enqueue(id, "bootstrap")
	}
	return d.Dequeue(id, true)
}

// SetStatus는 작업의 생명주기 상태를 고친다. 끝난 상태
// (done/failed/timeout)에 들어가면 completed_at을 한 번 찍는다(COALESCE가 첫 도장을 유지한다).
// 끝나지 않은 상태로 돌아가면 지운다. 그래서 다시 실행할 때 낡은 종료 시각이 없다.
func (d *DB) SetStatus(id int64, status string) error {
	_, err := d.Exec(`
UPDATE tasks
   SET status = $1,
       completed_at = CASE WHEN $1 IN ('done','failed','timeout') THEN COALESCE(completed_at, now()) ELSE NULL END
 WHERE id = $2`, status, id)
	return err
}

// SetTerminalStatusGuarded는 작업이 아직 끝나지 않았을 때만 끝난 상태를 넣는다.
// 그래서 완료와 시간 초과가 겨루면 먼저 쓴 쪽이 이긴다(won=true).
// 다른 끝난 상태가 이미 붙어 있으면 won=false를 돌려준다(오류는 아님).
// 호출자는 그때 그대로 둔다. 끝나지 않은 전환과 다시 실행은 여전히 SetStatus를 쓴다.
func (d *DB) SetTerminalStatusGuarded(id int64, status string) (won bool, err error) {
	res, err := d.Exec(`
UPDATE tasks
   SET status = $1,
       completed_at = COALESCE(completed_at, now())
 WHERE id = $2 AND status NOT IN ('done','failed','timeout')`, status, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StampFirstRun은 작업이 실제로 처음 실행된 시각을 기록하고 절대
// 마감 시각(= now + timeoutSeconds)을 계산합니다. 멱등합니다. first_run_at이
// 아직 NULL일 때만 기록하므로, 재시작과 재진입은 원래 시계를 유지합니다. timeoutSeconds<=0이면
// deadline_at을 NULL로 둡니다(시간 제한 없음). 계산된 마감 시각을 반환합니다(nil = 제한 없음/변하지 않음).
func (d *DB) StampFirstRun(id int64, timeoutSeconds int) (*time.Time, error) {
	var deadline *time.Time
	err := d.QueryRow(`
UPDATE tasks
   SET first_run_at = COALESCE(first_run_at, now()),
       deadline_at  = CASE
           WHEN first_run_at IS NOT NULL THEN deadline_at            -- 이미 도장을 찍음: 변경하지 않음
           WHEN $2 > 0 THEN now() + make_interval(secs => $2)
           ELSE NULL END
 WHERE id = $1
 RETURNING deadline_at`, id, timeoutSeconds).Scan(&deadline)
	return deadline, err
}

// DeleteTask는 예전 동작을 유지한다. 작업과 그
// 탐색 부분 그래프를 지우고, 전역 자산은 남긴다.
func (d *DB) DeleteTask(id int64) error {
	_, err := d.DeleteTaskCascade(id, false, false)
	return err
}

// DeleteTaskCascade는 작업과 그 탐색 부분 그래프를 완전히 지운다
// (노드·간선·활동은 ON DELETE CASCADE). 선택적으로, 이 작업만 소유한 독립 발견과
// 자산도 같은 트랜잭션에서 지운다. 공유
// 자산은 남기고 이 작업 id만 뗀다. deleteLLMRecords는
// 선택이다. 원래 옵션 두 개 API의 호출자를 지키기 위해서다.
func (d *DB) DeleteTaskCascade(id int64, deleteAssets, deleteFindings bool, deleteLLMRecords ...bool) (TaskDeleteResult, error) {
	deleteRecords := len(deleteLLMRecords) > 0 && deleteLLMRecords[0]
	return d.DeleteTaskCascadePrepared(id, deleteAssets, deleteFindings, deleteRecords, nil)
}

// DeleteTaskCascadePrepared는 되돌릴 수 있는 바깥 삭제를 PostgreSQL 연쇄 삭제와 맞춘다.
// prepare가 nil이 아니면 호스트 소유를 정하고, 자산·앵커를 쓰는 쪽을 막은 채
// 이 트랜잭션 안에서 prepare를 부른다. 잠금은 커밋까지 유지되어,
// 트래픽을 준비한 뒤에 호스트가 공유로 바뀌는 틈을 닫는다.
//
// prepare는 되돌릴 수 있는 일만 준비해야 한다. 돌려준 오류나 이후 데이터베이스
// 오류는 PostgreSQL을 롤백한다. 콜백이 성공시킨 바깥 작업을 되돌리는 책임은 호출자에게 있다.
func (d *DB) DeleteTaskCascadePrepared(
	id int64,
	deleteAssets, deleteFindings, deleteLLMRecords bool,
	prepare func(TaskDeletePreparation) error,
) (TaskDeleteResult, error) {
	var result TaskDeleteResult
	tx, err := d.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var expID int64
	if err := tx.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1 FOR UPDATE`, id).Scan(&expID); err != nil {
		if err == sql.ErrNoRows {
			return result, nil // 이미 없다
		}
		return result, err
	}

	// SHARE ROW EXCLUSIVE는 이 테이블의 모든 INSERT/UPDATE/DELETE와
	// 다른 조율된 삭제와 충돌한다. 둘을 고정된 순서로 잡으면
	// 유령 행(같은 호스트의 다른 자산 행)과 새
	// 소유·앵커 참조를 삭제 트랜잭션이 커밋할 때까지 막는다.
	if deleteAssets || prepare != nil {
		if _, err := tx.Exec(`LOCK TABLE assets, exploration_anchors IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return result, err
		}
	}
	if prepare != nil {
		hosts, err := hostsForTaskDeletion(tx, id, expID)
		if err != nil {
			return result, err
		}
		if err := prepare(TaskDeletePreparation{
			ExplorationID: expID,
			TrafficHosts:  hosts,
		}); err != nil {
			return result, err
		}
	}
	if deleteAssets {
		// 소유는 명시적 task_ids와 이 탐색의 앵커를 합친 것이다.
		// 자산은 다른 살아있는 작업이 어느 쪽으로도 참조하지 않을 때만 지울 수 있다.
		// 앵커만 있던 옛 씨앗도 덮고, 다른 작업이 앵커한
		// 증거를 지우지는 않는다.
		res, err := tx.Exec(`
WITH candidate_assets AS (
  SELECT id FROM assets WHERE $1 = ANY(task_ids)
  UNION
  SELECT ea.asset_id
  FROM exploration_anchors ea
  JOIN exploration_nodes n ON n.id=ea.node_id
  WHERE n.exploration_id=$2
),
deletable AS (
  SELECT a.id
  FROM assets a
  JOIN candidate_assets c ON c.id=a.id
  WHERE NOT EXISTS (
    SELECT 1 FROM tasks t
    WHERE t.id<>$1 AND t.deleted_at IS NULL AND t.id=ANY(a.task_ids)
  ) AND NOT EXISTS (
    SELECT 1
    FROM exploration_anchors ea
    JOIN exploration_nodes n ON n.id=ea.node_id
    JOIN tasks t ON t.exploration_id=n.exploration_id
    WHERE ea.asset_id=a.id AND t.id<>$1 AND t.deleted_at IS NULL
  )
)
DELETE FROM assets a USING deletable d WHERE a.id=d.id`, id, expID)
		if err != nil {
			return result, err
		}
		result.AssetsDeleted, _ = res.RowsAffected()

		res, err = tx.Exec(`UPDATE assets SET task_ids = array_remove(task_ids, $1) WHERE $1 = ANY(task_ids)`, id)
		if err != nil {
			return result, err
		}
		result.AssetsDetached, _ = res.RowsAffected()
	}
	if deleteFindings {
		res, err := tx.Exec(`DELETE FROM findings WHERE task_id = $1`, id)
		if err != nil {
			return result, err
		}
		result.FindingsDeleted, _ = res.RowsAffected()
	}
	if deleteLLMRecords {
		res, err := tx.Exec(`DELETE FROM llm_records WHERE COALESCE(task_id,'')=$1`, strconv.FormatInt(id, 10))
		if err != nil {
			return result, err
		}
		result.LLMRecordsDeleted, _ = res.RowsAffected()
	}
	// llm_usage(토큰 계량 장부)는 작업과 함께 일부러 지우지 않는다.
	// 작업이 없어진 뒤에도 과거 계량으로 남긴다.
	if _, err := tx.Exec(`DELETE FROM tasks WHERE id=$1`, id); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`DELETE FROM explorations WHERE id=$1`, expID); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
