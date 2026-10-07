package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// AgentTrigger는 사용자 에이전트에 붙은 P3 트리거 하나다. 여섯 조건이
// 동시에 켜질 수 있다. interval / on_finding / on_goal_met /
// on_task_timeout / on_tool_call / on_task_create. ToolNames는 도구 호출
// 트리거를 비어 있지 않은 도구 키 묶음으로 제한한다(on_tool_call에서 빈 목록은 API가 거절한다).
// 발견·목표 달성·작업 시간 초과는 탐색 그래프의 사건이 이 트리거를 깨운다.
type AgentTrigger struct {
	ID                 int64      `json:"id"`
	AgentKey           string     `json:"agent_key"`
	Enabled            bool       `json:"enabled"`
	IntervalSec        int        `json:"interval_sec"`
	OnFinding          bool       `json:"on_finding"`
	OnGoalMet          bool       `json:"on_goal_met"`
	OnTaskTimeout      bool       `json:"on_task_timeout"`
	OnToolCall         bool       `json:"on_tool_call"`
	OnTaskCreate       bool       `json:"on_task_create"`
	IntervalMessage    string     `json:"interval_message"` // 각 트리거 조건의 독립 사용자 메시지
	FindingMessage     string     `json:"finding_message"`
	GoalMessage        string     `json:"goal_message"`
	TaskTimeoutMessage string     `json:"task_timeout_message"`
	ToolCallMessage    string     `json:"tool_call_message"`
	TaskCreateMessage  string     `json:"task_create_message"`
	ToolNames          []string   `json:"tool_names"` // 선택된 도구 key 목록(DB 에는 JSON 텍스트로 저장)
	LastFire           *time.Time `json:"last_fire,omitempty"`
}

const triggerCols = `id, agent_key, enabled, interval_sec, on_finding, on_goal_met, on_task_timeout, on_tool_call, on_task_create, interval_message, finding_message, goal_message, task_timeout_message, tool_call_message, task_create_message, tool_names, last_fire`

// marshalToolNames는 도구 키 목록을 tool_names 열용 JSON 글로 바꾼다.
// nil이거나 빈 목록은 "null"/"[]"이 아니라 ""로 저장해, 열 기본값이 깨끗하게 남는다.
func marshalToolNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	b, err := json.Marshal(names)
	if err != nil {
		return ""
	}
	return string(b)
}

func scanTrigger(sc interface{ Scan(...any) error }) (*AgentTrigger, error) {
	var t AgentTrigger
	var lf sql.NullTime
	var toolNames string
	if err := sc.Scan(&t.ID, &t.AgentKey, &t.Enabled, &t.IntervalSec, &t.OnFinding, &t.OnGoalMet, &t.OnTaskTimeout, &t.OnToolCall, &t.OnTaskCreate,
		&t.IntervalMessage, &t.FindingMessage, &t.GoalMessage, &t.TaskTimeoutMessage, &t.ToolCallMessage, &t.TaskCreateMessage, &toolNames, &lf); err != nil {
		return nil, err
	}
	t.ToolNames = []string{}
	if toolNames != "" {
		_ = json.Unmarshal([]byte(toolNames), &t.ToolNames)
	}
	if lf.Valid {
		t.LastFire = &lf.Time
	}
	return &t, nil
}

// CreateTrigger는 agentKey의 트리거를 넣고 그 행을 돌려준다.
func (d *DB) CreateTrigger(t *AgentTrigger) (*AgentTrigger, error) {
	row := d.QueryRow(`
INSERT INTO agent_triggers(agent_key, enabled, interval_sec, on_finding, on_goal_met, on_task_timeout, on_tool_call, on_task_create, interval_message, finding_message, goal_message, task_timeout_message, tool_call_message, task_create_message, tool_names)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING `+triggerCols,
		t.AgentKey, t.Enabled, t.IntervalSec, t.OnFinding, t.OnGoalMet, t.OnTaskTimeout, t.OnToolCall, t.OnTaskCreate,
		t.IntervalMessage, t.FindingMessage, t.GoalMessage, t.TaskTimeoutMessage, t.ToolCallMessage, t.TaskCreateMessage, marshalToolNames(t.ToolNames))
	return scanTrigger(row)
}

