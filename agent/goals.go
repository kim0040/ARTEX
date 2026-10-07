package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl 은 목표 분해기 프롬프트의 내장 편집 본문(구간 [A])입니다.
// agent_prompts 에 심습니다. 지금은 템플릿 변수를 쓰지 않습니다.
const goalsDefaultTmpl = `你是渗透测试目标分解器。你的职责是从用户输入中识别出**最终要达成的结果**，而不是规划攻击步骤。

**第一步（拆分目标之前先做）：抽取操作约束**
从「任务目标 / 任务描述」里识别操作员对【可以做什么、不可以做什么操作】的明确规定，调用 set_constraints 逐条登记（如果描述、目标中不涉及操作约束可以不进行提取操作约束）：
- type=deny：禁止的操作（如「不扫端口」「不得对生产环境做写/删操作」「禁止爆破」「不碰某子域」）。
- type=allow：明确允许/限定的操作范围（如「只允许被动侦察」「仅针对某域名」）。
- 约束 ≠ 目标，也 ≠ 攻击步骤：它是对操作行为边界的规定。
- **约束必须【自包含、写死具体目标】**：把「当前目标/当前端口/当前IP/当前域名/本站」这类**指代词**替换成任务目标/描述里的**具体值**。约束会被单独注入到执行阶段的提示里，脱离上下文后指代词无法判断指谁。
  例：目标是 https://abc.example.net → 写「只允许测试 abc.example.net」而不是「只允许测试当前目标」；「仅测目标端口 443，不扫其他端口」而不是「只测当前端口」。若原文只说「当前目标」但目标地址已明确，就把地址填进去。
- **只登记目标/描述里【明确写出或强调】的约束，严禁臆造**；拿不准类型时用 deny（更保守）。
- 若目标/描述里确实没有任何操作约束，则**不要**调用 set_constraints。
登记完约束（如有）后，再进行下面的目标拆分。

**目标 = 最终可交付/可核验的结果**

**不是目标的内容（禁止列为子目标）**：
- 信息收集、侦察、端点扫描
- 漏洞分析与验证过程
- 攻击步骤、利用手段
- 结果验证步骤

**拆分原则**：
- 用户描述的最终目标只有一个 → 输出一个
- 存在多个**相互独立**的最终交付物 → 分别列出
- 能对应明确漏洞类的标注 vulnclass；信息收集/业务逻辑类目标留空
- 严禁臆造用户未提及的目标

调用 set_goals 提交结果。`

// goalsScopeTail 은 자산 저장소와 작업 맥락이 있을 때만, 고칠 수 있는 목표 본문 뒤에
// 붙는 코드 소유 꼬리입니다. 분해기가 목표/설명에서 명시된 자산 범위를 뽑아
// add_task_scope 로 등록하게 가르칩니다. DB 에서 고칠 수 있는 본문이 아니라 코드에 두어,
// 배포된 DB 에서도 항상 적용되고 지워지지 않습니다. trafficTool 꼬리와 같은 방식입니다.
// 초보: 작업의 자산 그래프 범위(분모)를 목표 문장에서 여기 안내로 등록하게 합니다.
const goalsScopeTail = `

**额外职责：登记测试资产范围**
除拆分目标外，你还要从「任务目标 / 任务描述」里识别出**明确给出的测试资产范围**，调用 add_task_scope 登记（本任务的授权边界，也是资产测试覆盖度的分母）。**最小范围原则：只登记用户明确点到的那一个目标，绝不擅自放大。**
- 目标是 URL 或带主机名的地址（如 https://xxx.example.com/path、app.example.com）→ 取其**完整主机名**，kind=subdomain，value=完整主机名。
  例：目标 https://a1b2c3.lab.example.net/path → kind=subdomain，value=a1b2c3.lab.example.net（**不是** example.net）。
  **严禁**把带子域的主机名缩成根域名——看到 xxx.example.com 就登记整个 example.com 会把范围扩到用户目标之外，违背最小范围原则。
- 仅当用户给的就是**裸根域名、且不含任何子域**（如直接写 example.com），或明确说“整个站点 / 所有子域 / 全域名” → 才用 kind=root_domain，value=example.com。
- 纯 IP 或网段 → kind=ip / cidr，value=IP 或 CIDR。
- **不要**登记公司范围（company）——任务刚建立、资产系统里通常还没有这家公司，登记不上，公司级范围交由后续 plan 阶段处理。
其它规则：
- 只登记**目标/描述里明确写出**的范围；严禁臆造或推断未提及的域名/IP。
- reason 简述依据来自哪句话，便于审计。
- 若目标/描述中没有任何明确资产范围，则**不要**调用 add_task_scope。
先用 add_task_scope 登记范围（如有），再调用 set_goals 提交目标。`

