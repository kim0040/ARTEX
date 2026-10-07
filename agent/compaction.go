package agent

// cold-digest §4/§5/§7: Compactor 는 순수한 알고리즘(coldgraph.go)을
// 저장소(db/digest.go)와 LLM 에 잇습니다. 두 방식으로 돕니다.
//
//	maintain — 싸고, 동기이며, 플래너 라운드마다 한 번: round_no 를 올리고,
//	           hot/cold 를 다시 계산하고, cold_since_round 를 찍거나 지웁니다(§2.3).
//	           플래너가 어차피 하는 장부입니다. LLM 을 부르지 않습니다.
//	minor/major — 백그라운드이고, 플래너의 바쁜 경로 밖입니다(§7). cold 노드를 묶고
//	           크기 2 이상 block 을 LLM 으로 digest 로 압축합니다. minor 는
//	           아직 안 덮인 cold 집합만 접습니다(층층이 덧붙임). major 는
//	           원본에서 묶음을 다시 만들고 조각을 합칩니다(§5.1/§5.2).
//	           서명이 안 바뀐 본문은 재사용합니다(§5.3).
//
// 동시성: 작업마다 압축은 한 번에 하나(뮤텍스), 실행 사이는 cooldown 이상,
// 커밋 때 살아 있는지 다시 봐서, 본문을 만드는 동안 다시 살아난 구성원은 뺍니다.
// digest 가 hot 노드를 덮지 않게 합니다.
// 초보: 탐색 그래프가 커지면, 식은 사실과 끝난 의도를 플래너가 읽기 쉬운 요약으로 여기서 접습니다.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// Compactor 는 여러 탐색의 cold 노드를 백그라운드에서 압축합니다.
type Compactor struct {
	prov     llm.Provider
	model    string
	params   coldParams
	n, m     int           // minor / major 문턱(§7 N=20, M=8)
	cooldown time.Duration // 작업마다 압축 사이 최소 간격(§7 60초)
	maxDur   time.Duration // 백그라운드 압축 한 번의 하드 상한

	mu      sync.Mutex
	running map[int64]bool
	lastRun map[int64]time.Time
}

// NewCompactor 는 압축기를 만듭니다. prov/model 은 §4 본문 LLM 호출에 씁니다
// (§4 대로, 에이전트가 도는 것과 같은 모델). nil Compactor 는 안전하게 아무 일도 하지 않습니다.
func NewCompactor(prov llm.Provider, model string) *Compactor {
	return &Compactor{
		prov:     prov,
		model:    model,
		params:   defaultColdParams(),
		n:        20,
		m:        8,
		cooldown: 60 * time.Second,
		maxDur:   5 * time.Minute,
		running:  map[int64]bool{},
		lastRun:  map[int64]time.Time{},
	}
}

// OnPlannerRound 는 플래너가 깨어날 때마다 부르는 유일한 입구입니다. 라운드를 올리고,
// cold 도장을 동기적으로 유지한 뒤, 문턱에 닿고 압축이 안 돌고 식는 중이 아니면
// 이 플래너 라운드보다 오래 사는 백그라운드 압축을 띄웁니다.
func (c *Compactor) OnPlannerRound(ctx context.Context, ts *db.ExplorationStore) {
	if c == nil || c.prov == nil || ts == nil {
		return
	}
	round, uncompressed, activeDigests, err := c.maintain(ts)
	if err != nil {
		log.Printf("[compaction] maintain exp=%d: %v", ts.ID(), err)
		return
	}
	needMinor := uncompressed >= c.n
	needMajor := activeDigests >= c.m
	if !needMinor && !needMajor {
		return
	}
	if !c.tryStart(ts.ID()) {
		return // already running, or within cooldown — 파생 상태는 결국 같아지고, 다음 턴에 다시 압축합니다
	}
	go func() {
		defer c.finish(ts.ID())
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.maxDur)
		defer cancel()
		// 압축은 맨 provider 호출입니다(compress 가 prov.Complete 를 직접 부름). agentcore
		// 세션 루프를 타지 않아 ctx 에 session id 가 없습니다. session-id 헤더로 프롬프트 캐시나 고정
		// 라우팅을 하는 게이트웨이(opencode zen 은 x-opencode-session 이 없으면 바로 400)는 그 헤더를 받지 못합니다.
		// 여기서 탐색마다 안정된 id 를 채웁니다. 같은 탐색의 모든 압축 요청이 그것을 공유해 헤더를 실을 수 있고,
		// llmrec 이 이 호출의 token 을 그 탐색에 귀속할 수 있습니다(이전에는 기록되지 않음).
		bg = transcript.WithSessionID(bg, fmt.Sprintf("exp%d-compactor", ts.ID()))
		if needMajor {
			c.major(bg, ts)
		} else {
			c.minor(bg, ts)
		}
	}()
	_ = round
}

