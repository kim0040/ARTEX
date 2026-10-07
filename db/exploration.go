package db

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrIntentStateConflict는 비교 후 설정이 실패했다는 표시다. 호출자는
// 예상된 제어 경합(HTTP 409)과 저장 실패를 이것으로 가른다.
var ErrIntentStateConflict = errors.New("intent state changed concurrently")

// utf8Clean은 문자열을 PostgreSQL text 열에 안전하게 만든다. (1) 잘못된
// UTF-8 바이트 열을 U+FFFD로 바꾸고 (2) NUL(0x00) 바이트를 뺀다.
// 도구 출력(표준 출력)에는 잘리거나 날것의 바이트가 있을 수 있고, PostgreSQL
// UTF8은 그것을 거절한다("invalid byte sequence for encoding UTF8"). 이 처리가 없으면
// INSERT가 실패하고 활동 기록이 조용히 사라진다. NUL은 *유효한* UTF-8
// (U+0000)이라 ToValidUTF8은 그대로 두지만, PostgreSQL text는 여전히 거절한다
// (SQLSTATE 22021). 그래서 따로 빼야 한다. JSONB 열은 아래
// jsonbClean이 따로 필요하다. json.Marshal은 NUL을 이스케이프 backslash-u-0000으로 넣고,
// text 형 json은 받지만 jsonb는 거절한다(SQLSTATE 22P05).
func utf8Clean(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.ToValidUTF8(s, "�")
}

// jsonbClean은 마셜한 JSON을 PostgreSQL jsonb 열에 안전하게 만든다. json.Marshal은
// NUL 바이트(U+0000)를 6바이트 이스케이프 \u0000으로 그대로 넣는다.
// json 형은 저장하지만 jsonb는 "unsupported Unicode escape
// sequence"(SQLSTATE 22P05)로 거절한다. 잡은 HTTP·도구 바이트에 NUL이 있을 수 있어
// 이스케이프를 뺀다. json.RawMessage 안에 중첩된 NUL도 덮는다. 그 필드는
// 출력에 그대로 복사되기 때문이다. *진짜* 이스케이프만 뺀다. \u0000은
// 앞에 역슬래시가 짝수 개일 때 진짜다. 그래서 \\u0000처럼
// 역슬래시를 이스케이프한 것(글자 그대로 u0000)은 그대로 둔다.
func jsonbClean(b []byte) []byte {
	if !bytes.Contains(b, []byte("\\u0000")) {
		return b
	}
	out := make([]byte, 0, len(b))
	bs := 0 // 위치 i 앞에 이미 내보낸 연속 역슬래시 수
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && bs%2 == 0 && i+5 < len(b) &&
			b[i+1] == 'u' && b[i+2] == '0' && b[i+3] == '0' && b[i+4] == '0' && b[i+5] == '0' {
			i += 5 // \u0000 전체를 건너뛴다
			bs = 0
			continue
		}
		if b[i] == '\\' {
			bs++
		} else {
			bs = 0
		}
		out = append(out, b[i])
	}
	return out
}

// Node는 종류가 있는 추론 노드다(옛 task_nodes). kind는 goal|intent|finding|hint.
// 탐색 그래프의 목표·의도·발견·힌트가 이 노드다.
type Node struct {
	FindingID     int64           `json:"finding_id,omitempty"` // 발견을 아는 읽기가 채운다. ID에서 추측하지 않는다
	FindingNodeID int64           `json:"finding_node_id,omitempty"`
	TrafficCount  int             `json:"traffic_count,omitempty"`
	ID            int64           `json:"id"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
	Priority      int             `json:"priority"`
	State         string          `json:"state"`
	Origin        string          `json:"origin,omitempty"`
	Owner         string          `json:"owner,omitempty"`
	BlockedReason string          `json:"blocked_reason,omitempty"`
	DeleteReason  string          `json:"delete_reason,omitempty"` // 의도 가짜 삭제(state='deleted')일 때만 비어 있지 않다
	Anchors       []int64         `json:"anchors,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SourceTaskID  int64           `json:"source_task_id,omitempty"`
	Inherited     bool            `json:"inherited,omitempty"`
}

// Activity는 워커 실행 단계 하나다(옛 task_activity).
type Activity struct {
	ID        int64           `json:"id"`
	NodeID    *int64          `json:"node_id,omitempty"`
	Worker    string          `json:"worker,omitempty"`
	Kind      string          `json:"kind,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error"`
	Summary   string          `json:"summary,omitempty"`
	Detail    string          `json:"-"` // 전체 본문. 필요할 때 읽고, 목록 본문에서는 뺀다
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	// 토큰 사용. 끝난 kind='result' 기록에만 넣고, 다른 곳에서는 nil이다.
	InputTokens      *int  `json:"input_tokens,omitempty"`
	OutputTokens     *int  `json:"output_tokens,omitempty"`
	CacheReadTokens  *int  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int  `json:"cache_write_tokens,omitempty"`
	SourceTaskID     int64 `json:"source_task_id,omitempty"`
	Inherited        bool  `json:"inherited,omitempty"`
	// MainSeg는 메인 에이전트 대화 구간이다(메인 에이전트가 아닌 행은 nil.
	// nil/0은 원래 세션). UI가 메인 에이전트 행을 그 구간으로 보내게 한다.
	MainSeg *int `json:"main_seg,omitempty"`
}

// TokenUsage는 워커별 토큰 합계다(TokenStatsByWorker).
type TokenUsage struct {
	Worker           string `json:"worker"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// SessionTokenUsage는 작업 UI 세션 하나의, 끝난 실행 합계의 기준이다.
// 워커 세션은 다시 쓰는 work#N 실행기 이름이 아니라 의도가 키다.
// 그래서 재시도와 재배정이 같은 행에 남는다.
type SessionTokenUsage struct {
	Session          string `json:"session"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// DailyTokenBucket은 모든 작업에 걸친 하루의 전역 토큰 합계다.
type DailyTokenBucket struct {
	Day             string `json:"day"` // 날짜, "YYYY-MM-DD"
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
}

// TokenDailyAll은 모든 탐색에서 지난 days일의 토큰 사용을
// 달력 날(UTC)별로 모은다. kind='result' 행만 토큰
// 수를 담는다(실행마다의 종료 요약). 그래서 두 번 세지 않는다.
func (d *DB) TokenDailyAll(days int) ([]DailyTokenBucket, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.Query(`
		SELECT TO_CHAR(DATE_TRUNC('day', created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0)
		FROM activity
		WHERE kind = 'result'
		  AND created_at >= NOW() - ($1 * INTERVAL '1 day')
		GROUP BY day
		ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyTokenBucket
	for rows.Next() {
		var b DailyTokenBucket
		if err := rows.Scan(&b.Day, &b.InputTokens, &b.OutputTokens, &b.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ExplorationStore는 탐색 하나의 추론 그래프와 활동을 가리키는 손잡이다.
type ExplorationStore struct {
	db    *DB
	expID int64
}

// CreateExploration은 새 탐색 뿌리를 만들고 id를 돌려준다.
func (d *DB) CreateExploration(description, goal string) (int64, error) {
	var id int64
	err := d.QueryRow(`INSERT INTO explorations(description, goal) VALUES ($1, $2) RETURNING id`, description, goal).Scan(&id)
	return id, err
}

// Exploration은 탐색 id에 묶인 손잡이를 돌려준다.
func (d *DB) Exploration(id int64) *ExplorationStore { return &ExplorationStore{db: d, expID: id} }

func (s *ExplorationStore) ID() int64 { return s.expID }

// Root는 탐색의 설명과 목표를 돌려준다.
func (s *ExplorationStore) Root() (description, goal string, err error) {
	var d sql.NullString
	err = s.db.QueryRow(`SELECT description, goal FROM explorations WHERE id=$1`, s.expID).Scan(&d, &goal)
	return d.String, goal, err
}

// AddNode는 종류가 있는 추론 노드를 쓰고, 선택적으로 자산 id에 앵커한다.
func (s *ExplorationStore) AddNode(kind string, payload map[string]any, priority int, state, origin string, anchors []int64) (int64, error) {
	raw, _ := json.Marshal(payload)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		s.expID, kind, string(raw), priority, state, origin).Scan(&id); err != nil {
		return 0, err
	}
	for _, a := range anchors {
		if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, a); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// AddIntent는 편의 함수다. 열린 의도 하나를 넣는다.
func (s *ExplorationStore) AddIntent(payload map[string]any, priority int, anchors []int64, origin string) (int64, error) {
	if origin == "" {
		origin = "planner"
	}
	return s.AddNode("intent", payload, priority, "open", origin, anchors)
}

// AddGoal은 목표 노드를 쓴다(상태는 open).
func (s *ExplorationStore) AddGoal(payload map[string]any, origin string) (int64, error) {
	return s.AddNode("goal", payload, 0, "open", origin, nil)
}

// OriginFactID는 이 탐색의 뿌리 사실 id를 돌려준다. 작업을 만들 때 심은
// state='origin'인 KindFact 노드다(작업의 뿌리). 모든 의도는
// 여기로 거슬러 올라간다. 없으면 0을 돌려준다(오류는 아님). 옛 'begin' 뿌리로 만든
// 탐색이거나, 아직 없을 때다.
func (s *ExplorationStore) OriginFactID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND state='origin' ORDER BY id LIMIT 1`, s.expID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// Anchor는 노드→자산 참조 간선을 기록한다(다대다). 탐색 노드가 어느
// 전역 자산을 만졌는지 계보·출처로 남긴다. 자산
// 그래프 자체는 전역으로 공유된다. 앵커는 더 이상 보임을 막지 않는다(아무
// 작업이나 검색으로 자산을 읽을 수 있다). 추론 흔적만 보존한다.
// 같은 요청을 다시 해도 된다.
func (s *ExplorationStore) Anchor(nodeID, assetID int64) error {
	if nodeID <= 0 || assetID <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, nodeID, assetID)
	return err
}

// Link는 종류가 있는 탐색 간선을 더한다. 같은 요청을 다시 해도 된다.
func (s *ExplorationStore) Link(from int64, rel string, to int64) error {
	_, err := s.db.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id) VALUES ($1,$2,$3,$4)
ON CONFLICT (exploration_id, src_id, rel, dst_id) DO NOTHING`, s.expID, from, rel, to)
	return err
}

// SetNodeState는 아무 노드의 상태를 고친다(지우지는 않는다). content_version이 올라
// 접힌 구성원의 상태 변화가 다이제스트 캐시 본문을 무효로 만든다(§5.3).
func (s *ExplorationStore) SetNodeState(id int64, state string) error {
	_, err := s.db.Exec(`UPDATE exploration_nodes SET state=$1, blocked_reason=NULL, content_version=content_version+1 WHERE id=$2 AND exploration_id=$3`, state, id, s.expID)
	return err
}

// UpdateGoalPayload는 목표 노드의 본문 글(그리고 선택인 vulnclass)을 다시 쓴다.
// kind='goal'로 범위를 묶어, id로 의도·사실을 고치지 못한다.
// 이 탐색에 그 목표가 없으면 오류를 돌려준다.
func (s *ExplorationStore) UpdateGoalPayload(id int64, text, vulnclass string) error {
	payload := map[string]any{"text": text}
	if vulnclass != "" {
		payload["vulnclass"] = vulnclass
	}
	raw, _ := json.Marshal(payload)
	res, err := s.db.Exec(`UPDATE exploration_nodes SET payload=$1 WHERE id=$2 AND exploration_id=$3 AND kind='goal'`, string(raw), id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("대상이 없습니다")
	}
	return nil
}

// DeleteGoal은 목표 노드를 완전히 지운다. kind='goal'로 범위를 묶어
// id로 의도·사실을 지우지 못한다. 간선(spawns)과 앵커는 ON DELETE
// CASCADE로 참조하므로 함께 지워진다. activity.node_id는 ON DELETE SET NULL이다.
// 이 탐색에 그 목표가 없으면 오류를 돌려준다.
func (s *ExplorationStore) DeleteGoal(id int64) error {
	res, err := s.db.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='goal'`, id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("대상이 없습니다")
	}
	return nil
}

