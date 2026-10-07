package agent

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// captureRun 은 Session.Prompt 로 에이전트 한 턴을 끝까지 돌리고, 실행 단계마다
// 모은 활동 기록(tool_use / tool_result / text / thinking / result)을 냅니다.
// 시스템의 모든 LLM 에이전트(워커, 플래너 등)가 같이 써서, 실행이 검은 상자가 되지 않습니다.
// 예전 agentcore.Run 은 이벤트를 모두 버렸습니다. 나가는 기록에는
// Kind/Tool/ToolUseID/IsError/Summary/Detail 만 있고, 호출자의 emit 이
// IntentID/Worker 를 채웁니다. 마지막 도우미 글과 종료 오류를 돌려줍니다.
//
// KindText/KindThinking 은 스트리밍 조각(조각마다 이벤트 하나)으로 옵니다.
// 이어진 구간은 기록 하나로 모아, 활동 기록에 조각 수십 개가 아니라 통째 문장이 보이게 합니다.
// 초보: 워커와 플래너가 도구를 쓰는 과정이 UI 활동 기록에 여기서 올라갑니다.
func captureRun(ctx context.Context, opts agentcore.Options, input string, emit func(db.Activity)) (string, harness.TerminalReason, error) {
	s := agentcore.NewSession(opts)
	defer s.Close() // 세션의 백그라운드 작업 관리자(임시 디렉터리와 프로세스)를 놓습니다
	return captureRunSession(ctx, s, input, emit)
}

// captureRunSession 은 이미 있는 세션 위의 captureRun 입니다. 호출자가 같은 대화에
// 프롬프트를 여러 번 돌릴 수 있습니다(예: 본 실행이 max_turns 에 닿은 뒤, 워커가 쌓은
// 맥락을 그대로 쓰는 마무리 라운드).
func captureRunSession(ctx context.Context, s *agentcore.Session, input string, emit func(db.Activity)) (string, harness.TerminalReason, error) {
	ctx, auditTrace := intercept.WithTrace(ctx, input, approvalHistory(s.Messages()))
	defer auditTrace.Finish()
	var reason harness.TerminalReason
	rec := func(r db.Activity) {
		if r.Kind == "text" || r.Kind == "tool_result" {
			auditTrace.Append(db.InterceptContextEntry{Kind: r.Kind, Tool: r.Tool, ToolUseID: r.ToolUseID, Text: r.Detail, IsError: r.IsError})
		}
		if emit != nil {
			emit(r)
		}
	}
	toolNames := map[string]string{} // tool_use id → 이름. 결과에 이름을 붙입니다

	var tbuf strings.Builder
	var tkind string
	flush := func() {
		if tbuf.Len() == 0 {
			return
		}
		s := strings.TrimSpace(tbuf.String())
		k := tkind
		tbuf.Reset()
		tkind = ""
		if s != "" {
			rec(db.Activity{Kind: k, Summary: firstLine(s, 200), Detail: s})
		}
	}
	addDelta := func(kind, text string) {
		if text == "" {
			return
		}
		if tkind != "" && tkind != kind {
			flush()
		}
		tkind = kind
		tbuf.WriteString(text)
	}
	lastTool := &runTrace{startedAt: time.Now()}
	var lastUsage *llm.Usage

	var finalText string
	var rerr error
	for ev, err := range s.Prompt(ctx, input) {
		if err != nil {
			flush()
			if ctx.Err() != nil { // 엔진이나 사용자의 취소입니다. 공급자 실패가 아닙니다
				sum, detail := terminalText(ctx, &harness.Terminal{Reason: reason, Err: ctx.Err()}, lastTool)
				rec(activityWithUsage(db.Activity{Kind: "result", Summary: firstLine(sum, 400), Detail: detail}, lastUsage))
				return finalText, reason, ctx.Err()
			}
			rec(activityWithUsage(db.Activity{Kind: "result", IsError: true, Summary: "실행 오류: " + err.Error(), Detail: err.Error()}, lastUsage))
			return finalText, reason, err
		}
		switch ev.Kind {
		case harness.KindToolUse:
			if ev.ToolUse == nil {
				continue
			}
			flush()
			toolNames[ev.ToolUse.ID] = ev.ToolUse.Name
			in := string(ev.ToolUse.Input)
			lastTool.start(ev.ToolUse.ID, ev.ToolUse.Name, in)
			auditTrace.Start(ev.ToolUse.ID, ev.ToolUse.Name, ev.ToolUse.Input)
			rec(db.Activity{Kind: "tool_use", Tool: ev.ToolUse.Name, ToolUseID: ev.ToolUse.ID,
				Summary: ev.ToolUse.Name + " " + firstLine(in, 200), Detail: in})
		case harness.KindToolResult:
			if ev.ToolResult == nil {
				continue
			}
			flush()
			out := blocksText(ev.ToolResult.Content)
			lastTool.done(ev.ToolResult.ToolUseID)
			auditTrace.Complete(ev.ToolResult.ToolUseID, out, ev.ToolResult.IsError)
			rec(db.Activity{Kind: "tool_result", Tool: toolNames[ev.ToolResult.ToolUseID], ToolUseID: ev.ToolResult.ToolUseID,
				IsError: ev.ToolResult.IsError, Summary: firstLine(out, 200), Detail: out})
		case harness.KindText:
			addDelta("text", ev.Text)
		case harness.KindThinking:
			addDelta("thinking", ev.Text)
		case harness.KindUsage:
			// 살아 있는 누적 token 사용량(모델 턴마다)입니다. 화면에 그리지 않는
			// "usage" 활동으로 나가고 token 필드만 담습니다. UI 는 돌고 있는 세션의
			// 실시간 token 수에 가장 최근 것을 씁니다. 여기서 flush() 하지 않습니다.
			// 버퍼에 있는 마지막 답 글은 KindResult 중복 제거에 남아 있어야 합니다.
			if ev.Usage != nil {
				u := *ev.Usage
				lastUsage = &u
				rec(db.Activity{Kind: "usage",
					InputTokens: &u.InputTokens, OutputTokens: &u.OutputTokens,
					CacheReadTokens: &u.CacheReadTokens, CacheWriteTokens: &u.CacheWriteTokens})
			}
		case harness.KindResult:
			if ev.Terminal != nil {
				if ev.Terminal.Reason != harness.ReasonAbortedStreaming {
					sidequestion.Finish(ctx, ev.Terminal.Messages)
				}
				finalText = ev.Terminal.Text
				reason = ev.Terminal.Reason
				// 버퍼의 꼬리 글은 보통 Terminal.Text(마지막 답)와 같습니다.
				// 기록이 두 번 나가지 않게 버립니다. 결과 행이 그 글을 담습니다.
				if tkind == "text" && strings.TrimSpace(tbuf.String()) == strings.TrimSpace(ev.Terminal.Text) {
					tbuf.Reset()
					tkind = ""
				}
				flush() // 뒤에 남은 생각이나, 마지막 답이 아닌 글을 비웁니다
				sum, detail := ev.Terminal.Text, ev.Terminal.Text
				if sum == "" || ev.Terminal.Reason == harness.ReasonAbortedTools || ev.Terminal.Reason == harness.ReasonAbortedStreaming {
					sum, detail = terminalText(ctx, ev.Terminal, lastTool)
				}
				u := ev.Terminal.Usage // 이 세션의 누적 token 사용량
				rec(db.Activity{Kind: "result", IsError: ev.Terminal.Err != nil,
					Summary: firstLine(sum, 400), Detail: detail,
					InputTokens: &u.InputTokens, OutputTokens: &u.OutputTokens,
					CacheReadTokens: &u.CacheReadTokens, CacheWriteTokens: &u.CacheWriteTokens})
				if ev.Terminal.Err != nil {
					rerr = ev.Terminal.Err
				}
			}
		}
	}
	flush() // 안전장치: KindResult 없이 스트림이 끝나면 아직 안 비운 글을 비웁니다
	return finalText, reason, rerr
}

