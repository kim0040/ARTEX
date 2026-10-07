package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// fakeProv 는 대본이 있는 provider 입니다. script[i] 가 i 번째 호출의 결과입니다.
// 이벤트 몇 개, 그다음 선택적으로 오류입니다.
type fakeProv struct {
	name   string
	calls  int
	events [][]llm.StreamEvent // 오류 전에 내는 이벤트. 호출마다
	errs   []error             // 호출을 끝내는 오류. nil 이면 정상 종료
}

// at 은 대본의 n 번째를 돌려줍니다. 대본이 끝나면 마지막 항목을 반복해서,
// "항상 성공" / "항상 402" 로 정의한 provider 가 반복 호출에서도 그대로입니다.
func at[T any](s []T, n int) (T, bool) {
	var zero T
	if len(s) == 0 {
		return zero, false
	}
	if n >= len(s) {
		n = len(s) - 1
	}
	return s[n], true
}

func (f *fakeProv) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	n := f.calls
	f.calls++
	return func(yield func(llm.StreamEvent, error) bool) {
		if evs, ok := at(f.events, n); ok {
			for _, e := range evs {
				if !yield(e, nil) {
					return
				}
			}
		}
		err, _ := at(f.errs, n)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (f *fakeProv) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range f.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

// okProv 는 텍스트 델타 하나로 항상 성공하는 provider 를 만듭니다.
func okProv(name, text string) *fakeProv {
	return &fakeProv{name: name, events: [][]llm.StreamEvent{{{Type: llm.SETextDelta, Text: text}}}}
}

// failProv 는 이벤트를 내기 전에 바로 실패하는 provider 를 만듭니다. status 는 HTTP 상태입니다.
func failProv(name string, status int) *fakeProv {
	return &fakeProv{name: name, errs: []error{fmt.Errorf("anthropic: status %d: nope", status)}}
}

func member(id int64, name string, rank int, p llm.Provider) *Member {
	return &Member{ID: id, Name: name, Model: "m" + name, Rank: rank, Prov: p}
}

// drain 은 스트림을 끝까지 읽고, 이어 붙인 글과 끝 오류를 돌려줍니다.
func drain(seq iter.Seq2[llm.StreamEvent, error]) (string, error) {
	var sb strings.Builder
	for ev, err := range seq {
		if err != nil {
			return sb.String(), err
		}
		if ev.Type == llm.SETextDelta {
			sb.WriteString(ev.Text)
		}
	}
	return sb.String(), nil
}

func TestFailoverOnNoCredit(t *testing.T) {
	a, b := failProv("a", 402), okProv("b", "hello")
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, NewRegistry(nil, nil))

	got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if got != "hello" {
		t.Fatalf("text = %q, want %q", got, "hello")
	}
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("calls: a=%d b=%d, want 1/1", a.calls, b.calls)
	}
}

// 402 는 결정적입니다. 실패 한 번이면 차단기가 열려, 다음 요청은 그 설정을
// 통째로 건너뜁니다. 왕복을 한 번 더 내지 않습니다.
func TestHardFailureTripsBreakerImmediately(t *testing.T) {
	a, b := failProv("a", 402), okProv("b", "x")
	reg := NewRegistry(nil, nil)
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, reg)

	_, _ = drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if !reg.IsOpen(1) {
		t.Fatal("402 should have tripped the breaker on the first failure")
	}
	_, _ = drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if a.calls != 1 {
		t.Fatalf("tripped profile was called again: calls=%d, want 1", a.calls)
	}
	if b.calls != 2 {
		t.Fatalf("fallback calls=%d, want 2", b.calls)
	}
}

// 429 는 일시적입니다. SDK 가 이미 재시도했지만, 여러 번 실패하기 전에는
// 설정을 버리듯 기록하지 않습니다.
func TestSoftFailureNeedsRepeats(t *testing.T) {
	reg := NewRegistry(nil, nil)
	for i := 1; i < softTripAfter; i++ {
		if reg.Trip(1, "429", false) {
			t.Fatalf("tripped after %d transient failures, want %d", i, softTripAfter)
		}
	}
	if !reg.Trip(1, "429", false) {
		t.Fatalf("should trip on failure #%d", softTripAfter)
	}
	if !reg.IsOpen(1) {
		t.Fatal("breaker should be open")
	}
}

