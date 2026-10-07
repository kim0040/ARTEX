package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner 는 이벤트로 도는 LLM 플래너입니다(문서 §4.3). 자산 그래프나
// 탐색 그래프가 바뀌면(디바운스) 탐색 경로를 읽고, 자산을 조회하고,
// 작업 목표가 충족됐는지 판단한 뒤 탐색 의도 0..N 개를 프론티어에 넣습니다.
// 의도를 만드는 유일한 역할입니다.
// 초보: 워커는 의도를 만들지 않습니다. 플래너만 탐색 그래프에 의도를 넣고, 워커가 하나를 집어 실행합니다.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // LLM 원문 대화를 남김 (nil = 끔)
	window            int                                    // 맥락 창 크기(token). 압축에 씁니다
	windowFn          func() int                             // 선택. 작업 사슬의 동적 하한
	maxTurns          int                                    // 실행 한 번의 최대 턴 (0 = 무제한)
	killWork          func(intentID int64) error             // 엔진 콜백: 돌고 있는 작업을 끊음 (nil = 끔)
	steerWork         func(intentID int64, msg string) error // 엔진 콜백: 돌고 있는 작업을 한가운데 돌림 (nil = 끔)
	proxyAddr         string                                 // WebFetch 용 기록 프록시 (비면 직접 연결)
	proxyCACert       string                                 // 기록 프록시 CA 인증서 경로 (HTTPS 검증)
	webSearch         WebSearchOpts                          // web_search 뒷단 선택 (기본은 꺼짐)
	workDir           string                                 // 공유 작업 디렉터리 (프롬프트에 산출물 위치로 나감)
	injectConstraints func() bool                            // 해석기: 작업 조작 제약을 시스템 프롬프트에 넣을까 (nil = 넣음)
	nonStreamingFn    func() bool                            // 해석기: 비스트리밍(Complete) 경로? (nil = 스트리밍)
	noaEnabledFn      func() bool                            // 해석기: 실험용 noa 압축? (nil = 끔)
	maxTokensFn       func() int                             // 해석기: 답 하나의 출력 상한 (nil/0 = 상한을 보내지 않음)
	compactor         *Compactor                             // cold 노드 압축(§7). nil = 꺼짐

	// todos 는 작업마다 계획 메모를 하나만 둡니다(탐색 id 가 키). 플래너의 여러 걸음 계획이
	// 깨어남 사이에 남습니다. Plan() 마다 세션은 새것이지만, 공유 저장소 덕분에
	// 이어지는 사슬을 한 번 적어 두고 라운드마다 한 걸음씩 보냅니다. 한 번에 병렬로 쏟지 않습니다.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor 는 cold 노드 압축기를 연결합니다(cold-digest §7). 플래너가 깨어날 때마다
// 라운드 수를 올리고, cold 도장을 유지하고, (바쁜 경로 밖에서) cold 노드를 digest 로 접습니다.
// nil 이면 기능이 꺼집니다.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming 은 실행이 비스트리밍 모델 경로를 쓸지 정하는 해석기를 연결합니다
// (true = 비스트리밍). nil 이거나 없으면 스트리밍입니다(기본).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled 는 실행이 실험용 noa 맥락 압축을 쓸지 정하는 해석기를 연결합니다.
// nil 이거나 없으면 꺼집니다(내장 압축). 실행마다 읽으므로, 에이전트를 다시 만들지 않아도
// 설정 스위치가 적용됩니다.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens 는 답 하나의 출력 상한 해석기를 연결합니다. nil, 없음, 또는 0 이면
// 상한을 보내지 않고 끝점이 정하게 둡니다. nonStreaming 처럼 실행마다 읽습니다.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy 는 플래너의 WebFetch 를 기록 프록시와, 그 HTTPS 를 검증할 CA 인증서로 보냅니다
// (주소가 비면 직접 연결).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch 는 플래너의 web_search 뒷단을 고릅니다(기본은 꺼짐).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject 는 이 작업의 조작 제약을 플래너 시스템 프롬프트에 넣을지 정하는
// 해석기를 연결합니다. 라운드마다 읽으므로, 에이전트를 다시 만들지 않아도 설정 스위치가 적용됩니다.
// nil 이면 넣습니다(기본).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints 는 제약 주입이 켜졌는지 봅니다(기본은 켬).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor 는 이 작업의 지속 계획 메모 저장소를 돌려줍니다. 처음 쓸 때 만듭니다.
// 이 작업의 플래너가 깨어날 때마다 같이 씁니다.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork 는 엔진의 작업 하나 종료 콜백을 연결합니다. 플래너의
// kill_work 도구가 돌고 있는 워커 하나를 멈출 수 있습니다.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork 는 엔진의 작업 하나 조향 콜백을 연결합니다. 플래너의
// steer_work 도구가 돌고 있는 워커 한가운데 방향 수정을 넣을 수 있습니다.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos 는 지속 계획 메모를 깨어남 프롬프트에 넣을 모양으로 만듭니다
// (아직 할 일이 없으면 비어 있습니다. 첫 깨어남).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【你的规划待办（跨唤醒保留，上一轮你写的）】：\n") // han-allow 업스트림 프롬프트·픽스처
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("据此推进：只对【前置步骤已完成 / 其依赖的 fact 已存在】的下一步派意图；用 TodoWrite 更新清单（把已被 fact 满足的步骤标 completed）。不要重复派已在清单里 pending/in_progress 的步骤。") // han-allow 업스트림 프롬프트·픽스처
	return b.String()
}

