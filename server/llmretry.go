package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 재시도 정책의 서버 측 해석이다. docs/LLM재시도설계.md 참고. 이 정책은 엔진의 재시도만 다루며 자산 그래프와 탐색 그래프를 바꾸지 않는다. 다섯 층 가운데:
//   - 연결 수립 / 빈 응답 / 같은 provider 안전 창은 「엔드포인트를 따라가는」 것이며, LLM 설정마다 덮어쓸 수 있다
//     전역 기본값(profile 의 어느 항목을 비우면 전역을 상속하고, 전역에도 없으면 내장 기본값을 쓴다).
//   - 차단기 / 의도 재실행은 프로세스 단위이며, 전역에 하나만 있다.
//
// 전역 정책은 DB 의 settings 한 행을 한 번 읽는다. 호출 지점은 모두 저빈도 경로(provider 를 만들 때, work 를 마무리할 때,
// 설정을 저장할 때)라서 캐시를 한 층 더 둘 가치가 없다. 차단기 매개변수는 예외다. 실패 경로에서 매번 읽히므로
// applyRetryPolicy 가 Registry 에 밀어 넣어 저장한다.

// retryPolicy는 전역 정책을 읽습니다. DB가 nil이면 영 정책입니다(모든
// 층이 내장 기본값을 씀).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry는 전역 정책 위에 설정 하나의 덮어쓰기를 얹고
// agent.Config가 가지는 형태로 바꿉니다. 규칙은 필드마다
// 합칩니다. 간격만 고정한 설정도 횟수는 전역을 물려받습니다.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 횟수는 여기서 「0=기본 / 음수=끄기」라는 원래 의미를 유지한다. SDK 의 MaxRetries /
		// EmptyResponseRetries 와 완전히 같은 구조이므로, 그쪽이 알아서 해석하게 두면 된다.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry는 DB에서 읽은 설정의 cfg.Retry를 채웁니다.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 차단기(폴링 냉각)의 기본값이며, llmpool 에 내장된 값과 같다. 여기서는 「사용자가 값을 넣었을」 때만 덮어쓴다.
// 의도 재실행의 기본값은 engine.go 의 modelErrorRetries / modelErrorRetryBackoff 를 본다.

// applyRetryPolicy는 정책의 프로세스 전역 층을, 핫 패스에서 그것을 쓰는
// 대상에 넣습니다. 바로 차단기 레지스트리입니다. 시작 때와
// 정책을 저장할 때마다 부릅니다.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy는 의도 수준 재생 손잡이(⑤층)를 고릅니다.
// model_error 일을 몇 번 다시 돌릴지, 사이에 얼마나 쉴지입니다.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit은 일 하나가 빈 턴 이어가기를 몇 번까지
// 주입한다(steerHooks.Stop 참고). 일부러 ②층의 손잡이를 다시 쓴다 —— 「빈 응답
// 재시도 횟수」: 둘은 같은 일의 두 가지 수단이다. SDK 그 층은 「내용 블록이 하나도 없음」을 맡고, 수단은
// 같은 요청을 그대로 다시 보내는 것이다. 여기서는 「생각만 있고 본문도 도구도 없음」을 맡으며, 수단은 지시 한 줄을 덧붙여
// 모델이 이미 한 생각을 들고 이어서 하게 한다(문맥 모양이 정한 이런 공회전에는 그대로 재전송이 의미가 없다). 빔 판정 기준이
// 다른 까닭은 SDK 는 「이벤트를 yield 한 적이 있는지」를 기준으로 하고, 생각의 증분 자체가 이벤트이기 때문이다. 그러나 사용자가
// 「빈 응답을 몇 번 재시도할지」를 정할 때 나타내려는 뜻은 「모델이 실질 내용을 내지 않으면 한 번 더」이므로, 두 층이 횟수 하나를 함께 써야
// 그 이해와 맞는다.
//
// 어떤 profile 의 덮어쓰기가 아니라 전역 정책을 읽는다. 한 run 도중에 장애 조치로 profile 이 바뀔 수 있고, 이것은
// 의도 전체의 총량 게이트라서 엔드포인트가 바뀐다고 같이 바뀌면 안 된다. 의미는 SDK 의 emptyRetries() 와 같은 꼴이다.
// 0 = 기본값 defaultEmptyTurnNudges;-1(음수) = 공회전 이어가기를 끔;>0 = 그 값을 쓴다.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
