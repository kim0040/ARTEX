package db

import (
	"testing"
	"time"
)

// 프로필의 재시도 덮어쓰기는 실제
// 열 목록을 왕복해야 한다. 열 여섯 개를 더한 뒤 scan/insert 순서가 어긋나면 여기서 잡힌다.
// 빈 api 키로 수정해도 행을 쓸 수 있다는 경로도 고정한다.
// 그 UPDATE는 매개변수 번호가 따로다.
func TestProfileRetryRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	want := RetryOverride{
		Connect: RetryRule{Attempts: 5, IntervalMS: 2000},
		Empty:   RetryRule{Attempts: -1},
		Stream:  RetryRule{IntervalMS: 250},
	}
	id, err := d.SaveProfile(&LLMProfile{
		Name: "t-retry-roundtrip", Format: "openai", Model: "m", APIKey: "k", Retry: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	got, err := d.ProfileByID(id)
	if err != nil || got == nil {
		t.Fatalf("ProfileByID: %v", err)
	}
	if got.Retry != want {
		t.Fatalf("retry=%+v, want %+v", got.Retry, want)
	}
	// 키 없는 수정 경로(사용자가 키를 다시 치지 않으면 UI가 키를 보내지 않는다).
	got.APIKey = ""
	got.Retry.Stream = RetryRule{Attempts: 3, IntervalMS: 700}
	if _, err := d.SaveProfile(got); err != nil {
		t.Fatal(err)
	}
	after, err := d.ProfileByID(id)
	if err != nil || after == nil {
		t.Fatalf("ProfileByID after update: %v", err)
	}
	if after.Retry.Stream != (RetryRule{Attempts: 3, IntervalMS: 700}) {
		t.Fatalf("stream=%+v after update", after.Retry.Stream)
	}
	if after.Retry.Connect != want.Connect || after.Retry.Empty != want.Empty {
		t.Fatalf("untouched rules changed: %+v", after.Retry)
	}
	// 목록 쿼리는 다른 열 목록을 읽는다. 서로 맞아야 한다.
	profiles, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.ID == id && p.Retry.Connect != want.Connect {
			t.Fatalf("ListProfiles retry=%+v, want %+v", p.Retry, want)
		}
	}
}

// 범위를 넘는 값은 넣을 때 잘라 둔다. 그래서 DB CHECK 제약이
// 사용자에게 들리는 오류가 되지 않는다.
func TestProfileRetryClampedOnSave(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	id, err := d.SaveProfile(&LLMProfile{
		Name: "t-retry-clamp", Format: "openai", Model: "m", APIKey: "k",
		Retry: RetryOverride{Connect: RetryRule{Attempts: -99, IntervalMS: -5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	got, err := d.ProfileByID(id)
	if err != nil || got == nil {
		t.Fatalf("ProfileByID: %v", err)
	}
	if got.Retry.Connect != (RetryRule{Attempts: -1}) {
		t.Fatalf("connect=%+v, want attempts -1 and no interval", got.Retry.Connect)
	}
}

// 전역 정책은 settings를 왕복하고, 설정되지 않은 키는
// "전부 내장 기본값"으로 다시 읽힌다.
func TestLLMRetryPolicyRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	before, hadBefore, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadBefore {
			d.SetSetting(settingLLMRetryPolicy, before)
		} else {
			d.Exec(`DELETE FROM settings WHERE key=$1`, settingLLMRetryPolicy)
		}
	})

	want := LLMRetryPolicy{
		Connect: RetryRule{Attempts: 4, IntervalMS: 1500},
		Breaker: RetryRule{Attempts: 2, IntervalMS: 90_000},
		Intent:  RetryRule{Attempts: -1},
	}
	if err := d.SetLLMRetryPolicy(want); err != nil {
		t.Fatal(err)
	}
	got := d.LLMRetryPolicy()
	if got != want {
		t.Fatalf("policy=%+v, want %+v", got, want)
	}
	if d := got.Breaker.Interval(); d != 90*time.Second {
		t.Fatalf("breaker interval=%v, want 90s", d)
	}

	// 범위를 벗어난 값을 써도 구간 안으로 다시 맞추며, 읽어 낸 값은 맞춘 뒤의 값이다.
	if err := d.SetLLMRetryPolicy(LLMRetryPolicy{Stream: RetryRule{Attempts: 999, IntervalMS: 99_999_999}}); err != nil {
		t.Fatal(err)
	}
	if got := d.LLMRetryPolicy().Stream; got.Attempts != 20 || got.IntervalMS != 3600_000 {
		t.Fatalf("stream=%+v, want the 20 / 1h caps", got)
	}

	// 키가 없으면 = 모두 기본값.
	if _, err := d.Exec(`DELETE FROM settings WHERE key=$1`, settingLLMRetryPolicy); err != nil {
		t.Fatal(err)
	}
	if got := d.LLMRetryPolicy(); got != (LLMRetryPolicy{}) {
		t.Fatalf("unset policy=%+v, want zero", got)
	}
}
