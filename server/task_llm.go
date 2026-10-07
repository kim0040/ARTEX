package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type taskAgentBundle struct {
	runtime        *taskLLMRuntime // 목표 분해용 런타임
	plannerRuntime *taskLLMRuntime
	workerRuntime  *taskLLMRuntime
	mainRuntime    *taskLLMRuntime
	pl             *agent.Planner
	wk             *agent.Worker
	main           *agent.MainAgent
}

type llmAuditProfile struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Format string `json:"format"`
	Model  string `json:"model"`
}

type llmTransitionAudit struct {
	Mode     string           `json:"mode"` // automatic=자동 | manual=수동 | exhausted=한도 소진
	Reason   string           `json:"reason"`
	Previous *llmAuditProfile `json:"previous,omitempty"`
	Next     *llmAuditProfile `json:"next,omitempty"`
}

type llmActivityMetadata struct {
	LLMTransition llmTransitionAudit `json:"llm_transition"`
}

type taskLLMRuntime struct {
	s        *Server
	taskID   string
	agentKey string
}

type taskLLMError struct {
	taskID         string
	chainExhausted bool
	cause          error
}

func (e *taskLLMError) Error() string {
	if e.chainExhausted {
		return fmt.Sprintf("task %s LLM profile chain exhausted: %v", e.taskID, e.cause)
	}
	return e.cause.Error()
}

func (e *taskLLMError) Unwrap() error { return e.cause }

func isTaskLLMRuntimeError(err error) bool {
	var target *taskLLMError
	return errors.As(err, &target)
}

func isTaskLLMChainExhausted(err error) bool {
	var target *taskLLMError
	return errors.As(err, &target) && target.chainExhausted
}

// isQuotaExhaustedError는 일부러 엄격합니다. 일반 429, 인증 오류,
// 네트워크 실패, 5xx는 프로바이더를 바꾸지 않습니다. 응답이 명시적으로
// 할당량, 크레딧, 청구 잔액, 결제 소진을 말해야 합니다.
func isQuotaExhaustedError(err error) bool {
	return err != nil && agent.IsQuotaExhaustedMessage(err.Error())
}

type taskLLMSelection struct {
	task      *Task
	profileID int64
	revision  int64
	provider  llm.Provider
	// retry는 이번에 고른 설정에서 풀어 낸 재시도 매개변수이다(profile 덮어쓰기 → 전역 정책 → 내장 기본값).
	// 같은 provider의 안전 구간 재시도는 이것을 따른다. 그래서 profile을 바꾸면 재시도 리듬도 한 세트가 바뀐다.
	retry agent.RetryConfig
}

type taskLLMStreamHooks struct {
	current    func() (taskLLMSelection, error)
	exhaust    func(taskLLMSelection, error) (db.TaskLLMTransition, error)
	transition func(taskLLMSelection, db.TaskLLMTransition, error)
}

// current는 이 역할이 돌 프로바이더를 이 순서로 고릅니다.
// Agent 바인딩 → 작업 LLM 설정 체인 → 전역/환경 설정.
// 바인딩은 작업 체인보다 우선한다: 어떤 역할에 모델이 명시되면 그 모델에서 계속 돈다. 바인딩이 없거나
// 구성에 실패할 때만 작업 체인으로 내리고, 작업 체인이 비면 다시 전역 설정으로 내린다.
// 반환된 profile id는 작업 체인을 탈 때만 0이 아니다 —— streamTaskLLM은 이것으로 한도 오류가
// 작업의 장애 조치 상태를 진행해야 하는지 판단한다(바인딩/전역 경로는 작업 체인 상태를 바꾸지 않고, 기존 의미를 그대로 쓴다).
func (r *taskLLMRuntime) current() (taskLLMSelection, error) {
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return taskLLMSelection{}, err
	}
	pt, err := r.s.m.pg.GetTask(taskNum)
	if err != nil {
		return taskLLMSelection{}, err
	}
	if pt == nil {
		return taskLLMSelection{}, fmt.Errorf("작업 %s 을(를) 찾을 수 없습니다", r.taskID)
	}
	r.s.syncTaskLLMState(pt)
	t, _ := r.s.m.Task(r.taskID)
	sel := taskLLMSelection{task: t, revision: pt.LLMChainRevision}
	if prov, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		sel.provider, sel.retry = prov, cfg.Retry
		return sel, nil
	}
	if len(pt.LLMProfileIDs) > 0 {
		if pt.ActiveLLMProfileID == nil {
			return sel, &taskLLMError{taskID: r.taskID, chainExhausted: true, cause: errors.New("선택한 구성의 사용량이 모두 소진되었습니다")}
		}
		sel.profileID = *pt.ActiveLLMProfileID
		prov, cfg, ok := r.s.providerForProfile(sel.profileID)
		if !ok {
			return sel, fmt.Errorf("LLM profile #%d is missing or invalid", sel.profileID)
		}
		sel.provider, sel.retry = prov, cfg.Retry
		return sel, nil
	}
	prov, cfg, ok := r.s.globalProvider()
	if !ok {
		return sel, fmt.Errorf("task %s has no available fallback LLM provider", r.taskID)
	}
	sel.provider, sel.retry = prov, cfg.Retry
	return sel, nil
}