// SetIntentState는 의도 노드의 상태를 고친다.
// ResetRunningIntents는 'running'으로 남은 의도를 'open'으로 되돌려 다시 집히게 한다.
// 시작 때 부른다. 살아있는 워커가 없는 'running' 의도(백엔드 재시작이나
// 죽은 워커 고루틴이 남겨 둔 것)는 그렇지 않으면 UI에서
// 영원히 돈다. 워커는 기록에서 이어가므로 다시 열어도 안전하다.
func (s *ExplorationStore) ResetRunningIntents() (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='running'`, s.expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReopenIntent 는 성공으로 끝나지 않은 의도 하나(blocked/exhausted/stopped)를
// 다시 'open' 으로 되돌려 워커가 다시 가져가게 한다(이미 만든 그래프 쓰기는 그대로 둔다).
// 워커는 처음부터 다시 시작하지 않고, 이전 LLM 기록에서 이어 간다.
// done/open/running 은 건드리지 않는다. 행이 바뀌었는지 반환한다. 「다시 실행」에서 쓴다.
func (s *ExplorationStore) ReopenIntent(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE id=$1 AND exploration_id=$2 AND kind='intent'
  AND state IN ('blocked','exhausted','stopped')`, id, s.expID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReopenBlockedIntents는 이 탐색의 'blocked' 의도를 모두 'open'으로 되돌린다
// (LLM·네트워크 장애로 여러 개가 한 번에 막힌 뒤의 일괄 재실행). 다시 연
// 개수를 돌려준다. 워커는 기록에서 이어가고, 남겨 둔 그래프 쓰기는 그대로다.
func (s *ExplorationStore) ReopenBlockedIntents() (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='blocked'`, s.expID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *ExplorationStore) SetIntentState(id int64, state string) error {
	// 끝난 상태는 completed_at을 찍는다. 다시 열면(open/running으로 돌아가면) 지운다.
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	_, err := s.db.Exec(`UPDATE exploration_nodes
SET state=$1, blocked_reason=NULL, content_version=content_version+1, completed_at = CASE WHEN $4 THEN now() ELSE NULL END
WHERE id=$2 AND exploration_id=$3 AND kind='intent'`, state, id, s.expID, terminal)
	return err
}

// CompareAndSetIntentState는 현재 상태가 기대한 값과 같을 때만 로컬 의도 하나를 옮긴다.
// 일시 정지·재개와 워커 마감이 쓰는 상태 경계다. 그래서 낡은 API 읽기가
// 동시에 끝난 결과를 덮어쓰지 못한다.
func (s *ExplorationStore) CompareAndSetIntentState(id int64, expected, state string) (bool, error) {
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state=$1, blocked_reason=NULL, content_version=content_version+1, completed_at = CASE WHEN $5 THEN now() ELSE NULL END
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state=$4`, state, id, s.expID, expected, terminal)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IntentCleanup은 사용자가 워커 의도 하나를 취소할 때 지운 칠판 기록의 요약이다.
// 전역 자산과 트래픽은 일부러 이 작업에 넣지 않는다.
// 탐색 앵커는 소유 노드가 사라져서만 함께 없어진다.
type IntentCleanup struct {
	Intents    int64 `json:"intents"`
	Facts      int64 `json:"facts"`
	Findings   int64 `json:"findings"`
	Activities int64 `json:"activities"`
}

// SoftDeleteIntent 는 대기 중/실행 중/일시 중지된 의도 하나를 가짜 삭제한다. state='deleted' 로 두고, 사용자가 적은
// 삭제 이유를 delete_reason 필드에 남긴다. 의도 노드와 그 산출물/혈통은 모두 보존한다(예전 구현처럼 그래프에
// fact 를 하나 더 달지 않는다). 삭제 전 의도의 summary 를 반환해 플래너 알림에 쓴다. 보조 세션은 삭제 상태와 함께 정리한다.
// 호출자는 먼저 실행 중인 워커를 멈춰, 이후 쓰기를 막아야 한다.
func (s *ExplorationStore) SoftDeleteIntent(id int64, reason string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var state string
	var rawPayload []byte
	if err := tx.QueryRow(`SELECT state, payload FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(&state, &rawPayload); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("의도를 찾을 수 없습니다")
		}
		return "", err
	}
	if state != "running" && state != "paused" && state != "open" {
		return "", fmt.Errorf("%w: intent state %s cannot be deleted", ErrIntentStateConflict, state)
	}
	if _, err := tx.Exec(`UPDATE exploration_nodes
		SET state='deleted', delete_reason=$3, blocked_reason=NULL,
		    content_version=content_version+1, completed_at=now()
		WHERE id=$1 AND exploration_id=$2`, id, s.expID, reason); err != nil {
		return "", err
	}
	// 주 의도 감사 궤적은 남기되, 보조 질의응답 세션은 삭제 상태와 함께 원자적으로 정리한다(지연 스냅샷은 이를 거부한다).
	if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE intent_id=$1`, id); err != nil {
		return "", err
	}
	var summary string
	var p map[string]any
	if json.Unmarshal(rawPayload, &p) == nil {
		if sm, ok := p["summary"].(string); ok {
			summary = sm
		}
	}
	return summary, tx.Commit()
}

// CancelIntent 는 의도 하나를 물리 삭제하고, "그 의도만 받치고 있는" 독점 자손 노드를 모두 지운다. 해당 의도에서
// yields/derived_from 을 따라 아래로 닿고, 모든 부모 노드(그 노드를 가리키는 모든 간선의 출발)가 삭제 집합 안에 있는 노드다.
// goal 과 origin fact 는 절대 지우지 않는다. 삭제 집합 밖의 의도/digest 가 아직 참조하는 공유 노드도 남겨, 다른
// 분기가 깨지거나 끊긴 링크가 생기지 않게 한다. 지우는 intent 마다 먼저 token rollup 을 하고(되돌릴 수 없는 계량을 남긴다) activity
// 와 보조 세션을 정리한다. 지우는 finding 은 먼저 findings 테이블 행을 지운다(node_id FK 가 ON DELETE SET NULL 이라, 그렇지 않으면 고아가 남는다).
// 간선은 노드와 함께 CASCADE 로 정리되고, 정리 전체는 한 트랜잭션 안에 있다. 호출자는 먼저 실행 중인 워커를 멈춰 이후 쓰기를 막아야 한다.
func (s *ExplorationStore) CancelIntent(id int64) (IntentCleanup, error) {
	var out IntentCleanup
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// 의도 행을 잠그고 존재를 확인한다(멱등: 이미 지웠으면 not found). 상태는 검사하지 않는다. 진짜 삭제는 어떤 상태에서든 성립하며,
	// running 인 워커는 호출자가 먼저 멈춘다.
	if err := tx.QueryRow(`SELECT 1 FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(new(int)); err != nil {
		if err == sql.ErrNoRows {
			return out, fmt.Errorf("의도를 찾을 수 없습니다")
		}
		return out, err
	}

	// 그래프 전체의 노드(protected 판정)와 간선(아래로 도달 가능 여부 + 부모 집합 계산)을 불러온다. 그래프 규모는 실제 작업에서 작다.
	kind := map[int64]string{}
	protected := map[int64]bool{}
	nrows, err := tx.Query(`SELECT id, kind, state FROM exploration_nodes WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return out, err
	}
	for nrows.Next() {
		var nid int64
		var k, st string
		if err := nrows.Scan(&nid, &k, &st); err != nil {
			nrows.Close()
			return out, err
		}
		kind[nid] = k
		if k == KindGoal || (k == KindFact && st == StateOrigin) {
			protected[nid] = true // 목표와 작업 루트 사실은 의도 삭제와 함께 지워지지 않는다
		}
	}
	nrows.Close()
	if err := nrows.Err(); err != nil {
		return out, err
	}

	parentsOf := map[int64][]int64{} // dst -> 자신을 가리키는 모든 간선의 src(아무 rel, covers 포함: digest 에 덮인 구성원은 digest 부모가 있어 남는다)
	downOf := map[int64][]int64{}    // src -> yields/derived_from 을 따른 아래 이웃
	erows, err := tx.Query(`SELECT src_id, rel, dst_id FROM exploration_edges WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return out, err
	}
	for erows.Next() {
		var src, dst int64
		var rel string
		if err := erows.Scan(&src, &rel, &dst); err != nil {
			erows.Close()
			return out, err
		}
		parentsOf[dst] = append(parentsOf[dst], src)
		if rel == RelYields || rel == RelDerivedFrom {
			downOf[src] = append(downOf[src], dst)
		}
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return out, err
	}

	// 독점 연쇄: 의도에서 아래로 넓힌다. 노드가 삭제 집합에 들어가는 조건은 보호되지 않았고, 그 부모 전부가 이미 집합 안에 있을 때뿐이다
	// (즉 지워지는 노드를 거치는 길 외에 들어오는 길이 없다). 고정점에 이를 때까지 반복한다.
	del := map[int64]bool{id: true}
	for changed := true; changed; {
		changed = false
		for src := range del {
			for _, dst := range downOf[src] {
				if del[dst] || protected[dst] {
					continue
				}
				exclusive := true
				for _, p := range parentsOf[dst] {
					if !del[p] {
						exclusive = false
						break
					}
				}
				if exclusive {
					del[dst] = true
					changed = true
				}
			}
		}
	}

	// 유형별로 나눈다.
	var ids, intentIDs, findingIDs []int64
	for nid := range del {
		ids = append(ids, nid)
		switch kind[nid] {
		case KindIntent:
			intentIDs = append(intentIDs, nid)
			out.Intents++
		case KindFact:
			out.Facts++
		case KindFinding:
			findingIDs = append(findingIDs, nid)
			out.Findings++
		}
	}

	// 지워지는 의도마다: token rollup(되돌릴 수 없는 계량을 남긴다) 뒤에 activity 를 지운다. rollup 행의 node_id 는 NULL 이라,
	// 아래에서 node_id 로 하는 삭제에 걸리지 않는다.
	for _, iid := range intentIDs {
		tokenBuckets, err := intentTokenRollup(tx, s.expID, iid)
		if err != nil {
			return out, err
		}
		res, err := tx.Exec(`DELETE FROM activity WHERE exploration_id=$1 AND node_id=$2`, s.expID, iid)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out.Activities += n
		}
		for _, bucket := range tokenBuckets {
			metadata, _ := json.Marshal(map[string]any{
				"cancelled_intent_id": iid,
				"token_day":           bucket.Day.Format(time.DateOnly),
				"token_rollup":        true,
			})
			if _, err := tx.Exec(`INSERT INTO activity(
				exploration_id, worker, kind, summary, metadata,
				input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at)
				VALUES ($1,'token-ledger','result',$2,$3,$4,$5,$6,$7,$8)`,
				s.expID, fmt.Sprintf("취소된 의도 #%d 의 Token 계량", iid), metadata,
				bucket.Usage.InputTokens, bucket.Usage.OutputTokens,
				bucket.Usage.CacheReadTokens, bucket.Usage.CacheWriteTokens, bucket.Day); err != nil {
				return out, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM side_question_sessions WHERE intent_id=$1`, iid); err != nil {
			return out, err
		}
	}

	// finding 노드를 지우기 전에 findings 테이블 행을 먼저 지운다(그렇지 않으면 node_id=NULL 인 고아가 남는다).
	for _, fid := range findingIDs {
		if _, err := tx.Exec(`DELETE FROM findings WHERE node_id=$1`, fid); err != nil {
			return out, err
		}
	}

	// 노드를 지운다(간선은 CASCADE 로 정리된다). 하나씩 지우고 행 수를 맞춰, 동시 변경을 막는다.
	var removed int64
	for _, nid := range ids {
		res, err := tx.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, nid, s.expID)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += n
		}
	}
	if removed != int64(len(ids)) {
		return out, fmt.Errorf("intent cleanup changed concurrently")
	}
	return out, tx.Commit()
}