// TriggerEvent 는 이번 계획 라운드가 왜 일어났는지 구체적으로 적습니다. 플래너가
// 개요 전체를 다시 훑기 전에 실제 변화를 먼저 보게 합니다. Kind:
//
//	"done"    — 워커가 의도 IntentID 를 마쳤습니다(출력 결론을 가져옵니다).
//	"finding" — 워커가 의도 IntentID 에 발견을 보고했습니다(Detail = 요약).
//	"goal"    — 사람이(메인 에이전트의 set_goals 로) 한 호출에 목표를 하나 이상 더했습니다
//	            (Goals = 이번에 추가된 목표 텍스트, 1개 이상. set_goals 는 일괄을 지원).
//	"goal_deleted" — 사람이 개요의 목표 관리에서 목표를 지웠습니다(Detail = 삭제된 목표 텍스트).
//	"goal_edited"  — 사람이 개요의 목표 관리에서 목표를 고쳤습니다(OldGoal→NewGoal 텍스트).
//	"cancelled" — 사람이 의도 IntentID 를 지웠습니다(Detail = 삭제 이유). 그 의도는
//	            멈춘 것이지 지워진 것이 아니고, 이유가 사실로 붙습니다.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 전용: 삭제 전에 잡아 둔 의도 요약(진짜 삭제 뒤에는 노드가 없어 다시 조회할 수 없음)
	Goals    []string // Kind=="goal" 전용: 이번 set_goals 가 추가한 목표 텍스트(1개 또는 여러 개)
	OldGoal  string   // Kind=="goal_edited" 전용: 수정 전 목표 텍스트
	NewGoal  string   // Kind=="goal_edited" 전용: 수정 후 목표 텍스트
	Hints    []string // Kind=="hint" 전용: 이번 add_hint 가 추가한 힌트 텍스트(1개 또는 여러 개)
}

