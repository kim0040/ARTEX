package server

import (
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 설정이 하나도 없으면 해석 결과는 모두 0이어야 한다 —— 곧 SDK와 task_llm 각자의 내장 기본값이며,
// 「재시도를 설정할 수 있음」 기능이 나오기 전과 바이트마다 같다.
func TestResolveRetryUnconfigured(t *testing.T) {
	got := resolveRetry(db.RetryOverride{}, db.LLMRetryPolicy{})
	if got != (agent.RetryConfig{}) {
		t.Fatalf("resolveRetry=%+v, want zero", got)
	}
	retries, backoff := sameProviderRetryPolicy(got)
	if retries != sameProviderStreamRetries {
		t.Fatalf("retries=%d, want %d", retries, sameProviderStreamRetries)
	}
	if d := backoff(1); d != time.Second {
		t.Fatalf("backoff(1)=%v, want 1s (the default ladder)", d)
	}
}

// 아무것도 덮어쓰지 않은 설정에는 전역 정책이 적용됩니다.
func TestResolveRetryFromGlobal(t *testing.T) {
	pol := db.LLMRetryPolicy{
		Connect: db.RetryRule{Attempts: 5, IntervalMS: 2000},
		Empty:   db.RetryRule{Attempts: -1},
		Stream:  db.RetryRule{Attempts: 4, IntervalMS: 1500},
	}
	got := resolveRetry(db.RetryOverride{}, pol)
	want := agent.RetryConfig{
		ConnectAttempts: 5, ConnectInterval: 2 * time.Second,
		EmptyAttempts:  -1,
		StreamAttempts: 4, StreamInterval: 1500 * time.Millisecond,
	}
	if got != want {
		t.Fatalf("resolveRetry=%+v, want %+v", got, want)
	}
	retries, backoff := sameProviderRetryPolicy(got)
	if retries != 4 {
		t.Fatalf("retries=%d, want 4", retries)
	}
	for _, attempt := range []int{0, 3} {
		if d := backoff(attempt); d != 1500*time.Millisecond {
			t.Fatalf("backoff(%d)=%v, want a fixed 1.5s", attempt, d)
		}
	}
}

// 설정 덮어쓰기는 필드마다 이깁니다. 고정한 횟수는 전역을 바꾸고,
// 가만히 둔 간격은 여전히 전역 정책에서 옵니다.
func TestResolveRetryProfileOverridesPerField(t *testing.T) {
	pol := db.LLMRetryPolicy{
		Connect: db.RetryRule{Attempts: 5, IntervalMS: 2000},
		Stream:  db.RetryRule{Attempts: 4, IntervalMS: 1500},
	}
	over := db.RetryOverride{
		Connect: db.RetryRule{Attempts: 1},
		Stream:  db.RetryRule{IntervalMS: 250},
	}
	got := resolveRetry(over, pol)
	if got.ConnectAttempts != 1 || got.ConnectInterval != 2*time.Second {
		t.Fatalf("connect=%d/%v, want 1 attempt inheriting the 2s interval", got.ConnectAttempts, got.ConnectInterval)
	}
	if got.StreamAttempts != 4 || got.StreamInterval != 250*time.Millisecond {
		t.Fatalf("stream=%d/%v, want the global 4 attempts with a pinned 250ms", got.StreamAttempts, got.StreamInterval)
	}
}

// -1은 같은 프로바이더 층을 통째로 끕니다(재시도 0). "안 넣음"으로
// 오해하면 안 됩니다.
func TestSameProviderRetryDisabled(t *testing.T) {
	retries, _ := sameProviderRetryPolicy(agent.RetryConfig{StreamAttempts: -1})
	if retries != 0 {
		t.Fatalf("retries=%d, want 0 (disabled)", retries)
	}
}

// 멀쩡한 범위를 넘는 값은 DB에 넣기 전에 자릅니다. 적대적이거나
// 잘못 누른 본문이 워커를 하루 동안 세워 두지 못하게 합니다.
func TestRetryRuleClamp(t *testing.T) {
	pol := db.LLMRetryPolicy{
		Connect: db.RetryRule{Attempts: -50, IntervalMS: -1},
		Stream:  db.RetryRule{Attempts: 9999, IntervalMS: 99_999_999},
	}
	got := resolveRetry(db.RetryOverride{}, pol.Clamped())
	if got.ConnectAttempts != -1 || got.ConnectInterval != 0 {
		t.Fatalf("connect=%d/%v, want -1 attempts and no interval", got.ConnectAttempts, got.ConnectInterval)
	}
	if got.StreamAttempts != 20 || got.StreamInterval != time.Hour {
		t.Fatalf("stream=%d/%v, want the 20-attempt / 1h caps", got.StreamAttempts, got.StreamInterval)
	}
}