// UpdateTrigger는 트리거 필드를 고친다. last_fire는 건드리지 않는다.
func (d *DB) UpdateTrigger(t *AgentTrigger) error {
	_, err := d.Exec(`UPDATE agent_triggers SET enabled=$2, interval_sec=$3, on_finding=$4, on_goal_met=$5, on_task_timeout=$6, on_tool_call=$7, on_task_create=$8, interval_message=$9, finding_message=$10, goal_message=$11, task_timeout_message=$12, tool_call_message=$13, task_create_message=$14, tool_names=$15 WHERE id=$1`,
		t.ID, t.Enabled, t.IntervalSec, t.OnFinding, t.OnGoalMet, t.OnTaskTimeout, t.OnToolCall, t.OnTaskCreate,
		t.IntervalMessage, t.FindingMessage, t.GoalMessage, t.TaskTimeoutMessage, t.ToolCallMessage, t.TaskCreateMessage, marshalToolNames(t.ToolNames))
	return err
}

// DeleteTrigger는 트리거를 지운다.
func (d *DB) DeleteTrigger(id int64) error {
	_, err := d.Exec(`DELETE FROM agent_triggers WHERE id=$1`, id)
	return err
}

// ListTriggersFor는 에이전트의 트리거를 돌려준다.
func (d *DB) ListTriggersFor(agentKey string) ([]*AgentTrigger, error) {
	return d.queryTriggers(`SELECT `+triggerCols+` FROM agent_triggers WHERE agent_key=$1 ORDER BY id`, agentKey)
}

// ListEnabledTriggers는 켜진 트리거를 모두 돌려준다. 스케줄러가 쓴다.
func (d *DB) ListEnabledTriggers() ([]*AgentTrigger, error) {
	return d.queryTriggers(`SELECT ` + triggerCols + ` FROM agent_triggers WHERE enabled ORDER BY id`)
}

func (d *DB) queryTriggers(q string, args ...any) ([]*AgentTrigger, error) {
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentTrigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TouchTriggerFire는 간격 트리거가 지금 발화했다고 기록한다.
func (d *DB) TouchTriggerFire(id int64) error {
	_, err := d.Exec(`UPDATE agent_triggers SET last_fire=now() WHERE id=$1`, id)
	return err
}

// DeleteTriggersForAgent는 에이전트의 트리거를 모두 지운다. 사용자 에이전트를 삭제할 때 쓴다.
func (d *DB) DeleteTriggersForAgent(agentKey string) error {
	_, err := d.Exec(`DELETE FROM agent_triggers WHERE agent_key=$1`, agentKey)
	return err
}

// ---------- scheduler_state (키-값 워터마크) ----------

func (d *DB) GetSchedState(key string) (string, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM scheduler_state WHERE key=$1`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) SetSchedState(key, value string) error {
	_, err := d.Exec(`INSERT INTO scheduler_state(key,value) VALUES ($1,$2)
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, key, value)
	return err
}

// ---------- 사건 조회 (모든 탐색을 가로질러, 스케줄러용) ----------

// TaskEvent는 발견·목표 사건이다. 그 작업의 정보를 담아 트리거 메시지 맥락을 만든다.
type TaskEvent struct {
	NodeID     int64  `json:"node_id"`
	TaskID     int64  `json:"task_id"`
	TaskDesc   string `json:"task_description"`
	TaskGoal   string `json:"task_goal"`
	Summary    string `json:"summary"`     // 발견 요약 / 목표 글
	VulnClass  string `json:"vulnclass"`   // 발견만
	Severity   string `json:"severity"`    // 발견만
	Tool       string `json:"tool"`        // 도구 호출만: 도구 이름
	ToolInput  string `json:"tool_input"`  // tool-call only: 입력 인자(JSON 텍스트)
	ToolOutput string `json:"tool_output"` // tool-call only: 반환 내용
	ToolIsErr  bool   `json:"tool_is_err"` // tool-call only: 도구 반환이 오류인지 여부
}

// NewFindingsSince는 살아있는 모든 작업에서 노드 id가 lastID보다 큰 발견을
// id 순으로 돌려준다. 워터마크가 단조라 같은 사건이 두 번 발화하지 않는다.
func (d *DB) NewFindingsSince(lastID int64) ([]TaskEvent, error) {
	rows, err := d.Query(`
SELECT n.id, t.id, t.description, t.goal, n.payload
FROM exploration_nodes n JOIN tasks t ON t.exploration_id = n.exploration_id
WHERE n.kind='finding' AND n.id > $1 AND t.deleted_at IS NULL
ORDER BY n.id`, lastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var e TaskEvent
		var payload []byte
		if err := rows.Scan(&e.NodeID, &e.TaskID, &e.TaskDesc, &e.TaskGoal, &payload); err != nil {
			return nil, err
		}
		var p struct{ Summary, Vulnclass, Severity string }
		_ = json.Unmarshal(payload, &p)
		e.Summary, e.VulnClass, e.Severity = p.Summary, p.Vulnclass, p.Severity
		out = append(out, e)
	}
	return out, rows.Err()
}