// maintain 은 round_no 를 올리고, 그래프 전체의 hot/cold 를 다시 계산하고,
// cold_since_round 도장/지우기를 적용합니다(§2.3). 새 라운드와, 트리거를 움직이는
// 개수를 돌려줍니다. 아직 안 덮인 eligible-cold 노드 수(minor)와
// 활성 digest 수(major)입니다.
func (c *Compactor) maintain(ts *db.ExplorationStore) (round int64, uncompressed, activeDigests int, err error) {
	round, err = ts.BumpRound()
	if err != nil {
		return
	}
	g, _, err := loadColdGraph(ts)
	if err != nil {
		return
	}
	stamps, err := ts.ColdStamps()
	if err != nil {
		return
	}
	hot := g.hotSet()
	structCold := g.structuralCold(hot)
	ops := computeStampOps(structCold, stamps, round)
	if err = ts.ApplyStampOps(toDBStampOps(ops)); err != nil {
		return
	}
	applyStampsInPlace(stamps, ops)
	elig := g.eligibleCold(structCold, stamps, round, c.params)
	covered, err := ts.CoveredMembers()
	if err != nil {
		return
	}
	for id := range elig {
		if _, ok := covered[id]; !ok {
			uncompressed++
		}
	}
	ad, err := ts.ActiveDigests()
	if err != nil {
		return
	}
	activeDigests = len(ad)
	return
}

// minor 는 아직 안 덮인 eligible-cold 집합을 새 digest 조각으로 접습니다
// (층층이 덧붙임, §5). 기존 digest 는 건드리지 않습니다.
func (c *Compactor) minor(ctx context.Context, ts *db.ExplorationStore) {
	round, err := ts.RoundNo()
	if err != nil {
		return
	}
	g, nodeByID, err := loadColdGraph(ts)
	if err != nil {
		return
	}
	stamps, err := ts.ColdStamps()
	if err != nil {
		return
	}
	cvers, err := ts.ContentVersions()
	if err != nil {
		return
	}
	covered, err := ts.CoveredMembers()
	if err != nil {
		return
	}
	hot := g.hotSet()
	elig := g.eligibleCold(g.structuralCold(hot), stamps, round, c.params)
	uncompressed := map[int64]bool{}
	for id := range elig {
		if _, ok := covered[id]; !ok {
			uncompressed[id] = true
		}
	}
	blocks := g.group(uncompressed, c.params)
	if len(blocks) == 0 {
		return // 이 묶음에는 연결됐거나 부모를 공유하는 크기 2 이상 block 이 없습니다. 접을 것이 없습니다(§7)
	}
	for _, b := range blocks {
		c.foldBlock(ctx, ts, g, b, nodeByID, cvers, c.generationFor(b, nil))
	}
	// minor 가 조각 수를 M 넘게 밀었을 수 있습니다. 같은 실행에서 합칩니다.
	if ad, e := ts.ActiveDigests(); e == nil && len(ad) >= c.m {
		c.major(ctx, ts)
	}
}