type tokenUsageBucket struct {
	Day   time.Time
	Usage TokenUsage
}

// intentTokenRollup은 원래 UTC 날짜별로 묶은, 한 번만 세는 사용량을 돌려준다.
// 각 result는 실행 하나를 끝낸다. 사용량을 담은 result가 기준이다.
// 사용량이 없는 더 옛 오류·취소 result는 이전 result 이후의 최신 누적
// 사용 프레임으로 내려간다. 끝의 프레임은 종료 result를 저장하기 전에
// 끊긴 실행이다.
func intentTokenRollup(tx *sql.Tx, explorationID, intentID int64) ([]tokenUsageBucket, error) {
	rows, err := tx.Query(`SELECT kind, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at
		FROM activity
		WHERE exploration_id=$1 AND node_id=$2 AND kind IN ('usage','result')
		ORDER BY id`, explorationID, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[time.Time]TokenUsage)
	add := func(at time.Time, usage TokenUsage) {
		if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
			usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
			return
		}
		utc := at.UTC()
		day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		total := byDay[day]
		total.add(usage)
		byDay[day] = total
	}

	var pending TokenUsage
	var pendingAt time.Time
	hasPending := false
	for rows.Next() {
		var kind string
		var input, output, read, write sql.NullInt64
		var createdAt time.Time
		if err := rows.Scan(&kind, &input, &output, &read, &write, &createdAt); err != nil {
			return nil, err
		}
		current := TokenUsage{
			InputTokens:      int(input.Int64),
			OutputTokens:     int(output.Int64),
			CacheReadTokens:  int(read.Int64),
			CacheWriteTokens: int(write.Int64),
		}
		hasCurrent := input.Valid || output.Valid || read.Valid || write.Valid
		if kind == "usage" {
			if hasCurrent {
				pending, pendingAt, hasPending = current, createdAt, true
			}
			continue
		}

		if hasCurrent {
			// 일부만 채워진 옛 종료 행은 그대로 두고, 빠진 축만
			// 최신 누적 사용 프레임에서 채운다.
			if hasPending {
				if !input.Valid {
					current.InputTokens = pending.InputTokens
				}
				if !output.Valid {
					current.OutputTokens = pending.OutputTokens
				}
				if !read.Valid {
					current.CacheReadTokens = pending.CacheReadTokens
				}
				if !write.Valid {
					current.CacheWriteTokens = pending.CacheWriteTokens
				}
			}
			add(createdAt, current)
		} else if hasPending {
			add(createdAt, pending)
		}
		pending, pendingAt, hasPending = TokenUsage{}, time.Time{}, false
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if hasPending {
		add(pendingAt, pending)
	}

	buckets := make([]tokenUsageBucket, 0, len(byDay))
	for day, usage := range byDay {
		buckets = append(buckets, tokenUsageBucket{Day: day, Usage: usage})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Day.Before(buckets[j].Day) })
	return buckets, nil
}

