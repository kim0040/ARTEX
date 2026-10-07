package agent

// cold-digest §2/§3: 식은 노드를 접는 순수한 그래프 알고리즘입니다.
//
// 이 파일은 일부러 DB 와 LLM 에 의존하지 않습니다. hot/cold 판정, 연결로 묶기(§3),
// 같은 부모의 외톨이 구출(§3.1)을 단위 테스트로 따로 볼 수 있습니다. 호출자는
// db.Node/db.Edge 를 가벼운 cgNode/cgEdge 로 옮기고, 노드마다의 장부
// (cold_since 도장, 내용 버전)를 같이 넣습니다.
//
// 간선 방향 약속(db 와 agent/tools.go 의 graphOverviewData 와 같음):
// 모든 간선 From→To 는 From 이 부모/상류, To 가 자식/하류입니다. 모든 관계에
// 적용됩니다(yields: 의도→사실, derived_from/spawns: 부모→자식).
// 그래서 "하류"는 From→To 를 따라갑니다.
// 초보: 탐색 그래프에서 오래 안 쓰인 사실과 끝난 의도를 묶어, 플래너 개요를 짧게 만듭니다.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/Autumn-27/artex/db"
)

// cgNode 는 cold 그래프 알고리즘이 필요로 하는 최소 노드 모양입니다.
type cgNode struct {
	ID    int64
	Kind  string
	State string
}

// cgEdge 는 탐색 간선 하나입니다(From = 부모/상류, To = 자식/하류).
type cgEdge struct {
	From int64
	Rel  string
	To   int64
}

// coldParams 는 조절할 수 있는 문턱입니다(cold-digest §7).
type coldParams struct {
	R int // debounce: 노드가 플래너 라운드 R번 이상 계속 쉬어야 합니다(§7 R=6)
	K int // 접을 최소 덩어리 크기. K=2 는 변질된 외톨이만 건너뜁니다(§7 K=2)
}

func defaultColdParams() coldParams { return coldParams{R: 6, K: 2} }

// coldGraph 는 진짜 탐색 간선 위의 메모리 인접 보기입니다. 파생 보기 층
// (kind=digest 노드, rel=covers 간선)은 만들 때 빼서, 인과적 도달이나
// 묶기를 비틀지 못하게 합니다(§2/§3).
type coldGraph struct {
	nodes    map[int64]cgNode
	children map[int64][]int64 // From → [To]   (하류)
	parents  map[int64][]int64 // To   → [From] (상류)
}

func newColdGraph(nodes []cgNode, edges []cgEdge) *coldGraph {
	g := &coldGraph{
		nodes:    make(map[int64]cgNode, len(nodes)),
		children: map[int64][]int64{},
		parents:  map[int64][]int64{},
	}
	for _, n := range nodes {
		g.nodes[n.ID] = n
	}
	for _, e := range edges {
		if e.Rel == db.RelCovers { // 파생 보기 층입니다. 탐색의 인과가 아닙니다(§2/§3)
			continue
		}
		if _, ok := g.nodes[e.From]; !ok {
			continue
		}
		if _, ok := g.nodes[e.To]; !ok {
			continue
		}
		g.children[e.From] = append(g.children[e.From], e.To)
		g.parents[e.To] = append(g.parents[e.To], e.From)
	}
	return g
}

// isLiveIntent 는 노드가 아직 안 끝난 의도인지 봅니다. 그 조상을 hot 으로
// 붙잡아 두는 프론티어입니다. paused 도 살아 있는 것으로 봅니다(다시 돌 수 있음).
// 끝난 상태 = done/blocked/exhausted/stopped.
func isLiveIntent(n cgNode) bool {
	if n.Kind != db.KindIntent {
		return false
	}
	switch n.State {
	case "open", "running", "paused":
		return true
	}
	return false
}

// foldableKind 는 그 종류가 아예 접힐 수 있는지 봅니다(§2: 사실과 끝난 의도만.
// 발견/목표/힌트/begin/digest 는 절대 접지 않습니다).
func foldableKind(k string) bool { return k == db.KindIntent || k == db.KindFact }

