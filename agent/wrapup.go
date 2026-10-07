package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 마무리 프롬프트(wrap-up / settlement prompt): 에이전트가 【걸음 수 소진(MaxTurns)】이나
// 【시간 초과(run_seconds/MaxDuration)】로 멈출 때, SDK 의 settlement 단계가 이 프롬프트를 넣습니다.
// 에이전트가 알아냈지만 아직 안 쓴 내용을 먼저 기록하고, 한 문장 요약을 내게 해서 끝이 흐지부지되지 않게 합니다.
// 초보 안내: 워커는 탐색 그래프의 의도 하나를 실행하다 예산이 끝나면, 발견·사실을 남기고 멈춥니다.
//
// 에이전트마다 마무리 프롬프트는 관리 화면에서 덮어쓸 수 있습니다(agents.wrapup_prompt). 비우면 여기
// 내장 기본을 씁니다. 【프롬프트 본문】만 고칠 수 있습니다. 어떤 도구를 끄는지, 마무리에 몇 턴을 줄지는 코드에 고정된 정책입니다.

// WrapupOverride 가 설정되면, 에이전트 key 의 저장된 마무리 프롬프트와
// 비어 있지 않은 것이 있는지를 돌려줍니다. 서버가 agents 표에 연결합니다
// (시스템 프롬프트의 PromptOverride 와 같습니다). nil 이거나 비면 내장 기본을 씁니다.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride 가 설정되면, 관리 화면이 정한 그 에이전트 마무리 단계의
// 턴 예산과, 양수가 있는지를 돌려줍니다. agents 표에 연결됩니다.
// nil 이거나 0 이하면 에이전트마다의 내장 기본(wrapupTurnDefaults)을 씁니다.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 내장 기본 마무리 프롬프트. 에이전트 key 로 찾습니다. 워커는 예전에 코드에 박아 둔 settleWrapUpPrompt 를
// 다시 씁니다(worker.go 에 정의). 플래너/메인 에이전트는 각자 한 벌이 있고, 못 찾으면(사용자 에이전트) 공통 기본으로 갑니다.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 각 에이전트 마무리 단계 【자신】의 턴 예산 내장 기본(관리 화면에서 >0 이면 덮어씀).
// 모두 10턴을 줘서 마무리에 기록할 걸음이 있게 합니다. 못 찾으면 genericWrapupTurns 로 갑니다.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "你本轮规划的步数即将用尽——注意只是【这一轮】结束,系统之后仍会随态势变化再次唤醒你继续规划,并非任务终止,你无需在此收束整个规划。请把本轮已经想清楚的结论落地、别让这一轮白跑,但也【不要为了收尾硬凑意图】(本轮 0 个意图仍是完全正常的结果)：(1) 若已判断出【当前就该派发】的探索方向,用一次 add_intent 批量提交(想好的别憋着不发);(2) 对已被某发现/事实证明达成的目标,调 prove_goal 标记 met(别漏判);(3) 若识别出需要分步的串行利用链,用 TodoWrite 记下,便于下次唤醒接着派。做完直接结束本轮,无需输出总结文本。" // han-allow 업스트림 프롬프트·픽스처

const mainAgentWrapUpDefault = "你的步数即将用尽,本次交互就要结束。不要再发起新的探索/操作。请**单独用一句话纯文本**向用户总结当前进展、关键结论,以及建议的下一步。" // han-allow 업스트림 프롬프트·픽스처

const genericWrapUpDefault = "你即将因预算耗尽被终止。请先把已完成但未落库的结果写回,再**单独用一句话纯文本**总结你做了什么、得到哪些关键结论(这句会作为本次运行的结果展示)。" // han-allow 업스트림 프롬프트·픽스처