func (u *TokenUsage) add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

const nodeCols = `id, kind, payload, priority, state, COALESCE(origin,''), COALESCE(owner,''), COALESCE(blocked_reason,''), COALESCE(delete_reason,''), created_at`

func scanNode(sc interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var payload []byte
	if err := sc.Scan(&n.ID, &n.Kind, &payload, &n.Priority, &n.State, &n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason, &n.CreatedAt); err != nil {
		return nil, err
	}
	n.Payload = json.RawMessage(payload)
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]*Node, error) {
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListByKind는 한 종류의 노드를 최신 순으로 돌려준다.
func (s *ExplorationStore) ListByKind(kind string, limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind=$2 ORDER BY id DESC LIMIT $3`, s.expID, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ListByKindPage는 역방향 페이지용으로, 한 종류의 노드를 최신 먼저 한 페이지 돌려준다.
// id가 before보다 작은 노드를 최대 limit개 본다(before가 0 이하면 최신 페이지).
// hasMore는 더 오래된 노드가 있는지를 알려, 클라이언트(예: 워커
// 세션 목록)가 옛 고정 상한을 넘어 페이지할 수 있게 한다. 더 옛 의도를 잃지 않는다.
func (s *ExplorationStore) ListByKindPage(kind string, before int64, limit int) ([]*Node, bool, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind=$2 AND ($3 <= 0 OR id < $3)
ORDER BY id DESC LIMIT $4`, s.expID, kind, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(nodes) > limit
	if hasMore {
		nodes = nodes[:limit]
	}
	return nodes, hasMore, nil
}

// listByKindPageFiltered는 선택인 요약 키워드 필터가 있는 ListByKindPage다
// (payload->>'summary' ILIKE %q%). 한 줄을 더 읽어 호출자가
// hasMore를 알게 한다. q가 비면 필터가 없다. id 기준 최신 먼저다.
func (s *ExplorationStore) listByKindPageFiltered(kind string, before int64, limit int, q string) ([]*Node, error) {
	where := `exploration_id=$1 AND kind=$2 AND ($3 <= 0 OR id < $3)`
	args := []any{s.expID, kind, before}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND payload->>'summary' ILIKE $%d`, len(args))
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE `+where+` ORDER BY id DESC LIMIT $`+fmt.Sprintf("%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// countByKindFiltered는 같은 요약 필터로 한 종류의 노드를 센다.
// 페이지의 전체 개수용이다. q가 비면 그 종류를 모두 센다.
func (s *ExplorationStore) countByKindFiltered(kind, q string) (int, error) {
	where := `exploration_id=$1 AND kind=$2`
	args := []any{s.expID, kind}
	if q != "" {
		args = append(args, "%"+q+"%")
		where += fmt.Sprintf(` AND payload->>'summary' ILIKE $%d`, len(args))
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE `+where, args...).Scan(&n)
	return n, err
}

// CountFinishedIntents는 이 탐색에서 탐색을 끝낸 의도(done/blocked/exhausted)를 센다.
// graph_overview의 done_intents_total이다. 그래서 플래너는
// recent_done_intents(최근 창으로 잘림)가 잘린 보기임을 알고
// 「이미 시도함」 중복 제거를 조심한다. 'stopped'(죽임/삭제)는 빼며,
// recent_done_intents가 보여 주는 것과 정확히 같다.
func (s *ExplorationStore) CountFinishedIntents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state IN ('done','blocked','exhausted')`, s.expID).Scan(&n)
	return n, err
}

// CountOpenIntents는 이 탐색의 열린 의도를 센다. graph_overview의
// frontier_open이다. 그래서 플래너는 open_intents(우선순위 상위 N개로 잘림)가
// 잘린 보기이고, 집을 수 있는 일이 더 있을 수 있음을 안다.
func (s *ExplorationStore) CountOpenIntents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open'`, s.expID).Scan(&n)
	return n, err
}

// GetNode는 id로 이 탐색의 노드 하나를 돌려준다. 없으면 nil, nil이다.
func (s *ExplorationStore) GetNode(id int64) (*Node, error) {
	n, err := scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, id, s.expID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return n, err
}

// Nodes는 모든 노드를 돌려준다. 그래프 그림용이다.
func (s *ExplorationStore) Nodes(limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 ORDER BY id LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// NodeFilter는 NodesPage 조회를 좁힌다. 빈 필드는 모든 값을 포함한다.
type NodeFilter struct {
	Kinds  []string // exploration_nodes.kind (노드 종류)
	States []string // exploration_nodes.state (노드 상태)
	Query  string   // payload 글 / origin에서 대소문자 무시 부분 문자열
	Asc    bool     // true면 오래된 것 먼저(재생). 기본은 최신 먼저(방송)
}

// NodesPage 는 이 탐색의 노드를 1부터 세는 한 페이지와 일치하는 전체
// 개수를 반환한다. 방송판은 그래프를 시계열로 읽으므로, Nodes 처럼 그래프 전체를 당기지 않고 SQL 에서 페이지를 나눈다.
// 정렬은 id 기준이다. id 는 BIGSERIAL 이라 생성 순서다. created_at 초가 같은 노드가 여러 개여도 순서가 안정적이다.
func (s *ExplorationStore) NodesPage(f NodeFilter, page, size int) ([]*Node, int, error) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	offset := (page - 1) * size

	conds := []string{"exploration_id=$1"}
	args := []any{s.expID}
	addIn := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		marks := make([]string, 0, len(values))
		for _, v := range values {
			args = append(args, v)
			marks = append(marks, "$"+fmt.Sprint(len(args)))
		}
		conds = append(conds, column+" IN ("+strings.Join(marks, ",")+")")
	}
	addIn("kind", f.Kinds)
	addIn("state", f.States)
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		mark := "$" + fmt.Sprint(len(args))
		ors := []string{"payload::text ILIKE " + mark, "COALESCE(origin,'') ILIKE " + mark}
		// 순수 숫자(또는 UI 에서 # 접두가 붙은 형태, 예: 「#41」)는 노드 id 정확 일치로 보아, 어떤 노드든 바로 찾을 수 있게 한다.
		if idStr := strings.TrimPrefix(q, "#"); idStr != "" {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				args = append(args, id)
				ors = append(ors, "id = $"+fmt.Sprint(len(args)))
			}
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "DESC"
	if f.Asc {
		order = "ASC"
	}
	args = append(args, size, offset)
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes`+where+
		` ORDER BY id `+order+
		` LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, 0, err
	}
	return nodes, total, nil
}

