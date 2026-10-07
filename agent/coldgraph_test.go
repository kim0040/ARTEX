package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 도우미: 의도/사실 노드
func intent(id int64, state string) cgNode { return cgNode{ID: id, Kind: db.KindIntent, State: state} }
func fact(id int64) cgNode                 { return cgNode{ID: id, Kind: db.KindFact, State: "confirmed"} }
func yields(from, to int64) cgEdge         { return cgEdge{From: from, Rel: db.RelYields, To: to} }
func derived(from, to int64) cgEdge        { return cgEdge{From: from, Rel: db.RelDerivedFrom, To: to} }

func ptr(v int64) *int64 { return &v }

// 부록 스냅샷 1: a→b→c, b→d. d 는 살아 있음(running), c 는 정착/비활성.
// 기대: d/b/a 는 hot(b 는 살아 있는 b→d 가지 때문에 hot). c 는 cold 후보이지만
// 고립된 하나라 접히지 않습니다.
func TestHotCold_AnyLiveBranchKeepsChainHot(t *testing.T) {
	// a(의도) → b(의도) → c(사실). b → d(의도, running)
	nodes := []cgNode{intent(1, "done"), intent(2, "done"), fact(3), intent(4, "running")}
	edges := []cgEdge{derived(1, 2), yields(2, 3), derived(2, 4)}
	g := newColdGraph(nodes, edges)
	hot := g.hotSet()
	for _, id := range []int64{1, 2, 4} {
		if !hot[id] {
			t.Fatalf("node %d should be hot (ancestor of / is live intent 4)", id)
		}
	}
	if hot[3] {
		t.Fatalf("node 3 (dead leaf c) should be cold")
	}
	structCold := g.structuralCold(hot)
	if !structCold[3] || len(structCold) != 1 {
		t.Fatalf("only node 3 should be structurally cold, got %v", structCold)
	}
}

// 부록 스냅샷 2: d 도 끝나면 a/b/c/d 가 모두 차갑고 연결되어 블록 하나가 됩니다.
func TestHotCold_WholeChainFoldsWhenAllSettled(t *testing.T) {
	nodes := []cgNode{intent(1, "done"), intent(2, "done"), fact(3), intent(4, "done")}
	edges := []cgEdge{derived(1, 2), yields(2, 3), derived(2, 4)}
	g := newColdGraph(nodes, edges)
	hot := g.hotSet()
	if len(hot) != 0 {
		t.Fatalf("nothing should be hot once all settled, got %v", hot)
	}
	structCold := g.structuralCold(hot)
	// 모두 R 라운드보다 전에 도장이 찍힘
	stamps := map[int64]*int64{1: ptr(1), 2: ptr(1), 3: ptr(1), 4: ptr(1)}
	elig := g.eligibleCold(structCold, stamps, 100, defaultColdParams())
	if len(elig) != 4 {
		t.Fatalf("all 4 nodes should be eligible cold, got %d", len(elig))
	}
	blocks := g.group(elig, defaultColdParams())
	if len(blocks) != 1 || len(blocks[0].Members) != 4 {
		t.Fatalf("expected one 4-member block, got %+v", blocks)
	}
}

// §2.3 디바운스: 방금 식은 노드(도장이 너무 최근)는 아직 대상이 아닙니다.
func TestDebounce_RecentlyCooledNotEligible(t *testing.T) {
	nodes := []cgNode{intent(1, "done"), fact(2)}
	edges := []cgEdge{yields(1, 2)}
	g := newColdGraph(nodes, edges)
	structCold := g.structuralCold(g.hotSet())
	stamps := map[int64]*int64{1: ptr(98), 2: ptr(98)} // 98라운드에 식음
	elig := g.eligibleCold(structCold, stamps, 100, defaultColdParams())
	if len(elig) != 0 {
		t.Fatalf("nodes cooled only 2 rounds ago (<R=6) must not be eligible, got %v", elig)
	}
	elig = g.eligibleCold(structCold, stamps, 104, defaultColdParams()) // 이제 6라운드
	if len(elig) != 2 {
		t.Fatalf("after R rounds both should be eligible, got %v", elig)
	}
}

