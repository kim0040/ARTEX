package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "모델이 이번 실행을 정상 종료했지만 글 요약은 남기지 않았습니다. 사실과 자산은 이번 도구 호출 기록을 기준으로 봅니다",
	harness.ReasonMaxTurns:          "걸음 수 상한(MaxTurns)에 도달했습니다. SDK가 마무리를 하고 사실과 자산을 기록했으며, 인텐트는 exhausted로 표시됩니다. 플래너가 방향을 바꿔 이어 가고, 실패로 보지 않습니다",
	harness.ReasonTimeout:           "한 번 실행의 벽시계 예산(MaxDuration)에 도달했습니다. 시간이 되면 돌고 있던 도구를 끊고 그 자리에서 마무리하며, 이미 알아낸 사실과 자산을 기록합니다. 인텐트는 exhausted로 표시됩니다",
	harness.ReasonModelError:        "모델 또는 API 호출에 실패했습니다(네트워크, 인증, 속도 제한, 공급자 5xx 등). 재시도를 다 쓰면 인텐트는 blocked로 표시됩니다. 전송 계층 문제라 이 인텐트는 거의 탐색되지 않았습니다. get_worker_trace로 실행 과정을 본 뒤 다시 보내거나 방법을 바꾸세요",
	harness.ReasonBlockingLimit:     "컨텍스트 길이가 하드 상한에 닿아, 요청을 보내기 전에 막혔습니다. 인텐트를 더 잘게 나누거나 도구 반환을 줄이세요",
	harness.ReasonPromptTooLong:     "프롬프트가 너무 길고 컨텍스트 압축 재시도도 끝났습니다. 더 실행할 수 없습니다",
	harness.ReasonImageError:        "현재 모델은 이번 실행의 멀티모달 내용을 지원하지 않습니다. 비전을 지원하는 모델로 바꾸거나 도구가 이미지를 돌려주지 않게 하세요",
	harness.ReasonStopHookPrevented: "Stop 훅이 이번 실행의 종료를 막은 뒤 이어가지 못했습니다. 작업 Guard 규칙이 너무 엄격한지 확인하세요",
	harness.ReasonHookStopped:       "도구나 훅이 실행 계속을 스스로 멈췄습니다. 예를 들어 범위 밖 목표나 금지된 명령입니다. 마지막 tool_result의 차단 설명을 확인하세요",
	harness.ReasonAbortedStreaming:  "모델 출력 스트리밍 단계에서 실행이 취소되었습니다",
	harness.ReasonAbortedTools:      "도구 실행 단계에서 실행이 취소되었습니다",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
//
// 초보용: 플래너·워커·메인 에이전트가 글 요약 없이 멈추면, 활동 기록에 보이는
// 한 줄 요약과 마크다운 상세를 만듭니다. 탐색 그래프에 이미 쓴 사실·자산은 그대로입니다.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "취소 이유를 얻지 못했습니다"
		}
		stage := "실행 도중"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "모델 출력 단계"
		case harness.ReasonAbortedTools:
			stage = "도구 실행 단계"
		}
		sum = "(실행이 중단됨: " + short + "; 멈춘 곳 " + stage + progressSuffix(term, tr) + ", 미완료)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(실행 예산 상한에 도달(" + string(reason) + "), 마무리를 하고 사실을 기록함" + progressSuffix(term, tr) + "; 이번엔 글 요약 없음)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(글 요약 없음, 최종 상태 " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **최종 상태**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **중단 이유** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **중단 이유**: 알 수 없음. 취소 쪽이 context.WithCancelCause로 이름 있는 이유를 붙이지 않았을 수 있습니다\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **하위 오류**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **취소 전에 이미 만든 부분 출력**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **실행함**: %d회 모델 턴\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **이번 실행 시간**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **누적 token**: 입력 %d / 출력 %d / 캐시 읽기 %d / 캐시 쓰기 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **도구 호출**: 이번 실행은 도구를 호출하기 전에 끝났습니다\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **중단 당시 실행 중이던 도구**: `%s`(실행 %s, **결과를 아직 반환하지 않음**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **중단 직전 마지막 도구**: `%s`(정상 반환됨)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "실행 context는 취소됐지만 하위에서 Terminal 이벤트는 나오지 않았습니다"
	}
	return "알 수 없는 최종 상태입니다. harness가 TerminalReason을 추가했을 수 있으니 reasonHint를 보완하세요"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d회", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", 실행 " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
