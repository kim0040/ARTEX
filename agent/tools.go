package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 상류: 이 의도가 어느 노드에서 나왔는지(사실 여러 개가 의도 하나를 함께 만들 수 있음)
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 하류: 이 의도가 만든 사실/발견
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "사람이 목표 N개를 추가했습니다: …" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "사람이 전략 힌트 N개를 추가했습니다: …"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "사실1 자산25 발견0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("사실%d 자산%d 발견%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own 범위 정의 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/사용자 에이전트/곁길 질문). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " 에는 작업 맥락(탐색 그래프)이 필요합니다. 이 에이전트는 작업 안에서 돌고 있지 않아 탐색 그래프를 읽을 수 없고, 이 도구를 쓸 수 없습니다. 작업 안에서 쓰거나, task_id 가 있는 작업 간 조회 도구(get_task_node_detail / list_task_findings / get_task_graph 등)를 쓰세요."), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(探索链路图)探索态势蒸馏摘要：资产计数、无接口的站点、frontier、发现、hints(人类/主 agent 的战略提示，生成意图时须纳入)。规划时先调它。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 사람/메인 에이전트가 add_hint 로 그래프에 건 전략 힌트. folded in so the
	// planner reads them every round when generating intents (안 그러면 쓰기만 하고 읽지 않음).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC —— 우선순위가 가장 높은 앞 N개. 실제 총수는 frontier_open
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 최신이 앞(all 은 id 내림차순). 접힌 것은 뺐고, 출력할 때 최신 N개만 자름
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total: 끝난 의도(done/blocked/exhausted) 총수. recent_done_intents 와
	// 이름을 나란히 둡니다. 후자는 그 최신 창의 잘린 뷰입니다. 두 키를 나란히 두면 "보이는 것은 N/총수"가 스스로 설명되어,
	// 플래너가 중복을 없앨 때 "안 보임"을 "안 보냄"으로 착각하지 않습니다. 프롬프트에서 따로 설명할 필요가 없습니다.
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open: 열린 의도의 실제 총수(open_intents 는 그중 우선순위가 가장 높은 앞 N개의 잘린 뷰).
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 확인된 발견 총수(목표 판정이 이것을 봄). 상세는 finding_list(최신 한 창)
	out["facts"] = len(factNodes)                      // 탐색 사실/결론 수(부정 결론 포함)
	// findings 는 작업에서 가장 값진 산출물입니다. 개요에 최신 한 창을 붙입니다(10개 이하, vulnNodes 는 id 내림차순이라 최신이 앞).
	// 플래너가 매 턴 목표를 판단할 때 최근 확인된 발견을 한눈에 보게 합니다. 전체나 더 이전 것은 list_findings 로 가져옵니다.
	// 각 항목은 {id, summary, from_intent?} 만 둡니다. from_intent 는 이 발견을 만든 의도입니다.
	// evidence/assets/vulnclass/severity/state 등은 list_findings / node_detail(id) 로 가져올 수 있습니다.
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 이 발견을 어느 의도가 만들었는지
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts: 접히지 않은 사실 중 최신 한 창(N개 이하, factNodes 는 id 내림차순이라 최신이 앞). 이미
	// digest 에 접혔고 아직 차가운 것(hidden)은 cold_digests 로 가고 여기서 반복하지 않습니다. 각 항목은 {id, summary, from_intent?,
	// confidence?}. evidence 등 상세는 node_detail(id). 더 이전 것은 list_facts 로 넘깁니다.
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // 이미 digest 에 접혔고 아직 차가움 —— cold_digests 참고
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 이 사실을 어느 의도가 만들었는지
		}
		// confidence 를 개요에 넣습니다. 플래너가 어느 결론이 inferred 뿐인지 한눈에 보게 합니다(특히 부정 결론을
		// 확정으로 보지 않게). evidence 는 길어서 node_detail(id) 에 남깁니다.
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents: 접히지 않은 끝난 의도 중 최신 한 창(N개 이하, recentDone 은 id 내림차순).
	// 더 이전 것은 done_intents_total 개수 + node_detail(id) 로 봅니다.
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 접힌 차가운 구역의 digest body. 최신 구성원 시간 내림차순으로 앞 N개. 잘린 더 오래된 digest 는
	// id 만 줍니다(expand_digest 로 펼칠 수 있음). 차가운 구역의 유일한 출구가 끝없이 길어지지 않게 합니다.
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— body 를 직접 읽음 (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 잘린 더 오래된 digest 의 id. expand_digest(id) 로 펼침
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage: 대략적인 자산 테스트 커버리지 참고. 범위(task_scope) 안의 자산 중 fact 가 닿은
	// 비율 + by_type(유형별 총수/테스트됨). 안 본 구체 자산은 에이전트가 필요할 때 list_untested_assets 를 호출해 스스로 판단합니다. 작업 맥락에서만 있습니다.
	// 자산 커버리지를 끄면(coverageDisabled) host_count(대상 호스트 수를 알게 하는 정보)만 남기고,
	// denominator/tested/pct/by_type/note 같은 커버리지 측정은 버립니다. 맥락을 더럽히지 않고,
	// 이미 숨긴 add_task_scope/list_untested_assets 를 유도하지도 않습니다.
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage资产测试覆盖度（包括接口等各种相关资产），粗略估计、仅供参考：包含当前任务与直接关联任务的 scope、事实锚点；关联 scope 只读。容器型资产/大量枚举会让它偏低，勿据此认为已测完；可用 add_task_scope 增补本任务范围、list_untested_assets 看未测资产【通常不调用list_untested_assets，按照任务推进即可】；" // han-allow 업스트림 프롬프트·픽스처
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "范围未锚定" // han-allow 업스트림 프롬프트·픽스처
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// 호스트 총수만 줍니다. host 목록을 graph_overview 에 펼치지 않습니다(범위가 큰 작업에서는 매 턴
				// 반복해서 싣는 긴 문자열이고, 계획 결정에 가치가 적습니다). 구체 호스트는 필요할 때 list_assets 로 조회합니다.
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 잘린 더 오래된 digest 의 id. expand_digest(id) 로 펼침
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "列本任务及直接关联任务的【确认漏洞】(紧凑：id+task_id+intent_id+vulnclass+severity+摘要+状态)。关联任务条目带 source_task_id/inherited=true 且只读。这里只含漏洞；普通探索事实用 list_facts，详情用 node_detail(id)。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> 그것을 만든 intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "分页列本任务及直接关联任务的【探索事实/结论】，最新在前(紧凑：id+摘要+状态，摘要过长会截断，全文用 node_detail(id))。参数均可选：limit(默认 20，上限 100)、before(游标，传上一页返回的 next_before 取更旧的一页；省略/0=最新一页)、q(按摘要关键词过滤)。返回 {facts, total, has_more, next_before}：total 是过滤后的总数，has_more=true 时用 next_before 继续翻页。关联任务条目带 source_task_id/inherited=true 且只读。漏洞看 list_findings。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"limit":  intp("返回条数，默认 20，上限 100"),                  // han-allow 업스트림 프롬프트·픽스처
			"before": intp("分页游标：只返回 id 小于该值的更旧事实；省略或 0 = 最新一页"), // han-allow 업스트림 프롬프트·픽스처
			"q":      str("按事实摘要关键词过滤（不区分大小写）；省略 = 不过滤"),         // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // 이것을 돌려주면 다음 페이지(더 오래된 것)를 가져옵니다
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "按 id 取本任务或直接关联任务的【探索图节点】完整内容。继承节点带 source_task_id/inherited=true 且只读。仅限 list_facts/list_findings/graph_overview 返回的探索节点 id；资产请用 list_assets/asset_neighbors。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{"id": idp("探索图节点 id(非资产 id)")}, "id"), // han-allow 업스트림 프롬프트·픽스처
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id는 필수입니다"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("탐색 노드 %d 을(를) 찾지 못했습니다. 자산을 보려면 list_assets / asset_neighbors 를 쓰세요. 자산과 탐색 노드는 id 공간이 달라, 자산 id 를 node_detail 에 넘길 수 없습니다.", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem 은 add_intent 일괄/한 건의 탐색 방향 하나입니다.
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent 는 의도 노드 하나를 만들고 상류 혈통을 연결한 뒤 id 를 돌려줍니다.
// 제약: 의도는 이미 확인된 지식에만 앵커할 수 있습니다. 각 parent_id 는 이미 있는 fact/finding
// 노드여야 합니다(다른 의도/목표/힌트에 걸 수 없음). 최상위의 새 방향은 parent_ids 를 비우고, 없으면 origin fact 에 연결합니다.
// 그래서 모든 의도는 fact 노드에 연결되고, 발견이 이끌며 빈 계획이 아니라는 점이 만드는 경로에서 강제됩니다.
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary는 비울 수 없습니다")
	}
	// 앵커를 먼저 검사합니다(노드를 만들기 전, 나쁜 앵커가 고아 의도를 남기지 않게).
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d 이(가) 이 작업이나 직접 연관된 작업에 없습니다. parent_ids 는 이미 있는 사실(fact) 또는 발견(finding) 노드 id 여야 합니다. 최상위의 새 방향이면 parent_ids 를 비우세요", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d 은(는) %q 노드라 의도의 앵커가 될 수 없습니다. 의도는 확인된 사실(fact) 또는 발견(finding)에만 걸 수 있고, 의도/목표/힌트에는 걸 수 없습니다. 최상위의 새 방향이면 parent_ids 를 비우세요", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 자산 가로채기: 의도에 묶인 자산이 시스템 자산 가로채기 규칙에 맞으면 그 의도를 내리지 않습니다.
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("자산 가로채기 검증에 실패했습니다: %w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "의도 「%s」에 묶인 자산이 테스트 범위 검증을 통과하지 못했습니다. 관련 자산에 대한 테스트를 멈추세요:", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "生成【探索方向】写入 frontier，并连入探索链路。意图是开放的探索方向，不是固定类型——用 summary 一句话自由描述要探索/验证/利用什么。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"★优先批量：一轮筛出的多个新方向放进 intents 数组一次提交（比逐条调用省往返）。返回 ids 数组，与 intents 等长同序（失败项 id=0，详情见 errors）。单条则省略 intents 直接给顶层 summary。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【优先用这个】要新增的探索方向数组，按顺序处理。每个元素字段同下方顶层字段（summary/asset_ids/parent_ids/priority）。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}},                                                                                     // han-allow 업스트림 프롬프트·픽스처
			"summary":    str("[单条] 一句话描述这个探索方向：做什么+为什么。已写清方向即可，不依赖资产 id。"),                                                                                                                                                                                                                               // han-allow 업스트림 프롬프트·픽스처
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "本方向要测试/攻击的【目标资产 id】（**尽量传**，0/1/多个；是 list_assets 返回的资产 id，不是探索节点 id）：这条探索方向针对哪些资产（站点/接口/参数/主机等）。只要方向围绕某些具体资产就务必传上——它是「这条探索打哪些目标」的结构化标记，用于覆盖去重、把意图连入资产链路。仅当纯全局侦察、确实没有具体目标资产时才留空。"},   // han-allow 업스트림 프롬프트·픽스처
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上游锚点 id（可选，0/1/多个）：本方向由哪些【已确认的事实(fact)/发现(finding)】综合得出。**只能填已存在的 fact/finding 节点 id,不能填意图/目标/提示**——意图必须锚在已确认知识上,发现驱动而非凭空规划。多个事实共同产生一个新意图就传多个;顶层全新侦察方向请留空（会自动挂到任务起点 origin fact）。"}, // han-allow 업스트림 프롬프트·픽스처
			"priority":   intp("优先级 0-10，默认5"),                                                                                                                                                                                                                                                            // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 한 건 모드: 최상위 summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 사람이 메인 에이전트로 의도를 직접 넣으면, 작업이 이미 done(열린 목표가 없는 goalless 분기)일 때 그것을
			// running 으로 되돌립니다. 워커가 이 의도를 받아 실행할 수 있게 합니다. resumeTask 는 메인 에이전트의 Chat 만 연결합니다
			// (SetResumeTask). 플래너의 ToolSet 은 nil 이라 플래너가 add_intent 를 직접 부를 때는 이 구간이
			// no-op 이고, 의도를 정상적으로 만드는 데는 영향이 없습니다. 의도 노드는 위에서 이미 만들어졌고(open), 되살릴 때 고갈로 잘못 보지 않습니다.
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 한 건: 원래 반환을 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "列出本任务的目标节点及其状态（open/met），用于判断是否达成。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "当你判断某个发现/事实证明了某个目标达成时调用：把证据节点连到目标节点，并标记目标 met。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"goal_id":     idp("目标节点 id"),        // han-allow 업스트림 프롬프트·픽스처
			"evidence_id": idp("证明它的发现/事实节点 id"), // han-allow 업스트림 프롬프트·픽스처
			"reason":      str("为什么这个证据满足该目标"),   // han-allow 업스트림 프롬프트·픽스처
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id와 evidence_id는 필수입니다"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id는 이 작업의 목표 노드여야 합니다(연관 작업의 목표는 읽기 전용)"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id는 이 작업 또는 직접 연관된 작업의 사실/발견 노드여야 합니다"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// 목표 하나를 met 로 표시할 때마다 이 작업의 【모든 목표】가 met 인지 검사합니다. 그러면 자동으로
			// 작업 완료로 판정합니다(GoalMet). 모델이 goal_met 를 명시적으로 부를 필요가 없습니다.
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("목표 %d개가 모두 met 입니다(마지막은 goal %d)", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d 를 달성(met)으로 표시했습니다. 이 작업의 목표가 모두 달성되어 작업이 완료로 판정됩니다", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【立即结束整个任务】——仅当你确认任务的【全部目标都已真正达成、整体收官】时才调（注意是任务【整体】完成；仅仅达成了其中某一个目标/某一个 flag/某一个漏洞【不算】——那种情况用 prove_goal 标记该目标即可）。⚠️它不是用来“结束本轮规划”的：本轮没有新意图要派、或在等 worker 产出，都【直接结束本轮即可，不要调本工具】（0 个意图是完全正常的）。正常判定优先用 prove_goal 逐个证明目标；goal_met 只是绕过逐个证明、直接从全局收官的手段。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{"reason": str("达成理由（必须是目标真正达成的证据，不能是“本轮无新方向”这类结束本轮的理由）")}, "reason"), // han-allow 업스트림 프롬프트·픽스처
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "记录确认的漏洞，用 evidence 提供命令输出、日志等可验证证据。任务上下文传当前 intent_id。返回的 finding_id 是独立漏洞记录 ID，finding_node_id 是探索节点 ID（第一行保留该节点编号）。", obj(map[string]any{ // han-allow 업스트림 프롬프트·픽스처
		"vulnclass": str("漏洞类别"), "name": str("漏洞名称"), "severity": str("critical|high|medium|low"), "summary": str("发现摘要"), // han-allow 업스트림 프롬프트·픽스처
		"intent_id": idp("当前任务的意图 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "受影响资产 id"}, // han-allow 업스트림 프롬프트·픽스처
		"evidence":         str("证据/PoC 文本"),                                                   // han-allow 업스트림 프롬프트·픽스처
		"evidence_hint_id": idp("可选：本任务中对应此漏洞的提示节点 ID，自动携带其结构化 traffic_refs；不能引用继承提示或其他漏洞的提示"), // han-allow 업스트림 프롬프트·픽스처
		"traffic_refs": map[string]any{"type": "array", "description": "可选；HTTP/HTTPS 漏洞先检索并逐条核实请求/响应确实支持漏洞结论，再按复现顺序填写真实 ID。TCP 等非 HTTP 漏洞、未采集或找不到确切记录时省略或传 []，不阻止上报；可在 evidence 说明原因并提供其他可验证证据。不要猜测 ID、按域名/时间推定关联或仅为补包重复探测。用途 baseline 正常对照 / proof 漏洞证明 / verification 补充验证 / supporting 辅助证据。", // han-allow 업스트림 프롬프트·픽스처
			"items": obj(map[string]any{"traffic_id": str("traffic_search 返回的真实流量 ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("该流量如何支持漏洞结论")}, "traffic_id")}, // han-allow 업스트림 프롬프트·픽스처
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding 에는 작업 맥락이 필요합니다. 플랫폼 대화에서는 add_task_hint 로 해당 작업에 발견을 넘기고, 힌트에 이미 있는 traffic_refs 를 적으세요. 등록은 작업 에이전트가 합니다. 이미 등록된 발견은 bind_finding_traffic 으로 연결할 수 있습니다."), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "닫혀 있어, 페이지에서 사람이 연결할 수 있음" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id 는 유효한 힌트 노드 ID 여야 합니다. 넘길 힌트가 없으면 생략하세요"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("트래픽 증거 저장소를 쓸 수 없어 발견을 등록하지 않았습니다"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "발견은 저장되었고 트래픽은 연결되지 않았습니다. 패킷이 없는 경우에는 이대로 계속해도 됩니다. 확인된 HTTP 트래픽이 있으면 bind_finding_traffic 또는 발견 페이지에서 연결한 뒤 증거를 넘기세요. 발견을 다시 만들지 마세요."
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "발견은 저장되었습니다. 에이전트의 트래픽 자동 연결은 닫혀 있어, 페이지에서 사람이 연결할 수 있습니다."
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem 은 record_fact 일괄/한 건의 사실 하나입니다.
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 결론을 받치는 핵심 증거 한 줄(명령+핵심 출력 줄). 나중에 대조하기 쉽게 합니다
	Confidence string            `json:"confidence"` // observed(직접 봄) | inferred(현상으로 추론)
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact 는 fact 노드 하나를 쓰고 의도에 연결합니다(intent→yields→fact). defaultIntent 는
// 일괄일 때의 기본 의도입니다(이 항목에 intent_id 가 없으면 이것을 씀).
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary는 비울 수 없습니다")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id는 이 작업의 의도여야 합니다(연관 작업의 의도는 읽기 전용)")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "把探索【事实/结论】写入探索图，连到产生它的意图（intent_id）。用于记录探索结果——包括指纹/枚举等【正向结论】，和'端口关闭'/'参数不可注入'/'未发现登录入口'等【否定结论】。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"⚠️一次探索的多个观察要【汇总成一条事实】，不要拆成多条，可以合并成一条事实的就尽量用一条事实表示：summary=对本次结论的总结性一句话，detail=相关细节（可含多个具体项）。例：指纹意图→一条事实 {summary:'识别了 X 站点的技术栈与响应特征', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'}，而不是状态码、指纹、标题各记一条。一条意图通常只产出一条事实，拆太碎会让图谱无限膨胀。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"★facts 数组用于一次写多条【彼此不同】的结论（每条可省略 intent_id，默认用顶层 intent_id）。返回 ids 数组，与 facts 等长同序。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"⚠️只写你在工具输出里【真实看到】的结论，不要脑补。evidence 与 confidence 用来防止不准确的结论污染图谱：\n"+ // han-allow 업스트림 프롬프트·픽스처
		"  · evidence=支撑本结论的【一行】关键证据（命令+最能证明的那一两行输出），**务必简洁**——细节已在 detail，这里不要再粘大段输出。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"  · confidence=observed（输出里直接看到）| inferred（据现象推断）。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"  · **否定类结论**（不可注入/端口关闭/未发现入口等）只写\"观察 + 试探性读法\"——陈述你实际看到什么，方向是否放弃由规划者综合全局定；务必给 evidence，手段没穷尽或证据弱（含只探一次、看起来像）标 inferred，确已穷尽且直接看到才标 observed。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【有多条不同结论时用】事实数组，元素字段同下方顶层字段（summary/detail/evidence/confidence/intent_id/asset_ids）；省略 intent_id 则用顶层 intent_id。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}}, // han-allow 업스트림 프롬프트·픽스처
			"summary":    str("对本次探索结论的【总结性一句话】（是对 detail 的概括）"),                                                                                                                                                                                         // han-allow 업스트림 프롬프트·픽스처
			"intent_id":  idp("产生本事实的意图 id（你领到的意图；批量时作为各条默认）"),                                                                                                                                                                                           // han-allow 업스트림 프롬프트·픽스처
			"detail":     str("本事实的相关细节：把这次探索的多个观察事实都写进这里"),                                                                                                                                                                                              // han-allow 업스트림 프롬프트·픽스처
			"evidence":   str("【一行】关键证据：命令 + 最能证明结论的那一两行输出。务必简洁，不要粘大段输出（细节放 detail）。"),                                                                                                                                                                   // han-allow 업스트림 프롬프트·픽스처
			"confidence": str("observed（输出里直接看到）| inferred（据现象推断）。否定结论务必如实标注。"),                                                                                                                                                                          // han-allow 업스트림 프롬프트·픽스처
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "相关资产 id（可选，0/1/多个）：该事实涉及哪些资产"},                                                                                                     // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 한 건 모드 + 일괄 기본 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 최상위 intent_id = 일괄 기본

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 한 건: 원래 반환을 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint 는 hint 노드(active/human) 하나를 탐색 그래프에 겁니다. 자산에 앵커할 수 있고 id 를 돌려줍니다.
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("에이전트의 트래픽 자동 연결이 닫혀 있어, traffic_refs 가 있는 힌트는 저장하지 않았습니다. 시스템 설정에서 켜거나 글만 넘기세요")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text는 비울 수 없습니다")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// 플래너를 여기서 건건이 깨우지 않습니다. addHint 가 한 묶음을 다 쓴 뒤 한 번만 깨웁니다(힌트 텍스트를 함께).
	// add_hint 한 번에 힌트가 여러 개여도 플래너 트리거 줄이 줄줄이 생기지 않게 합니다.
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal 은 goal 노드(open) 하나를 탐색 그래프에 겁니다. 작업 뿌리(origin fact, rel spawns)에 연결합니다.
// origin 은 t.worker 를 씁니다(없으면 system). goals 분해기가 쓴 것은 "goals", 메인 에이전트 실행 중은
// "human" 으로 기록합니다. 플래너를 깨우는 일은 setGoals 가 한 묶음 뒤에 한 번 합니다(아래). 여기서는 기록만 합니다.
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text는 비울 수 없습니다")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals 는 【이 작업】에 탐색 목표(goal 노드)를 추가합니다. 목표 분해기의 제출 도구이면서 메인
// 에이전트가 실행 중에 목표를 보태는 도구이기도 합니다. 같은 관리 도구라 웹에서 설명/schema 를 고치고 에이전트별로 묶을 수 있습니다.
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"给【本任务】新增探索目标(goal)。目标=最终可交付/可核验的结果,不是攻击步骤或侦察动作。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"★优先批量:多个目标放进 goals 数组一次提交,返回 ids 与之等长同序(失败项 id=0,详情见 errors)。单条则省略 goals 直接给顶层 text。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"vulnclass 可选:对应漏洞类(如 SQLi/IDOR),业务逻辑类目标留空。目标是否达成由系统判定标记 met,本工具只负责新增。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【优先用这个】要新增的目标数组,按顺序处理。每个元素:text(必填,一个独立可验证的最终目标)+ vulnclass(可选)。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}}, // han-allow 업스트림 프롬프트·픽스처
			"text":      str("[单条] 一个独立可验证的最终目标"),                                                                                                                                                       // han-allow 업스트림 프롬프트·픽스처
			"vulnclass": str("[单条] 对应漏洞类(若明确),如 SQLi/IDOR;业务逻辑目标可留空"),                                                                                                                                   // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals 가 켜져 있지 않습니다. ExplorationStore 가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 한 건 모드: 최상위 text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// 플래너를 깨웁니다(한 묶음에 한 번). notifyGoal 을 우선합니다. set_goals 한 번에 「사람이
				// 목표 N개를 추가: …」 트리거 하나를 기록하고 건건이 화면을 채우지 않습니다. 분해기/워커에 이 콜백이 없으면 순수 notify 로 돌아갑니다(분해기의
				// round-0 은 notify 도 연결되지 않아 아무 일도 하지 않습니다. 그때는 플래너가 아직 시작되지 않았기 때문입니다).
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// 메인 에이전트가 실행 중에 목표를 추가하면, 완료/일시정지된 작업을 running 으로 되돌려 계속 돌립니다(종료 상태 문이
				// 보통 notify 를 삼키므로 명시적으로 되살려야 합니다). 이 콜백은 mainagent 만 연결합니다. 분해기/워커는 nil 입니다.
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 한 건: 원래 반환을 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint 는 조작 제약 하나를 task_constraints 에 기록합니다. origin 은 t.worker(없으면 system).
// 분해기는 "goals", 메인 에이전트는 "human" 으로 씁니다.
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text는 비울 수 없습니다")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 기본은 금지로 처리합니다. 유형이 없으면 더 보수적입니다
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type 은 allow 또는 deny 여야 합니다")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints 는 【이 작업】에 조작 제약을 추가합니다(allow=무엇을 허용할지 / deny=무엇을 금지할지). 목표
// 분해기 round-0 이 제약을 뽑는 제출 도구이면서, 메인 에이전트가 실행 중에 제약을 보태는 도구이기도 합니다. 같은 관리 도구라 웹에서
// 설명/schema 를 고치고 에이전트별로 묶을 수 있습니다. 제약은 플래너/워커 시스템 프롬프트에 들어가 탐색 경계를 제한합니다.
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"给【本任务】新增操作约束,用来框定探索边界:type=allow(允许做的操作)或 deny(禁止做的操作)。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"约束=对『可以/不可以做哪些操作』的规定(如『仅测当前端口,不扫其他端口』『禁止对生产库做写操作』『只允许被动侦察』),不是目标、也不是攻击步骤。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"★优先批量:多条放进 constraints 数组一次提交,返回 ids 与之等长同序(失败项 id=0,详情见 errors)。单条则省略 constraints 直接给顶层 text/type。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"只登记任务目标/描述里【明确写出】的约束,不要臆造;拿不准类型时用 deny(更保守)。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【优先用这个】要新增的约束数组,按顺序处理。每个元素:text(必填,一条约束)+ type(allow|deny)。返回 ids 与本数组等长、同序。", "items": map[string]any{"type": "object"}}, // han-allow 업스트림 프롬프트·픽스처
			"text":        str("[单条] 一条操作约束的内容"),                                                                                                                                                     // han-allow 업스트림 프롬프트·픽스처
			"type":        str("[单条] allow(允许)或 deny(禁止);缺省按 deny 处理"),                                                                                                                               // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints 가 켜져 있지 않습니다. ExplorationStore 가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 한 건 모드: 최상위 text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 한 건: 단순한 반환을 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "把人类/主 agent 的战略提示挂到探索图，规划者下次生成意图时会读到它。\n"+ // han-allow 업스트림 프롬프트·픽스처
		"★优先批量：多条提示放进 hints 数组一次提交（比逐条调用省往返）。返回 ids 数组，与 hints 等长同序（失败项 id=0，详情见 errors）。单条则省略 hints 直接给顶层 text。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【优先用这个】要新增的提示数组，按顺序处理。每个元素字段同下方顶层字段（text/asset_ids/traffic_refs）。返回 ids 与本数组等长、同序。", "items": obj(map[string]any{"text": str("提示内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})}, // han-allow 업스트림 프롬프트·픽스처
			"text":         str("[单条] 提示内容，如'重点挖认证后接口'"),                                                                                                                                                                                                                                                                                           // han-allow 업스트림 프롬프트·픽스처
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "锚定的资产 id（可选，0/1/多个）"}, // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 한 건 모드: 최상위 text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// 플래너를 깨웁니다(한 묶음에 한 번). notifyHint 를 우선합니다. add_hint 한 번에 「사람이
				// 전략 힌트 N개를 추가: …」 트리거를 기록해, 플래너가 이번 턴은 새 hint 때문임을 알고 힌트 내용을 보게 합니다.
				// 그 콜백이 없으면 순수 notify 로 돌아갑니다(bare wake. hint 는 그래프에 접혀 있어 스스로 읽습니다).
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 한 건: 원래 반환을 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "终止一条正在运行的意图(work)。用于叫停跑偏/无意义的探索；被终止的意图标记为 stopped，不再自动重领。先用 get_worker_output 看看它在干嘛再决定。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{"intent_id": idp("要终止的意图 id（= work 句柄）")}, "intent_id"), // han-allow 업스트림 프롬프트·픽스처
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work 를 지금 쓸 수 없습니다"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id는 이 작업의 의도여야 합니다(연관 작업의 의도는 읽기 전용)"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("의도 %d 의 work 에 종료 신호를 보냈습니다", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("X 는 멈추고 Y 에 집중"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "给一条正在运行的意图(work)实时注入纠偏指令，不打断它、不丢已有进展：worker 会在下一步动作前收到你的指令并据此调整。用于'别再走 X、聚焦 Y'这类【意图内】纠偏；若方向整个错了应改用 kill_work 再下新意图。建议先用 get_worker_output 看它在干嘛。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"intent_id": idp("要纠偏的意图 id（= work 句柄）"),         // han-allow 업스트림 프롬프트·픽스처
			"message":   str("给 worker 的纠偏指令，明确让它停止什么、转向什么"), // han-allow 업스트림 프롬프트·픽스처
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work 를 지금 쓸 수 없습니다"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id는 이 작업의 의도여야 합니다(연관 작업의 의도는 읽기 전용)"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message는 필수입니다"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("의도 %d 의 work 에 방향 수정 지시를 넣었습니다(다음 단계부터 적용)", id)), nil
		})
}

// getWorkerOutput returns a work's final (or 중단 시점까지의) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "取本任务或直接关联任务某条意图(work)的最终输出结论。关联任务结果带 source_task_id/inherited=true 且只读。正常结束返回其总结；被终止(stopped)/异常的 work 返回其截至中止时的最后输出。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{"intent_id": idp("意图 id（= work 句柄）")}, "intent_id"), // han-allow 업스트림 프롬프트·픽스처
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id가 이 작업이나 직접 연관된 작업의 것이 아닙니다"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "(이 work 에는 아직 출력이 없습니다)",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("(이 work 에는 아직 출력이 없습니다)"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"查看某条意图(work)的【执行过程】（区别于 get_worker_output 只给最终结论）。三种用法：\n"+ // han-allow 업스트림 프롬프트·픽스처
			"① 只传 intent_id → 返回该 work 每一步的摘要流（summary≤100字，含 step_id；只是动作轮廓，不含完整输出）；\n"+ // han-allow 업스트림 프롬프트·픽스처
			"② intent_id + q → 只返回命中关键字的步骤摘要（在摘要和完整输出里都搜；仍只给 summary，要看内容用③）；\n"+ // han-allow 업스트림 프롬프트·픽스처
			"③ intent_id + step_ids → 返回这些步骤的完整内容(detail)；一次最多取 5 个，超出只返回前 5 个并在 notice/omitted_step_ids 里告知未取的。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"典型流程：先①/②定位可疑步骤的 step_id，再用③取其完整输出。不含思考(thinking)步骤。支持直接关联任务的历史 trace；其结果带 source_task_id/inherited=true 且只读。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"intent_id": idp("意图 id（= work 句柄）"),                                                                                                                                              // han-allow 업스트림 프롬프트·픽스처
			"q":         str("关键字：只返回摘要/完整输出命中它的步骤（可选；与 step_ids 互斥）"),                                                                                                                        // han-allow 업스트림 프롬프트·픽스처
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "要取完整内容的 step_id（来自①/②返回；一次最多取 5 个，多传只返回前 5 个，其余在 omitted_step_ids 里列出）"}, // han-allow 업스트림 프롬프트·픽스처
			"limit":     intp("摘要流/检索的返回上限（可选）"),                                                                                                                                              // han-allow 업스트림 프롬프트·픽스처
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id가 이 작업이나 직접 연관된 작업의 것이 아닙니다"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"한 번에 최대 %d 개 단계의 전체 내용만 가져옵니다. 이번엔 앞 %d 개(%v)를 돌려주었고, 가져오지 않은 %d 개는 %v 입니다. "+
							"이 내용으로 위치를 찾을 수 있으면 나머지를 더 가져올 필요가 없습니다. 계속 필요하면 그 step_id 로 한 번 더 호출하세요.",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【通常不推荐使用，因为系统中已经给了大部分信息了】在【本任务其他 work 的执行过程】里按关键字(q)检索——用于找回某个 worker 见过、却没写进 fact 的东西（某路径/token/报错等）。"+ // han-allow 업스트림 프롬프트·픽스처
			"已自动排除你自己这条意图的步骤（那些本就在你上下文里）。"+ // han-allow 업스트림 프롬프트·픽스처
			"只返回命中步骤的摘要(summary≤100字)，每条带 intent_id；据此再用 get_worker_trace(intent_id, step_ids=[...]) 取完整内容。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"q":     str("关键字（在所有 work 步骤的摘要+完整输出里搜）"), // han-allow 업스트림 프롬프트·픽스처
			"limit": intp("返回上限，默认 100（可选）"),           // han-allow 업스트림 프롬프트·픽스처
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q는 필수입니다"), nil
			}
			// 호출자 자신의 이 의도 단계는 뺍니다(워커의 자기 trace 는 이미 그 맥락에 있음).
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【通常不推荐使用，因为系统中已经给了大部分信息了】列出本任务里【已跑过的 work（意图）】索引：intent_id + 一句话方向(summary) + 状态。"+ // han-allow 업스트림 프롬프트·픽스처
			"你(worker)看不到探索图，用它来发现有哪些 work 值得翻看——再用 get_worker_trace(intent_id) 看其步骤、get_worker_trace(intent_id, step_ids=[...]) 取详情。"+ // han-allow 업스트림 프롬프트·픽스처
			"只列已执行的(running/done/exhausted/blocked/stopped)，不含还没跑的 open。注意：你的任务边界仍是你领到的那条意图，看别的 work 只为复用观察/避免重复劳动。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"q":     str("按 summary 关键字过滤（可选）"), // han-allow 업스트림 프롬프트·픽스처
			"limit": intp("返回上限，默认 50（可选）"),     // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding: 계획 상황을 판단할 때 스스로 발견한 취약점을 확인했으면 바로 등록할 수 있습니다(워커와 같은 도구).
		t.addFinding(),
		// list_companies: 기업 목록 + scope + 자산 수를 봅니다(company_id 를 얻거나 소속 범위를 이해).
		t.listCompanies(),
		// list_assets: 계획할 때 DSL 로 전체 자산 저장소를 검색합니다(list_untested_assets 의 범위 안 미테스트 관점과 함께,
		// 도메인/지문/포트/상태 코드 등의 조건으로 저장소 전체를 조회하는 능력을 보탭니다).
		t.listAssets(),
		// add_company_scope: 계획할 때 도메인/IP/CIDR/ICP/키워드를 어떤 회사의 자산 범위에 넣을 수 있습니다(맞은 자산을 자동으로 가져옴).
		t.addCompanyScope(),
		// add_task_scope: 루트 도메인/회사 전체/어떤 서브도메인/IP 를 이 작업 테스트 범위(커버리지 분모)에 적극적으로 넣습니다.
		t.addTaskScope(),
		// list_untested_assets: 필요할 때 이 작업 범위의 미테스트 자산(유형+페이지)을 조회하고, 추가 테스트를 스스로 정합니다.
		t.listUntestedAssets(),
	}
}