// major 는 모든 eligible-cold 노드에 대해 원본에서 묶음을 다시 만듭니다
// (§5.1 원본으로 돌아가 다시 압축). 그다음 서명으로 활성 digest 와 맞춥니다.
// 안 바뀐 block 은 digest 를 유지합니다(LLM 없음). 낡은 digest 는 밀려나고,
// 새것이거나 바뀐 block 은 새로 압축합니다. 한 방향의 층층이 쌓인 조각이 합쳐지고,
// "나중에 연결됨" block 이 하나로 모이는 곳입니다(§5.2).
func (c *Compactor) major(ctx context.Context, ts *db.ExplorationStore) {
	round, err := ts.RoundNo()
	if err != nil {
		return
	}
	g, nodeByID, err := loadColdGraph(ts)
	if err != nil {
		return
	}
	stamps, err := ts.ColdStamps()
	if err != nil {
		return
	}
	cvers, err := ts.ContentVersions()
	if err != nil {
		return
	}
	active, err := ts.ActiveDigests()
	if err != nil {
		return
	}
	hot := g.hotSet()
	elig := g.eligibleCold(g.structuralCold(hot), stamps, round, c.params)
	blocks := g.group(elig, c.params)

	bySig := map[string]*db.Node{}
	for _, d := range active {
		sig, _ := digestSigGen(d)
		bySig[sig] = d
	}
	desired := map[string]bool{}
	var toCreate []block
	for _, b := range blocks {
		sig := blockSignature(b, cvers)
		desired[sig] = true
		if _, ok := bySig[sig]; ok {
			continue // 안 바뀜 → 기존 digest 를 재사용하고 LLM 을 건너뜁니다(§5.3)
		}
		toCreate = append(toCreate, b)
	}
	// 낡은 digest 를 먼저 밀어 냅니다(covers 간선을 원자적으로 끊음). 구성원이
	// 옛 digest 와 새 digest 에 동시에 덮이지 않게 합니다(§5.1 구성원 하나에 digest 하나).
	var stale []int64
	for _, d := range active {
		sig, _ := digestSigGen(d)
		if !desired[sig] {
			stale = append(stale, d.ID)
		}
	}
	if err := ts.SupersedeDigests(stale); err != nil {
		log.Printf("[compaction] supersede exp=%d: %v", ts.ID(), err)
	}
	for _, b := range toCreate {
		c.foldBlock(ctx, ts, g, b, nodeByID, cvers, c.generationFor(b, active))
	}
}

// foldBlock 은 block 하나를 압축하고 digest 를 씁니다. 커밋 때 살아 있는지를
// 다시 봅니다(동시성): 묶기와 쓰기 사이에 그래프가 바뀌었을 수 있으므로,
// 그 사이 hot 이 된(다시 살아난) 구성원은 covers 집합에서 뺍니다.
// block 이 K 밑으로 녹으면 건너뜁니다.
func (c *Compactor) foldBlock(ctx context.Context, ts *db.ExplorationStore, g *coldGraph, b block, nodeByID map[int64]*db.Node, cvers map[int64]int, generation int) {
	body, err := c.compress(ctx, g, b, nodeByID)
	if err != nil {
		log.Printf("[compaction] compress exp=%d block=%v: %v", ts.ID(), b.Members, err)
		return
	}
	// 새 상태를 다시 읽고, 압축하는 동안 다시 살아난 구성원은 뺍니다.
	fresh, _, err := loadColdGraph(ts)
	if err != nil {
		return
	}
	freshHot := fresh.hotSet()
	members := make([]int64, 0, len(b.Members))
	for _, mID := range b.Members {
		if !freshHot[mID] {
			members = append(members, mID)
		}
	}
	if len(members) < c.params.K {
		return // block 이 밑에서 다시 살아났습니다. 그 노드는 hot 으로 두고 접지 않습니다
	}
	final := block{Members: members, Anchors: b.Anchors}
	payload := digestPayload(body, final, nodeByID, generation, blockSignature(final, cvers))
	if _, err := ts.AddDigest(payload, members); err != nil {
		log.Printf("[compaction] add digest exp=%d: %v", ts.ID(), err)
	}
}

// generationFor 는 digest 의 다시 압축한 세대(§1)를 계산합니다. 새 접기는 1 입니다.
// major 병합이면, 이 block 의 구성원과 겹치는 활성 digest 의 generation 최댓값에 1 을 더합니다.
func (c *Compactor) generationFor(b block, active []*db.Node) int {
	if len(active) == 0 {
		return 1
	}
	memberSet := make(map[int64]bool, len(b.Members))
	for _, m := range b.Members {
		memberSet[m] = true
	}
	best := 0
	for _, d := range active {
		_, gen := digestSigGen(d)
		for _, m := range digestMemberIDs(d) {
			if memberSet[m] {
				if gen > best {
					best = gen
				}
				break
			}
		}
	}
	return best + 1
}

// tryStart 는 작업마다의 압축 잠금을 잡습니다. cooldown 을 지킵니다.
func (c *Compactor) tryStart(expID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[expID] {
		return false
	}
	if t, ok := c.lastRun[expID]; ok && time.Since(t) < c.cooldown {
		return false
	}
	c.running[expID] = true
	return true
}