// NodesByIDs는 이 탐색의 주어진 노드를 id 순서로 불러온다. 그래프 전체를
// 가져오지 않고 중계 보드 페이지의 이웃을 푸는 데 쓴다.
func (s *ExplorationStore) NodesByIDs(ids []int64) ([]*Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, s.expID)
	marks := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		marks = append(marks, "$"+fmt.Sprint(len(args)))
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND id IN (`+strings.Join(marks, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// EdgesTouching은 한쪽 끝이 ids에 있는 모든 간선을 반환한다. 중계 보드는 이것으로
// 노드가 어디에서 왔는지, 무엇을 만들어 냈는지를 풀어 적는다.
func (s *ExplorationStore) EdgesTouching(ids []int64) ([]Edge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, s.expID)
	marks := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		marks = append(marks, "$"+fmt.Sprint(len(args)))
	}
	list := strings.Join(marks, ",")
	rows, err := s.db.Query(`SELECT src_id, rel, dst_id FROM exploration_edges
WHERE exploration_id=$1 AND (src_id IN (`+list+`) OR dst_id IN (`+list+`))`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Edge는 exploration_edges의 한 줄이다. 탐색 그래프의 간선이다.
type Edge struct {
	From int64
	Rel  string
	To   int64
}

// Edges는 탐색 그래프의 간선을 돌려준다. 그래프 화면이 이 목록을 그린다.
func (s *ExplorationStore) Edges(limit int) ([]Edge, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.db.Query(`SELECT src_id, rel, dst_id FROM exploration_edges WHERE exploration_id=$1 LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FactsYielded는 의도가 만든 사실 노드 id를 오래된 순으로 돌려준다(intent --yields--> fact).
// 플래너가 끝난 워커 출력과 함께 이번 라운드의 새 사실을 읽게 한다.
// 사실이 없거나 의도를 못 찾으면 nil을 돌려준다.
func (s *ExplorationStore) FactsYielded(intentID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT n.id
		FROM exploration_edges e
		JOIN exploration_nodes n ON n.id=e.dst_id AND n.exploration_id=e.exploration_id
		WHERE e.exploration_id=$1 AND e.src_id=$2 AND e.rel=$3 AND n.kind=$4
		ORDER BY n.id`, s.expID, intentID, RelYields, KindFact)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FindingLineage는 탐색 그래프 뿌리에서 이 노드까지 이어진 부분 그래프를 돌려준다.
// 노드 자신과 조상(간선을 거슬러 닿는 노드), 그리고 그 집합 안의 간선만 포함한다.
// 화면의 "이 발견에 어떻게 닿았나"(origin → … → finding)에 쓴다.
// 노드가 없으면 오류 대신 빈 결과를 돌려준다.
func (s *ExplorationStore) FindingLineage(nodeID int64) ([]*Node, []Edge, error) {
	// anc = 이 노드와, 간선을 거슬러(src<-dst) 여기로 닿는 모든 노드.
	nodeRows, err := s.db.Query(`
WITH RECURSIVE anc(id) AS (
    SELECT $2::bigint
  UNION
    SELECT e.src_id
    FROM exploration_edges e
    JOIN anc ON e.dst_id = anc.id
    WHERE e.exploration_id = $1
)
SELECT `+nodeCols+`
FROM exploration_nodes
WHERE exploration_id = $1 AND id IN (SELECT id FROM anc)
ORDER BY id`, s.expID, nodeID)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := scanNodes(nodeRows)
	if err != nil {
		return nil, nil, err
	}
	if len(nodes) == 0 {
		return nil, nil, nil
	}
	// 조상 집합 안의 간선만. 이 계보의 내부 관계다.
	edgeRows, err := s.db.Query(`
WITH RECURSIVE anc(id) AS (
    SELECT $2::bigint
  UNION
    SELECT e.src_id
    FROM exploration_edges e
    JOIN anc ON e.dst_id = anc.id
    WHERE e.exploration_id = $1
)
SELECT src_id, rel, dst_id
FROM exploration_edges
WHERE exploration_id = $1
  AND src_id IN (SELECT id FROM anc)
  AND dst_id IN (SELECT id FROM anc)`, s.expID, nodeID)
	if err != nil {
		return nil, nil, err
	}
	defer edgeRows.Close()
	var edges []Edge
	for edgeRows.Next() {
		var e Edge
		if err := edgeRows.Scan(&e.From, &e.Rel, &e.To); err != nil {
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	return nodes, edges, edgeRows.Err()
}

// FindingIntents는 발견 id를 그것을 만든 의도에 연결한다(intent --yields--> finding, report_finding이 잇는다).
// 만든 의도가 없는 발견은 빠진다. JOIN이라 간선 스캔 상한이 없다.
func (s *ExplorationStore) FindingIntents() (map[int64]int64, error) {
	rows, err := s.db.Query(`SELECT e.dst_id, e.src_id
FROM exploration_edges e
JOIN exploration_nodes n ON n.id = e.dst_id AND n.exploration_id = e.exploration_id
WHERE e.exploration_id=$1 AND e.rel=$2 AND n.kind='finding'`, s.expID, RelYields)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var findingID, intentID int64
		if err := rows.Scan(&findingID, &intentID); err != nil {
			return nil, err
		}
		out[findingID] = intentID
	}
	return out, rows.Err()
}

// FindingIntentsTerminal은 물려받은 이력용이다. 만든 의도가 더 바뀌지 않는 종료 상태일 때만 발견 계보를 돌려준다.
// 그래서 원본 작업의 살아 있는 워커 배치는 숨긴다.
func (s *ExplorationStore) FindingIntentsTerminal() (map[int64]int64, error) {
	rows, err := s.db.Query(`SELECT e.dst_id, e.src_id
	FROM exploration_edges e
	JOIN exploration_nodes finding ON finding.id=e.dst_id AND finding.exploration_id=e.exploration_id
	JOIN exploration_nodes intent ON intent.id=e.src_id AND intent.exploration_id=e.exploration_id
	WHERE e.exploration_id=$1 AND e.rel=$2 AND finding.kind='finding'
	  AND intent.kind='intent' AND intent.state IN ('done','blocked','exhausted','stopped')`, s.expID, RelYields)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var findingID, intentID int64
		if err := rows.Scan(&findingID, &intentID); err != nil {
			return nil, err
		}
		out[findingID] = intentID
	}
	return out, rows.Err()
}

// Frontier는 열린 의도를 우선순위 내림차순, 같으면 id 오름차순(먼저 넣은 것부터)으로 돌려준다.
func (s *ExplorationStore) Frontier(limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open'
ORDER BY priority DESC, id ASC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// HasActiveIntent는 이 탐색에 아직 진행 중인 의도(open 또는 running)가 있는지 본다.
// 첫 플래너 라운드를 돌릴지 정할 때 쓴다. 시드된 작업의 의도를 워커가 이미 집어갔을 수 있어서
// open만 세면(Frontier) 워커 선점과 경합해 첫 라운드를 중복으로 돌릴 수 있다.
// open과 running을 같이 세면 그 경합이 없다.
func (s *ExplorationStore) HasActiveIntent() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running'))`, s.expID).Scan(&exists)
	return exists, err
}

// HasOpenGoal은 이 탐색에 state가 open인 goal이 아직 있는지 보고한다.
// false ⇒ 모든 목표가 이미 met/abandoned이거나(또는 이 작업에 목표가 없음) ⇒ goalless(사람이 직접 전달) 분기로 들어간다.
// 플래너는 실행을 멈추고, 작업이 끝났는지는 frontier가 비었는지로 결정한다. met와 abandoned는 모두 "이미 매듭지음"으로 친다.
func (s *ExplorationStore) HasOpenGoal() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes
WHERE exploration_id=$1 AND kind='goal' AND state='open')`, s.expID).Scan(&exists)
	return exists, err
}

// ClaimIntent는 열린 의도를 한 번에 running으로 바꾼다. 선점했으면 true.
func (s *ExplorationStore) ClaimIntent(id int64, owner string) (bool, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes SET state='running', owner=$1
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state='open'`, owner, id, s.expID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Stats는 종류별 노드 개수를 돌려준다. 대시보드가 쓴다.
func (s *ExplorationStore) Stats() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT kind, count(*) FROM exploration_nodes WHERE exploration_id=$1 GROUP BY kind`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// --- 활동 (전역 id 커서로 폴링) ---

// AppendActivity는 워커 단계 하나를 남기고 그 id를 돌려준다.
func (s *ExplorationStore) AppendActivity(a Activity) (int64, error) {
	metadata := a.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO activity(exploration_id, node_id, worker, kind, tool, tool_use_id, is_error, summary, detail, metadata, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15)
RETURNING id`, s.expID, a.NodeID, utf8Clean(a.Worker), utf8Clean(a.Kind), utf8Clean(a.Tool), utf8Clean(a.ToolUseID), a.IsError, utf8Clean(a.Summary), utf8Clean(a.Detail),
		metadata, a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.MainSeg).Scan(&id)
	return id, err
}

// ExplorationDiag는 활동의 외래 키 위반(23503)이 난 순간, 부모 행에 왜 못 닿는지 본다.
// 탐색 행이 아직 있는지, 그 행을 가리키는 작업이 몇 개인지(살아 있는 작업은 정확히 1개여야 하고,
// tasks.exploration_id의 RESTRICT 때문에 그 행이 있는 동안 탐색은 지울 수 없다),
// 지금 MAX(explorations.id)가 얼마인지를 돌려준다. 실패 형태를 이렇게 가른다.
//   - expExists=false, taskRefs=0 → 행 묶음이 통째로 없다(DB 초기화 / 다른 DB).
//   - expExists=false, taskRefs=1 → RESTRICT 아래서는 불가능하다. 외래 키가 깨진 경우다.
//   - expExists=true              → 쓰려던 expID가 이 탐색이 아니다(메모리 저장소의
//     낡은 id). Store.ID()와 비교한다.
func (d *DB) ExplorationDiag(expID int64) (expExists bool, taskRefs int, maxExpID int64, err error) {
	err = d.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM explorations WHERE id=$1),
		(SELECT COUNT(*) FROM tasks WHERE exploration_id=$1),
		COALESCE((SELECT MAX(id) FROM explorations),0)`, expID).Scan(&expExists, &taskRefs, &maxExpID)
	return
}

// TokenTotal은 이 탐색의 모든 워커 토큰을 합친다(작업 전체).
// TokenStatsByWorker와 같은 출처(kind='result' 행)이고, 워커별로 나누지 않는다.
func (s *ExplorationStore) TokenTotal() (TokenUsage, error) {
	var u TokenUsage
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0)
	FROM activity WHERE exploration_id=$1 AND kind='result'`, s.expID).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens)
	return u, err
}