// activeCfg는 작업이 지금 쓰는 LLM 설정을 고릅니다. current()와
// 같은 출처 순서입니다(에이전트 바인딩 → 활성 체인 설정 → 전역). 읽기 전용이고
// 실패해도 흐름은 계속합니다. 아무것도 안 풀리면 ok=false이고, 설정별 폴백은
// 호출자에게 맡깁니다. 장애 조치로 설정이 바뀌면, 다음
// 에이전트 실행부터 적용됩니다(captureRun이 실행마다 세션을 새로 만듦).
func (r *taskLLMRuntime) activeCfg() (agent.Config, bool) {
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return agent.Config{}, false
	}
	pt, err := r.s.m.pg.GetTask(taskNum)
	if err != nil || pt == nil {
		return agent.Config{}, false
	}
	if _, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		return cfg, true
	}
	if len(pt.LLMProfileIDs) > 0 && pt.ActiveLLMProfileID != nil {
		if _, cfg, ok := r.s.providerForProfile(*pt.ActiveLLMProfileID); ok {
			return cfg, true
		}
	}
	if _, cfg, ok := r.s.globalProvider(); ok {
		return cfg, true
	}
	return agent.Config{}, false
}

// nonStreaming은 작업이 지금 쓰는 LLM 출처가 비스트리밍인지 알려 줍니다.
// 못 풀면 스트리밍입니다(false). 안전한 기본값.
func (r *taskLLMRuntime) nonStreaming() bool {
	cfg, ok := r.activeCfg()
	return ok && !cfg.Stream
}

// maxTokens는 지금 쓰는 출처의 답 하나 출력 상한입니다.
// 못 풀면 0, 즉 상한을 안 보냅니다. 설정이 생기기 전과 같습니다.
func (r *taskLLMRuntime) maxTokens() int {
	cfg, _ := r.activeCfg() // 설정을 풀지 못하면 제로 값 0이다
	return cfg.MaxTokens
}

func parseTaskID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("작업 id %q 이(가) 올바르지 않습니다", id)
	}
	return n, nil
}

// streamHooks는 Stream과 Complete가 같이 쓰는 장애 조치/소진 콜백을 만듭니다.
// 지금 설정 선택을 어떻게 읽고, 할당량 소진을 어떻게 표시하고,
// 장애 조치 전환을 어떻게 낼지입니다.
func (r *taskLLMRuntime) streamHooks() taskLLMStreamHooks {
	return taskLLMStreamHooks{
		current: r.current,
		exhaust: func(selection taskLLMSelection, cause error) (db.TaskLLMTransition, error) {
			taskNum, _ := parseTaskID(r.taskID)
			transition, err := r.s.m.pg.MarkTaskLLMProfileQuotaExhaustedAtRevision(taskNum, selection.profileID, selection.revision, cause.Error())
			if err != nil {
				return transition, err
			}
			if pt, getErr := r.s.m.pg.GetTask(taskNum); getErr == nil && pt != nil {
				r.s.syncTaskLLMState(pt)
			}
			return transition, nil
		},
		transition: func(selection taskLLMSelection, transition db.TaskLLMTransition, cause error) {
			r.s.emitTaskLLMTransition(selection.task, transition, cause)
		},
	}
}

func (r *taskLLMRuntime) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	ctx = llmrec.WithTaskID(ctx, r.taskID)
	return streamTaskLLM(ctx, r.taskID, req, r.streamHooks())
}