// GoalSpec 은 쪼개진 목표 하나입니다.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals 는 LLM 에게 침투 작업의 목표를, 따로 검증할 수 있는 목표들로 쪼개라고 합니다
// (각각 목표 노드가 됩니다). 공급자가 없거나 호출이 아무것도 안 내면 nil 입니다.
// 그러면 호출자가 규칙 기반 분할로 돌아가, 목표 노드는 항상 있게 합니다.
//
// prov 는 여기서 Config 로 만들지 않고 호출자가 넣습니다. 목표 분해가 엔진의 나머지와
// 같은 공급자 인스턴스를 탑니다. 속도 제한을 공유하고, llmrec 에 기록되고, LLM
// 장애 조치에 참여합니다. 셋을 조용히 우회하지 않습니다.
//
// desc 는 작업의 자유 설명입니다(배경: 대상 범위와 교전 설명 등).
// 목표와 함께 넣어, 분해기가 눈을 감고 쪼개지 않게 합니다. 프롬프트는 여전히
// 두 글에 없는 내용을 지어내는 것을 금지합니다.
//
// emit 이 nil 이 아니면 LLM 단계(thinking/tool_use/result)를 모두 받습니다.
// Worker="planner" 라서, 0라운드 목표 분해 활동이 UI 에 보입니다.
//
// as 와 taskID 가 nil 이 아니고 양수이면 add_task_scope 도구를 연결합니다.
// 분해기가 목표에서 뽑은 명시적 자산 범위를 등록할 수 있습니다.
//
// ts 는 이 작업의 탐색 저장소입니다. set_goals 가 쪼갠 목표 노드를 바로 거기에 씁니다
// (메인 에이전트가 실행 중에 목표를 더할 때 쓰는 것과 같은 관리 도구).
// 돌려주는 명세는 저장소에서 다시 읽습니다. 호출자가 목표마다 활동을 내고,
// "LLM 이 아무것도 안 냈다"를 알아 폴백할 수 있습니다.
// 초보: 작업을 시작할 때 탐색 그래프의 목표 노드를 여기서 만듭니다. 플래너는 그 목표를 보고 의도를 만듭니다.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider 는 작업에 순서가 있는 공급자 사슬이 있을 때의 실행 변형입니다.
// 도구와 쓰기 동작은 같고, 공급자 선택과 장애 조치는 호출자가 맡습니다.
// maxTokens 는 그 profile 의 답 하나 출력 상한입니다(0 = 보내지 않음).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 목표 분해는 한 번만 호출합니다. transcript store 를 걸지 않으므로 agentcore 는 ctx 에
	// session id 를 걸지 않습니다(writer 가 있을 때만 겁니다. agentcore.Prompt 참고). session-id 헤더로
	// 프롬프트 캐시나 고정 라우팅을 하는 게이트웨이(opencode zen 은 x-opencode-session 이 없으면 바로 400
	// MissingSessionID)는 ctx 의 이 값을 읽습니다. 안 채우면 대화는 되는데 분해만 400 이 됩니다.
	// 안정된 id 를 명시적으로 겁니다. 같은 탐색의 분해 요청이 그것을 공유해 캐시에 유리하고, 이름은
	// planner/worker 와 겹치지 않아 llmrec.parseSession 이 올바르게 귀속합니다.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" 는 목표 노드의 출처를 표시합니다. ts/taskID 로 set_goals 가
	// 각 목표를 작업 뿌리 아래에 잇습니다. 이것은 목록의 진짜 set_goals 도구라,
	// 웹에서 고친 설명/schema 도 여기서 적용됩니다.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// 설명은 목표와 같은 통로인 사용자 메시지에 탑니다. {{.EngagementDescription}}
	// 템플릿 변수로는 넣지 않습니다. 그 변수를 가리키는 프롬프트가 설명을 두 번 넣게 됩니다.
	// 시스템 프롬프트는 고정된 지시만 담습니다.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints 는 항상 쓸 수 있습니다(asset store 에 의존하지 않음). 본문에 이미 조작 제약을 먼저 뽑고 목표를 쪼개는 단계가 있습니다
	// (에이전트 편집 페이지에서 문장을 바꿀 수 있음). 여기서는 도구만 연결하면 됩니다.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// 쓸 자산 저장소와 작업이 있을 때만 add_task_scope 를 연결합니다.
	// 범위 추출 꼬리도 같이 붙여, 프롬프트가 없는 도구를 시키지 않게 합니다.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "任务目标：\n" + goalText // han-allow 업스트림 프롬프트·픽스처
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\n任务描述（背景信息，可能含靶标范围/flag 数量/交战说明；仅供参考，不要臆造其中未提及的内容）：\n" + d // han-allow 업스트림 프롬프트·픽스처
	}
	// captureRun 을 써서 LLM 단계마다 활동 기록으로 나갑니다(계획 탭의 0라운드 표시 아래).
	// emit 이 nil 이면 조용히 넘어갑니다.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3단계(제약 추출, 범위 등록, 목표 분해)는 각각 도구 호출이 한 번 필요합니다. 턴을 넉넉히 줘 마무리 전에 set_goals 호출이 빠지지 않게 합니다.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 이 profile 이 비스트리밍이면 Provider.Complete 를 탑니다
		MaxTokens:    maxTokens,    // 0 = 상한을 보내지 않음. 서버 기본값
	}, userMsg, captureEmit)
	// set_goals 가 목표를 바로 저장했습니다. 다시 읽어 호출자가 무엇을 썼는지 보게 합니다
	// (빈 조각이면 LLM 이 아무것도 안 낸 것이고, 호출자가 폴백합니다).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
