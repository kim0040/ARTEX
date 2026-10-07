package db

import (
	"fmt"
	"time"
)

// Constraint는 운영자가 작업에 적은 운용 제약 하나다. kind=allow는 허용된 동작,
// kind=deny는 금지된 동작이며 글은 자유 형식이다. task_constraints에 있고
// exploration_id로 묶인다(탐색이 지워지면 함께 지워진다). 플래너와 워커 프롬프트에 들어가 경계를 보여 준다.
type Constraint struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"` // allow(허용) | deny(금지)
	Text      string    `json:"text"`
	Origin    string    `json:"origin,omitempty"` // goals(목표에서 추출) | human(사람) | system(시스템)
	CreatedAt time.Time `json:"created_at"`
}

// ListConstraints는 이 탐색의 제약을 돌려준다. allow이 deny보다 먼저이고,
// 같은 종류 안에서는 오래된 것이 먼저다(프롬프트 블록과 UI의 순서를 고정한다).
func (s *ExplorationStore) ListConstraints() ([]Constraint, error) {
	rows, err := s.db.Query(`
SELECT id, kind, text, COALESCE(origin,''), created_at
FROM task_constraints WHERE exploration_id=$1
ORDER BY (kind='deny'), id`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Constraint
	for rows.Next() {
		var c Constraint
		if err := rows.Scan(&c.ID, &c.Kind, &c.Text, &c.Origin, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddConstraint는 제약 하나를 넣고(kind는 allow 또는 deny) 그 id를 돌려준다.
func (s *ExplorationStore) AddConstraint(kind, text, origin string) (int64, error) {
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("kind 는 allow 또는 deny 여야 합니다")
	}
	if origin == "" {
		origin = "system"
	}
	var id int64
	err := s.db.QueryRow(`
INSERT INTO task_constraints(exploration_id, kind, text, origin)
VALUES ($1, $2, $3, $4) RETURNING id`, s.expID, kind, text, origin).Scan(&id)
	return id, err
}

// UpdateConstraint는 이 탐색 안의 제약 kind와 글을 다시 쓴다.
// 해당 제약이 없으면 오류를 돌려준다.
func (s *ExplorationStore) UpdateConstraint(id int64, kind, text string) error {
	if kind != "allow" && kind != "deny" {
		return fmt.Errorf("kind 는 allow 또는 deny 여야 합니다")
	}
	res, err := s.db.Exec(`
UPDATE task_constraints SET kind=$1, text=$2, updated_at=now()
WHERE id=$3 AND exploration_id=$4`, kind, text, id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("제약 조건이 없습니다")
	}
	return nil
}

// DeleteConstraint는 이 탐색 안의 제약 하나를 지운다.
// 해당 제약이 없으면 오류를 돌려준다.
func (s *ExplorationStore) DeleteConstraint(id int64) error {
	res, err := s.db.Exec(`DELETE FROM task_constraints WHERE id=$1 AND exploration_id=$2`, id, s.expID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("제약 조건이 없습니다")
	}
	return nil
}