// Complete는 Stream의 비스트리밍 짝입니다. 비스트리밍 호출은
// 원자적입니다. 중간 출력을 주지 않으므로, 실패는 모두 같은 프로바이더에서
// 다시 시도하거나 다음 설정으로 넘어가도 안전합니다. 모델 출력이나 도구 실행이
// 중복될 위험이 없습니다(스트리밍 경로의 "확정됨" 장부는
// 여기서는 필요 없습니다).
func (r *taskLLMRuntime) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx = llmrec.WithTaskID(ctx, r.taskID)
	return completeTaskLLM(ctx, r.taskID, req, r.streamHooks())
}

func completeTaskLLM(ctx context.Context, taskID string, req llm.CompletionRequest, hooks taskLLMStreamHooks) (llm.Message, string, llm.Usage, error) {
	for {
		selection, err := hooks.current()
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		var (
			msg     llm.Message
			sr      string
			usage   llm.Usage
			callErr error
		)
		// 같은 provider 안전 구간 재시도: 비스트리밍 호출은 전체 성공이거나 전체 실패이며,
		// 도중에 이미 출력을 넘긴 문제가 없다. 그래서 어떤 일시 실패도 그대로 재시도할 수 있다.
		retries, backoffOf := sameProviderRetryPolicy(selection.retry)
		for attempt := 0; ; attempt++ {
			msg, sr, usage, callErr = selection.provider.Complete(ctx, req)
			if callErr != nil && ctx.Err() == nil &&
				attempt < retries && isRetryableStreamError(callErr) {
				backoff := backoffOf(attempt)
				log.Printf("[task-llm] task %s 비스트리밍 호출 실패,%v 후 같은 provider로 재시도 (%d/%d): %v",
					taskID, backoff, attempt+1, retries, callErr)
				if sleepCtx(ctx, backoff) {
					break // 백오프 동안 ctx가 취소되면 → 재시도를 멈춘다
				}
				continue
			}
			break
		}
		if callErr == nil {
			return msg, sr, usage, nil
		}
		// profileID=0: 명시적 체인이 이미 비워짐; 할당량 오류가 아님: 그대로 전달. 둘 다 작업 failover 상태를 바꾸지 않는다.
		if selection.profileID == 0 || !isQuotaExhaustedError(callErr) {
			return llm.Message{}, "", llm.Usage{}, callErr
		}
		transition, markErr := hooks.exhaust(selection, callErr)
		if markErr != nil {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("mark profile quota exhausted after %v: %w", callErr, markErr)
		}
		if transition.Advanced && !transition.Stale && hooks.transition != nil {
			hooks.transition(selection, transition, callErr)
		}
		if !transition.Stale && transition.NextProfileID == nil {
			return llm.Message{}, "", llm.Usage{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: callErr}
		}
		// 호출자에게 전달된 출력이 전혀 없으므로, 다음 profile로 바꿔 같은 논리 요청을 다시 재생해도 안전하다.
	}
}

