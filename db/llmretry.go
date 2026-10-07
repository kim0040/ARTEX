package db

import (
	"encoding/json"
	"time"
)

// LLM 재시도 전략: 다섯 계층 재시도의 「횟수 + 간격」 전역 설정. docs/LLM재시도설계.md 를 참고한다.
// settings 테이블의 JSON 값 하나에 둔다 —— 장비 전체에서 한 벌인 실행 매개변수라 이를 위해 테이블을 따로 만들 가치가 없다.
// 읽기는 내장 기본값으로 폴백하므로, 키가 없을 때(새 데이터베이스이거나 한 번도 설정하지 않았을 때) 동작은 상수를 코드에 고정해 두던 때와 완전히 같다.

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule 은 한 층의 조절 값 쌍이다. 제로 값은 "unset" 을 뜻한다:
//
//	Attempts   0 = 내장 기본 횟수를 쓴다; -1 = 그 층 재시도를 끈다; >0 = 그 값을 쓴다
//	IntervalMS 0 = 그 층 원래의 간격 전략을 쓴다(보통 지수 백오프); >0 = 고정 밀리초 간격으로 바꾼다
//
// -1 은 「명시적으로 끔」이고 「0 회」가 아니다. 0 은 이미 「미설정」이 차지하기 때문이다.
// 이 값은 워커가 탐색 그래프의 의도를 실행할 때 LLM 호출을 몇 번, 어느 간격으로 다시 시도할지 정한다.
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy 는 다섯 층 재시도 설정을 담는다. Connect/Empty/Stream 은
// 요청 단위 층이다(프로파일이 덮어쓸 수 있다. LLMProfile.Retry 를 본다).
// Breaker 와 Intent 는 본질적으로 프로세스 전역이라 여기에만 있다.
// 이 정책은 워커가 탐색 그래프에서 모델 호출과 의도 재실행을 어디까지 다시 시도할지 한곳에 모은다.
type LLMRetryPolicy struct {
	// Connect: SDK 연결 수립 재시도(연결 리셋/시간 초과/429/5xx, 스트림이 시작되기 전). 기본 3회, 지수 백오프.
	Connect RetryRule `json:"connect"`
	// Empty: SDK 빈 응답 재시도(완료됐지만 content block 이 없음, openai 형식만). 기본 2회, 지수 백오프.
	Empty RetryRule `json:"empty"`
	// Stream: 같은 provider 의 안전 구간 재시도(출력을 넘기기 전에 끊긴 스트림을 다시 재생). 기본 2회, 0.5s 부터 지수(상한 4s).
	Stream RetryRule `json:"stream"`
	// Breaker: 폴링 차단. Attempts=연속된 순간 실패가 몇 번이면 차단을 켜는지(기본 3, -1=순간 실패로는 차단하지 않음,
	// 잔액 부족/키 무효 같은 하드 실패는 여전히 즉시 차단한다); IntervalMS=고정 냉각 시간(0=기본 1/5/30min 단계).
	Breaker RetryRule `json:"breaker"`
	// Intent: worker 가 model_error 로 끝난 뒤 의도 전체를 다시 실행한다. 기본 2회, 고정 3s.
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