// TokenTotalsAll은 탐색마다 작업 전체 토큰 합계를 쿼리 한 번으로 돌려준다(exploration_id → 합계).
// 작업 목록이 행마다 쿼리하지 않고 작업별 사용량을 보여 주게 한다.
func (d *DB) TokenTotalsAll() (map[int64]TokenUsage, error) {
	rows, err := d.Query(`SELECT exploration_id,
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0)
	FROM activity WHERE kind='result' GROUP BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]TokenUsage{}
	for rows.Next() {
		var eid int64
		var u TokenUsage
		if err := rows.Scan(&eid, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens); err != nil {
			return nil, err
		}
		out[eid] = u
	}
	return out, rows.Err()
}

// LastActivityAll은 탐색마다 가장 최근 활동의 unix 시각을 반환한다
// (exploration_id → max created_at epoch). 모든 작업을 쿼리 한 번으로 다룬다.
// 저장된다(Engine.LastActivity의 메모리 맵과 다름). 그래서 재시작 뒤에도 남고,
// 종료 상태 작업에 실행 시간을 계산할 안정적인 "ran until" 시각을 준다.
func (d *DB) LastActivityAll() (map[int64]int64, error) {
	rows, err := d.Query(`SELECT exploration_id, EXTRACT(EPOCH FROM MAX(created_at))::bigint FROM activity GROUP BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var eid, ts int64
		if err := rows.Scan(&eid, &ts); err != nil {
			return nil, err
		}
		out[eid] = ts
	}
	return out, rows.Err()
}

// GoalCounts는 한 탐색의 목표 요약이다. 탐색 그래프의 목표(goal)가 몇 개이고 몇 개가 충족됐는지다.
type GoalCounts struct{ Total, Met int }

// FindingSeverityCounts는 작업 목록을 위해 한 작업의 발견을 심각도별로 나눈다
// (severity가 허용 목록 밖이거나 비어 있는 기록은 어느 등급에도 세지 않는다).
// 이 집계는 탐색 그래프의 발견을 작업 목록 UI에 심각도별로 보여 준다.
type FindingSeverityCounts struct{ Critical, High, Medium, Low int }

// TaskListMetrics는 작업 목록 화면에 그리는 집계다. 토큰, 최근 활동, 목표, 실행 중 의도, 발견을 한곳에 모은다.
type TaskListMetrics struct {
	Tokens         TokenUsage
	LastActivity   int64
	Goals          GoalCounts
	RunningIntents int                   // kind=intent 이고 state=running 인 행의 수, 즉 실행 중인 워커 수
	Findings       FindingSeverityCounts // findings 테이블에서 해당 작업의 발견 수(심각도별 구간)
}

// TaskListMetricsAll은 살아 있는 작업마다 목록 집계를 쿼리 한 번으로 돌려준다.
// 옆 조회는 탐색별 인덱스를 써서, 폴링마다 활동 이력 전체를 묶지 않는다.
func (d *DB) TaskListMetricsAll() (map[int64]TaskListMetrics, error) {
	rows, err := d.Query(`
		SELECT task.exploration_id,
		       COALESCE(token_metrics.input_tokens,0),
		       COALESCE(token_metrics.output_tokens,0),
		       COALESCE(token_metrics.cache_read_tokens,0),
		       COALESCE(token_metrics.cache_write_tokens,0),
		       COALESCE(latest_activity.created_at,0),
		       COALESCE(goal_metrics.total,0),
		       COALESCE(goal_metrics.met,0),
		       COALESCE(intent_metrics.running,0),
		       COALESCE(finding_metrics.critical,0),
		       COALESCE(finding_metrics.high,0),
		       COALESCE(finding_metrics.medium,0),
		       COALESCE(finding_metrics.low,0)
		FROM tasks task
		LEFT JOIN LATERAL (
			SELECT SUM(input_tokens) AS input_tokens,
			       SUM(output_tokens) AS output_tokens,
			       SUM(cache_read_tokens) AS cache_read_tokens,
			       SUM(cache_write_tokens) AS cache_write_tokens
			FROM activity
			WHERE exploration_id=task.exploration_id AND kind='result'
		) token_metrics ON true
		LEFT JOIN LATERAL (
			SELECT EXTRACT(EPOCH FROM created_at)::bigint AS created_at
			FROM activity
			WHERE exploration_id=task.exploration_id
			ORDER BY created_at DESC
			LIMIT 1
		) latest_activity ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS total,
			       COUNT(*) FILTER (WHERE state='met') AS met
			FROM exploration_nodes
			WHERE exploration_id=task.exploration_id AND kind='goal'
		) goal_metrics ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS running
			FROM exploration_nodes
			WHERE exploration_id=task.exploration_id AND kind='intent' AND state='running'
		) intent_metrics ON true
		LEFT JOIN LATERAL (
			SELECT COUNT(*) FILTER (WHERE severity='critical') AS critical,
			       COUNT(*) FILTER (WHERE severity='high')     AS high,
			       COUNT(*) FILTER (WHERE severity='medium')   AS medium,
			       COUNT(*) FILTER (WHERE severity='low')      AS low
			FROM findings
			WHERE task_id=task.id
		) finding_metrics ON true
		WHERE task.deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]TaskListMetrics{}
	for rows.Next() {
		var explorationID int64
		var metrics TaskListMetrics
		if err := rows.Scan(
			&explorationID,
			&metrics.Tokens.InputTokens,
			&metrics.Tokens.OutputTokens,
			&metrics.Tokens.CacheReadTokens,
			&metrics.Tokens.CacheWriteTokens,
			&metrics.LastActivity,
			&metrics.Goals.Total,
			&metrics.Goals.Met,
			&metrics.RunningIntents,
			&metrics.Findings.Critical,
			&metrics.Findings.High,
			&metrics.Findings.Medium,
			&metrics.Findings.Low,
		); err != nil {
			return nil, err
		}
		out[explorationID] = metrics
	}
	return out, rows.Err()
}

// GoalCountsAll은 탐색 id마다 목표 합계(전체 / 충족)를 쿼리 한 번으로 돌려준다.
// listTasks가 작업 목록에서 모든 작업의 진행을 보여 주게 한다.
func (d *DB) GoalCountsAll() (map[int64]GoalCounts, error) {
	rows, err := d.Query(`
		SELECT exploration_id,
		       COUNT(*) AS total,
		       COUNT(*) FILTER (WHERE state = 'met') AS met
		FROM exploration_nodes
		WHERE kind = 'goal'
		GROUP BY exploration_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]GoalCounts{}
	for rows.Next() {
		var eid int64
		var gc GoalCounts
		if err := rows.Scan(&eid, &gc.Total, &gc.Met); err != nil {
			return nil, err
		}
		out[eid] = gc
	}
	return out, rows.Err()
}