func streamTaskLLM(ctx context.Context, taskID string, req llm.CompletionRequest, hooks taskLLMStreamHooks) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for {
			selection, err := hooks.current()
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			committed := false
			var pending []llm.StreamEvent
			var streamErr error
			retries, backoffOf := sameProviderRetryPolicy(selection.retry)
			// 같은 provider 안전 구간 재시도: committed 이전(아직 호출자에게 어떤 출력도 전달하지 않음)
			// 의 순간 실패는 그대로 다시 재생할 수 있으며, 모델 출력이나 도구 실행이 중복되지 않는다. committed 이후,
			// ctx 취소, 또는 결정적/할당량 오류이면 빠져나와, 아래의 기존 전달/장애 전환 로직에 맡긴다.
			for attempt := 0; ; attempt++ {
				committed = false
				pending = nil
				streamErr = nil
				for event, err := range selection.provider.Stream(ctx, req) {
					if err != nil {
						streamErr = err
						break
					}
					if !committed && !streamEventCommitsOutput(event) {
						pending = append(pending, event)
						continue
					}
					if !committed {
						for _, buffered := range pending {
							if !yield(buffered, nil) {
								return
							}
						}
						pending = nil
						committed = true
					}
					if !yield(event, nil) {
						return
					}
				}
				if streamErr != nil && !committed && ctx.Err() == nil &&
					attempt < retries && isRetryableStreamError(streamErr) {
					backoff := backoffOf(attempt)
					log.Printf("[task-llm] task %s 제출 전 스트림 실패, %v 후 같은 provider로 재시도 (%d/%d): %v",
						taskID, backoff, attempt+1, retries, streamErr)
					if sleepCtx(ctx, backoff) {
						break // 백오프 동안 ctx가 취소되면 → 재시도를 멈춘다
					}
					continue
				}
				break
			}
			if streamErr == nil {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				return
			}
			// profileID=0은 이 안정 작업 묶음이 아직 쓰이는 동안
			// 명시적 체인이 지워졌다는 뜻입니다. 에이전트/전역 폴백 오류는 예전
			// 동작을 따르고, 작업 장애 조치 상태는 바꾸지 않습니다.
			if selection.profileID == 0 || !isQuotaExhaustedError(streamErr) {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				yield(llm.StreamEvent{}, streamErr)
				return
			}
			transition, markErr := hooks.exhaust(selection, streamErr)
			if markErr != nil {
				cause := fmt.Errorf("mark profile quota exhausted after %v: %w", streamErr, markErr)
				if committed {
					// 출력이 이미 도구 실행을 일으켰을 수 있습니다. 저장
					// 실패를 알리되, 라우터가 처리한 것으로 분류합니다. 워커가
					// 의도 전체를 다시 돌려 그 부작용을 중복하지 않게 합니다.
					yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, cause: cause})
				} else {
					yield(llm.StreamEvent{}, cause)
				}
				return
			}
			if transition.Advanced && !transition.Stale && hooks.transition != nil {
				hooks.transition(selection, transition, streamErr)
			}
			if committed || (!transition.Stale && transition.NextProfileID == nil) {
				yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: streamErr})
				return
			}
			// 호출자에게 이벤트가 아직 안 갔습니다. 그래서 같은 논리 요청을
			// 다음 설정에서 다시 해도 모델 출력이나 도구 실행이 중복되지 않습니다.
		}
	}
}

func streamEventCommitsOutput(event llm.StreamEvent) bool {
	switch event.Type {
	case llm.SETextDelta, llm.SEThinkingDelta, llm.SEToolInputJSON:
		return event.Text != ""
	case llm.SEToolUseStart, llm.SEMessageDelta, llm.SEMessageStop:
		return true
	default:
		return false
	}
}

// 제출 전 안전 구간 안에서, 같은 provider에 대한 【기본】 재시도 횟수. SDK의 doStream은 연결 수립만 재시도하는
// 단계(200을 받기 전); 스트림이 시작되면, 중간 단절 / overloaded / 스트림 안 429 같은 순간 장애는 곧바로
// model_error로 올라오고 재시도는 0이다. 토큰이 하나도 아직 호출자에게 넘어가지 않았다면(!committed), 다시 재생하면
// 완전히 같은 요청은 모델 출력이나 도구 부작용을 중복하지 않는다. 그래서 여기서 같은 provider 백오프 재시도를 한 겹 보태,
// 이런 흔들림을 의도 전체 재실행 전에 막는다. LLM 설정의 재시도로 덮어쓰거나 전역 재시도 정책으로 바꿀 수 있다.
const sameProviderStreamRetries = 2

// sameProviderRetryBackoff는 attempt번째 재시도 전의 【기본】 백오프(0.5s, 1s…, 상한 4s)이며,
// SDK의 지수 기울기와 같은 스타일이지만 상한이 더 작아, 워커의 마무리/취소 응답을 붙잡지 않는다.
// 변수 형태로 드러내어, 테스트에서 백오프를 0으로 두기 쉽게 한다.
var sameProviderRetryBackoff = func(attempt int) time.Duration {
	return min(500*time.Millisecond*(1<<attempt), 4*time.Second)
}

// sameProviderRetryPolicy는 이번 호출이 어느 같은 provider 재시도 매개변수를 쓸지 해석한다: 설정에 횟수가 있으면
// 설정의 값을 쓴다(음수 = 이 층 재시도를 끔). 간격이 있으면 지수 백오프를 고정 간격으로 바꾸고, 둘 다 없을 때는
// 설정 가능하게 바꾸기 전과 바이트 단위로 같다.
func sameProviderRetryPolicy(r agent.RetryConfig) (retries int, backoff func(int) time.Duration) {
	retries, backoff = sameProviderStreamRetries, sameProviderRetryBackoff
	if r.StreamAttempts != 0 {
		retries = max(r.StreamAttempts, 0)
	}
	if r.StreamInterval > 0 {
		d := r.StreamInterval
		backoff = func(int) time.Duration { return d }
	}
	return retries, backoff
}

