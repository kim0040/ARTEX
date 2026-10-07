package db

// cold-digest §1/§2.3/§5: 식은 노드를 압축해 저장하는 계층이다. 라운드
// 카운터, cold_since_round 도장, 내용 버전, 다이제스트 노드, 그리고
// 「어느 다이제스트가 노드 X를 접었나」의 기준인 covers 간선을 담는다.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// BumpRound는 이 탐색의 플래너 라운드 카운터를 하나 올리고 새 값을 돌려준다(§2.3).
// 플래너가 깨어날 때마다 한 번 호출한다.
func (s *ExplorationStore) BumpRound() (int64, error) {
	var r int64
	err := s.db.QueryRow(`UPDATE explorations SET round_no = round_no + 1 WHERE id=$1 RETURNING round_no`, s.expID).Scan(&r)
	return r, err
}

// RoundNo는 현재 플래너 라운드 카운터를 돌려준다.
func (s *ExplorationStore) RoundNo() (int64, error) {
	var r int64
	err := s.db.QueryRow(`SELECT round_no FROM explorations WHERE id=$1`, s.expID).Scan(&r)
	return r, err
}

// ColdStamps는 접을 수 있는 노드(의도·사실)마다 cold_since_round를 돌려준다.
// id → *라운드(뜨겁거나 도장이 없으면 nil). ≥R 디바운스를 계산하고,
// 이번 라운드에 어느 도장을 찍거나 지울지 정할 때 쓴다.
func (s *ExplorationStore) ColdStamps() (map[int64]*int64, error) {
	rows, err := s.db.Query(`SELECT id, cold_since_round FROM exploration_nodes
		WHERE exploration_id=$1 AND kind IN ('intent','fact')`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*int64{}
	for rows.Next() {
		var id int64
		var cs sql.NullInt64
		if err := rows.Scan(&id, &cs); err != nil {
			return nil, err
		}
		if cs.Valid {
			v := cs.Int64
			out[id] = &v
		} else {
			out[id] = nil
		}
	}
	return out, rows.Err()
}

// StampOp는 cold_since_round 변경 하나다. Set이면 Round를 찍고, Set이 아니면 지운다.
type StampOp struct {
	ID    int64
	Set   bool
	Round int64
}

// ApplyStampOps는 cold_since_round 변경을 한 트랜잭션에 모아 쓴다.
func (s *ExplorationStore) ApplyStampOps(ops []StampOp) error {
	if len(ops) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, o := range ops {
		if o.Set {
			if _, err := tx.Exec(`UPDATE exploration_nodes SET cold_since_round=$1 WHERE id=$2 AND exploration_id=$3`, o.Round, o.ID, s.expID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`UPDATE exploration_nodes SET cold_since_round=NULL WHERE id=$1 AND exploration_id=$2`, o.ID, s.expID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ContentVersions는 모든 노드의 id → content_version을 돌려준다(§5.3 서명).
func (s *ExplorationStore) ContentVersions() (map[int64]int, error) {
	rows, err := s.db.Query(`SELECT id, content_version FROM exploration_nodes WHERE exploration_id=$1`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var v int
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// ActiveDigests는 이 탐색에서 살아있는 다이제스트 노드(state='active')를
// 오래된 것부터 돌려준다.
func (s *ExplorationStore) ActiveDigests() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
		WHERE exploration_id=$1 AND kind=$2 AND state=$3 ORDER BY id`, s.expID, KindDigest, StateDigestActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// AddDigest는 다이제스트 노드와 covers 간선(다이제스트→구성원)을 한 트랜잭션에 쓴다.
// payload는 본문 + member_ids + generation + 서명이다(cold-digest §1).
// 새 다이제스트 id를 돌려준다. 이 노드가 탐색 그래프의 식은 의도·사실을 접어 UI 개요에 보여 준다.
func (s *ExplorationStore) AddDigest(payload map[string]any, memberIDs []int64) (int64, error) {
	raw, _ := json.Marshal(payload)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, origin)
VALUES ($1, $2, $3, 0, $4, 'compactor') RETURNING id`,
		s.expID, KindDigest, string(raw), StateDigestActive).Scan(&id); err != nil {
		return 0, err
	}
	for _, m := range memberIDs {
		if m == id {
			continue
		}
		if _, err := tx.Exec(`
INSERT INTO exploration_edges(exploration_id, src_id, rel, dst_id) VALUES ($1,$2,$3,$4)
ON CONFLICT (exploration_id, src_id, rel, dst_id) DO NOTHING`, s.expID, id, RelCovers, m); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// CoveredMembers는 구성원 id → 그것을 덮는 다이제스트 id다. ACTIVE 다이제스트만 본다(§6.1 1항).
// 항목이 없는 노드는 지금 접혀 있지 않다. 구성원이 다이제스트 두 개의 covers를 가지면
// (큰 쓰기가 중간에 끊긴 경우) 더 작은 다이제스트 id가 이긴다. 호출자는 이 값으로 중복을 뺀다.
func (s *ExplorationStore) CoveredMembers() (map[int64]int64, error) {
	rows, err := s.db.Query(`
SELECT e.dst_id, e.src_id
FROM exploration_edges e
JOIN exploration_nodes d ON d.id=e.src_id AND d.exploration_id=e.exploration_id
WHERE e.exploration_id=$1 AND e.rel=$2 AND d.kind=$3 AND d.state=$4
ORDER BY e.src_id`, s.expID, RelCovers, KindDigest, StateDigestActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var member, digest int64
		if err := rows.Scan(&member, &digest); err != nil {
			return nil, err
		}
		if _, seen := out[member]; !seen { // 먼저 온 것(더 작은 다이제스트 id)이 이긴다
			out[member] = digest
		}
	}
	return out, rows.Err()
}

// DigestMembers는 다이제스트가 덮는 구성원 id(covers 간선)를 정렬해 돌려준다.
func (s *ExplorationStore) DigestMembers(digestID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT dst_id FROM exploration_edges
		WHERE exploration_id=$1 AND src_id=$2 AND rel=$3`, s.expID, digestID, RelCovers)
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
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, rows.Err()
}

// SupersedeDigests는 다이제스트 노드를 은퇴시키고(state→superseded) covers 간선도 지운다.
// 한 번에 처리해서, 큰 재압축 중에 활성 덮개(CoveredMembers)가 구성원을 두 번 세지 않게 한다(§5.1).
// 다이제스트 노드 자체는 남긴다(node_detail이 아직 찾을 수 있다).
func (s *ExplorationStore) SupersedeDigests(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM exploration_edges
			WHERE exploration_id=$1 AND src_id=$2 AND rel=$3`, s.expID, id, RelCovers); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE exploration_nodes SET state=$1 WHERE id=$2 AND exploration_id=$3 AND kind=$4`,
			StateDigestSuperseded, id, s.expID, KindDigest); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NodeAssets는 준 노드 id마다, 그 노드가 앵커된 자산 id를 돌려준다(exploration_anchors).
// 식은 다이제스트를 자산 그래프의 자산별로 묶을 때 쓴다(§6.2 색인).
func (s *ExplorationStore) NodeAssets(ids []int64) (map[int64][]int64, error) {
	out := map[int64][]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, s.expID)
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT a.node_id, a.asset_id
		FROM exploration_anchors a
		JOIN exploration_nodes n ON n.id=a.node_id
		WHERE n.exploration_id=$1 AND a.node_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var node, asset int64
		if err := rows.Scan(&node, &asset); err != nil {
			return nil, err
		}
		out[node] = append(out[node], asset)
	}
	return out, rows.Err()
}