// TimedOutTasksSince는 status가 'timeout'이고 id가 lastID보다 큰 작업을
// id 순으로 돌려준다. 워터마크가 단조라 재시작 뒤에도 두 번 발화하지 않는다.
func (d *DB) TimedOutTasksSince(lastID int64) ([]TaskEvent, error) {
	rows, err := d.Query(`
SELECT id, description, goal FROM tasks
WHERE status='timeout' AND deleted_at IS NULL AND id > $1
ORDER BY id`, lastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var e TaskEvent
		if err := rows.Scan(&e.NodeID, &e.TaskDesc, &e.TaskGoal); err != nil {
			return nil, err
		}
		e.TaskID = e.NodeID // 작업 id를 워터마크용 NodeID로 같이 쓴다
		e.Summary = e.TaskGoal
		out = append(out, e)
	}
	return out, rows.Err()
}

// NewTasksSince는 id가 lastID보다 큰, 지워지지 않은 작업을 id 순으로 돌려준다.
// 워터마크가 단조라 재시작 뒤에도 두 번 발화하지 않는다. 트리거로 깨어난
// 에이전트 실행은 작업이 아니라 대화라, 자기 출력으로는 다시 발화하지 않는다.
func (d *DB) NewTasksSince(lastID int64) ([]TaskEvent, error) {
	rows, err := d.Query(`
SELECT id, description, goal FROM tasks
WHERE deleted_at IS NULL AND id > $1
ORDER BY id`, lastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var e TaskEvent
		if err := rows.Scan(&e.NodeID, &e.TaskDesc, &e.TaskGoal); err != nil {
			return nil, err
		}
		e.TaskID = e.NodeID // 작업 id를 워터마크용 NodeID로 같이 쓴다
		e.Summary = e.TaskGoal
		out = append(out, e)
	}
	return out, rows.Err()
}

// NewToolCallsSince는 끝난 도구 호출(tool_result 행) 중 활동 id가 lastID보다 큰 것을
// 살아있는 모든 작업에서 id 순으로 돌려준다. 워터마크가 단조라 두 번 발화하지 않는다.
// tool_result가 기준이다. 도구가 끝났으므로 입력과 출력이 모두 있고, 짝인 tool_use에서 입력을 가져온다.
// 작업 실행 활동만 본다. 트리거로 깨어난 실행은 대화(conversation_activities)라,
// 도구 호출 트리거가 자기 출력으로 다시 발화하지 않는다.
func (d *DB) NewToolCallsSince(lastID int64) ([]TaskEvent, error) {
	rows, err := d.Query(`
SELECT r.id, t.id, t.description, t.goal, r.tool, COALESCE(u.detail,''), COALESCE(r.detail,''), r.is_error
FROM activity r
JOIN tasks t ON t.exploration_id = r.exploration_id
LEFT JOIN activity u ON u.exploration_id = r.exploration_id AND u.tool_use_id = r.tool_use_id AND u.kind='tool_use'
WHERE r.kind='tool_result' AND r.id > $1 AND r.tool <> '' AND t.deleted_at IS NULL
ORDER BY r.id`, lastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var e TaskEvent
		if err := rows.Scan(&e.NodeID, &e.TaskID, &e.TaskDesc, &e.TaskGoal, &e.Tool, &e.ToolInput, &e.ToolOutput, &e.ToolIsErr); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MetGoals는 살아있는 작업에서 달성한 목표를 모두 돌려준다.
// 스케줄러가 이미 발화한 것은 저장된 발화 집합으로 걸러 낸다.
func (d *DB) MetGoals() ([]TaskEvent, error) {
	rows, err := d.Query(`
SELECT n.id, t.id, t.description, t.goal, n.payload
FROM exploration_nodes n JOIN tasks t ON t.exploration_id = n.exploration_id
WHERE n.kind='goal' AND n.state='met' AND t.deleted_at IS NULL
ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var e TaskEvent
		var payload []byte
		if err := rows.Scan(&e.NodeID, &e.TaskID, &e.TaskDesc, &e.TaskGoal, &payload); err != nil {
			return nil, err
		}
		var p struct{ Text, Summary string }
		_ = json.Unmarshal(payload, &p)
		if p.Text != "" {
			e.Summary = p.Text
		} else {
			e.Summary = p.Summary
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