// isRetryableStreamError는 「제출 전의 스트림 실패」를 같은 provider에서 다시 재생할 가치가 있는지 판단한다.
// 순간의 전송 끊김 / 공급자 과부하 / 속도 제한은 스스로 회복되므로 안전하게 재시도할 수 있다. 다음 세 종류는 재시도하지 않는다:
//   - 할당량 소진: profile 장애 전환에 맡기고, 여기서 재시도를 헛되이 태우지 않는다
//   - 컨텍스트가 너무 김: 같은 요청을 다시 재생해도 소용없으므로, harness의 reactive 압축이 받친다
//   - 4xx 결정적 거부(400/401/403/404/422): 어느 provider로 가도 똑같이 실패한다
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	if isQuotaExhaustedError(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "too long") || strings.Contains(s, "context length") ||
		strings.Contains(s, "context_length") || strings.Contains(s, "maximum context") ||
		strings.Contains(s, "status 413") {
		return false
	}
	for _, code := range []string{"status 400", "status 401", "status 403", "status 404", "status 422"} {
		if strings.Contains(s, code) {
			return false
		}
	}
	// 나머지(전송 reset/EOF/timeout, 408/429/5xx, 스트림 안 error 이벤트 예: anthropic
	// overloaded_error 등)는 모두 순간적인 것으로 보고, 재시도를 허용한다.
	return true
}

// CompactionWindow는 current()와 같은 순서를 봅니다. 컨텍스트 창이
// 그 역할이 실제로 스트리밍할 프로바이더와 항상 같게 합니다.
func (r *taskLLMRuntime) CompactionWindow() int {
	if _, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		return cfg.CompactionWindow()
	}
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return (agent.Config{}).CompactionWindow()
	}
	chain, err := r.s.m.pg.TaskLLMProfiles(taskNum)
	if err != nil {
		return (agent.Config{}).CompactionWindow()
	}
	if len(chain) == 0 {
		if _, cfg, ok := r.s.globalProvider(); ok {
			return cfg.CompactionWindow()
		}
		return (agent.Config{}).CompactionWindow()
	}
	minimum := 0
	for _, entry := range chain {
		if cfg, ok := r.s.loadProfileConfig(entry.ProfileID); ok {
			window := cfg.CompactionWindow()
			if minimum == 0 || window < minimum {
				minimum = window
			}
		}
	}
	if minimum == 0 {
		return (agent.Config{}).CompactionWindow()
	}
	return minimum
}

// agentBindingProvider는 역할이 명시적으로 묶인 설정을 고릅니다
// (agents.llm_profile_id). 작업 에이전트에서 가장 높은 우선순위입니다. ok=false는
// 바인딩이 없거나, 묶인 설정을 더 이상 만들 수 없을 때입니다. 그러면 호출자는
// 작업 체인으로 내려갑니다. 묶인 설정은 독점입니다. 다만
// llm_pool_bind_fallback이 켜지면 예외이고, 그것을 poolForBinding이 나타냅니다.
// 초보용: 플래너·워커·메인 에이전트에 화면에서 묶은 모델이 있으면, 엔진은 그 모델을 먼저 씁니다.
func (s *Server) agentBindingProvider(agentKey string) (llm.Provider, agent.Config, bool) {
	id := s.effectiveProfileForAgent(agentKey, nil)
	if id == nil {
		return nil, agent.Config{}, false
	}
	prov, cfg, ok := s.providerForProfile(*id)
	if !ok {
		return nil, agent.Config{}, false
	}
	return s.poolForBinding(*id, prov, cfg), cfg, true
}

// globalProvider는 프로세스 전역 프로바이더를 돌려줍니다(저장된 활성 설정 또는
// 환경 설정). 역할에 바인딩도 작업 체인도 없을 때의
// 마지막 수단입니다.
func (s *Server) globalProvider() (llm.Provider, agent.Config, bool) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if !s.llmOn || s.llmProv == nil {
		return nil, agent.Config{}, false
	}
	return s.llmProv, s.llmCfg, true
}