// 성공은 카운터를 완전히 지워야 합니다. 가끔 실패하는 설정이 쌓여 트립까지
// 가지 않게 합니다.
func TestPassResetsCounters(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.Trip(1, "429", false)
	reg.Trip(1, "429", false)
	reg.Pass(1)
	if got := reg.Get(1).Fails; got != 0 {
		t.Fatalf("fails=%d after Pass, want 0", got)
	}
	if reg.Trip(1, "429", false) {
		t.Fatal("tripped immediately after a success — counters were not reset")
	}
}

func TestBackoffLadderGrows(t *testing.T) {
	reg := NewRegistry(nil, nil)
	var prev time.Duration
	for i := range 4 {
		reg.Reset(1)
		st := State{Trips: i}
		reg.Restore(1, st)
		reg.Trip(1, "402", true)
		d := time.Until(reg.Get(1).OpenUntil)
		// 줄어들지 않아야 합니다. 1초 여유: 사다리는 마지막 칸에서 멈추고,
		// Trip 마다 자기 time.Now() 를 찍습니다.
		if i > 0 && d < prev-time.Second {
			t.Fatalf("trip #%d cools for %v, shorter than the previous %v", i+1, d, prev)
		}
		prev = d
	}
	if prev < 25*time.Minute {
		t.Fatalf("ladder tops out at %v, want ~30m", prev)
	}
}

// 안전 규칙: 출력이 호출자에게 도달한 뒤의 스트림 중간 실패는 다른 모델로
// 다시 시도하면 안 됩니다. 어시스턴트 턴이 두 번 쌓입니다.
func TestNoFailoverAfterEmit(t *testing.T) {
	a := &fakeProv{
		name:   "a",
		events: [][]llm.StreamEvent{{{Type: llm.SETextDelta, Text: "partial"}}},
		errs:   []error{fmt.Errorf("anthropic: status 500: mid-stream drop")},
	}
	b := okProv("b", "full")
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, NewRegistry(nil, nil))

	got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if err == nil {
		t.Fatal("mid-stream error should surface, not be swallowed by a failover")
	}
	if got != "partial" {
		t.Fatalf("text = %q, want the partial output %q", got, "partial")
	}
	if b.calls != 0 {
		t.Fatalf("fell over to the backup after emitting output (calls=%d) — would duplicate the turn", b.calls)
	}
}

// 작업을 취소해도 예비 키를 태우면 안 되고, LLM 고장으로 진단하면 안 됩니다.
func TestNoFailoverOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &fakeProv{name: "a", errs: []error{context.Canceled}}
	b := okProv("b", "x")
	reg := NewRegistry(nil, nil)
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, reg)

	_, err := drain(p.Stream(ctx, llm.CompletionRequest{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if b.calls != 0 {
		t.Fatalf("failed over on cancellation (backup calls=%d)", b.calls)
	}
	if reg.Get(1).Fails != 0 {
		t.Fatal("cancellation counted as a profile failure")
	}
}

func TestShouldFailoverByStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{400, false}, // 나쁘거나 너무 긴 요청은 어디서나 같음
		{401, true}, {402, true}, {403, true}, {404, true},
		{408, true}, {429, true}, {500, true}, {503, true},
		{200, false},
	}
	for _, c := range cases {
		err := fmt.Errorf("openai: status %d: body", c.status)
		if got := shouldFailover(context.Background(), err); got != c.want {
			t.Errorf("status %d: shouldFailover=%v, want %v", c.status, got, c.want)
		}
	}
	// 전송 오류는 상태가 없고, 다음 설정으로 넘어가야 합니다.
	if !shouldFailover(context.Background(), errors.New("dial tcp: connection reset by peer")) {
		t.Error("network error should fail over")
	}
}

func TestHardVsSoftClassification(t *testing.T) {
	for _, s := range []int{401, 402, 403, 404} {
		if !isHardFailure(fmt.Errorf("status %d: x", s)) {
			t.Errorf("status %d should be a hard failure", s)
		}
	}
	for _, s := range []int{408, 429, 500, 502, 503} {
		if isHardFailure(fmt.Errorf("status %d: x", s)) {
			t.Errorf("status %d should be transient, not hard", s)
		}
	}
}

