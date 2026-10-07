package llmpool

import (
	"testing"
	"time"
)

// 설정된 일시 트립 문턱이 기본값을 바꿉니다. 일시적 실패 두 번이면 충분하고,
// 고정 쉬는 시간이 1/5/30분 사다리를 대신합니다.
func TestSetPolicyOverridesThresholdAndCooldown(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.SetPolicy(2, 90*time.Second)
	if reg.Trip(1, "429", false) {
		t.Fatal("tripped on the first transient failure, want the second")
	}
	if !reg.Trip(1, "429", false) {
		t.Fatal("should trip on transient failure #2")
	}
	st := reg.Get(1)
	if d := time.Until(st.OpenUntil); d < 80*time.Second || d > 90*time.Second {
		t.Fatalf("cooldown=%v, want ~90s", d)
	}
	// 이후 트립도 사다리를 오르지 않고 같은 고정 창을 유지합니다.
	reg.Trip(1, "429", true)
	st = reg.Get(1)
	if d := time.Until(st.OpenUntil); d < 80*time.Second || d > 90*time.Second {
		t.Fatalf("second cooldown=%v, want ~90s (fixed)", d)
	}
}

// 음수 문턱은 일시 트립을 완전히 끕니다. 일시적 실패는 차단기를 열지 않고,
// 원인이 분명한 실패는 여전히 바로 엽니다.
func TestSetPolicyDisablesSoftTrip(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.SetPolicy(-1, 0)
	for i := range 10 {
		if reg.Trip(1, "429", false) {
			t.Fatalf("transient failure #%d tripped the breaker, want never", i+1)
		}
	}
	if reg.IsOpen(1) {
		t.Fatal("breaker should stay closed for transient failures")
	}
	if !reg.Trip(1, "no credit", true) {
		t.Fatal("a hard failure must still trip immediately")
	}
	if d := time.Until(reg.Get(1).OpenUntil); d < 50*time.Second || d > 60*time.Second {
		t.Fatalf("cooldown=%v, want the default first rung (~1min)", d)
	}
}

// 0 정책은 예전 동작입니다. 일시적 실패 3번, 사다리 쉬는 시간.
func TestZeroPolicyKeepsDefaults(t *testing.T) {
	reg := NewRegistry(nil, nil)
	reg.SetPolicy(0, 0)
	for i := 1; i < softTripAfter; i++ {
		if reg.Trip(1, "429", false) {
			t.Fatalf("tripped after %d transient failures, want %d", i, softTripAfter)
		}
	}
	if !reg.Trip(1, "429", false) {
		t.Fatalf("should trip on failure #%d", softTripAfter)
	}
	if d := time.Until(reg.Get(1).OpenUntil); d < 50*time.Second || d > 60*time.Second {
		t.Fatalf("cooldown=%v, want the first ladder rung (~1min)", d)
	}
}