// hotSet 은 hot 노드를 계산합니다(§2 규칙 1+2). 노드가 hot 인 경우는, 하류로
// 가서 살아 있는 의도에 닿을 때(살아 있는 의도의 조상), 또는 스스로 살아 있는
// 의도일 때, 또는 살아 있는 의도의 직접 자식일 때입니다(규칙 1: 열리거나
// 돌고 있는 의도가 방금 만든 사실은 hot 으로 남음). 나머지는 cold 후보입니다.
// "살아 있는 가지가 하나라도 있으면 사슬 전체가 hot"은 조상 표시에서 나옵니다.
// 깊은 사슬과 순환을 견디려고 재귀 없이 반복합니다.
func (g *coldGraph) hotSet() map[int64]bool {
	hot := map[int64]bool{}
	var stack []int64
	for _, n := range g.nodes {
		if isLiveIntent(n) {
			stack = append(stack, n.ID)
		}
	}
	// 살아 있는 의도마다 상류로 걸어, 모든 조상을 hot 으로 표시합니다.
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if hot[id] {
			continue
		}
		hot[id] = true
		stack = append(stack, g.parents[id]...)
	}
	// 살아 있는 의도의 직접 자식(방금 만든 사실)은 hot 으로 남습니다(규칙 1).
	for _, n := range g.nodes {
		if isLiveIntent(n) {
			for _, c := range g.children[n.ID] {
				hot[c] = true
			}
		}
	}
	return hot
}

// structuralCold 는 지금 hot 이 아닌, 접힐 수 있는 노드 집합입니다.
// 끝났고, 살아 있는 의도까지 피가 닿지 않습니다(§2 규칙 1+2). ≥R debounce 전입니다.
func (g *coldGraph) structuralCold(hot map[int64]bool) map[int64]bool {
	cold := map[int64]bool{}
	for id, n := range g.nodes {
		if foldableKind(n.Kind) && !hot[id] {
			cold[id] = true
		}
	}
	return cold
}

// stampOp 은 cold_since_round 장부 변경 하나입니다(§2.3). Set=true 는 노드가
// cold 가 된 라운드를 찍습니다. Set=false 는 도장을 지웁니다(노드가 다시 살아났거나
// 다시 hot 이 됨).
type stampOp struct {
	ID    int64
	Set   bool
	Round int64
}

