package db

// 탐색 그래프 노드 종류(exploration_nodes.kind).
const (
	KindBegin   = "begin"   // 폐기: 예전 작업 뿌리. 새 작업은 기원 사실(KindFact + StateOrigin)을 심는다
	KindGoal    = "goal"    // 작업의 목표
	KindIntent  = "intent"  // 플래너가 만든 탐색 방향(의도)
	KindFact    = "fact"    // 워커의 탐색 결과·결론(부정 결과 포함). 그 의도에 묶인다
	KindFinding = "finding" // 확인된 취약점(report_finding). 사실과는 다르다
	KindHint    = "hint"
	KindDigest  = "digest" // 식은 의도·사실을 접어 압축한 노드(cold-digest-spec §1). 손실 없음 — 구성원은 남고 id로 되돌릴 수 있다
)

// 다이제스트 노드 상태(kind='digest'). graph_overview에 보이는 동안은 'active'다.
// 큰 압축이 합쳐 없앤 구간은 'superseded'로 보내고, covers 간선은 새 다이제스트를 가리킨다
// (cold-digest-spec §5.1). 식은 탐색 그래프의 의도·사실을 UI 개요용으로 접은 것이다.
const (
	StateDigestActive     = "active"
	StateDigestSuperseded = "superseded"
)

// StateOrigin은 작업을 만들 때 심는 뿌리 사실(KindFact)이다. 탐색 그래프의 출발점이다.
// 모든 의도는 여기로 거슬러 올라가므로, 첫 의도부터 「의도는 사실에 연결된다」가 성립한다.
// 워커가 만든 사실은 state가 'confirmed'라 이 값과 겹치지 않는다.
const StateOrigin = "origin"

// StateIntentDeleted는 사용자가 가삭제(soft delete)한 의도를 표시한다. 다른 종료 상태와 같이
// frontier와 graph_overview에서는 빠지지만, 노드와 전체 계보는 유지한다.
// 삭제 이유는 exploration_nodes.delete_reason에 있다.
// 이 상태는 탐색 그래프의 의도 노드를 요약 화면에서 빼되 계보는 남겨 이후 조회와 잇는다.
const StateIntentDeleted = "deleted"

// 탐색 그래프의 간선 관계(exploration_edges.rel).
const (
	RelSpawns      = "spawns"
	RelDerivedFrom = "derived_from"
	RelYields      = "yields"
	RelProves      = "proves"
	RelCovers      = "covers" // 다이제스트 --covers--> 구성원(cold-digest-spec §1). 「어느 다이제스트가 노드 X를 접었나」의 기준
)
