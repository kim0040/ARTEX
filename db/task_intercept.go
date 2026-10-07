package db

import (
	"database/sql"
	"fmt"
)

// TaskInterceptRuleInput은 작업을 만들 때 넣는 작업 단위 규칙 하나다.
// Action: block=가로채기, allow=허용(허용 목록). 비어 있으면 block으로 본다.
// 이 타입은 작업 생성 화면에서 넘긴 가로채기 규칙으로, 워커가 자산 그래프의 자산을 다루기 전에 block 또는 allow을 정한다.
type TaskInterceptRuleInput struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Note    string `json:"note"`
}

const taskInterceptRuleCols = `id, enabled, action, kind, pattern, note, created_at, updated_at`

func scanTaskInterceptRule(row interface{ Scan(...any) error }) (AssetInterceptRule, error) {
	var r AssetInterceptRule
	err := row.Scan(&r.ID, &r.Enabled, &r.Action, &r.Kind, &r.Pattern, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ListTaskInterceptRules는 작업의 가로채기 규칙(block과 allow 모두)을
// AssetInterceptRule로 돌려준다. Builtin은 항상 false이고, Action이 block/allow를 담는다.
// taskID가 0 이하면 아무것도 돌려주지 않는다.
func (s *AssetStore) ListTaskInterceptRules(taskID int64) ([]AssetInterceptRule, error) {
	if taskID <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT `+taskInterceptRuleCols+` FROM task_intercept_rules WHERE task_id=$1 ORDER BY action, id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetInterceptRule
	for rows.Next() {
		r, err := scanTaskInterceptRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskInterceptRulesSplit은 작업 규칙을 읽어 block과 allow로 나눈다.
// 워커가 자산 그래프의 자산을 다루기 전, 가로채기 문에서 이 두 묶음을 쓴다.
func (s *AssetStore) TaskInterceptRulesSplit(taskID int64) (block, allow []AssetInterceptRule, err error) {
	rules, err := s.ListTaskInterceptRules(taskID)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rules {
		if r.Action == "allow" {
			allow = append(allow, r)
		} else {
			block = append(block, r)
		}
	}
	return block, allow, nil
}

func normalizeRuleAction(action string) string {
	if action == "allow" {
		return "allow"
	}
	return "block"
}

// CreateTaskInterceptRule은 작업 아래에 규칙 하나를 넣는다.
func (s *AssetStore) CreateTaskInterceptRule(taskID int64, action, kind, pattern, note string, enabled bool) (AssetInterceptRule, error) {
	row := s.db.QueryRow(`
INSERT INTO task_intercept_rules(task_id, enabled, action, kind, pattern, note)
VALUES ($1,$2,$3,$4,$5,$6)
RETURNING `+taskInterceptRuleCols,
		taskID, enabled, normalizeRuleAction(action), kind, pattern, note)
	return scanTaskInterceptRule(row)
}

// UpdateTaskInterceptRule은 작업 규칙에서 고칠 수 있는 필드를 바꾼다.
// task_id로 범위를 묶어, 그 작업으로만 고칠 수 있다.
func (s *AssetStore) UpdateTaskInterceptRule(taskID, ruleID int64, action, kind, pattern, note string, enabled bool) (AssetInterceptRule, error) {
	row := s.db.QueryRow(`
UPDATE task_intercept_rules
   SET enabled=$3, action=$4, kind=$5, pattern=$6, note=$7
WHERE id=$1 AND task_id=$2
RETURNING `+taskInterceptRuleCols,
		ruleID, taskID, enabled, normalizeRuleAction(action), kind, pattern, note)
	return scanTaskInterceptRule(row)
}

// DeleteTaskInterceptRule은 작업의 규칙을 지운다. 없으면 false를 돌려준다.
func (s *AssetStore) DeleteTaskInterceptRule(taskID, ruleID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM task_intercept_rules WHERE id=$1 AND task_id=$2`, ruleID, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ToggleTaskInterceptRule은 작업 규칙의 켜짐/꺼짐을 바꾼다.
func (s *AssetStore) ToggleTaskInterceptRule(taskID, ruleID int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE task_intercept_rules SET enabled=$3 WHERE id=$1 AND task_id=$2`, ruleID, taskID, enabled)
	return err
}

// insertTaskInterceptRules는 작업을 만드는 트랜잭션 안에서 작업 단위 규칙을 넣는다
// (insertTaskCompanies와 같은 자리다).
func insertTaskInterceptRules(tx *sql.Tx, taskID int64, rules []TaskInterceptRuleInput) error {
	for _, r := range rules {
		if _, err := tx.Exec(`
INSERT INTO task_intercept_rules(task_id, enabled, action, kind, pattern, note)
VALUES ($1,$2,$3,$4,$5,$6)`, taskID, r.Enabled, normalizeRuleAction(r.Action), r.Kind, r.Pattern, r.Note); err != nil {
			return fmt.Errorf("insert task intercept rule %q: %w", r.Pattern, err)
		}
	}
	return nil
}