func (c *Compactor) finish(expID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running[expID] = false
	c.lastRun[expID] = time.Now()
}

// --- 도우미: db ↔ coldgraph ---

// loadColdGraph 는 탐색의 노드와 간선을 읽어 cold 그래프 보기와
// id→노드 색인을 만듭니다(압축 중 요약/payload 용).
func loadColdGraph(ts *db.ExplorationStore) (*coldGraph, map[int64]*db.Node, error) {
	// 압축은 기본 행 상한이 아니라 그래프 전체를 봐야 합니다. 한계를 아주 크게 넣어
	// 실제 작업 크기에서는 LIMIT 이 사실상 없게 합니다.
	const allRows = 1 << 30
	nodes, err := ts.Nodes(allRows)
	if err != nil {
		return nil, nil, err
	}
	edges, err := ts.Edges(allRows)
	if err != nil {
		return nil, nil, err
	}
	cgNodes := make([]cgNode, 0, len(nodes))
	byID := make(map[int64]*db.Node, len(nodes))
	for _, n := range nodes {
		cgNodes = append(cgNodes, cgNode{ID: n.ID, Kind: n.Kind, State: n.State})
		byID[n.ID] = n
	}
	cgEdges := make([]cgEdge, 0, len(edges))
	for _, e := range edges {
		cgEdges = append(cgEdges, cgEdge{From: e.From, Rel: e.Rel, To: e.To})
	}
	return newColdGraph(cgNodes, cgEdges), byID, nil
}

func toDBStampOps(ops []stampOp) []db.StampOp {
	out := make([]db.StampOp, len(ops))
	for i, o := range ops {
		out[i] = db.StampOp{ID: o.ID, Set: o.Set, Round: o.Round}
	}
	return out
}

// applyStampsInPlace 는 방금 적용한 연산을 메모리 도장 맵에 접어,
// 다시 읽지 않고 바로 자격 여부를 계산하게 합니다.
func applyStampsInPlace(stamps map[int64]*int64, ops []stampOp) {
	for _, o := range ops {
		if o.Set {
			r := o.Round
			stamps[o.ID] = &r
		} else {
			stamps[o.ID] = nil
		}
	}
}

// --- 도우미: digest payload ---

// digestPayload 는 digest 노드 payload 를 만듭니다(cold-digest §1). 본문,
// 종류별로 나눈 구성원 id(복원 캐시. 진실의 원천은 covers 간선),
// 앵커 id, 세대, 변경 감지 서명입니다.
func digestPayload(body string, b block, nodeByID map[int64]*db.Node, generation int, signature string) map[string]any {
	var facts, intents []int64
	for _, m := range b.Members {
		if n := nodeByID[m]; n != nil && n.Kind == db.KindIntent {
			intents = append(intents, m)
		} else {
			facts = append(facts, m)
		}
	}
	return map[string]any{
		"body":       body,
		"member_ids": map[string]any{"facts": facts, "intents": intents},
		"anchor_ids": b.Anchors,
		"generation": generation,
		"signature":  signature,
	}
}

