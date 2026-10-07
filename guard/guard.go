// Package guard 는 도구 실행 직전의 안전 경계입니다.
//
// 초보: 플래너·워커가 도구를 부르기 전에 PreToolUse 를 거칩니다. 사람이 둔
// 가로채기 규칙과 맞으면 호출을 막고, 그 이유는 작업 화면과 기록에 남습니다.
// 거절 문구는 여기 하드코딩되어 있지 않고, DB 에 심긴 [내장] 규칙입니다.
// 예전 허가 범위(RoE) 장치는 빠져 있습니다.
package guard

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/hook"
)

// AuditEntry 는 가드를 통과한 도구 호출 한 건의 기록입니다.
type AuditEntry struct {
	TS      int64  `json:"ts"`
	Tool    string `json:"tool"`
	Action  string `json:"action"` // allow|block. 통과 또는 차단
	Reason  string `json:"reason,omitempty"`
	Command string `json:"command,omitempty"`
}

// Guard 는 에이전트 훅으로 부작용 정책을 적용합니다.
// 초보: 도구가 실제로 돌기 직전 PreToolUse, 직후 PostToolUse 가 호출됩니다.
type Guard struct {
	mu          sync.Mutex
	audit       []AuditEntry
	attrib      map[string]int // 실패 분류 횟수(관찰 / G5)
	reg         *hook.Registry
	interceptor *intercept.Interceptor // 없으면 사람이 둔 규칙을 적용하지 않음
}

// New 는 사람 규칙 없는 Guard 를 만듭니다. 가로채기가 아직 없을 때 씁니다.
func New() *Guard { return newGuard(nil) }

// NewWithInterceptor 는 사람이 둔 가로채기 규칙이 붙은 Guard 를 만듭니다.
func NewWithInterceptor(ic *intercept.Interceptor) *Guard { return newGuard(ic) }

func newGuard(ic *intercept.Interceptor) *Guard {
	g := &Guard{attrib: map[string]int{}, interceptor: ic}
	g.reg = hook.NewRegistry().
		On(hook.PreToolUse, g.preToolUse).
		On(hook.PostToolUse, g.postToolUse)
	return g
}

// Hooks 는 에이전트 세션에 붙일 훅 등록부를 돌려줍니다.
func (g *Guard) Hooks() *hook.Registry { return g.reg }

func (g *Guard) preToolUse(ctx context.Context, ev hook.Event) hook.Result {
	// 감사 로그에 남길 셸 표면을 뽑습니다. Bash 와 대화형 셸 도구
	// (shell_open 의 command, shell_send 의 text)입니다. 파괴·유출 차단은
	// 여기 하드코딩되어 있지 않고, DB 가로채기 규칙에 있으며 아래 applyIntercept 가
	// 평가합니다. 다른 도구는 빈 명령을 기록합니다.
	var cmd string
	switch ev.ToolName {
	case "Bash", "shell_open":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Command
	case "shell_send":
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Input, &in)
		cmd = in.Text
	}
	g.record(ev.ToolName, "allow", "", cmd)
	return g.applyIntercept(ctx, ev)
}

// applyIntercept 는 사람이 둔 가로채기 규칙을 이 도구 호출에 적용합니다.
// 규칙과 폴백 심사 모두 도구 입력 전체를 받습니다.
func (g *Guard) applyIntercept(ctx context.Context, ev hook.Event) hook.Result {
	if g.interceptor == nil {
		return hook.Result{}
	}
	if !g.interceptor.IsToolEnabled(ev.ToolName) {
		return hook.Result{}
	}
	ctx = intercept.WithCall(ctx, ev.ToolName, ev.Input)
	dec, matched := g.interceptor.Match(ev.ToolName, ev.Input)
	if !matched {
		// 맞는 규칙이 없습니다. LLM 폴백 심사가 켜져 있으면 묻습니다. 꺼져 있거나
		// 연결되지 않았으면 지금처럼 통과시킵니다.
		d, judged := g.interceptor.Judge(ctx, ev.ToolName, ev.Input)
		if !judged {
			return hook.Result{}
		}
		dec = d
	}
	switch dec.Action {
	case "deny":
		// 관찰: deny에 맞으면 승인을 기다리지 않고 denied 한 줄을 남깁니다(기록/작업 가로채기 화면에 보입니다).
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "denied")
		return g.block(ev.ToolName, systemBlockMessage(dec.Message), "")
	case "allow":
		// 규칙과 모델의 명시적 승인을 남겨, 검토 내역을 나중에 볼 수 있게 합니다.
		g.interceptor.Log(ctx, intercept.ConvIDFromContext(ctx), dec, ev.ToolName, ev.Input, "allowed")
		return hook.Result{}
	case "ask":
		// 워커 context 가 이미 취소됐으면(작업 정지 / 종료) 대기 기록을 만들지
		// 않고 바로 막습니다. 고아 DB 행을 피하고, execOne 이 빨리 끝나
		// drainSynthetic 과의 경합이 줄어듭니다.
		if ctx.Err() != nil {
			return g.block(ev.ToolName, systemBlockMessage("작업이 취소되어, 플랫폼 안전 통제가 실행을 막았습니다"), "")
		}
		convID := intercept.ConvIDFromContext(ctx)
		if !g.interceptor.HandleAsk(ctx, convID, dec, ev.ToolName, ev.Input) {
			return g.block(ev.ToolName, systemBlockMessage("사람 승인이 통과하지 못했습니다(사용자가 거부했거나 승인 시간이 지났습니다)"), "")
		}
		return hook.Result{}
	}
	return hook.Result{}
}