// TokenStatsByWorker는 이 탐색의 워커별 토큰을 합친다(사용량이 있는 kind='result' 기록).
// 에이전트별 토큰 화면이 쓴다.
func (s *ExplorationStore) TokenStatsByWorker() ([]TokenUsage, error) {
	rows, err := s.db.Query(`SELECT COALESCE(worker,''),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0)
	FROM activity WHERE exploration_id=$1 AND kind='result'
	GROUP BY worker ORDER BY worker`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenUsage{}
	for rows.Next() {
		var u TokenUsage
		if err := rows.Scan(&u.Worker, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TokenStatsBySession은 저장된 완료 실행을 활동 이력 페이지와 상관없이 모두 모은다.
// 메인과 플래너는 고정 키를 쓰고, 워커 실행은 재시도를 다른 work#N이 맡았어도 의도 노드 id를 쓴다.
func (s *ExplorationStore) TokenStatsBySession() ([]SessionTokenUsage, error) {
	rows, err := s.db.Query(`SELECT
		CASE
			WHEN COALESCE(a.worker,'')='mainagent' THEN 'main:' || COALESCE(a.main_seg,0)::text
			WHEN COALESCE(a.worker,'')='planner' THEN 'plan'
			WHEN n.id IS NOT NULL THEN 'intent:' || n.id::text
			ELSE ''
		END AS session_key,
		COALESCE(SUM(a.input_tokens),0), COALESCE(SUM(a.output_tokens),0),
		COALESCE(SUM(a.cache_read_tokens),0), COALESCE(SUM(a.cache_write_tokens),0)
	FROM activity a
	LEFT JOIN exploration_nodes n
	  ON n.id=a.node_id AND n.exploration_id=a.exploration_id AND n.kind='intent'
	WHERE a.exploration_id=$1 AND a.kind='result'
	  AND (COALESCE(a.worker,'') IN ('mainagent','planner') OR n.id IS NOT NULL)
	GROUP BY session_key
	ORDER BY session_key`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionTokenUsage{}
	for rows.Next() {
		var u SessionTokenUsage
		if err := rows.Scan(&u.Session, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ActivityList는 sinceID 다음(그 id는 빼고) 단계를 돌려준다. 노드로 거를 수 있다.
// 항목과 새 커서(본 id의 최댓값)를 함께 돌려준다.
func (s *ExplorationStore) ActivityList(nodeID *int64, sinceID int64, limit int) ([]Activity, int64, error) {
	if limit <= 0 {
		limit = 300
	}
	var rows *sql.Rows
	var err error
	const cols = `id, node_id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), metadata, created_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg`
	if nodeID != nil {
		rows, err = s.db.Query(`SELECT `+cols+`
FROM activity WHERE exploration_id=$1 AND node_id=$2 AND id>$3 ORDER BY id LIMIT $4`, s.expID, *nodeID, sinceID, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+cols+`
FROM activity WHERE exploration_id=$1 AND id>$2 ORDER BY id LIMIT $3`, s.expID, sinceID, limit)
	}
	if err != nil {
		return nil, sinceID, err
	}
	defer rows.Close()
	out := []Activity{}
	cursor := sinceID
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.Metadata, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens, &a.MainSeg); err != nil {
			return nil, sinceID, err
		}
		if a.ID > cursor {
			cursor = a.ID
		}
		out = append(out, a)
	}
	return out, cursor, rows.Err()
}

// ActivityListForTerminalIntent는 물려받은 이력을 조금씩 읽는 경계다.
// 의도 상태 확인과 활동 읽기가 한 문장이라, API 확인 사이에 원본 의도가 다시 열려도 새 진행 흔적이 새지 않는다.
// 모델 추론과 회계 행은 물려주지 않는다.
func (s *ExplorationStore) ActivityListForTerminalIntent(nodeID, sinceID int64, limit int) ([]Activity, int64, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''),
		COALESCE(a.tool,''), COALESCE(a.tool_use_id,''), a.is_error, COALESCE(a.summary,''),
		a.created_at, a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cache_write_tokens
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2 AND a.id>$3
	  AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	ORDER BY a.id LIMIT $4`, s.expID, nodeID, sinceID, limit)
	if err != nil {
		return nil, sinceID, err
	}
	defer rows.Close()
	out := []Activity{}
	cursor := sinceID
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens); err != nil {
			return nil, sinceID, err
		}
		if a.ID > cursor {
			cursor = a.ID
		}
		out = append(out, a)
	}
	return out, cursor, rows.Err()
}

// ActivitySessionFilter는 한 작업의 활동 스트림 안에서 UI 세션 하나를 고른다.
// 의미 있는 필드는 정확히 하나다.
//   - Main == true  → 한 구간의 메인 에이전트 세션(worker="mainagent")
//     (MainSeg. nil/0은 원래 구간이며, 예전 NULL 행과도 맞는다).
//   - Worker != ""  → 워커 이름으로 거른다(Plan = "planner").
//     Goal Agent의 0라운드 분해도 worker="planner"로 저장되므로, Plan 세션은 Goal과 플래너를 빠짐없이 덮는다.
//   - NodeID != nil → node_id(= 의도 id)로 거른 워커 세션.
//
// 영 값(모두 비어 있음)은 세션 필터 없이 작업 전체와 맞는다.
// 이 필터는 UI가 활동 스트림을 세션으로 나눌 때 탐색 그래프의 의도 노드와 플래너 기록을 고르게 한다.
type ActivitySessionFilter struct {
	Worker  string
	NodeID  *int64
	Main    bool // 메인 에이전트 세션
	MainSeg *int // 메인 세션 구간(nil == 현재, 호출자가 정함; 0 == 원래 구간)
}

func (f ActivitySessionFilter) cond(argStart int) (string, []any) {
	switch {
	case f.NodeID != nil:
		return fmt.Sprintf(" AND node_id=$%d", argStart), []any{*f.NodeID}
	case f.Main:
		seg := 0
		if f.MainSeg != nil {
			seg = *f.MainSeg
		}
		if seg == 0 { // 원래 구간은 main_seg가 NULL인 예전 행도 맡는다
			return " AND worker='mainagent' AND COALESCE(main_seg,0)=0", nil
		}
		return fmt.Sprintf(" AND worker='mainagent' AND main_seg=$%d", argStart), []any{seg}
	case f.Worker != "":
		return fmt.Sprintf(" AND worker=$%d", argStart), []any{f.Worker}
	default:
		return "", nil
	}
}

// ActivityPage는 세션을 최신순으로 한 페이지 돌려준다.
// id `before` 앞(그 id는 빼고, before<=0이면 최신 페이지)에서 최대 `limit`개이며, 화면용으로 id 오름차순이다.
// hasMore는 이 창보다 더 오래된 단계가 있는지를 알려 준다. 클라이언트가 위로 스크롤을 멈출 수 있다.
// 요약 열만 읽고, 본문은 ActivityDetail로 나중에 읽는다.
func (s *ExplorationStore) ActivityPage(f ActivitySessionFilter, before int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	const cols = `id, node_id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), metadata, created_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg`
	args := []any{s.expID}
	cond, cargs := f.cond(len(args) + 1)
	args = append(args, cargs...)
	beforeArg := len(args) + 1
	args = append(args, before)
	limitArg := len(args) + 1
	args = append(args, limit+1) // 더 오래된 이력이 있는지 보려 한 줄 더 읽는다
	q := fmt.Sprintf(`SELECT `+cols+`
FROM activity WHERE exploration_id=$1%s AND ($%d <= 0 OR id < $%d)
ORDER BY id DESC LIMIT $%d`, cond, beforeArg, beforeArg, limitArg)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	desc := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.Metadata, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens, &a.MainSeg); err != nil {
			return nil, false, err
		}
		desc = append(desc, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	// 최신순으로 받은 창을 화면용 id 오름차순으로 뒤집는다.
	out := make([]Activity, len(desc))
	for i, a := range desc {
		out[len(desc)-1-i] = a
	}
	return out, hasMore, nil
}

// ActivityPageForTerminalIntent는 물려받은 세션 이력의 경계다.
// 소유, 종료 상태, 행 종류 확인을 한 쿼리에 넣어, 페이징 전에 끝난 원본 의도가 다시 열리는 경합을 막는다.
func (s *ExplorationStore) ActivityPageForTerminalIntent(nodeID, before int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''),
		COALESCE(a.tool,''), COALESCE(a.tool_use_id,''), a.is_error, COALESCE(a.summary,''),
		a.created_at, a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cache_write_tokens
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2
	  AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	  AND ($3 <= 0 OR a.id < $3)
	ORDER BY a.id DESC LIMIT $4`, s.expID, nodeID, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	desc := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens); err != nil {
			return nil, false, err
		}
		desc = append(desc, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	out := make([]Activity, len(desc))
	for i, a := range desc {
		out[len(desc)-1-i] = a
	}
	return out, hasMore, nil
}

// ActivityMaxID는 이 작업의 현재 최대 활동 id를 돌려준다(없으면 0).
// SSE에 넘기는 작업 단위 스냅샷 커서라, 이력(id<=커서)과 실시간 꼬리(id>커서)가 빈틈 없이 만난다.
// 세션이 아니라 작업 단위인 이유는 SSE 하나가 작업 전체를 덮기 때문이다.
func (s *ExplorationStore) ActivityMaxID() (int64, error) {
	var max sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(id) FROM activity WHERE exploration_id=$1`, s.expID).Scan(&max)
	if err != nil {
		return 0, err
	}
	return max.Int64, nil
}

// MainSession은 한 작업에서 다시 시작할 수 있는 메인 에이전트 대화 구간이다. Segment 0은
// 원래 세션이다(암묵적이며 저장하지 않는다). 그다음 구간은
// "새 세션"으로 만들어지며, 메인 에이전트를 깨끗한 기록에서 다시 시작하는 동안 작업의 그래프,
// 자산, 목표는 그대로 공유된다.
// 새 세션은 대화만 새로 열고, 자산 그래프와 탐색 그래프는 같은 작업에 남긴다.
type MainSession struct {
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
}

// CurrentMainSeg는 가장 큰 메인 세션 구간을 돌려준다(아직 없으면 0).
func (s *ExplorationStore) CurrentMainSeg() (int, error) {
	var seg int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM main_sessions WHERE exploration_id=$1`, s.expID).Scan(&seg)
	return seg, err
}

// ListMainSessions는 메인 세션 구간을 최신순으로 돌려준다. 암묵적인 원래 구간 0은 항상 포함한다.
// 구간 0에는 저장된 시각이 없다.
func (s *ExplorationStore) ListMainSessions() ([]MainSession, error) {
	rows, err := s.db.Query(`SELECT seq, created_at FROM main_sessions WHERE exploration_id=$1 ORDER BY seq DESC`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MainSession{}
	for rows.Next() {
		var m MainSession
		if err := rows.Scan(&m.Seq, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out = append(out, MainSession{Seq: 0}) // 암묵적인 원래 세션(가장 오래됨)
	return out, nil
}

// NewMainSession은 다음 메인 세션 구간(seq = 현재+1)을 만들고 돌려준다.
// main_sessions만 건드린다. 작업의 탐색 그래프, 자산 그래프, 목표는 그대로라 새 세션은 같은 작업 위에서 빈 대화로 시작한다.
func (s *ExplorationStore) NewMainSession() (MainSession, error) {
	var m MainSession
	err := s.db.QueryRow(`
INSERT INTO main_sessions(exploration_id, seq)
VALUES ($1, COALESCE((SELECT MAX(seq) FROM main_sessions WHERE exploration_id=$1),0)+1)
RETURNING seq, created_at`, s.expID).Scan(&m.Seq, &m.CreatedAt)
	return m, err
}

// ActivityDetail은 단계 하나의 본문 전체를 필요할 때 돌려준다.
func (s *ExplorationStore) ActivityDetail(id int64) (string, error) {
	var d sql.NullString
	err := s.db.QueryRow(`SELECT detail FROM activity WHERE id=$1 AND exploration_id=$2`, id, s.expID).Scan(&d)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return d.String, err
}

// scanTrace는 워커 흔적 도구(ActivityTrace / ActivityTraceSearch)가 같이 쓰는 요약 열만 읽는다.
// 여기서 본문은 고르지 않는다. 필요할 때 ActivityByIDs로 따로 읽는다.
func scanTrace(rows *sql.Rows) ([]Activity, error) {
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.IsError, &a.Summary); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const traceCols = `id, node_id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), is_error, COALESCE(summary,'')`

// ActivityTrace는 흔적 도구용으로 작업 하나의 단계(의도/노드 id)를 나열한다.
// 요약만 주고 본문은 나중에 ActivityByIDs로 읽는다. thinking/usage 행은 빼서
// 호출자가 행동과 관찰만 보고, 모델 내부 추론이나 토큰 회계는 보지 않게 한다.
func (s *ExplorationStore) ActivityTrace(nodeID int64, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')
ORDER BY id LIMIT $3`, s.expID, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceForTerminalIntent는 조회 순간에 원본 의도가 아직 종료 상태일 때만 물려받은 작업 흔적 하나를 돌려준다.
func (s *ExplorationStore) ActivityTraceForTerminalIntent(nodeID int64, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	ORDER BY a.id LIMIT $3`, s.expID, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceSearch는 요약 또는 본문이 q와 맞는 단계를 찾는다(대소문자 무시, 요약만 돌려준다).
// nodeID가 nil이 아니면 작업 하나로 좁힌다. nil이면 모든 워커를 찾는다
// (node_id IS NOT NULL — 플래너/메인 단계에는 node_id가 없어서, 이건 "워커 흔적"만 뜻한다).
// thinking/usage 행은 뺀다.
func (s *ExplorationStore) ActivityTraceSearch(nodeID *int64, q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	var rows *sql.Rows
	var err error
	if nodeID != nil {
		rows, err = s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')
AND (summary ILIKE $3 OR detail ILIKE $3) ORDER BY id LIMIT $4`, s.expID, *nodeID, like, limit)
	} else {
		rows, err = s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id IS NOT NULL AND kind NOT IN ('thinking','usage')
AND (summary ILIKE $2 OR detail ILIKE $2) ORDER BY id LIMIT $3`, s.expID, like, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceSearchForTerminalIntent는 범위를 좁힌 물려받기 검색이다.
// 종료 상태인지는 행을 읽는 그 문장에서 같이 판단한다.
func (s *ExplorationStore) ActivityTraceSearchForTerminalIntent(nodeID int64, q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	  AND (a.summary ILIKE $3 OR a.detail ILIKE $3)
	ORDER BY a.id LIMIT $4`, s.expID, nodeID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityTraceSearchTerminalIntents는 물려받은 워커 이력을 찾는다.
// 플래너/메인 행과, 원본에서 아직 open/running인 작업은 보여 주지 않는다.
func (s *ExplorationStore) ActivityTraceSearchTerminalIntents(q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
	WHERE a.exploration_id=$1 AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.kind NOT IN ('thinking','usage')
	  AND (a.summary ILIKE $2 OR a.detail ILIKE $2)
	ORDER BY a.id LIMIT $3`, s.expID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// AssetRef는 앵커로 자산을 가리키는 짧은 탐색 노드(의도/사실/발견)다.
// 커버리지 그래프의 노드 서랍이 이 자산에 붙은 탐색 그래프 노드를 보여 준다.
type AssetRef struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Summary      string `json:"summary"`
	SourceTaskID int64  `json:"source_task_id,omitempty"`
	Inherited    bool   `json:"inherited,omitempty"`
}

// AssetRefs는 이 탐색에서 주어진 자산 id에 앵커된 의도/사실/발견을 최신순으로 돌려준다.
// 이 작업이 그 자산에 대해 무엇을 시험하고 무엇을 결론 냈는지다.
func (s *ExplorationStore) AssetRefs(assetID int64) ([]AssetRef, error) {
	if assetID <= 0 {
		return []AssetRef{}, nil
	}
	rows, err := s.db.Query(`
SELECT en.id, en.kind, en.state, en.payload
FROM exploration_anchors ea
JOIN exploration_nodes en ON en.id = ea.node_id
WHERE ea.asset_id = $1 AND en.exploration_id = $2 AND en.kind IN ('intent','fact','finding')
ORDER BY en.id DESC`, assetID, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetRef{}
	for rows.Next() {
		var r AssetRef
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Kind, &r.State, &payload); err != nil {
			return nil, err
		}
		var p map[string]any
		_ = json.Unmarshal(payload, &p)
		for _, k := range []string{"summary", "text"} {
			if v, ok := p[k].(string); ok && strings.TrimSpace(v) != "" {
				r.Summary = v
				break
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActivityTraceSearchExcluding은 이 탐색의 모든 노드 단계를 키워드로 찾되, 한 노드(excludeNodeID)의 단계는 뺀다.
// 워커의 search_all_worker_traces가 이미 맥락에 있는 자기 진행 중 흔적을 다시 주지 않게 한다.
// excludeNodeID<=0이면 빼지 않고 모든 노드를 찾는다.
func (s *ExplorationStore) ActivityTraceSearchExcluding(excludeNodeID int64, q string, limit int) ([]Activity, error) {
	if excludeNodeID <= 0 {
		return s.ActivityTraceSearch(nil, q, limit)
	}
	if limit <= 0 {
		limit = 100
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT `+traceCols+`
FROM activity WHERE exploration_id=$1 AND node_id IS NOT NULL AND node_id <> $2
AND kind NOT IN ('thinking','usage')
AND (summary ILIKE $3 OR detail ILIKE $3) ORDER BY id LIMIT $4`, s.expID, excludeNodeID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrace(rows)
}

// ActivityByIDs는 지정한 단계 id의 본문 전체를 이 탐색 안에서 읽는다(흔적 드릴다운).
// thinking 행은 그 id를 물어도 돌려주지 않는다. 순서는 입력 순이 아니라 id 오름차순이다.
func (s *ExplorationStore) ActivityByIDs(ids []int64) ([]Activity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids)+1)
	args[0] = s.expID
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args[i+1] = id
	}
	rows, err := s.db.Query(`SELECT id, node_id, COALESCE(kind,''), COALESCE(tool,''), is_error, COALESCE(detail,'')
FROM activity WHERE exploration_id=$1 AND kind<>'thinking' AND id IN (`+strings.Join(ph, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Kind, &a.Tool, &a.IsError, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActivityByIDsForTerminalIntents는 물려받은 본문을 읽는 경계다.
// 노드가 없는 플래너/메인 행, 의도가 아닌 활동, 살아 있는 원본 작업, thinking은 일부러 뺀다.
// 예전 동작이 필요한 로컬 호출은 ActivityByIDs를 쓴다.
func (s *ExplorationStore) ActivityByIDsForTerminalIntents(ids []int64) ([]Activity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids)+1)
	args[0] = s.expID
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args[i+1] = id
	}
	rows, err := s.db.Query(`SELECT a.id, a.node_id, COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.detail,'')
	FROM activity a
	JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
		WHERE a.exploration_id=$1 AND a.kind NOT IN ('thinking','usage') AND n.kind='intent'
	  AND n.state IN ('done','blocked','exhausted','stopped')
	  AND a.id IN (`+strings.Join(ph, ",")+`) ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Kind, &a.Tool, &a.IsError, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddStandaloneFinding은 독립 발견 표에 발견을 쓴다. 작업을 지워도 이 행은 남고,
// 작업이나 탐색 노드가 지워지면 task_id / node_id는 NULL이 된다. taskID와 nodeID는 0일 수 있다(NULL로 저장).
func (s *ExplorationStore) AddStandaloneFinding(taskID, nodeID int64, vulnclass, name, severity, summary, evidence, worker string, assetIDs []int64) (int64, error) {
	return s.db.AddFinding(taskID, nodeID, vulnclass, name, severity, summary, evidence, worker, assetIDs)
}