// WrapupDefault 는 에이전트 key 의 내장 기본 마무리 프롬프트를 돌려줍니다.
// 관리 화면의 "기본으로 되돌리기" 값과, 빈 칸 자리표시로 씁니다.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault 는 에이전트 key 의 내장 마무리 턴 예산을 돌려줍니다.
// 관리 화면의 "0 = 기본 N" 안내로 씁니다.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup 은 실제로 쓸 마무리 프롬프트를 돌려줍니다.
// DB 덮어쓰기가 있고 비어 있지 않으면 그것을, 아니면 내장 기본을 씁니다.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns 는 실제로 쓸 마무리 턴 예산을 돌려줍니다.
// 양수인 DB 덮어쓰기가 있으면 그것을, 아니면 에이전트마다의 내장 기본을 씁니다.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement 는 에이전트 실행의 마무리 설정을 만듭니다. 프롬프트와 턴 예산은
// 에이전트마다 관리 화면에서 고칠 수 있습니다. 끄는 도구는 코드가 소유한 정책이라,
// 사용자가 "탐지를 멈춰라"는 안전선을 지울 수 없습니다. 실행마다 새로 해석합니다
// (DB 를 그 자리에서 읽음). 고친 내용은 재시작 없이 다음 실행에 적용됩니다.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- 작업 단위 시간 초과 마무리 문구 (작업 시간 초과와 마무리 설계) ----------
//
// per-run 마무리 문구와는 【두 벌】입니다. per-run 은 "이번 run 의 예산이 끝났다"이고, 작업 시간 초과는
// "작업 전체가 시각에 닿아 곧 끝난다"입니다. 뜻이 자주 반대입니다(특히 플래너: per-run 은 "멈추지 말고 계속 계획하라"이고,
// 작업 시간 초과는 "시각이 됐으니 계획을 멈추고 마지막 판정만 하라"). 워커/플래너에게만 설정합니다.

// WrapupTaskTimeoutOverride / …TurnsOverride: 작업 시간 초과 마무리 문구와 턴 수의 DB 덮어쓰기
// (agents.task_timeout_wrapup_prompt / _max_turns 에 연결, 워커/플래너만).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是你这次 run 的预算，是整场探索到点了）。这是最后机会：(1) 把你已识别但还没写回的内容【全部】落库——新资产 insert_assets、探索结论/事实 record_fact、确认漏洞 report_finding；(2) 不要再启动任何新命令/探测；(3) **最后单独用一句话纯文本**总结你在本意图上的关键结论。" // han-allow 업스트림 프롬프트·픽스처

const plannerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是本轮，是整个任务终止）。请基于当前【全部】事实与发现，做最后一次目标判定：对已被证据证明达成的目标调 prove_goal 标记 met（别漏判）。**不要再生成任何新意图**（此时派意图也不会再被执行）。判定完即收束，无需输出总结文本。" // han-allow 업스트림 프롬프트·픽스처

// TaskTimeoutWrapupDefault 는 어떤 에이전트의 작업 시간 초과 내장 기본 마무리 문구를 돌려줍니다(관리 화면 자리표시/기본 복구용).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 설정이 없으면(mainagent/chat) 빈 문자열
}

// resolveTaskTimeoutWrapup: DB 덮어쓰기(비어 있지 않음) > 내장 기본. 빈 문자열은 그 에이전트에 작업 시간 초과 문구가 없다는 뜻입니다
// (워커/플래너가 아님). 그때 호출자는 per-run 문구로 돌아가야 합니다.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 기본은 per-run 턴 수를 그대로 씀
}

// wrapupSettlementForTask 는 작업 마감을 아는 워커/플래너 실행의 마무리 설정을 만듭니다.
// 설계 문서 §5 를 보세요.
//   - clamped=true  → 이번 run 이 작업 deadline 에 눌림: Timeout 으로 마무리=작업 시각 도달→작업 시간 초과 문구;
//     MaxTurns 로 마무리=눌린 창 안에서 걸음이 먼저 소진, 작업에 몇 분 남음→per-run 문구로 돌아감.
//   - clamped=false → 작업이 아직 이름: 두 reason 모두 per-run 문구(즉 wrapupSettlement 로 퇴화).
//
// harness 에 넘기는 PromptByReason 은 마무리 때 【실제】 reason 으로 그 자리에서 고릅니다. 만들 때 미리 짝지으면 어긋납니다.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // 기본값(clamped 가 아닐 때 두 reason 이 쓰는 값이기도 함)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // 작업 시각 도달
				harness.ReasonMaxTurns: perRun, // 걸음이 먼저 소진, 작업에 시간이 남음
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