// systemBlockMessage 는 가로채기 차단을 ARTEX 플랫폼의 통제 결정으로 포장합니다.
// 에이전트가 대상 쪽 방어로 착각하지 않게 합니다.
//
// 맨 이유("이 도구 실행 금지" / "사용자 거부")는 대상의 WAF/403 과 똑같이 읽힙니다.
// 그래서 에이전트는 명령을 바꾸고, 페이로드를 갈아끼우고, 다시 인코딩하고,
// 재시도하고 싶어 합니다. 소용없습니다. 플랫폼은 문자열 하나가 아니라 그 종류를
// 막습니다. 정책 결정이지, 뚫어야 할 장애물이 아닙니다. 이 접두사는 차단이
// 플랫폼에서 왔고, 대상의 보호가 아니며, 그 동작은 금지라고 분명히 말합니다.
// 에이전트는 우회 대신 다른 접근으로 돌아갑니다. 감사/기록 행은 원래 이유를
// 유지합니다(Interceptor.Log). 이 포장은 모델에게 가는 tool_result 에만 붙습니다.
func systemBlockMessage(reason string) string {
	return "【ARTEX 플랫폼 통제·대상의 방어가 아님】이 호출은 플랫폼이 가로챘습니다. " +
		"이유: " + reason + ". 이 작업은 금지되어 있습니다."
}

var reBlocked = regexp.MustCompile(`(?i)\b(403|forbidden|waf|blocked|rate.?limit|429|captcha|denied)\b`)

// postToolUse 는 관찰용 실패 분류 훅(G5)입니다. 도구 결과를 blocked / error / ok 로
// 나눠, 플래너가 WAF 앞에서 포기하지 않고 전략을 바꾸게 합니다.
func (g *Guard) postToolUse(_ context.Context, ev hook.Event) hook.Result {
	if ev.ToolName != "Bash" {
		return hook.Result{}
	}
	class := "ok"
	switch {
	case reBlocked.Match(ev.Result):
		class = "blocked"
	case ev.IsError:
		class = "error"
	}
	g.mu.Lock()
	g.attrib[class]++
	g.mu.Unlock()
	return hook.Result{}
}

// Attributions 는 실패 분류 횟수를 돌려줍니다(관찰 / G5).
func (g *Guard) Attributions() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.attrib))
	for k, v := range g.attrib {
		out[k] = v
	}
	return out
}

func (g *Guard) block(tool, reason, cmd string) hook.Result {
	g.record(tool, "block", reason, cmd)
	return hook.Result{Decision: "block", Message: reason}
}

func (g *Guard) record(tool, action, reason, cmd string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.audit = append(g.audit, AuditEntry{TS: time.Now().Unix(), Tool: tool, Action: action, Reason: reason, Command: cmd})
	if len(g.audit) > 2000 {
		g.audit = g.audit[len(g.audit)-2000:]
	}
}

// Audit 는 최근 가드 통과 기록의 스냅샷을 돌려줍니다. 최근 것이 맨 뒤입니다.
func (g *Guard) Audit() []AuditEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]AuditEntry, len(g.audit))
	copy(out, g.audit)
	return out
}