// 컨텍스트 창이 요청을 못 담는 멤버는 400 이 확실합니다. 왕복으로
// 확인하느라 쓰지 말고 건너뜁니다.
func TestSkipsMembersTooSmallForRequest(t *testing.T) {
	small, big := okProv("small", "s"), okProv("big", "b")
	ms := member(1, "small", 10, small)
	ms.WindowTokens = 1000
	mb := member(2, "big", 5, big)
	mb.WindowTokens = 1_000_000
	p := New([]*Member{ms, mb}, NewRegistry(nil, nil))

	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText(strings.Repeat("x", 100_000))}}
	got, err := drain(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("text=%q, want the large-window member to serve it", got)
	}
	if small.calls != 0 {
		t.Fatalf("sent a request to a member that can't hold it (calls=%d)", small.calls)
	}
}

// 전부 트립된 상태: 엔진을 그냥 멈추는 것보다 머리를 한 번 두드리는 편이 낫습니다.
func TestAllTrippedStillProbesHead(t *testing.T) {
	a, b := okProv("a", "a"), okProv("b", "b")
	reg := NewRegistry(nil, nil)
	reg.Trip(1, "402", true)
	reg.Trip(2, "402", true)
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, reg)

	got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a" || a.calls != 1 {
		t.Fatalf("text=%q a.calls=%d — want the head probed as a last resort", got, a.calls)
	}
}

func TestExhaustedChainReportsClearly(t *testing.T) {
	a, b := failProv("a", 402), failProv("b", 402)
	p := New([]*Member{member(1, "a", 10, a), member(2, "b", 5, b)}, NewRegistry(nil, nil))

	_, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want it to wrap ErrExhausted", err)
	}
}

// 같은 Rank 멤버는 돌아가며 앞장을 섭니다. 그래서 중복 키가 부하를 나눕니다.
func TestEqualRankRotates(t *testing.T) {
	a, b := okProv("a", "a"), okProv("b", "b")
	head := member(1, "head", RankActive, failProv("head", 402))
	p := New([]*Member{head, member(2, "a", 5, a), member(3, "b", 5, b)}, NewRegistry(nil, nil))

	first := map[string]int{}
	for range 4 {
		txt, _ := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
		first[txt]++
	}
	if first["a"] == 0 || first["b"] == 0 {
		t.Fatalf("same-rank members did not rotate: %v", first)
	}
}

// 머리는 자기 Rank 를 유지하고 회전 묶음에 들어가지 않습니다.
func TestHeadAlwaysFirst(t *testing.T) {
	head := okProv("head", "H")
	p := New([]*Member{
		member(1, "head", RankActive, head),
		member(2, "a", 5, okProv("a", "a")),
		member(3, "b", 5, okProv("b", "b")),
	}, NewRegistry(nil, nil))
	for range 5 {
		if txt, _ := drain(p.Stream(context.Background(), llm.CompletionRequest{})); txt != "H" {
			t.Fatalf("head was not tried first, got %q", txt)
		}
	}
}

// 멤버가 하나인 사슬은 맨 provider 와 정확히 같아야 합니다.
func TestSingleMemberPassthrough(t *testing.T) {
	a := okProv("a", "solo")
	p := New([]*Member{member(1, "a", RankActive, a)}, NewRegistry(nil, nil))
	got, err := drain(p.Stream(context.Background(), llm.CompletionRequest{}))
	if err != nil || got != "solo" {
		t.Fatalf("got %q / %v, want solo / nil", got, err)
	}
}

func TestNewEmptyChainIsNil(t *testing.T) {
	if New(nil, nil) != nil {
		t.Fatal("empty chain should yield a nil pool so callers use the bare provider")
	}
}

// 쉬는 시간이 지난 창은 설정을 사슬 밖에 두면 안 됩니다.
func TestExpiredWindowIsClosed(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.Restore(1, State{Trips: 1, OpenUntil: time.Now().Add(-time.Second)})
	if reg.IsOpen(1) {
		t.Fatal("expired window should read as closed")
	}
}
