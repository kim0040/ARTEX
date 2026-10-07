package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const FindingRetestAgentKey = "retester"

var ErrRetestNotRunning = errors.New("이번 재테스트는 이미 끝났거나 아직 시작되지 않았습니다. 발견 상세에서 새 재테스트를 시작하세요")

// FindingRetest는 대화 차례가 끝나면 고칠 수 없는 과거 재시험이다.
// Snapshot은 에이전트에게만 불러 주고, 이력 목록에는 보내지 않는다.
type FindingRetest struct {
	ID             int64           `json:"id"`
	FindingID      int64           `json:"finding_id"`
	ConversationID *int64          `json:"conversation_id"`
	Status         string          `json:"status"`
	Verdict        string          `json:"verdict"`
	Notes          string          `json:"notes"`
	Snapshot       json.RawMessage `json:"snapshot,omitempty"`
	Summary        string          `json:"summary"`
	Evidence       string          `json:"evidence"`
	Error          string          `json:"error"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at"`
}

const retestCols = `id, finding_id, conversation_id, status, verdict, notes, summary, evidence, error, created_at, started_at, finished_at`

// ActiveFindingRetest는 발견 목록이 폴링하는 작은 상태 본문이다.
// 발견 ID 문자열은 발견 API와 같다.
type ActiveFindingRetest struct {
	ID             int64  `json:"id"`
	FindingID      int64  `json:"finding_id,string"`
	ConversationID int64  `json:"conversation_id"`
	Status         string `json:"status"`
}

