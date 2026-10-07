package intercept

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

const (
	contextLimit = 24
	entryLimit   = 8 * 1024
	promptLimit  = 32 * 1024
	outputLimit  = 64 * 1024
)

type traceKey struct{}
type callKey struct{}
type completion func(status, output string, truncated bool)

type tracedCall struct {
	key       string
	audit     db.InterceptAudit
	claimed   bool
	ambiguous bool
	complete  completion
}

// Trace 는 Prompt 호출 하나에만 속합니다. SDK v0.3.6 훅은 도구 ID 를 주지 않습니다.
// 입력이 같은, 아직 끝나지 않은 이벤트가 정확히 하나일 때만 연결합니다.
// 같은 요청이 동시에 둘이면, 결과가 들어온 순서여도 어느 쪽인지 추측하지 않습니다.
// 초보: 승인 기록과 도구 결과를 잇는 감사 추적입니다. 가드 판정 자체는 아닙니다.
type Trace struct {
	mu      sync.Mutex
	runID   string
	user    string
	userCut bool
	entries []db.InterceptContextEntry
	cut     bool
	calls   map[string]*tracedCall
}

func WithTrace(ctx context.Context, user string, prior []db.InterceptContextEntry) (context.Context, *Trace) {
	t := &Trace{runID: rand.Text(), calls: make(map[string]*tracedCall)}
	t.user, t.userCut = bounded(user, promptLimit)
	for _, e := range prior {
		t.append(e)
	}
	return context.WithValue(ctx, traceKey{}, t), t
}

func (t *Trace) append(e db.InterceptContextEntry) {
	var cut bool
	e.Text, cut = bounded(e.Text, entryLimit)
	e.Truncated = e.Truncated || cut
	t.cut = t.cut || e.Truncated
	t.entries = append(t.entries, e)
	if len(t.entries) > contextLimit {
		t.entries = append([]db.InterceptContextEntry(nil), t.entries[len(t.entries)-contextLimit:]...)
		t.cut = true
	}
}

func (t *Trace) Append(e db.InterceptContextEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.append(e)
}

func (t *Trace) Start(id, tool string, input []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls[id] = &tracedCall{key: tool + ":" + digestInput(input), audit: db.InterceptAudit{
		RunID: t.runID, ToolUseID: id, Correlation: "exact", InputDigest: digestInput(input),
		UserMessage: t.user, UserTruncated: t.userCut, CapturedAt: time.Now().UTC(),
		Context: append([]db.InterceptContextEntry{}, t.entries...), ContextTruncated: t.cut,
	}}
	t.append(db.InterceptContextEntry{Kind: "tool_use", Tool: tool, ToolUseID: id, Text: string(input)})
}

// WithCall 은 모델 심사가 시작되기 전에 이 이벤트를 자기 것으로 표시합니다.
// 그래서 판정이 도는 동안, 뒤에 나온 도구가 이 승인의 맥락을 바꾸지 못합니다.
func WithCall(ctx context.Context, tool string, input []byte) context.Context {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	a := db.InterceptAudit{Correlation: "unavailable", InputDigest: digestInput(input), CapturedAt: time.Now().UTC()}
	if t != nil {
		t.mu.Lock()
		var candidates []*tracedCall
		for _, c := range t.calls {
			if !c.claimed && c.key == tool+":"+a.InputDigest {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) == 1 && !candidates[0].ambiguous {
			candidates[0].claimed = true
			a = candidates[0].audit
		} else {
			a.RunID, a.UserMessage, a.UserTruncated = t.runID, t.user, t.userCut
			if len(candidates) > 0 {
				a.Correlation = "ambiguous"
				for _, c := range candidates {
					c.ambiguous = true
				}
			}
		}
		t.mu.Unlock()
	}
	return context.WithValue(ctx, callKey{}, a)
}

func (t *Trace) bind(id string, f completion) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.calls[id]; c != nil {
		c.complete = f
	}
}

func (t *Trace) Complete(id, output string, isError bool) {
	t.mu.Lock()
	c := t.calls[id]
	delete(t.calls, id)
	t.mu.Unlock()
	if c == nil || c.complete == nil {
		return
	}
	status := "succeeded"
	if isError {
		status = "failed"
	}
	out, cut := bounded(output, outputLimit)
	c.complete(status, out, cut)
}

// Finish 는 빠진 결과를 성공이 아니라 unknown 으로 표시합니다.
// 도구가 중간에 끊겼거나 마지막 이벤트를 잃은 경우이며, 도구가 오류를 낸 것과는 다릅니다.
func (t *Trace) Finish() {
	t.mu.Lock()
	calls := t.calls
	t.calls = make(map[string]*tracedCall)
	t.mu.Unlock()
	for _, c := range calls {
		if c.complete != nil {
			c.complete("unknown", "실행은 끝났지만 도구 결과를 받지 못했습니다", false)
		}
	}
}

func auditFor(ctx context.Context, dec Decision, input []byte, status string) *db.InterceptAudit {
	a, ok := ctx.Value(callKey{}).(db.InterceptAudit)
	if !ok {
		a = db.InterceptAudit{Correlation: "unavailable", InputDigest: digestInput(input), CapturedAt: time.Now().UTC()}
	}
	a.InitialAction, a.InitialReason = dec.Action, dec.Message
	a.ModelFallback = dec.ModelFallback
	a.ModelInput, a.ModelInputDigest = dec.ModelInput, dec.ModelInputDigest
	a.RuleName, a.ConfigDigest, a.ProfileID = dec.RuleName, dec.ConfigDigest, dec.ProfileID
	a.ExecutionStatus = "not_started"
	if status == "allowed" {
		a.EffectiveAction, a.ExecutionStatus = "allow", "awaiting_result"
		if a.Correlation != "exact" {
			a.ExecutionStatus = "unknown"
		}
		// 자동 allow 를 포함한 모든 모델 판정에, 모델이 본 입력을 그대로 남깁니다.
		// 원본 감사 이력은 모델 입력이 아닙니다.
		// 기존의 가벼운 allow 보관 방식은 유지하고, 저장된 입력은 그대로 보여 줍니다.
		a.UserMessage, a.UserTruncated = "", false
		a.Context, a.ContextTruncated = nil, false
	}
	if status == "denied" {
		a.EffectiveAction, a.ExecutionStatus = "deny", "not_executed"
	}
	return &a
}

func (i *Interceptor) bindResult(ctx context.Context, id int64, audit *db.InterceptAudit) {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	if t == nil || audit.Correlation != "exact" || audit.ToolUseID == "" {
		return
	}
	t.bind(audit.ToolUseID, func(status, output string, cut bool) {
		_ = i.db.CompleteIntercept(id, audit.RunID, audit.ToolUseID, status, output, cut)
	})
}

func digestInput(input []byte) string {
	var value any
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	if d.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			input = canonical
		}
	}
	h := sha256.Sum256(input)
	return hex.EncodeToString(h[:])
}

func bounded(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}