// renderTriggers 는 이번 라운드를 깨운 변화를 풀어 씁니다. 끝난 워커면
// 어느 의도와 출력 결론인지, 발견이면 어느 의도와 무엇을 찾았는지입니다.
// 시간/심장박동으로 깨어나면 비어 있습니다. 저장소를 읽습니다(최선을 다함.
// 빈 칸이 라운드를 막지는 않습니다).
// 초보: 탐색 그래프의 변화가 플래너 프롬프트 맨 위에 여기 문장으로 들어갑니다.
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【本次触发本轮的实际变动（先看这里，再决定是否补方向）】：") // han-allow 업스트림 프롬프트·픽스처
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了一个目标：%s —— 新的待达成目标，请据此补充探索方向（若尚无对应意图）。", ev.Goals[0])) // han-allow 업스트림 프롬프트·픽스처
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了 %d 个目标：%s —— 均为新的待达成目标，请逐一为尚无对应意图的目标补充探索方向。", len(ev.Goals), strings.Join(ev.Goals, "；"))) // han-allow 업스트림 프롬프트·픽스처
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了一条战略提示：%s —— 已挂到探索图上，请据此调整/补充探索方向（若尚无对应意图）。", ev.Hints[0])) // han-allow 업스트림 프롬프트·픽스처
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了 %d 条战略提示：%s —— 均已挂到探索图上，请逐一据此调整/补充探索方向。", len(ev.Hints), strings.Join(ev.Hints, "；"))) // han-allow 업스트림 프롬프트·픽스처
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人删除了该目标：%s —— 该目标已移除，请据此重判剩余目标/方向（不必再为它派意图）。", ev.Detail)) // han-allow 업스트림 프롬프트·픽스처
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人修改了目标，由「%s」变为「%s」—— 请据新目标调整探索方向（原方向若已不适用请停派）。", ev.OldGoal, ev.NewGoal)) // han-allow 업스트림 프롬프트·픽스처
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 意图 #%d（%s）的 worker 报告了一个 finding：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail)) // han-allow 업스트림 프롬프트·픽스처
		case "cancelled":
			// 의도 내용은 삭제 때 잡아 둔 Summary 를 우선합니다(진짜 삭제 뒤에는 노드가 없어 intentSummary 로 찾을 수 없음).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 意图 #%d 由用户删除，意图内容是：%s、删除原因是：%s。该意图已删除（不再执行）；请据此重新规划。", ev.IntentID, sm, ev.Detail)) // han-allow 업스트림 프롬프트·픽스처
		default: // "done" 기본. 워커가 의도를 마침
			b.WriteString(fmt.Sprintf("\n- 意图 #%d（%s）的 worker 结束，输出结论：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID))) // han-allow 업스트림 프롬프트·픽스처
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("；本意图新产生的事实 id：%s ", fids)) // han-allow 업스트림 프롬프트·픽스처
			}
		}
	}
	b.WriteString("\n（完整细节可 node_detail / get_worker_output / list_findings 再查。）") // han-allow 업스트림 프롬프트·픽스처
	return b.String()
}

// factIDsYielded 는 이 실행에서 의도가 만든 사실 id 를 "#12、#15" 로 나열합니다.
// 플래너가 이번 라운드의 새 사실로 바로 갈 수 있습니다. 사실이 없거나 조회가 실패하면
// 비어 있습니다(최선을 다함).
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary 는 의도 노드의 한 줄 요약을 읽습니다(최선을 다함. 없으면 "?").
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput 는 끝난 워커가 그 의도에 남긴 결론을 돌려줍니다. 마지막
// 'result'(없으면 'text') 활동의 전체 상세를 잘라 냅니다. get_worker_output 과 같은 출처입니다.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(출력을 가져오지 못했습니다)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(이 work 에는 아직 출력 기록이 없습니다)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput 는 워커 출력 덩어리를 잘라, 트리거 맥락이 라운드마다 시스템 프롬프트를
// 부풀리지 않게 합니다. 전문은 get_worker_output 한 번이면 됩니다.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(잘렸습니다. 전체는 get_worker_output)"
}

// renderGraphOverview 는 미리 계산한 graph_overview 스냅샷을 깨어남 프롬프트에 접어 넣습니다.
// 플래너가 라운드마다 전체 상황을 손에 쥔 채 시작합니다. 도구를 한 번 왕복하지 않아도 됩니다.
// graph_overview 가 돌려줄 JSON 과 정확히 같습니다. 더 깊은 내용은 여전히 도구 한 번입니다
// (node_detail / list_facts 같은 도구 한 번).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // 모델이 graph_overview 를 직접 부르게 폴백합니다
	}
	return "\n\n【本轮态势（graph_overview 预取，等同你调用该工具的返回；需要细节再按需调 node_detail/list_facts 等）】：\n" + string(b) // han-allow 업스트림 프롬프트·픽스처
}

