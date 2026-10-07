package db

import "fmt"

// DiscardOpenIntent는 작업 입장이 실패했을 때, 방금 만든 후속 의도를 되돌린다.
// 호출자가 작업 실행 잠금을 계속 잡고 있어야, 확인과 삭제 사이에 워커가 의도를 집어 가지 못한다.
// 간선과 앵커는 노드와 함께 지워진다. 활동 기록은 노드 외래 키가 NULL이 되므로 따로 지운다.
func (s *ExplorationStore) DiscardOpenIntent(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM activity WHERE exploration_id=$1 AND node_id=$2`, s.expID, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM exploration_nodes
		WHERE id=$1 AND exploration_id=$2 AND kind='intent' AND state='open'`, id, s.expID)
	if err != nil {
		return err
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if removed != 1 {
		return fmt.Errorf("open intent %d was not available for admission rollback", id)
	}
	return tx.Commit()
}