func digestSigGen(n *db.Node) (string, int) {
	var p struct {
		Signature  string `json:"signature"`
		Generation int    `json:"generation"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return p.Signature, p.Generation
}

func digestMemberIDs(n *db.Node) []int64 {
	var p struct {
		MemberIDs struct {
			Facts   []int64 `json:"facts"`
			Intents []int64 `json:"intents"`
		} `json:"member_ids"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return append(append([]int64{}, p.MemberIDs.Facts...), p.MemberIDs.Intents...)
}

// --- 도우미: 압축 입력과 LLM (§4) ---

func nodeSummary(n *db.Node) string {
	if n == nil {
		return ""
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok {
			return s
		}
		if t, ok := p["text"].(string); ok {
			return t
		}
	}
	return ""
}

func nodeConfidence(n *db.Node) string {
	if n == nil {
		return ""
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if c, ok := p["confidence"].(string); ok {
			return c
		}
	}
	return ""
}

// buildCompressionInput 은 §4 프롬프트용 연결된 부분 그래프를 그립니다.
// 구성원 노드(요약 + id + 종류 + 상태 + 확신), 구성원 사이의 혈통 간선,
// 그리고 §3.1 공통 부모 묶음이면 앵커 부모를 맥락으로 넣습니다("공통 부모 #p").
// 앵커는 구성원이 아닙니다.
func buildCompressionInput(g *coldGraph, b block, nodeByID map[int64]*db.Node) string {
	memberSet := make(map[int64]bool, len(b.Members))
	for _, m := range b.Members {
		memberSet[m] = true
	}
	var sb strings.Builder
	sb.WriteString("【成员节点（要压缩的）】：\n") // han-allow 업스트림 프롬프트·픽스처
	for _, m := range b.Members {
		n := nodeByID[m]
		kind := "fact"
		if n != nil && n.Kind == db.KindIntent {
			kind = "intent"
		}
		state := ""
		if n != nil {
			state = n.State
		}
		line := fmt.Sprintf("- #%d [%s/%s] %s", m, kind, state, nodeSummary(n))
		if conf := nodeConfidence(n); conf != "" {
			line += fmt.Sprintf(" (confidence=%s)", conf)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	// 구성원 사이의 내부 간선
	var edgeLines []string
	for _, m := range b.Members {
		for _, to := range g.children[m] {
			if memberSet[to] {
				edgeLines = append(edgeLines, fmt.Sprintf("- #%d 产出/派生→ #%d", m, to)) // han-allow 업스트림 프롬프트·픽스처
			}
		}
	}
	if len(edgeLines) > 0 {
		sb.WriteString("\n【成员之间的血缘边（父→子）】：\n") // han-allow 업스트림 프롬프트·픽스처
		sort.Strings(edgeLines)
		sb.WriteString(strings.Join(edgeLines, "\n"))
		sb.WriteByte('\n')
	}
	if len(b.Anchors) > 0 {
		sb.WriteString("\n【共同父 / 上下文锚（不是成员，只用于理解这些结果从哪个意图探出）】：\n") // han-allow 업스트림 프롬프트·픽스처
		for _, a := range b.Anchors {
			n := nodeByID[a]
			state := ""
			if n != nil {
				state = n.State
			}
			fmt.Fprintf(&sb, "- #%d [%s] %s\n", a, state, nodeSummary(n))
		}
	}
	return sb.String()
}

// compress 는 block 하나에 §4 본문 LLM 호출을 돌립니다. 에이전트가 도는 것과 같은 모델을 씁니다.
// 생각은 끕니다(순수한 요약 단계).
func (c *Compactor) compress(ctx context.Context, g *coldGraph, b block, nodeByID map[int64]*db.Node) (string, error) {
	req := llm.CompletionRequest{
		System:    []string{compressionSystemPrompt},
		Messages:  []llm.Message{llm.UserText(buildCompressionInput(g, b, nodeByID))},
		MaxTokens: 1500,
		Thinking:  "disabled",
	}
	msg, _, _, err := c.prov.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(msg.Text())
	if body == "" {
		return "", fmt.Errorf("empty body from model")
	}
	return body, nil
}

// compressionSystemPrompt 는 §4 본문 프롬프트입니다.
const compressionSystemPrompt = `你在压缩一组【彼此关联】的探索节点，产出一段综合结论(body)，供规划者快速掌握"这一片已经探明了什么"。

输入是一个连通子图：
- 节点：每条是一个意图或事实的 summary（一句话），带 id、类型(intent/fact)、state、confidence(若有)。
- 关系：节点之间的血缘边（A 派生自 B / A 产出 B），说明它们如何串联。
- 若节点间没有直接血缘边、但同属一个上游意图（会另给出该上游意图作为"共同父 #p"），则按"这个意图（#p）探到了什么"来综合它们——共同父只是上下文锚、不是要压缩的成员。

据此写一段 body：
1. 综合、不罗列：顺着关系把因果串起来（哪个事实催生哪个意图、哪条意图产出了哪个结论），讲成"这一片探索得出了什么"，不要把每条 summary 抄一遍。
2. 保留区分度：彼此不同的结论分别说清，别揉成一句笼统的话。
3. 保留证据强度：带 confidence 的结论标出 observed / inferred；inferred 的否定/存疑结论要点明它只是推断、可复核，别写成定论。
4. 带上 id：每条结论后标注来源节点 id（如"…（#12,#28）"），让规划者能按 id 还原原节点。
5. 正向陈述、只写输入里有的：不脑补、不引入输入中没有的判断。
6. 长度随内容自适应：结论少就短，多且互不相同就写够——但整体显著短于所有输入 summary 的总和。

只输出 body 正文本身。`