func (d *DB) ListActiveFindingRetests(ctx context.Context) ([]ActiveFindingRetest, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, finding_id, conversation_id, status FROM finding_retests
	WHERE status IN ('pending','running') AND conversation_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ActiveFindingRetest{}
	for rows.Next() {
		var item ActiveFindingRetest
		if err := rows.Scan(&item.ID, &item.FindingID, &item.ConversationID, &item.Status); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanRetest(row interface{ Scan(...any) error }) (*FindingRetest, error) {
	r := &FindingRetest{}
	err := row.Scan(&r.ID, &r.FindingID, &r.ConversationID, &r.Status, &r.Verdict, &r.Notes,
		&r.Summary, &r.Evidence, &r.Error, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// CreateFindingRetest는 원본을 한 번에 스냅샷하고, 대화를 만든 뒤 첫 메시지를 저장한다.
// 발견 행 잠금이 여러 클라이언트의 동시 클릭을 하나로 합친다.
// 이미 진행 중인 실행이 있으면 새로 보내지 않고 그것을 돌려준다.
func (d *DB) CreateFindingRetest(ctx context.Context, findingID int64, notes string) (*FindingRetest, *Conversation, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback()
	var title string
	var snapshot []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(f.name,''), NULLIF(f.vulnclass,''), '미분류'),
	jsonb_build_object('finding', to_jsonb(f),
	 'assets', COALESCE((SELECT jsonb_agg(to_jsonb(a)) FROM assets a WHERE f.asset_ids @> to_jsonb(ARRAY[a.id])), '[]'::jsonb),
	 'constraints', COALESCE((SELECT jsonb_agg(to_jsonb(c)) FROM task_constraints c JOIN tasks t ON t.exploration_id=c.exploration_id WHERE t.id=f.task_id), '[]'::jsonb))
	FROM findings f WHERE f.id=$1 FOR UPDATE OF f`, findingID).Scan(&title, &snapshot)
	if err != nil {
		return nil, nil, false, err
	}
	r, err := scanRetest(tx.QueryRowContext(ctx, `SELECT `+retestCols+` FROM finding_retests WHERE finding_id=$1 AND status IN ('pending','running')`, findingID))
	if err != nil {
		return nil, nil, false, err
	}
	if r != nil {
		return r, nil, false, nil
	}
	// 제목 길이는 일반 대화와 같은 상한에 맞춘다.
	if runes := []rune(title); len(runes) > 100 {
		title = string(runes[:100])
	}
	c, err := scanConv(tx.QueryRowContext(ctx, `INSERT INTO conversations(agent_key,title) VALUES ($1,$2) RETURNING `+convCols,
		FindingRetestAgentKey, fmt.Sprintf("재테스트 #%d · %s", findingID, title)))
	if err != nil {
		return nil, nil, false, err
	}
	r, err = scanRetest(tx.QueryRowContext(ctx, `INSERT INTO finding_retests(finding_id,conversation_id,notes,snapshot) VALUES ($1,$2,$3,$4) RETURNING `+retestCols,
		findingID, c.ID, strings.TrimSpace(notes), snapshot))
	if err != nil {
		return nil, nil, false, err
	}
	msg := r.InitialMessage()
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_activities(conversation_id,worker,kind,summary,detail) VALUES ($1,$2,'user',$3,$4)`,
		c.ID, FindingRetestAgentKey, fmt.Sprintf("발견 #%d을 재테스트하세요", findingID), msg)
	if err != nil {
		return nil, nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, false, err
	}
	return r, &c, true, nil
}

func (r *FindingRetest) InitialMessage() string {
	msg := fmt.Sprintf("발견 #%d을 재테스트하세요. 먼저 get_finding_retest_context를 호출해 이 세션에 연결된 원래 증거와 제약을 읽고, 이어서 표적 검증을 실행한 뒤, 마지막으로 record_finding_retest_result를 호출해 결론을 저장하세요.", r.FindingID)
	if r.Notes != "" {
		msg += "\n\n이번 재테스트 보충 설명:\n" + r.Notes
	}
	return msg
}

func (d *DB) ListFindingRetests(findingID int64) ([]*FindingRetest, error) {
	rows, err := d.Query(`SELECT `+retestCols+` FROM finding_retests WHERE finding_id=$1 ORDER BY id DESC`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FindingRetest{}
	for rows.Next() {
		r, err := scanRetest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) FindingRetestForConversation(ctx context.Context, conversationID int64) (*FindingRetest, error) {
	r, err := scanRetest(d.QueryRowContext(ctx, `SELECT `+retestCols+` FROM finding_retests WHERE conversation_id=$1`, conversationID))
	if err != nil || r == nil {
		return r, err
	}
	err = d.QueryRowContext(ctx, `SELECT snapshot FROM finding_retests WHERE id=$1`, r.ID).Scan(&r.Snapshot)
	return r, err
}

// FailPendingRetestForConversation은 러너가 재테스트를 아예 불러오지 못했을 때 그 대화의 끝나지 않은 재테스트를 봉인한다.
// 그 경로에서는 재테스트 ID를 모르므로, 대화 ID만이 유일한 핸들이다. 이것이 없으면 일시적 읽기 오류가
// 행을 pending으로 영원히 남긴다. 발견 목록은 계속 재테스트 중을 보여주고,
// 이후의 모든 재테스트 시작은 실제로 진행되지 않는 실행과 중복으로 걸러지며, 오직
// 프로세스 재시작(RecoverFindingRetests)만이 이를 해제할 수 있다.
func (d *DB) FailPendingRetestForConversation(conversationID int64, reason string) error {
	_, err := d.Exec(`UPDATE finding_retests SET status='failed', error=$2, finished_at=now()
		WHERE conversation_id=$1 AND status IN ('pending','running')`, conversationID, reason)
	return err
}

func (d *DB) StartFindingRetest(ctx context.Context, id int64) (bool, error) {
	res, err := d.ExecContext(ctx, `UPDATE finding_retests SET status='running', started_at=now() WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecordFindingRetestResult는 발견 ID를 받지 않는다. 소유는 실행 중인 대화에서 온다.
// 같은 재시도는 안전하고, 두 번째 판정은 거절한다.
func (d *DB) RecordFindingRetestResult(ctx context.Context, conversationID int64, verdict, summary, evidence string) error {
	if verdict != "reproduced" && verdict != "fixed" && verdict != "inconclusive" {
		return errors.New("verdict는 reproduced / fixed / inconclusive 여야 합니다")
	}
	summary, evidence = strings.TrimSpace(summary), strings.TrimSpace(evidence)
	if summary == "" || evidence == "" {
		return errors.New("summary와 evidence는 비울 수 없습니다. 확인할 수 없을 때는 실제로 한 점검과 막힌 이유를 적으세요")
	}
	if len(summary) > 16000 || len(evidence) > 128000 {
		return errors.New("재테스트 결론이 너무 깁니다(summary ≤ 16KB, evidence ≤ 128KB)")
	}
	res, err := d.ExecContext(ctx, `UPDATE finding_retests SET verdict=$2,summary=$3,evidence=$4
	WHERE conversation_id=$1 AND status='running' AND (verdict='' OR (verdict=$2 AND summary=$3 AND evidence=$4))`, conversationID, verdict, summary, evidence)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrRetestNotRunning
	}
	return nil
}

// FinishFindingRetest는 결과를 봉인한다. 취소·실패가 미리 적어 둔 판정보다 우선한다.
// 그래서 끊긴 시험이 고친 것처럼 보이지 않는다.
// 새로 완료된 「고침」 판정만 같은 트랜잭션에서 분류(triage)를 고친다.
func (d *DB) FinishFindingRetest(id int64, status, reason string) error {
	if status != "completed" && status != "failed" && status != "stopped" {
		return errors.New("invalid terminal retest status")
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 재시험보다 먼저 발견을 잠근다. 생성과 연쇄 삭제와 같은 순서다.
	var findingID int64
	err = tx.QueryRow(`SELECT f.id FROM findings f WHERE f.id=(SELECT finding_id FROM finding_retests WHERE id=$1) FOR UPDATE OF f`, id).Scan(&findingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 발견이나 재시험이 이미 지워졌다.
	}
	if err != nil {
		return err
	}
	var finalStatus, verdict string
	err = tx.QueryRow(`UPDATE finding_retests SET
	status=CASE WHEN $2='completed' AND verdict='' THEN 'failed' ELSE $2 END,
	error=CASE WHEN $2='completed' AND verdict='' THEN 'Agent가 재검사 결론을 저장하지 않았습니다. 세션을 확인한 뒤 다시 재검사하세요' ELSE $3 END,
	finished_at=now() WHERE id=$1 AND status IN ('pending','running') RETURNING status,verdict`, id, status, reason).Scan(&finalStatus, &verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 재생이 나중에 사람이 고른 분류 결정을 덮어쓰면 안 된다.
	}
	if err != nil {
		return err
	}
	if finalStatus == "completed" && verdict == "fixed" {
		// 알림이 있는 버전을 타며, 사람이 상세 페이지에서 상태를 바꿀 때와 같은 의미를 공유한다.
		//
		// 이전에는 여기가 그냥 UPDATE였다. 재테스트가 「수정됨」으로 판정하면 상태는 실제로 바뀌지만, 설정된
		// on_status_change 채널은 푸시를 전혀 받지 못한다. 상태가 화면에서 조용히 바뀌어,
		// 운영자는 플랫폼을 열어야만 알 수 있다. 상태 갱신과 푸시 이벤트는 함께 저장되어야 하고,
		// SetFindingStatusTx 안에서 「상태가 그대로면 등록하지 않음」 같은 세부 사항을 처리한다.
		// context.Background()를 쓴다. 이 함수 전체가 ctx 없는 옛 스타일(d.Begin()/
		// tx.QueryRow/tx.Exec)이라 전달할 취소 신호가 없고, ctx 매개변수를 억지로 넣으면
		// server 쪽 호출부와 여러 테스트가 함께 바뀌어, 이번 변경 범위를 넘는다.
		if _, _, _, _, err := SetFindingStatusTx(context.Background(), tx, findingID, FindingFixed); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) RecoverFindingRetests() error {
	_, err := d.Exec(`UPDATE finding_retests SET status='stopped', error='서비스가 다시 시작되어 재검사가 중단되었습니다. 다시 시작하세요', finished_at=now() WHERE status IN ('pending','running')`)
	return err
}