// taskRuntimeAvailable은 나열한 역할이 모두 프로바이더를 고를 수 있는지 알려 줍니다.
// current()의 런타임 순서입니다. 역할 자신의 바인딩, 그다음
// 작업 체인, 그다음 전역. 소진된 체인은 안 묶인
// 역할에게 하드 정지입니다. 전역으로 조용히 내려가지 않습니다. current()와 같습니다.
func (s *Server) taskRuntimeAvailable(t *Task, agentKeys ...string) bool {
	if t == nil || len(agentKeys) == 0 {
		return false
	}
	state := t.llmStateSnapshot()
	unboundReady := false
	if len(state.ProfileIDs) > 0 {
		if state.ActiveID != nil {
			_, _, unboundReady = s.providerForProfile(*state.ActiveID)
		}
	} else {
		_, _, unboundReady = s.globalProvider()
	}
	for _, key := range agentKeys {
		if _, _, ok := s.agentBindingProvider(key); ok {
			continue
		}
		if !unboundReady {
			return false
		}
	}
	return true
}

func (s *Server) invalidateTaskAgents() {
	s.taskAgentMu.Lock()
	s.taskAgents = map[string]*taskAgentBundle{}
	s.taskAgentMu.Unlock()
}

func (s *Server) agentsForTask(t *Task) *taskAgentBundle {
	s.taskAgentMu.Lock()
	defer s.taskAgentMu.Unlock()
	if bundle := s.taskAgents[t.ID]; bundle != nil {
		return bundle
	}
	goalRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "goals"}
	plannerRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "planner"}
	workerRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "worker"}
	mainRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "mainagent"}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	window := workerRuntime.CompactionWindow()
	wk := agent.NewWorker(workerRuntime, "task-router", s.m.dir, tx, window, s.agentMaxTurns("worker"))
	wk.SetFindingRecorder(s.evidenceStore())
	wk.SetCompactionWindowResolver(workerRuntime.CompactionWindow)
	wk.SetNonStreaming(workerRuntime.nonStreaming) // 작업의 현재 활성 profile 스트리밍 스위치에 따른다(매 라운드에 읽음)
	wk.SetMaxTokens(workerRuntime.maxTokens)       // 위와 같이, 출력 상한도 현재 활성 profile을 따른다
	wk.SetNoaEnabled(s.m.NoaCompactionEnabled)     // 실험 기능: noa 컨텍스트 압축(플랫폼 수준 스위치, run마다 읽음)
	wk.SetRunTimeout(time.Duration(s.agentRunSeconds("worker")) * time.Second)
	wk.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	wk.SetWebSearch(s.webSearchFor("worker"))
	wk.SetConstraintInject(s.constraintInjectWorker) // 조작 제약을 워커에 주입(설정 가능, 기본 켜짐)
	pl := agent.NewPlanner(plannerRuntime, "task-router", s.m.dir, tx, plannerRuntime.CompactionWindow(), s.agentMaxTurns("planner"))
	pl.SetFindingRecorder(s.evidenceStore())
	pl.SetCompactionWindowResolver(plannerRuntime.CompactionWindow)
	pl.SetNonStreaming(plannerRuntime.nonStreaming)
	pl.SetMaxTokens(plannerRuntime.maxTokens)
	pl.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(플랫폼 수준 스위치, run마다 읽음)
	pl.SetKillWork(s.engine.KillWork)
	pl.SetSteerWork(s.engine.SteerWork)
	pl.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	pl.SetWebSearch(s.webSearchFor("planner"))
	pl.SetConstraintInject(s.constraintInjectPlanner) // 조작 제약을 플래너에 주입(설정 가능, 기본 켜짐)
	// cold-digest §7: 콜드 노드 백그라운드 압축. 엔진이 권위 해석기를 거쳐 실제로 구동하는 것이 바로 이 per-task planner
	// (agentsForTask)이며, Compactor는 반드시 여기에 붙는다. 작업 라우팅의 planner provider(§4: agent와
	// 같은 모델이고, 작업 LLM 체인으로 해석됨)를 타며, 압축은 Complete로 body를 한 번에 생성한다. 초보자: 이 압축은 엔진이 작업 플래너 문맥을 줄여, 자산 그래프와 탐색 그래프의 기록을 다음 의도로 넘기기 전에 가볍게 유지한다.
	pl.SetCompactor(agent.NewCompactor(plannerRuntime, "task-router"))
	main := agent.NewMainAgent(mainRuntime, "task-router", s.m.dir, tx, mainRuntime.CompactionWindow(), s.agentMaxTurns("mainagent"))
	main.SetFindingRecorder(s.evidenceStore())
	main.SetCompactionWindowResolver(mainRuntime.CompactionWindow)
	main.SetNonStreaming(mainRuntime.nonStreaming)
	main.SetMaxTokens(mainRuntime.maxTokens)
	main.SetNoaEnabled(s.m.NoaCompactionEnabled) // 실험 기능: noa 컨텍스트 압축(플랫폼 수준 스위치, run마다 읽음)
	main.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	main.SetWebSearch(s.webSearchFor("mainagent"))
	main.SetSteerWork(s.engine.SteerWork) // steer_work: 사람이 실행 중인 work를 실시간으로 바로잡음. 초보자: 이 보정은 UI에서 들어와 엔진이 워커의 다음 의도를 바꾸며, 자산 그래프와 탐색 그래프에 남기는 방식은 그대로다.
	bundle := &taskAgentBundle{
		runtime: goalRuntime, plannerRuntime: plannerRuntime, workerRuntime: workerRuntime,
		mainRuntime: mainRuntime, pl: pl, wk: wk, main: main,
	}
	s.taskAgents[t.ID] = bundle
	return bundle
}