// computeStampOps 는 이번 라운드의 cold_since_round 갱신을 만듭니다. 노드가
// 처음으로 cold 가 된 라운드를 찍고(빈 값→round_no), 더 이상 cold 가 아니면
// 도장을 지웁니다. 이미 찍힌 cold 노드를 다시 찍지 않습니다. 그래야
// "얼마나 오래 cold 였는지"가 유지됩니다(§2.3: 마지막으로 hot 이었던 라운드가 아니라,
// cold 로 바뀐 라운드를 잽니다). coldSince 는 노드 id → 도장입니다(nil = 도장 없음 / hot).
func computeStampOps(structCold map[int64]bool, coldSince map[int64]*int64, roundNo int64) []stampOp {
	var ops []stampOp
	seen := map[int64]bool{}
	for id := range structCold {
		seen[id] = true
		if coldSince[id] == nil {
			ops = append(ops, stampOp{ID: id, Set: true, Round: roundNo})
		}
	}
	// 도장은 있는데 더 이상 cold 가 아닌 노드(다시 살아남/hot)의 도장을 지웁니다.
	for id, cs := range coldSince {
		if cs != nil && !seen[id] {
			ops = append(ops, stampOp{ID: id, Set: false})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops
}

// eligibleCold 는 structuralCold 를, R 라운드 이상 계속 cold 인 노드로 좁힙니다
// (§2.3 / §3②). 도장이 없거나 아직 R 라운드가 안 된 노드는 hot 구역에 더 둡니다
// (보수적으로 기울입니다).
func (g *coldGraph) eligibleCold(structCold map[int64]bool, coldSince map[int64]*int64, roundNo int64, p coldParams) map[int64]bool {
	out := map[int64]bool{}
	for id := range structCold {
		if cs := coldSince[id]; cs != nil && roundNo-*cs >= int64(p.R) {
			out[id] = true
		}
	}
	return out
}

// block 은 하나의 digest 로 접을 cold 노드 묶음과, 그 바깥의
// 부모 노드입니다(§3.1 "부모는 앵커이지 구성원이 아님"). 앵커는
// 압축기에 맥락으로만 들어가고, 구성원이 되거나 covers 간선을 받지 않습니다.
type block struct {
	Members []int64 // 정렬됨. 이 digest 가 덮는 노드
	Anchors []int64 // 정렬됨. 바깥(구성원이 아닌) 부모. 맥락만
}

// group 은 `set` 을 접을 수 있는 block 으로 나눕니다(§3 + §3.1). union-find 를 두 번 돕니다.
//
//	규칙 ①  진짜 탐색 간선으로 이어진 cold 노드를 연결합니다(§3).
//	규칙 ②  공통의 직접 부모를 가진, 남은 외톨이들을 연결합니다
//	        (§3.1 — "hot 허브 + 납작한 죽은 잎" 부채꼴을 구함). 이미 만들어진
//	        크기 2 이상 block 은 건드리지 않습니다.
//
// 크기가 K 이상인 성분만 남습니다(§3① 은 변질된 외톨이를 건너뜁니다).
func (g *coldGraph) group(set map[int64]bool, p coldParams) []block {
	uf := newUnionFind(set)
	// 규칙 ①: 진짜 cold↔cold 간선.
	for from := range set {
		for _, to := range g.children[from] {
			if set[to] {
				uf.union(from, to)
			}
		}
	}
	// 규칙 ②: 공통 부모를 가진, 남은 외톨이.
	comps := uf.components()
	byParent := map[int64][]int64{}
	for _, ids := range comps {
		if len(ids) != 1 {
			continue // 외톨이만 구합니다. 크기 2 이상 block 은 다시 섞지 않습니다
		}
		s := ids[0]
		for _, par := range g.parents[s] {
			if g.nodes[par].Kind == db.KindDigest { // 앵커는 진짜 노드여야 합니다. digest 가 아닙니다
				continue
			}
			byParent[par] = append(byParent[par], s)
		}
	}
	for _, sibs := range byParent {
		if len(sibs) < 2 {
			continue // 부모 아래 cold 자식이 하나면 진짜 외톨이로 남습니다(§3①)
		}
		for i := 1; i < len(sibs); i++ {
			uf.union(sibs[0], sibs[i])
		}
	}
	// 살아남은 성분을 block 으로 냅니다. 각각 바깥 부모 앵커를 답니다.
	comps = uf.components()
	var blocks []block
	for _, ids := range comps {
		if len(ids) < p.K {
			continue
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		memberSet := make(map[int64]bool, len(ids))
		for _, m := range ids {
			memberSet[m] = true
		}
		anchorSet := map[int64]bool{}
		for _, m := range ids {
			for _, par := range g.parents[m] {
				if memberSet[par] {
					continue
				}
				pn, ok := g.nodes[par]
				if !ok || pn.Kind == db.KindDigest {
					continue
				}
				anchorSet[par] = true
			}
		}
		anchors := make([]int64, 0, len(anchorSet))
		for a := range anchorSet {
			anchors = append(anchors, a)
		}
		sort.Slice(anchors, func(i, j int) bool { return anchors[i] < anchors[j] })
		blocks = append(blocks, block{Members: ids, Anchors: anchors})
	}
	// 순서를 고정합니다. 가장 작은 구성원 id 순입니다.
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Members[0] < blocks[j].Members[0] })
	return blocks
}

// blockSignature 는 변경 감지 키입니다(§5.3). 정렬된 구성원 id 와 각 구성원의
// content_version, 그리고 앵커 id 와 버전을 해시한 값입니다(앵커의 요약/상태가
// 바뀌어도 캐시된 본문이 무효가 됩니다). 다시 압축할 때 block 이 기존 활성 digest 의
// 서명과 같으면 저장된 본문을 재사용하고 LLM 을 아예 건너뜁니다.
func blockSignature(b block, contentVer map[int64]int) string {
	h := sha256.New()
	for _, m := range b.Members {
		fmt.Fprintf(h, "m:%d:%d;", m, contentVer[m])
	}
	for _, a := range b.Anchors {
		fmt.Fprintf(h, "a:%d:%d;", a, contentVer[a])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- union-find (서로소 집합) ---

type unionFind struct{ parent map[int64]int64 }

func newUnionFind(set map[int64]bool) *unionFind {
	uf := &unionFind{parent: make(map[int64]int64, len(set))}
	for id := range set {
		uf.parent[id] = id
	}
	return uf
}

func (u *unionFind) find(x int64) int64 {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int64) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}

func (u *unionFind) components() map[int64][]int64 {
	out := map[int64][]int64{}
	for id := range u.parent {
		r := u.find(id)
		out[r] = append(out[r], id)
	}
	return out
}
