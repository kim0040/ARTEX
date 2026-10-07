// Package llmpool 은 LLM 설정을 순서대로 넘기는 장식자입니다.
//
// 초보: 플래너·워커·메인 에이전트가 쓰는 설정을 한 줄로 잇습니다. 잔액이 없거나
// 키가 죽었거나 호출 한도를 넘으면 다음 설정으로 넘어갑니다. 서버가 llm_profiles 에서
// 이 줄을 만들고, 프로세스 전체가 하나를 공유합니다. 한 작업이 잔액 소진을 보면
// 다른 작업도 그 설정을 바로 건너뜁니다. 그 사실은 로그와 상태 화면에 남습니다.
package llmpool

import (
	"sync"
	"time"
)

// backoff 는 차단 후 쉬는 시간 사다리입니다. 트립 횟수로 고릅니다. 첫 트립은
// 1분, 둘째는 5분, 그 뒤는 30분입니다. 호출 한도만 걸린 설정은 빨리 돌아오고,
// 계속 실패하는 설정은 매 라운드마다 두드리지 않습니다.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// State 는 설정 하나의 회로 차단기 상태입니다.
// 초보: 회로 차단기는 고장 난 백엔드를 잠시 건너뛰는 스위치입니다. 열려 있으면
// 그 설정을 쓰지 않고, 쉬는 시간이 지나면 다시 시도합니다.
type State struct {
	Fails     int       // 연속 실패. 성공하면 0 으로 돌아갑니다
	Trips     int       // 트립 누적. 쉬는 시간 사다리의 칸을 고릅니다
	OpenUntil time.Time // 0 이거나 과거면 닫힘(쓸 수 있음)
	LastError string
	LastAt    time.Time
}

// Open 은 지금 차단기가 열려 있는지 보고합니다. 열려 있으면 그 설정을 건너뜁니다.
func (s State) Open() bool { return time.Now().Before(s.OpenUntil) }

// softTripAfter 는 일시적 실패(429 / 5xx / 네트워크)가 이만큼 연속되면
// 차단기를 여는 기본값입니다. 잔액 없음·나쁜 키처럼 원인이 분명한 실패는
// 횟수와 상관없이 한 번에 엽니다. Registry.SetPolicy 로 바꿀 수 있습니다.
const softTripAfter = 3

// Registry 는 설정 id 마다 회로 차단기 상태를 담습니다.
// 프로세스 전체에서 하나이고, Pool 을 다시 만들어도 남습니다. 관계없는 설정을
// 저장하거나 토글을 바꿔 Pool 을 다시 짜도, 어느 백엔드가 고장인지 배운 내용은
// 지워지지 않습니다.
type Registry struct {
	mu sync.Mutex
	m  map[int64]*State

	// persist / forget 은 쉬는 시간이 재시작 뒤에도 남도록 DB 에 상태를 비춥니다.
	// 둘 다 nil 일 수 있습니다(DB 없음). 둘 다 요청의 뜨거운 경로 밖에서 부릅니다.
	persist func(id int64, st State)
	forget  func(id int64)

	// softTrip / cooldown 은 운영자가 덮어쓴 값입니다(0 = 내장 기본값 / 사다리).
	// 실패마다 설정을 다시 읽지 않고 여기 둡니다. Trip 은 모든 요청의 실패 경로에서 돕니다.
	softTrip int
	cooldown time.Duration
}

// SetPolicy 는 차단기의 두 손잡이를 바꿉니다. softTrip 은 일시적 실패가 몇 번
// 연속이면 여는지입니다(0 = 기본 softTripAfter, 음수 = 일시적 실패로는 열지 않고
// 원인이 분명한 실패만 엽니다). cooldown 은 고정 쉬는 시간입니다(0 = 1분/5분/30분
// 사다리). 언제든 호출해도 안전합니다.
func (r *Registry) SetPolicy(softTrip int, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.softTrip, r.cooldown = softTrip, cooldown
}