func (s *Server) syncTaskLLMState(pt *db.Task) {
	if pt == nil {
		return
	}
	id := fmt.Sprintf("%d", pt.ID)
	s.m.mu.Lock()
	if task := s.m.tasks[id]; task != nil {
		task.setLLMState(pt.LLMProfileID, pt.ActiveLLMProfileID, pt.LLMProfileIDs, pt.LLMChainRevision, pt.LLMFailoverState, pt.LLMFailoverReason)
	}
	s.m.mu.Unlock()
}

func (s *Server) emitTaskLLMTransition(t *Task, transition db.TaskLLMTransition, cause error) {
	if t == nil {
		return
	}
	previous := s.llmAuditProfile(transition.PreviousProfileID)
	var next *llmAuditProfile
	if transition.NextProfileID != nil {
		next = s.llmAuditProfile(*transition.NextProfileID)
	}
	mode := "automatic"
	kind := "llm_switch"
	summary := fmt.Sprintf("%s 할당량 부족", llmAuditProfileLabel(previous))
	if transition.NextProfileID != nil {
		summary += fmt.Sprintf(", 이후 호출은 %s(으)로 전환", llmAuditProfileLabel(next))
	} else {
		mode = "exhausted"
		kind = "llm_failover"
		summary += ", 설정 체인이 소진됨"
	}
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: mode, Reason: cause.Error(), Previous: previous, Next: next,
	}})
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: kind, IsError: transition.ChainExhausted, Summary: summary, Detail: cause.Error(), Metadata: metadata})
	log.Printf("[llm-failover] task %s: %s", t.ID, summary)
}

func (s *Server) llmAuditProfile(id int64) *llmAuditProfile {
	if id <= 0 || s.m == nil || s.m.pg == nil {
		return nil
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return &llmAuditProfile{ID: id, Name: fmt.Sprintf("설정 #%d", id)}
	}
	return &llmAuditProfile{ID: p.ID, Name: p.Name, Format: p.Format, Model: p.Model}
}

func llmAuditProfileLabel(profile *llmAuditProfile) string {
	if profile == nil {
		return "기본 설정"
	}
	name := profile.Name
	if name == "" {
		name = fmt.Sprintf("설정 #%d", profile.ID)
	}
	detail := []string{}
	if profile.Format != "" {
		detail = append(detail, profile.Format)
	}
	if profile.Model != "" {
		detail = append(detail, profile.Model)
	}
	if len(detail) == 0 {
		return name
	}
	return fmt.Sprintf("%s（%s）", name, strings.Join(detail, " / "))
}

func sameOptionalID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Server) emitManualTaskLLMSwitch(t *Task, previousID, nextID *int64) db.Activity {
	previous := (*llmAuditProfile)(nil)
	next := (*llmAuditProfile)(nil)
	if previousID != nil {
		previous = s.llmAuditProfile(*previousID)
	}
	if nextID != nil {
		next = s.llmAuditProfile(*nextID)
	}
	summary := fmt.Sprintf("사용자가 작업 LLM을 %s에서 %s(으)로 수동 전환", llmAuditProfileLabel(previous), llmAuditProfileLabel(next))
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: "manual", Reason: "사용자가 작업 LLM을 수동 전환", Previous: previous, Next: next,
	}})
	return s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "llm_switch", Summary: summary, Detail: summary, Metadata: metadata})
}