// plannerDefaultTmpl 은 플래너 프롬프트의 내장 편집 본문(구간 [A])입니다.
// agent_prompts 에 심습니다. Goal 은 {{.Goal}} 템플릿 변수입니다. 중간 산출물 출력 규약
// 꼬리는 코드가 소유합니다(artifactSpec). plannerSystem 이 렌더 뒤에 붙입니다.
const plannerDefaultTmpl = `你是一个网络安全平台授权渗透测试系统的"规划者"，被频繁唤醒（图一变就唤醒）。职责：读态势 → 判目标 → **只在确有未被覆盖的新方向时**补充探索意图。你是规划者、不是执行者：本轮所有产物只能是【生成/说清意图】或【判定目标】，绝不在 plan 里把活干了。

任务目标：{{.Goal}}

**本轮该产出几个意图（先想清楚这条）**：
- **硬底线（最高优先）**：只要【目标未达成】且【当前没有任何 open 或 running 意图】（frontier_open=0 且 running_intents 为空），本轮就【必须】产出至少一个向目标推进的意图——没有在跑的 work 可等、也没有在排队的方向时，产出 0 意图=任务停摆；哪怕已知方向都只在 recent_done 里，也要据下面 done/exhausted/blocked 的判断另开一条或续派一条。
- 硬底线之外，**产出 0 个意图是正常结果，但要有正当理由**（不是"少派更稳"的默认）：①**已覆盖**——你想到的方向都已被仍在 open/running 的意图处理（换措辞重复生成已存在的意图是严重错误）；②**等待依赖**——下一步依赖当前在跑 work 的产出、而它还没出来（此时硬派会让下游拿不到前置而空转，应等下次唤醒图更新后再派）。
- 反过来：确有【未覆盖、且不依赖在跑 work】的新方向，或目标未达成且范围内仍有未测面，就该派——别把 0 意图当偷懒的默认。

**每次唤醒的决策流程**：

1. **完整态势已附在本提示下方**（就是 graph_overview 的返回，无需再调它）：task（原始标题+目标/根节点）、资产计数、goals+状态、open/running/recent_done 意图、sites_without_endpoints（无端点的站点，提示可能待探的方向）、facts（探索事实数，与漏洞是两类）、recent_facts（{id,summary,confidence?}）。
   - **范围**：探索节点（goals/意图/facts/findings）只含本任务；**资产图全局共享**（多任务同一份，资产计数是全局在范围内的、非本任务独有）——出现非本任务相关的资产时忽略。
   - **血缘**：每个意图带 parents（上游：派生自哪些事实/意图）和 yields（下游：产生了哪些事实/发现），recent_facts 每条带 from_intent；据此理解"哪些事实来自哪个方向、能否综合出新方向"。
   - **否定/存疑观察**（recent_facts 里"端口关闭/不可注入"等）是 worker 的观察、不是定论：采信前先 node_detail(id) 看 evidence——evidence 扎实、confidence=observed 且手段已穷尽的才视为该方向暂时封住；evidence 缺失、只是"看起来像/只探一次"、或 confidence=inferred 的，按【尚未探明】处理，若在范围内且无其它意图覆盖，默认派一条复核意图去证实或推翻（**同一否定方向至多复核一次**；复核后仍为否定、且证据合理，就尊重该结论、不再派）。
   - **要更深细节才按需调**：list_facts（分页，最新在前，默认 20，可 q 过滤、before 翻页，带 total/has_more）、list_findings（全部漏洞）、node_detail(id)（完整证据/详情；列表/recent_facts 只给摘要）、list_assets（pull：q 搜索、type/company_id/task_id 过滤、分页，或 id/ids 直取）、asset_neighbors。资产全局共享，别默认拉全量。

2. **判目标（核心职责）**：goals 字段已含目标与状态；对已被某发现/事实证明的未达成目标，调 prove_goal(goal_id, evidence_id, reason) 标 met。**当你标记的恰是最后一个未完成目标时，系统自动判定整个任务完成**——收官只由逐个 prove_goal 驱动，没有别的"一键完成"手段。
   - ⚠️ **量化验收核对（严禁提前盖章）**：目标含可量化条件（覆盖度达 X%、拿 N 个 flag、获得某权限）时，prove_goal 前【必须】核对上方 graph_overview 的实测值（coverage.pct、findings_total 计数等）：未达标就【禁止】prove_goal，改派意图补差；不得以"大体达成/核心已拿下"为由提前标 met。例：要求覆盖度 100% 而实测 coverage.pct=40% → 未达成，继续派补测意图。

3. **（可选，仅开局、极轻量）探测理解**：仅当图里几乎还没有 fact（recent_facts 基本为空、任务刚开始）、仅凭态势无法把初始意图说具体时，才用 Bash 等对目标做极少量、只读的探测（如 1–2 次 curl 看首页/指纹）。**唯一合法产物是一句更精准的意图描述**——绝不是漏洞的发现/验证/利用，也不是端点/目录/参数的枚举结果（那些是 worker 的活，写成意图派下去）。三条硬边界：
   - 图里已有 worker 产出的 fact（facts>0 / recent_facts 非空）→【禁止】再自己探测，一切判断基于已有 fact，本轮产物只能是"派新意图"或"结束"；想深挖某线索 → 派意图让 worker 去查，不是自己 curl。
   - 即使开局也最多探 ≤3 次就收手，只为把初始意图说清；一旦发现自己在"深入查证"而非"快速定方向"（逐个枚举端点/目录、逐个试 id、解码链、反复探同一接口、任何注入/越权/漏洞的测试验证——全是 worker 的重活），立刻停手写成意图。
   - 能从现有事实/态势判断的，根本不必探测。

4. **决定补哪些新方向**：**这里的"克制"只指【不重复已存在的意图】，不是"能少派就少派"**——目标未达成时，默认追问是"为逼近目标，还有哪些更深、更狠、尚未覆盖的打法"，而不是"是否可以收尾"。意图是【开放的探索方向】（不是固定类型/菜单），结合已知事实、资产、目标自判方向，逐一与 open + running + recent_done 比对：
   - 已有 open/running 覆盖 → 不再生成（正在处理）。
   - 在 recent_done 里出现过 → **先看该意图的 state（每条都带）分辨怎么停的，再决定**：
     · **done（正常跑完）**：已覆盖 → 不原样重派；是否死路看它 yields 出的 fact 结论、而非 state；仅出现【材料性新机理】（新事实/资产/参数/明显不同的打法）才重派，且 summary 写清与上次的不同；换措辞、"再试一次说不定行"不算，禁止重试。
     · **exhausted（预算耗尽、探到一半被掐断，只写回部分）/ blocked（模型或网络失败、基本没探成）**：都是中途没善终、信息不全——先用 get_worker_trace / get_worker_output 看它实际做到哪、卡在哪，再从下列里选：接近突破被预算掐 → 派"接上次进度继续"；纯外部故障没跑成（blocked 常是）→ 直接重派同方向；每次卡同一处 → 换打法/方向。依据永远是 trace 里的真实进度，不是 state 本身。
   - 完全无任何意图覆盖的全新方向 → 生成。
   - 所有已知方向都被仍在 open/running 的意图覆盖 → 不生成、直接结束（有在跑/在排队的 work，等它们推进）；但若只剩 recent_done 覆盖、已无 open/running 而目标未达成 → 按顶部硬底线必须另开或续派。
   - **深度优先于覆盖度**：coverage 是下限/验收项、不是探索目标本身；发现高价值入口（可能通向 RCE/提权/数据外泄）后，优先派意图把那条路【往深打穿】，而不是为拉平覆盖度去铺广、逐个资产浅测。
   - **保持路线多样、别过早收敛**：目标未达成时，若现有意图都挤在同一条路线/入口，而存在【本质不同】的未覆盖方向（另一入口面/另一类资产/另一条利用链），优先补那条分歧方向，而不是在同一线上加同义意图（看实质差异，不看措辞）；若该分歧方向已被现有意图覆盖，仍不生成。理想是 2–3 条机理不同的路线并存（如"从上传链打"与"从认证绕过打"），某条交出【目标逼近】的证据后才把资源集中过去。**但多样性永远服从顶部【操作约束】**：被约束排除的入口面/端口/主机/操作，即使本质不同也绝不生成意图。

   **串行利用链：分步派，别拆成并行。** 强依赖串行链（①→②→③，后一步依赖前一步的实际产出）：不要一次性并行下发（下游拿不到还不存在的前置只会重复/空转）；用 TodoWrite 把整条链记成待办（每步一条），本轮只派"前置已满足"的那步（通常第一步），待它产出 fact 后下次唤醒（提示会带上待办清单）再派下一步并把已满足的标 completed。"同一件事"别拆两条（"确认触发点"和"触发触发点"是同一步）；只有【平行、互不依赖】的维度（如枚举多个不相关端点）才用多意图并行。

5. **提交**：用【一次】add_intent 批量提交筛出的新方向（intents 数组，最多 4 个最高价值的，不要逐条多次调）：
   - **summary**：一句话自然语言描述该方向（测试目标完整地址 + 做什么 + 为什么），不套固定分类；去重主要靠它与已有意图比对。
   - **asset_ids**：本方向要测试/攻击的目标资产 id（尽量传，0/1/多个，来自 list_assets）——只要方向围绕具体资产（站点/接口/参数/主机）就务必传，用于覆盖去重、连入资产链路，跨多资产就都传；纯全局侦察无具体资产才留空。
   - **parent_ids**：本方向由哪些上游节点综合得出（可选，0/1/多个）——多个事实结合产生一个意图就都传，派生自某上游意图/发现也传其 id，顶层全新方向留空。

不重复、不硬凑；但目标未达成、又有未覆盖且更深的打法时，该派就派。简洁、聚焦、高效。`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan 은 계획 라운드 하나를 돌립니다. emit 이 nil 이 아니면 플래너의 실행 단계를 받습니다
// (상황을 어떻게 읽고 목표를 판단하는지 사람이 봅니다. 플래너는 의도를 만드는 쪽이고
// 예전에는 검은 상자였습니다). 플래너가 목표 달성을 판정했는지를 돌려줍니다.
// triggers 는 이번 라운드를 깨운 구체적 변화입니다. 워커가 끝났거나 발견이 보고됐거나
// (여러 개일 수 있습니다. 엔진이 폭주를 디바운스합니다. 시간/심장박동이면 비어 있음).
// 프롬프트 맨 위에 풀어 써서, 플래너가 실제 변화(어느 의도, 그 출력/발견)를 먼저 보게 합니다.
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: 이 작업의 플래너 라운드 수를 올리고, cold_since_round 도장을
	// 유지하고, 문턱에 닿으면 백그라운드 압축을 띄웁니다. 동기 부분은 쌉니다(쿼리 몇 개).
	// LLM 압축은 떨어진 고루틴에서 돌아, 이번 라운드에 지연을 더하지 않습니다.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // kill_work 도구를 켭니다(nil = 없음)
	tsx.steerWork = p.steerWork // steer_work 도구를 켭니다(nil = 없음)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // 플래너 쪽 앵커의 기본은 작업 뿌리(origin 사실)입니다
	}
	// 도메인 도구 + 기본 도구 묶음(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지를 끄면 add_task_scope/list_untested_assets 를 뺍니다(프롬프트에 넣지 않음).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 핵심 상황(방금 끝난 의도 + 미리 읽은 전체 그래프)은 【이번 user 입력】으로 옮깁니다(아래 input). system 에는
	// 정적 계획 본문만 둡니다. 빼 두면 system 이 매 턴 안정되어 캐시에 유리합니다. 한 턴이 길어지면 상황이
	// compaction 에 눌릴 수 있습니다(플래너 한 턴은 보통 짧아 위험이 낮음). situational 은 아래 input 에 붙습니다.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 작업 deadline / 종료 모드(ctx 로 주입, taskclock.go). 종료 턴에는 작업 시간 초과
	// 플래너 마무리 문구를 【이번 조작 지시】로 이번 user 입력에 붙입니다(situational 과 함께). 마지막
	// 목표 판정만 하고 새 의도는 만들지 않게 합니다.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【任务终局收尾（本轮特殊指令，覆盖上面的常规规划流程）】：" + resolveTaskTimeoutWrapup("planner") // han-allow 업스트림 프롬프트·픽스처
	}
	// 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>. 먼저 만들어 둡니다.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 조작 제약(있으면)을 시스템 프롬프트에 넣어 탐색 경계를 정합니다
	}
	system, boundary := deferredSystem(sysBody, def)
	// 플래너 자신에게는 벽시계 예산이 없습니다. deadline 이 있으면 MaxDuration 을 남은 시간으로 눌러, 진행 중인 계획 턴이
	// 작업 시각에 마무리를 타게 합니다(시간 초과→작업 시간 초과 문구, 걸음 수→per-run 문구).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시에 흔적을 남깁니다. 프록시 CA 를 읽어 MITM 으로 다시 서명된 HTTPS 인증서를 검증합니다
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 인터넷 검색(선택). ddgs 는 키가 필요 없습니다. brave-free 는 BraveKey, tavily 는 TavilyKey 가 필요합니다.
		// WebSearchProxy 는 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 별개입니다. 비우면 직접 연결합니다.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 자식 프로세스는 기본적으로 프록시를 타고 CA 를 신뢰합니다
		WorkingDir:            taskDir,                              // 이 작업 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = 무제한(에이전트 관리에서 고칠 수 있음)
		MaxDuration:           maxDur,     // 0=제한 없음. deadline 이 있으면 deadline 까지 남은 시간
		Compaction:            compactionConfig(p.compactionWindow()),
		// 깨어날 때마다 공유하는 계획 할 일: 직렬 사슬을 여러 턴에 걸쳐 유지합니다(session 은 새것이고 store 는 아닙니다).
		Todos: p.todoFor(ts.ID()),
		// 【이번 턴】 걸음 예산에 닿으면 SDK 가 마무리를 실행합니다. 이번 턴에 정리한 결론을 남깁니다(보낼 add_intent,
		// 증명할 prove_goal, 직렬 사슬은 TodoWrite). 계획 자체를 멈추지는 않습니다. 플래너는 이후에도 반복해서 깨어납니다.
		// clamped(작업 deadline 에 눌림)이면 PromptByReason 을 씁니다(wrapupSettlementForTask 참고).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 이 profile 이 비스트리밍이면 Provider.Complete 를 탑니다
		MaxTokens:    p.maxTokens(),    // 0 = 상한을 보내지 않음. 서버 기본값
	}
	if p.tx != nil { // LLM 원문 대화를 남깁니다. 작업의 플래너마다 파일이 하나씩 쌓입니다
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 실험 기능: 켜면 noa 가 맥락 압축을 맡습니다(아카이브는 <workDir>/noa/<SessionID> 아래, 유지됨).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 상황(방금 끝난 의도 + 전체 그래프)은 이제 이번 user 입력에 붙습니다(아래 input). user 에는 또
	// 지시 + 깨어남 사이의 할 일이 있습니다(todo 는 모델 자신의 계획 메모라 다시 만들 수 있어 user 에 두어도 됩니다).
	// 시작 문구는 「이번 턴에 구체적 변화가 있는지」로 둘로 나뉩니다. 변화가 있으면 아래 【실제 변화】 블록을 가리킵니다. 변화가 없으면
	// (하트비트 정기 점검 / hint / 복구 등) 그래프가 바뀌었다고 말하지 말고, 실행 중인 의도를 같이 다시 보라고 알립니다.
	lead := "刚有具体变动（见下面的【本次触发本轮的实际变动】），据此规划下一步：" // han-allow 업스트림 프롬프트·픽스처
	if len(triggers) == 0 {
		lead = "本轮是**定时巡检（心跳到点）/无具体变动信号**的唤醒——图不一定有新变动。顺带复查在跑意图：长时间无进展或跑偏的用 steer_work 纠偏、方向整个错的用 kill_work 止损；再判定目标、决定是否补方向：" // han-allow 업스트림 프롬프트·픽스처
		// 하트비트나 변화 없는 깨움에서 그래프 전체에 open 이나 running 의도가 없으면 탐색이 멈춘 것입니다(워커가 안 돌고
		// 대기 중인 방향도 없음). 플래너에게 분명히 알리고 이번 턴에 새 방향을 내게 합니다. 실행 중인 의도만 보고 빈 턴을 돌지 않게 합니다.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "本轮是**定时巡检（心跳到点）**的唤醒,且当前**已没有任何 open 或 running 的意图**——没有 worker 在跑、也没有排队中的方向,探索已停摆。你**必须**在本轮产出一个或多个向目标推进、且与图中既有意图**互不重复**的新意图(不得产出 0 意图);先据下面的态势判定目标是否已达成,未达成则立即补方向：" // han-allow 업스트림 프롬프트·픽스처
		}
	}
	input := lead + situational + "\n\n据上面的态势，判定目标。目标已【真正达成】（已拿到目标成果/已确认目标漏洞）时用 prove_goal 逐个标记。**硬底线：只要目标尚未达成、且当前没有任何 open 或 running 意图（frontier_open=0 且 running_intents 为空），本轮就必须产出至少一个向目标推进的意图——此时没有在跑的 work 可等、也没有在排队的方向，产出 0 意图=任务停摆。仅当已有 open/running 意图在推进、或目标已达成时，本轮才可以不产出新意图。**" + // han-allow 업스트림 프롬프트·픽스처
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 은 이제 벽시계가 되면 실행 중인 도구를 끊고 그 자리에서 마무리를 탑니다(살아있는 ctx 위에서). 한 턴이 멈춰도
	// 마무리를 피하지 않으므로 바깥의 하드 ctx 가 필요 없습니다. ctx 는 pause / kill / shutdown 만 실어 나릅니다.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // 플래너 활동에는 intent_id 가 없습니다(의도를 만드는 쪽입니다)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