// tripAfter 는 실제로 쓰는 연속 일시 실패 문턱입니다. 호출자는 r.mu 를 잡고 있습니다.
// 음수 덮어쓰기는 0 이 되고, Trip 은 그것을 "일시 트립 안 함"으로 읽습니다.
func (r *Registry) tripAfter() int {
	if r.softTrip == 0 {
		return softTripAfter
	}
	return max(r.softTrip, 0)
}

// coolFor 는 trips 번째 트립(1부터)의 쉬는 시간입니다. 고정값이 있으면 그것을,
// 없으면 사다리(마지막 칸은 반복)를 씁니다. 호출자는 r.mu 를 잡고 있습니다.
func (r *Registry) coolFor(trips int) time.Duration {
	if r.cooldown > 0 {
		return r.cooldown
	}
	if trips-1 < len(backoff) {
		return backoff[trips-1]
	}
	return backoff[len(backoff)-1]
}

// NewRegistry 는 빈 등록부를 만듭니다. persist/forget 은 nil 일 수 있습니다.
func NewRegistry(persist func(id int64, st State), forget func(id int64)) *Registry {
	return &Registry{m: map[int64]*State{}, persist: persist, forget: forget}
}

// Restore 는 시작 때 DB 에서 읽은 상태를 심습니다. 다시 저장하지는 않습니다.
func (r *Registry) Restore(id int64, st State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := st
	r.m[id] = &cp
}

// Get 은 설정 하나의 상태 복사본을 돌려줍니다.
func (r *Registry) Get(id int64) State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.m[id]; st != nil {
		return *st
	}
	return State{}
}

// IsOpen 은 그 설정이 쉬는 시간 안에 있는지 보고합니다.
func (r *Registry) IsOpen(id int64) bool { return r.Get(id).Open() }

// Pass 는 성공한 호출을 기록합니다. 실패 카운터를 지워, 회복한 설정을 다시
// 완전히 믿습니다. 저장된 행도 지웁니다.
func (r *Registry) Pass(id int64) {
	r.mu.Lock()
	st := r.m[id]
	if st == nil || (st.Fails == 0 && st.Trips == 0 && st.OpenUntil.IsZero()) {
		r.mu.Unlock()
		return // 이미 깨끗함 — 쓸 것이 없음
	}
	delete(r.m, id)
	forget := r.forget
	r.mu.Unlock()
	if forget != nil {
		forget(id)
	}
}

// Trip 은 실패한 호출을 기록합니다. hard=true 는 원인이 분명한 실패(잔액 없음,
// 잘못된 키, 없는 모델)라 바로 차단기를 엽니다. hard=false 는 일시적 실패
// (429 / 5xx / 네트워크)라 softTripAfter 번 연속이어야 엽니다. 이번 호출이
// 차단기를 연 당사자면 true 를 돌려, 호출자가 로그를 한 번만 남기게 합니다.
func (r *Registry) Trip(id int64, errMsg string, hard bool) (tripped bool) {
	r.mu.Lock()
	st := r.m[id]
	if st == nil {
		st = &State{}
		r.m[id] = st
	}
	st.Fails++
	st.LastError = errMsg
	st.LastAt = time.Now()
	softTrip := r.tripAfter()
	if hard || (softTrip > 0 && st.Fails >= softTrip) {
		st.Trips++
		st.OpenUntil = time.Now().Add(r.coolFor(st.Trips))
		st.Fails = 0 // 이번 트립에 반영됨. 반열림 탐색은 0부터 다시 셉니다
		tripped = true
	}
	snap := *st
	persist := r.persist
	r.mu.Unlock()
	if persist != nil {
		persist(id, snap)
	}
	return tripped
}

// Reset 은 설정 하나의 상태를 지웁니다. 화면의 "바로 복구" 동작입니다.
func (r *Registry) Reset(id int64) {
	r.mu.Lock()
	delete(r.m, id)
	forget := r.forget
	r.mu.Unlock()
	if forget != nil {
		forget(id)
	}
}

// Snapshot 은 상태 API 용으로, 추적 중인 상태 전체의 복사본을 돌려줍니다.
func (r *Registry) Snapshot() map[int64]State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]State, len(r.m))
	for id, st := range r.m {
		out[id] = *st
	}
	return out
}