func activityWithUsage(activity db.Activity, usage *llm.Usage) db.Activity {
	if usage == nil {
		return activity
	}
	u := *usage
	activity.InputTokens = &u.InputTokens
	activity.OutputTokens = &u.OutputTokens
	activity.CacheReadTokens = &u.CacheReadTokens
	activity.CacheWriteTokens = &u.CacheWriteTokens
	return activity
}

// blocksText 는 도구 결과의 content 블록 글을 이어 붙입니다.
func blocksText(blocks []llm.ContentBlock) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == llm.BlockText && bl.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// firstLine 은 요약 칸에 넣을, 한 줄이고 글자 수 상한이 있는 미리보기를 돌려줍니다.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if before, _, found := strings.Cut(s, "\n"); found {
		s = before
	}
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// 기록된 세션에서 보이는 메시지를 남깁니다. 생각 블록은 뺍니다.
// 이것은 감사 맥락입니다. 판정자는 여기서 고른, 짝이 맞는 실행 증거만 받고
// 도우미의 산문이나 생각 블록은 받지 않습니다.
// 초보: 가로채기 판정이 워커 실행의 도구 호출만 보게, 활동 기록을 여기서 고릅니다.
func approvalHistory(messages []llm.Message) []db.InterceptContextEntry {
	var entries []db.InterceptContextEntry
	for _, message := range messages {
		for _, block := range message.Content {
			entry := db.InterceptContextEntry{Kind: string(message.Role)}
			switch block.Type {
			case llm.BlockText:
				entry.Text = block.Text
			case llm.BlockToolUse:
				entry.Kind, entry.Tool, entry.ToolUseID, entry.Text = "tool_use", block.Name, block.ID, string(block.Input)
			case llm.BlockToolResult:
				entry.Kind, entry.ToolUseID, entry.Text, entry.IsError = "tool_result", block.ToolUseID, blocksText(block.Content), block.IsError
			default:
				continue
			}
			entries = append(entries, entry)
		}
	}
	return entries
}