// §3.1: 정착한 허브가 살아 있는 자손의 조상이라서만 hot 입니다(진짜
// grap.log 모양. 끝난/소진된 의도에 살아 있는 가지가 하나). 다른
// 자식은 납작한 죽은 잎입니다. 그 잎들은 hot 허브를 부모로 공유하고
// cold↔cold 간선은 없지만, 묶여야 합니다(하나로 남지 않음). 허브
// 자신이 살아 있으면(running) 규칙 1이 그 사실을 hot 으로 강제합니다. 그건 다른
// 경우입니다. 여기서 허브는 소진됐고 50→77 살아 있는 가지 때문에만 hot 입니다.
func TestGrouping_SharedParentRescuesFlatFanout(t *testing.T) {
	// 허브(50)는 소진, 살아 있는 자손 77 때문에 hot. 죽은 cold 사실 51..54.
	nodes := []cgNode{intent(50, "exhausted"), intent(77, "running")}
	edges := []cgEdge{derived(50, 77)}
	for id := int64(51); id <= 54; id++ {
		nodes = append(nodes, fact(id))
		edges = append(edges, yields(50, id))
	}
	g := newColdGraph(nodes, edges)
	hot := g.hotSet()
	if !hot[50] || !hot[77] {
		t.Fatalf("hub 50 (ancestor of live 77) and live 77 must be hot")
	}
	structCold := g.structuralCold(hot)
	stamps := map[int64]*int64{}
	for id := int64(51); id <= 54; id++ {
		stamps[id] = ptr(1)
	}
	elig := g.eligibleCold(structCold, stamps, 100, defaultColdParams())
	blocks := g.group(elig, defaultColdParams())
	// 규칙①만이면 51..54 가 4개의 하나로 남습니다. 규칙②가 1개로 묶습니다.
	if len(blocks) != 1 {
		t.Fatalf("expected 1 shared-parent block, got %d: %+v", len(blocks), blocks)
	}
	if len(blocks[0].Members) != 4 {
		t.Fatalf("block should hold all 4 dead leaves, got %v", blocks[0].Members)
	}
	// hot 허브는 앵커입니다. 구성원이 아닙니다.
	if len(blocks[0].Anchors) != 1 || blocks[0].Anchors[0] != 50 {
		t.Fatalf("hub 50 should be the sole anchor, got %v", blocks[0].Anchors)
	}
	for _, m := range blocks[0].Members {
		if m == 50 {
			t.Fatalf("hub 50 must not be a member")
		}
	}
}

// §3①: hot 부모 아래 cold 자식이 하나면, 접히지 않은 하나로 남습니다.
func TestGrouping_LoneColdChildStaysSingleton(t *testing.T) {
	nodes := []cgNode{intent(50, "running"), intent(77, "running"), fact(51)}
	edges := []cgEdge{derived(50, 77), yields(50, 51)}
	g := newColdGraph(nodes, edges)
	structCold := g.structuralCold(g.hotSet())
	elig := g.eligibleCold(structCold, map[int64]*int64{51: ptr(1)}, 100, defaultColdParams())
	blocks := g.group(elig, defaultColdParams())
	if len(blocks) != 0 {
		t.Fatalf("a single cold leaf must not fold, got %+v", blocks)
	}
}

// §5.3: 서명은 순서를 바꿔도 같고, 구성원의 content_version 이 오르면 바뀝니다.
func TestSignature_StableAndVersionSensitive(t *testing.T) {
	b := block{Members: []int64{12, 28, 41}, Anchors: []int64{50}}
	cv := map[int64]int{12: 0, 28: 0, 41: 0, 50: 0}
	s1 := blockSignature(b, cv)
	// 같은 구성원, 같은 버전 → 같은 서명
	if s1 != blockSignature(block{Members: []int64{12, 28, 41}, Anchors: []int64{50}}, cv) {
		t.Fatalf("signature must be deterministic")
	}
	// 구성원 버전을 올리면 서명이 바뀝니다
	cv2 := map[int64]int{12: 0, 28: 1, 41: 0, 50: 0}
	if s1 == blockSignature(b, cv2) {
		t.Fatalf("signature must change when a member content_version changes")
	}
	// 앵커 버전을 올리면 서명이 바뀝니다(앵커 요약이 본문에 영향을 줌)
	cv3 := map[int64]int{12: 0, 28: 0, 41: 0, 50: 1}
	if s1 == blockSignature(b, cv3) {
		t.Fatalf("signature must change when an anchor content_version changes")
	}
}

// computeStampOps: 처음 식을 때 도장을 찍고, 다시 찍지 않으며, 되살아나면 지웁니다.
func TestStampOps(t *testing.T) {
	structCold := map[int64]bool{1: true, 2: true}
	coldSince := map[int64]*int64{2: ptr(5), 3: ptr(4)} // 2는 이미 도장. 3은 도장이 있지만 되살아남
	ops := computeStampOps(structCold, coldSince, 10)
	got := map[int64]stampOp{}
	for _, o := range ops {
		got[o.ID] = o
	}
	if o, ok := got[1]; !ok || !o.Set || o.Round != 10 {
		t.Fatalf("node 1 should be stamped at round 10, got %+v", got[1])
	}
	if _, ok := got[2]; ok {
		t.Fatalf("node 2 already stamped, must not be re-stamped")
	}
	if o, ok := got[3]; !ok || o.Set {
		t.Fatalf("node 3 revived (not cold) → stamp must be cleared, got %+v", got[3])
	}
}
