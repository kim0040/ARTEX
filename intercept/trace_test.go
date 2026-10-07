package intercept

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

func TestTraceExactCorrelationAndSnapshot(t *testing.T) {
	ctx, trace := WithTrace(context.Background(), "save report", []db.InterceptContextEntry{{Kind: "user", Text: "prior message"}})
	trace.Start("call-a", "Write", []byte(`{"path":"a","n":12345678901234567890}`))
	trace.Append(db.InterceptContextEntry{Kind: "text", Text: "later context"})
	callCtx := WithCall(ctx, "Write", []byte(`{ "n":12345678901234567890, "path":"a" }`))
	// "pending"(ask 경로)은 전체 스냅샷을 남깁니다.
	// 평범한 allow 가 남기지 않는 이유는 TestAuditAllowDropsSnapshot 을 보세요.
	a := auditFor(callCtx, Decision{Action: "ask"}, nil, "pending")
	if a.Correlation != "exact" || a.ToolUseID != "call-a" || len(a.Context) != 1 || a.Context[0].Text != "prior message" {
		t.Fatalf("wrong snapshot: %+v", a)
	}
	var got string
	trace.bind("call-a", func(status, output string, cut bool) { got = status + ":" + output })
	trace.Complete("different-call", "wrong", false)
	if got != "" {
		t.Fatal("unrelated result attached")
	}
	trace.Complete("call-a", "written", false)
	trace.Finish()
	if got != "succeeded:written" {
		t.Fatalf("result %q", got)
	}
}

// 규칙의 allow 는 감사하려고 기록하지만, 재생용 스냅샷은 담으면 안 됩니다.
// 예비 판정이 켜지면 그런 행이 도구 호출마다 생깁니다.
// 호출마다 24×8KiB 맥락과 32KiB 프롬프트를 남기면 intercept_pending 에 수백 MB 가 쌓입니다.
// 그 데이터는 이어서 작업 보관함에도 들어갑니다.
// 결정에 대한 정보와 연결 정보는 남아야, 실행 결과가 계속 묶입니다.
func TestAuditAllowDropsSnapshot(t *testing.T) {
	ctx, trace := WithTrace(context.Background(), "a long user prompt", []db.InterceptContextEntry{{Kind: "user", Text: "prior message"}})
	input := []byte(`{"path":"a"}`)
	trace.Start("call-a", "Write", input)
	a := auditFor(WithCall(ctx, "Write", input), Decision{Action: "allow", RuleName: "auto"}, input, "allowed")
	if a.UserMessage != "" || a.UserTruncated || a.Context != nil || a.ContextTruncated {
		t.Fatalf("allow kept the replay snapshot: %+v", a)
	}
	if a.Correlation != "exact" || a.ToolUseID != "call-a" || a.EffectiveAction != "allow" ||
		a.ExecutionStatus != "awaiting_result" || a.RuleName != "auto" || a.InputDigest == "" {
		t.Fatalf("allow lost decision metadata: %+v", a)
	}

	// 차단은 드물고 전체 맥락을 남길 가치가 있어, 스냅샷을 유지합니다.
	denied := []byte(`{"path":"b"}`)
	trace.Start("call-b", "Write", denied)
	d := auditFor(WithCall(ctx, "Write", denied), Decision{Action: "deny"}, denied, "denied")
	if len(d.Context) == 0 || d.Context[0].Text != "prior message" || d.UserMessage == "" {
		t.Fatalf("deny lost the snapshot: %+v", d)
	}
}

func TestTraceIdenticalParallelCallsAreNeverGuessed(t *testing.T) {
	ctx, trace := WithTrace(context.Background(), "test", nil)
	input := []byte(`{"command":"pwd"}`)
	trace.Start("first", "Bash", input)
	trace.Start("second", "Bash", input)
	for range 2 {
		a := auditFor(WithCall(ctx, "Bash", input), Decision{}, input, "pending")
		if a.Correlation != "ambiguous" || a.ToolUseID != "" {
			t.Fatalf("guessed identity: %+v", a)
		}
	}
	trace.Complete("first", "first-result", false)
	a := auditFor(WithCall(ctx, "Bash", input), Decision{}, input, "pending")
	if a.Correlation != "ambiguous" {
		t.Fatal("ambiguity must survive the other call completing")
	}
}

func TestTraceSequentialIdenticalCallsAndRunIsolation(t *testing.T) {
	ctx, trace := WithTrace(context.Background(), "test", nil)
	input := []byte(`{}`)
	trace.Start("a", "Read", input)
	a := auditFor(WithCall(ctx, "Read", input), Decision{}, input, "pending")
	trace.Start("b", "Read", input)
	b := auditFor(WithCall(ctx, "Read", input), Decision{}, input, "pending")
	other, next := WithTrace(context.Background(), "next run", nil)
	next.Start("a", "Read", input)
	c := auditFor(WithCall(other, "Read", input), Decision{}, input, "pending")
	if a.ToolUseID != "a" || b.ToolUseID != "b" || a.RunID == c.RunID {
		t.Fatal("run or call identities overlap")
	}
}

func TestTraceBoundsAndMissingResult(t *testing.T) {
	ctx, trace := WithTrace(context.Background(), strings.Repeat("中文", promptLimit), nil) // han-allow 업스트림 프롬프트·픽스처
	for range contextLimit + 5 {
		trace.Append(db.InterceptContextEntry{Kind: "text", Text: strings.Repeat("中", entryLimit)}) // han-allow 업스트림 프롬프트·픽스처
	}
	trace.Start("a", "Read", []byte(`{}`))
	a := auditFor(WithCall(ctx, "Read", []byte(`{}`)), Decision{}, nil, "pending")
	if !a.UserTruncated || !a.ContextTruncated || len(a.Context) != contextLimit || !utf8.ValidString(a.UserMessage) {
		t.Fatal("unbounded/invalid snapshot")
	}
	for _, e := range a.Context {
		if len(e.Text) > entryLimit || !utf8.ValidString(e.Text) {
			t.Fatal("invalid entry bound")
		}
	}
	got := ""
	trace.bind("a", func(status, _ string, _ bool) { got = status })
	trace.Finish()
	if got != "unknown" {
		t.Fatalf("missing result presented as %q", got)
	}
}

func TestTraceConcurrentDistinctCalls(t *testing.T) {
	ctx, trace := WithTrace(context.Background(), "test", nil)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Go(func() {
			input := []byte(`{"path":"` + id + `"}`)
			trace.Start(id, "Read", input)
			a := auditFor(WithCall(ctx, "Read", input), Decision{}, input, "allowed")
			if a.ToolUseID != id {
				t.Errorf("got %s for %s", a.ToolUseID, id)
			}
			trace.Complete(id, "ok", false)
		})
	}
	wg.Wait()
}
