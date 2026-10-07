package db

import (
	"database/sql"
	"sort"
)

// DirectSourceStore는 직접 연결된 작업 하나를 그 탐색 저장소에 묶는다.
// 일부러 읽기 전용 도우미다. 그래프를 고치거나, 프론티어를 찾거나, 의도를 집을 때는
// 호출자가 받는 쪽 저장소를 그대로 쓴다.
type DirectSourceStore struct {
	Task  TaskSource
	Store *ExplorationStore
}

// DirectSourceStores는 이 탐색의 살아있는 직접 작업 관계를 찾는다.
// 호출마다 일부러 다시 조회한다. 원본 작업이 지워지면 관계도 사라지고,
// 물려받은 맥락이 낡은 행을 남기면 안 된다. 원본의 원본은 펼치지 않는다.
func (s *ExplorationStore) DirectSourceStores() ([]DirectSourceStore, error) {
	rows, err := s.db.Query(`
SELECT source.id, source.exploration_id, source.description, source.goal, source.status
FROM tasks owner
JOIN task_relations relation ON relation.task_id=owner.id
JOIN tasks source ON source.id=relation.source_task_id AND source.deleted_at IS NULL
WHERE owner.exploration_id=$1 AND owner.deleted_at IS NULL
ORDER BY relation.created_at, source.id`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DirectSourceStore{}
	for rows.Next() {
		var source TaskSource
		if err := rows.Scan(&source.TaskID, &source.ExplorationID, &source.Description, &source.Goal, &source.Status); err != nil {
			return nil, err
		}
		out = append(out, DirectSourceStore{Task: source, Store: s.db.Exploration(source.ExplorationID)})
	}
	return out, rows.Err()
}

// TaskID는 이 탐색에 묶인 살아있는 작업을 돌려준다.
// 테스트나 유지보수 코드가 직접 만든 탐색은 작업이 없어 0을 돌려준다.
func (s *ExplorationStore) TaskID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL`, s.expID).Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

func markInheritedNode(n *Node, taskID int64) *Node {
	if n == nil {
		return nil
	}
	n.SourceTaskID = taskID
	n.Inherited = true
	return n
}

func markInheritedActivities(in []Activity, taskID int64) []Activity {
	for i := range in {
		in[i].SourceTaskID = taskID
		in[i].Inherited = true
	}
	return in
}

func inheritedIntentTerminal(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// ListByKindWithSources는 로컬 노드를 먼저, 그다음 각 직접 원본의 노드를 관계 순으로 돌려준다.
// 상한은 탐색마다 그대로다. 기존 ListByKind와 같고, 큰 작업 하나가
// 다른 원본의 물려받은 맥락을 전부 가리지 않게 한다.
func (s *ExplorationStore) ListByKindWithSources(kind string, limit int) ([]*Node, error) {
	out, err := s.ListByKind(kind, limit)
	if err != nil {
		return nil, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		nodes, err := source.Store.ListByKind(kind, limit)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			if kind == KindIntent && !inheritedIntentTerminal(node.State) {
				continue
			}
			out = append(out, markInheritedNode(node, source.Task.TaskID))
		}
	}
	return out, nil
}

// ListByKindPageWithSources는 ListByKindWithSources의 페이지·키워드 버전이다.
// 이 탐색과 직접 원본을 가로질러 최신 먼저 페이지 하나를 돌려준다
// (id < before, before가 0 이하면 최신). hasMore와 필터된 전체 개수도 함께다.
// 노드 id는 전역으로 유일하다. 저장소마다 자기 페이지를 모아 id 내림차순으로 다시 정렬하면
// 진짜 전역 페이지가 된다. 저장소마다 limit+1을 읽어야 합친 상위 limit이 빠지지 않는다.
func (s *ExplorationStore) ListByKindPageWithSources(kind string, before int64, limit int, q string) (nodes []*Node, hasMore bool, total int, err error) {
	if limit <= 0 {
		limit = 20
	}
	own, err := s.listByKindPageFiltered(kind, before, limit, q)
	if err != nil {
		return nil, false, 0, err
	}
	merged := own
	total, err = s.countByKindFiltered(kind, q)
	if err != nil {
		return nil, false, 0, err
	}

	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, false, 0, err
	}
	for _, source := range sources {
		page, err := source.Store.listByKindPageFiltered(kind, before, limit, q)
		if err != nil {
			return nil, false, 0, err
		}
		for _, n := range page {
			merged = append(merged, markInheritedNode(n, source.Task.TaskID))
		}
		cnt, err := source.Store.countByKindFiltered(kind, q)
		if err != nil {
			return nil, false, 0, err
		}
		total += cnt
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].ID > merged[j].ID })
	hasMore = len(merged) > limit
	if hasMore {
		merged = merged[:limit]
	}
	return merged, hasMore, total, nil
}

// GetNodeWithSources는 노드가 이 탐색이거나 직접 원본 중 하나일 때만 읽는다.
// 물려받은 노드는 표시를 달아, 도구 호출자가 읽기 전용으로 두고 출처를 보여 주게 한다.
func (s *ExplorationStore) GetNodeWithSources(id int64) (*Node, error) {
	node, err := s.GetNode(id)
	if err != nil || node != nil {
		return node, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		node, err = source.Store.GetNode(id)
		if err != nil {
			return nil, err
		}
		if node != nil {
			if node.Kind == KindIntent && !inheritedIntentTerminal(node.State) {
				continue
			}
			return markInheritedNode(node, source.Task.TaskID), nil
		}
	}
	return nil, nil
}

// FindingIntentsWithSources는 현재 탐색과 각 직접 원본의 발견 계보를 합친다.
// 노드 id는 전역으로 유일하다.
func (s *ExplorationStore) FindingIntentsWithSources() (map[int64]int64, error) {
	out, err := s.FindingIntents()
	if err != nil {
		return nil, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		items, err := source.Store.FindingIntentsTerminal()
		if err != nil {
			return nil, err
		}
		for findingID, intentID := range items {
			out[findingID] = intentID
		}
	}
	return out, nil
}

// ActivityTraceWithSources는 의도가 현재 탐색이거나 직접 원본일 때 작업 흔적을 돌려준다.
// 간접 원본은 찾지 않는다.
func (s *ExplorationStore) ActivityTraceWithSources(nodeID int64, limit int) ([]Activity, error) {
	acts, err := s.ActivityTrace(nodeID, limit)
	if err != nil || len(acts) > 0 {
		return acts, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		node, nodeErr := source.Store.GetNode(nodeID)
		if nodeErr != nil {
			return nil, nodeErr
		}
		if node == nil || node.Kind != KindIntent {
			continue
		}
		acts, err = source.Store.ActivityTraceForTerminalIntent(nodeID, limit)
		if err != nil {
			return nil, err
		}
		if len(acts) > 0 {
			return markInheritedActivities(acts, source.Task.TaskID), nil
		}
	}
	return []Activity{}, nil
}

// ActivityListWithSources는 get_worker_output이 쓰는, 원본을 아는 목록이다.
// 노드 id는 전역이라, 아직 활동이 없어도 처음 소유한 탐색이 분명하다.
func (s *ExplorationStore) ActivityListWithSources(nodeID, sinceID int64, limit int) ([]Activity, int64, error) {
	node, err := s.GetNode(nodeID)
	if err != nil {
		return nil, sinceID, err
	}
	if node != nil {
		return s.ActivityList(&nodeID, sinceID, limit)
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, sinceID, err
	}
	for _, source := range sources {
		node, err = source.Store.GetNode(nodeID)
		if err != nil {
			return nil, sinceID, err
		}
		if node == nil || node.Kind != KindIntent {
			continue
		}
		acts, cursor, err := source.Store.ActivityListForTerminalIntent(nodeID, sinceID, limit)
		if err != nil {
			return nil, sinceID, err
		}
		return markInheritedActivities(acts, source.Task.TaskID), cursor, nil
	}
	return []Activity{}, sinceID, nil
}

// ActivityDetailWithSources는 예전 로컬 작업 조회(로컬 생각 행 포함)를 유지한다.
// 물려받은 자세한 내용은 끝난 워커 의도만 본다.
// 원본의 플래너·메인 행은 노드 id가 없어, 물려받은 맥락에 들어가면 안 된다.
func (s *ExplorationStore) ActivityDetailWithSources(id int64) (string, error) {
	detail, err := s.ActivityDetail(id)
	if err != nil || detail != "" {
		return detail, err
	}
	acts, err := s.ActivityByIDsWithSources([]int64{id})
	if err != nil || len(acts) == 0 {
		return "", err
	}
	return acts[0].Detail, nil
}

// ActivityTraceSearchWithSources는 get_worker_trace가 쓰는, 범위가 있는 워커 흔적 검색이다.
// 탐색 노드 id는 전역이라 노드 id는 탐색 하나에만 속한다.
func (s *ExplorationStore) ActivityTraceSearchWithSources(nodeID int64, q string, limit int) ([]Activity, error) {
	acts, err := s.ActivityTraceSearch(&nodeID, q, limit)
	if err != nil || len(acts) > 0 {
		return acts, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		node, nodeErr := source.Store.GetNode(nodeID)
		if nodeErr != nil {
			return nil, nodeErr
		}
		if node == nil || node.Kind != KindIntent {
			continue
		}
		acts, err = source.Store.ActivityTraceSearchForTerminalIntent(nodeID, q, limit)
		if err != nil {
			return nil, err
		}
		if len(acts) > 0 {
			return markInheritedActivities(acts, source.Task.TaskID), nil
		}
	}
	return []Activity{}, nil
}

// ActivityTraceSearchAllWithSources는 로컬 워커 흔적과 모든 직접 원본을 검색한다.
// 로컬 소유자 제외는 현재 탐색에만 적용된다. 물려받은 흔적은 고칠 수 없는 과거 맥락이다.
func (s *ExplorationStore) ActivityTraceSearchAllWithSources(excludeNodeID int64, q string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	out, err := s.ActivityTraceSearchExcluding(excludeNodeID, q, limit)
	if err != nil {
		return nil, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		acts, err := source.Store.ActivityTraceSearchTerminalIntents(q, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, markInheritedActivities(acts, source.Task.TaskID)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ActivityByIDsWithSources는 로컬 단계 자세한 내용을 예전 방식으로 읽는다.
// 직접 원본은 끝난 의도에 붙은 행만 돌려준다. 아무 전역 활동 id로
// 원본 플래너·메인 기록을 보지 못하게 한다.
func (s *ExplorationStore) ActivityByIDsWithSources(ids []int64) ([]Activity, error) {
	out, err := s.ActivityByIDs(ids)
	if err != nil {
		return nil, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		acts, err := source.Store.ActivityByIDsForTerminalIntents(ids)
		if err != nil {
			return nil, err
		}
		out = append(out, markInheritedActivities(acts, source.Task.TaskID)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AssetRefsWithSources는 이 탐색과 각 직접 원본에서 앵커된 노드를 돌려준다.
// 물려받은 항목은 소유 작업 id를 남겨, API와 UI가 고칠 수 없는 맥락으로 보여 주게 한다.
func (s *ExplorationStore) AssetRefsWithSources(assetID int64) ([]AssetRef, error) {
	out, err := s.AssetRefs(assetID)
	if err != nil {
		return nil, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		refs, err := source.Store.AssetRefs(assetID)
		if err != nil {
			return nil, err
		}
		for i := range refs {
			if refs[i].Kind == KindIntent && !inheritedIntentTerminal(refs[i].State) {
				continue
			}
			refs[i].SourceTaskID = source.Task.TaskID
			refs[i].Inherited = true
			out = append(out, refs[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}
