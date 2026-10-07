package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5/pgconn"
)

// isFKViolation은 err가 Postgres 외래 키 위반(SQLSTATE
// 23503)인지 봅니다. 예를 들어 부모 행이 없는 exploration_id로 활동을 넣는 경우입니다.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// dropReason은 활동 쓰기가 왜 버려졌는지 나눕니다. 로그를 날것 오류 글이 아니라
// 원인별로 묶고 분석하게 합니다.
func dropReason(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return "fk_violation(23503,부모 exploration이 존재하지 않음)"
		case "23505":
			return "unique_violation(23505)"
		default:
			return "pg_error(" + pgErr.Code + ")"
		}
	}
	return "write_error"
}

// bumpDrop은 작업에서 버려진(저장 못 한) 활동 기록의 누적 수를 하나 올리고
// 그 수를 돌려줍니다. 플래너와 워커가 동시에 내보내면 여기서 겹치므로,
// 카운터는 sync.Map 안의 원자 값입니다. 로그의 이 수로 손실 규모를
// 한눈에 보고, grep으로 세지 않아도 됩니다.
func (e *Engine) bumpDrop(taskID string) int64 {
	v, _ := e.dropCnt.LoadOrStore(taskID, new(int64))
	return atomic.AddInt64(v.(*int64), 1)
}

// preview는 줄바꿈을 접고 s를 룬 단위로 짧게 잘라 한 줄 로그에 씁니다
// (몇 KB짜리 요약이나 상세를 로그에 통째로 넣지 않으려고요).
func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// model_error(provider/API 장애: LLM 계층의 일시 재시도가 소진되었거나, 스트림이 시작된 뒤 중간에 끊김)
// 마감한 work는 「시도했으나 끝내지 못한 것」도 아니고 진짜 실패도 아니며, 외부 흔들림이다. 기본값은 이것을 영구적인 것으로 본다
// blocked는 의도 하나를 헛되이 버리므로, 여기서 그 종료 상태에 추가로 몇 번 재실행하고, 매번 사이에 백오프를 두어
// provider가 회복할 시간을 준다. 재시도 중에 일시정지/종료/취소되면 즉시 해당 분기 처리에 자리를 넘긴다.
const (
	modelErrorRetries      = 2               // model_error로 마감한 뒤의 추가 재시도 횟수
	modelErrorRetryBackoff = 3 * time.Second // 매번 재시도 전의 백오프
	workControlWaitTimeout = 30 * time.Second
)

var errWorkControlConflict = errors.New("작업 제어가 충돌했습니다")

// retryableWorkerModelError는 작업 라우터가 이미 처리한 오류를 뺍니다.
// 특히 일부만 스트리밍된 뒤의 할당량 오류는 다음 LLM 호출을 위해 작업 커서를
// 앞으로 옮기지만, 이 의도 전체를 백업에서 다시 돌리면 안 됩니다.
func retryableWorkerModelError(reason harness.TerminalReason, err error) bool {
	return reason == harness.ReasonModelError && !isTaskLLMRuntimeError(err)
}

// Engine은 실제 LLM 에이전트로 이벤트 기반 탐색 루프를 돌립니다
// (docs §4.3/§4.4). 자산 그래프나 탐색 그래프가 바뀌면(모아서) 플래너를
// 깨웁니다. 플래너는 경로를 읽고, 자산을 조회하고, 목표를 판단하고, 의도를 냅니다.
// 워커 N개가 동시에 의도를 집어 실행합니다. 시뮬레이션 모드는
// 없습니다. LLM 제공자가 필요합니다. 플래너와 워커는 실행 중에
// (다시) 붙일 수 있습니다(화면에서 LLM을 설정). 루프는 항상 돌지만
// LLM이 정해지기 전에는 쉽니다.
// 초보용: 자산 그래프와 탐색 그래프의 변화를 모아 플래너를 깨우고, 워커가 프론티어의 의도를 실행합니다.
type Engine struct {
	m        *Manager
	debounce time.Duration

	bc *Broadcaster // 실시간 활동 발행/구독(SSE)

	started  sync.Map // taskID -> bool. Run이 작업마다 한 번만 돌게 합니다
	lastAct  sync.Map // taskID -> int64 유닉스 시각. 플래너/워커의 마지막 활동(심장박동)
	llmCalls sync.Map // taskID -> *int64. 플래너/워커/메인 에이전트의 실제 LLM 호출 수
	paused   sync.Map // taskID -> bool. 사용자 일시정지(플래너와 워커는 쉬고 루프는 살아 있음)
	deleting sync.Map // taskID -> bool. 삭제 장벽(그 작업에 속한 새 쓰기는 거절)
	dropCnt  sync.Map // taskID -> *int64. 버려진(저장 못 한) 활동 기록의 누적 수

	// deleteMu는 삭제 장벽을 거는 일과 새 작업 조작을 등록하는 일을 한 덩어리로 만듭니다.
	// BeginDelete가 돌아오면, 이미 들어온 쓰는 쪽은 전부 inflight에 반영되고
	// 그 뒤의 쓰는 쪽은 거절됩니다.
	deleteMu sync.RWMutex

	// 오래 사는 작업 고루틴(플래너, 워커, 마감 조율기)은
	// 작업 하나의 컨텍스트 아래에서 돕니다. 삭제가 성공하면 그 컨텍스트를 취소하고,
	// 모든 고루틴을 기다린 뒤 작업 수준의 Engine 참조를 전부 놓습니다.
	runtimeMu sync.Mutex
	runtimes  map[string]*taskRuntime

	// 작업별 실행 컨텍스트입니다. planner.Plan과 worker.Execute가 이 아래에서 돕니다.
	// 그래서 일시정지는 다음 번을 건너뛰는 데 그치지 않고, 진행 중인 실행을 취소할 수 있습니다. 취소는
	// 한 번뿐이라 재개할 때 새로 만듭니다. 취소마다 이름이 있는
	// 원인을 실어, 활동 기록에서 어떤 제어 경로가 시작했는지 알 수 있습니다.
	execMu     sync.Mutex
	execCancel map[string]context.CancelCauseFunc
	execCtx    map[string]context.Context

	// 실행 하나 제어는 플래너가 워커를 끊고, 화면이 작업 전체를 멈추지 않고
	// 의도 하나만 일시정지하거나 취소하게 합니다. done 채널은
	// runWorkerStep이 쓰기를 멈추고 최종 상태를 확정한 뒤 결과 오류를 한 번 전달합니다.
	workMu sync.Mutex
	work   map[int64]*workExecution

	// steerBox는 도는 실행에 플래너의 방향 수정을 쌓습니다(의도
	// id가 키). 워커의 PreToolUse 훅이 다음 도구 호출 전에 꺼내
	// 모델에 넘기고 그 호출을 막습니다. 그래서 죽이지 않고 다시 계획합니다.
	steerMu  sync.Mutex
	steerBox map[int64][]string

	plannerRound sync.Map // taskID -> int. 플래너 라운드 카운터(화면의 라운드 구분선)

	// 작업 수준 타임아웃(docs/작업-수준-타임아웃과-마감-설계.md 참고):
	settling     sync.Map // taskID -> bool, 작업이 마감 시퀀스에 들어감(새 의도 배분/수령 중지)
	deadline     sync.Map // taskID -> int64 unix, 절대 마감 시각(최초 실행 때 도장; 0/기본값=무제한)
	stamped      sync.Map // taskID -> bool, first_run_at에 이미 도장을 찍었는지(이 프로세스 안에서는 한 번만)
	inflight     sync.Map // taskID -> *int64, 실행 중인 planner.Plan + worker.Execute 개수(drain에 사용)
	coordStarted sync.Map // taskID -> bool, deadline 조율기가 이미 시작되었는지(Run/reload 중복 제거)

	// resolve는 작업 전용 플래너/워커를 돌려줍니다(서버가 권위 있는 작업 라우터로 연결).
	// nil,nil은 이 작업을 일부러 못 쓰게 한 것입니다
	// (예를 들어 장애 조치 사슬이 바닥남). 전역 쌍으로 물러서지 않습니다.
	resolve              func(t *Task) (*agent.Planner, *agent.Worker)
	resolveAuthoritative bool
	// readiness는 전역 LLM 제공자가 설정됐는지 알립니다. Ready()와
	// llm_configured 표시의 근거입니다. 시작 때 한 번 연결합니다. nil이면 준비 안 됨.
	readiness func() bool
}

type taskRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workExecution struct {
	cancel context.CancelCauseFunc
	done   chan error
	action string // 사용자 동작: pause(일시정지) | cancel(취소)
}

// nextPlannerRound는 작업의 다음 플래너 라운드 번호(1부터)를 돌려줍니다.
func (e *Engine) nextPlannerRound(taskID string) int {
	v, _ := e.plannerRound.LoadOrStore(taskID, 0)
	n := v.(int) + 1
	e.plannerRound.Store(taskID, n)
	return n
}

// Pause는 작업을 멈춥니다. 일시정지로 표시하고, 진행 중인 플래너/워커 실행도
// 취소합니다. 안 그러면 긴 worker.Execute가 끝날 때까지 계속됩니다.
func (e *Engine) Pause(taskID string, cause error) {
	e.paused.Store(taskID, true)
	e.cancelExec(taskID, cause)
}

// BeginDelete는 작업 데이터와 파일을 지우기 전에 실행 장벽을 겁니다.
// 이 일시정지는 사용자의 일시정지가 아닙니다. 서버는 이 전환을
// 수명 주기 입장과 순서를 맞추고, 정리가 실패하면 AbortDelete에 저장된 작업이
// 일시정지였는지 대기였는지 알립니다.
// 초보용: 삭제 중에는 그 작업에 속한 새 조작이 엔진에 들어가지 못하게 막습니다.
func (e *Engine) BeginDelete(taskID string) bool {
	e.deleteMu.Lock()
	if _, loaded := e.deleting.LoadOrStore(taskID, true); loaded {
		e.deleteMu.Unlock()
		return false
	}
	e.paused.Store(taskID, true)
	e.deleteMu.Unlock()
	e.cancelExec(taskID, agent.AbortTaskDeleted)
	return true
}

func (e *Engine) AbortDelete(taskID string, keepPaused bool) {
	e.deleteMu.Lock()
	if !e.IsDeleting(taskID) {
		e.deleteMu.Unlock()
		return
	}
	e.deleting.Delete(taskID)
	if !keepPaused {
		e.paused.Delete(taskID)
	}
	e.deleteMu.Unlock()
	if !keepPaused && e.m != nil {
		if t, ok := e.m.Task(taskID); ok {
			t.Notify()
		}
	}
}

func (e *Engine) IsDeleting(taskID string) bool {
	_, ok := e.deleting.Load(taskID)
	return ok
}

// registerTaskRoutines는 작업 런타임에 고루틴 count개를 예약합니다. 호출자는
// deleteMu를 읽기로 잡고 있어, StopTask가 WaitGroup.Add와 Wait을 겹치지 않게 합니다.
func (e *Engine) registerTaskRoutines(parent context.Context, taskID string, count int) *taskRuntime {
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	rt := e.runtimes[taskID]
	if rt == nil {
		ctx, cancel := context.WithCancel(parent)
		rt = &taskRuntime{ctx: ctx, cancel: cancel}
		e.runtimes[taskID] = rt
	}
	rt.wg.Add(count)
	return rt
}

func runTaskRoutine(rt *taskRuntime, fn func(context.Context)) {
	go func() {
		defer rt.wg.Done()
		fn(rt.ctx)
	}()
}

// StopTask는 오래 사는 고루틴을 모두 영원히 멈추고, 삭제가 성공한 작업의
// Engine 상태를 전부 지웁니다. 정리가 끝날 때까지 삭제 장벽은 걸린 채라,
// 새 작업 조작이 해체와 경주하지 않습니다.
func (e *Engine) StopTask(taskID string) {
	e.deleteMu.Lock()
	e.deleting.Store(taskID, true)
	e.deleteMu.Unlock()

	e.cancelExec(taskID, agent.AbortTaskDeleted)
	e.runtimeMu.Lock()
	rt := e.runtimes[taskID]
	if rt != nil {
		rt.cancel()
	}
	e.runtimeMu.Unlock()
	if rt != nil {
		rt.wg.Wait()
	}

	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(agent.AbortTaskDeleted)
	}
	delete(e.execCancel, taskID)
	delete(e.execCtx, taskID)
	e.execMu.Unlock()

	e.runtimeMu.Lock()
	if e.runtimes[taskID] == rt {
		delete(e.runtimes, taskID)
	}
	e.runtimeMu.Unlock()

	e.started.Delete(taskID)
	e.lastAct.Delete(taskID)
	e.llmCalls.Delete(taskID)
	e.paused.Delete(taskID)
	e.dropCnt.Delete(taskID)
	e.plannerRound.Delete(taskID)
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.inflight.Delete(taskID)
	e.coordStarted.Delete(taskID)
	e.deleteMu.Lock()
	e.deleting.Delete(taskID)
	e.deleteMu.Unlock()
}

// cancelExec는 작업의 현재 실행 컨텍스트(진행 중인
// planner.Plan / worker.Execute)가 있으면 취소합니다. Pause와 마감
// 시퀀스의 강제 비우기 안전장치가 같이 씁니다.
func (e *Engine) cancelExec(taskID string, cause error) {
	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(cause)
	}
	e.execMu.Unlock()
}

// Resume은 작업의 일시정지를 풀고 새 계획 라운드를 한 번 밉니다. 그 다음 실행은
// 취소되지 않은 새 컨텍스트를 받습니다.
func (e *Engine) Resume(t *Task) {
	// 삭제가 시작되면 일시정지 장벽은 BeginDelete가 가집니다. 동시에 온
	// 재개가 그것을 지우면 안 됩니다. 정리가 작업 조작이 빠지기를 기다리는 동안
	// 플래너나 워커가 다시 들어오면 안 됩니다.
	if t == nil {
		return
	}
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(t.ID) {
		return
	}
	e.paused.Delete(t.ID)
	t.Notify()
}

// execContextFor는 parent에서 나온, 살아 있는 작업별 컨텍스트를 돌려줍니다. 앞선
// 일시정지가 취소했으면 새로 만듭니다.
func (e *Engine) execContextFor(parent context.Context, taskID string) context.Context {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if e.IsPaused(taskID) {
		// 일시정지 중에는 살아 있는 컨텍스트를 넘기지 않습니다(집기에서 Execute로 가는 경주를 막음).
		c, cancel := context.WithCancelCause(parent)
		cancel(agent.AbortPausedRaceGuard)
		return c
	}
	if c := e.execCtx[taskID]; c != nil && c.Err() == nil {
		return c
	}
	c, cancel := context.WithCancelCause(parent)
	e.execCtx[taskID] = c
	e.execCancel[taskID] = cancel
	return c
}

// IsPaused는 작업을 사용자가 일시정지했는지 알립니다.
func (e *Engine) IsPaused(taskID string) bool {
	v, ok := e.paused.Load(taskID)
	return ok && v.(bool)
}

// Started는 그 작업의 엔진 루프가 도는지 알립니다.
func (e *Engine) Started(taskID string) bool {
	_, ok := e.started.Load(taskID)
	return ok
}

// LastActivity는 작업에서 플래너나 워커가 마지막으로 활동한 유닉스 시각을
// 돌려줍니다. 아직 없으면 0입니다.
func (e *Engine) LastActivity(taskID string) int64 {
	if v, ok := e.lastAct.Load(taskID); ok {
		return v.(int64)
	}
	return 0
}

// BeginLLMCall/EndLLMCall은 스케줄러의 작업 조작 카운터와 따로, 실제 제공자 호출을 셉니다.
// 작업은 루프가 살아 있어도 전부 트리거를 기다릴 수 있습니다. 그 상태는 화면에서
// 쉬는 중으로 남아야 합니다.
// 초보용: 화면의 도는 중 표시는 루프가 아니라 실제 LLM 호출로 판단합니다.
func (e *Engine) BeginLLMCall(taskID string) {
	v, _ := e.llmCalls.LoadOrStore(taskID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (e *Engine) EndLLMCall(taskID string) {
	if v, ok := e.llmCalls.Load(taskID); ok {
		p := v.(*int64)
		if atomic.AddInt64(p, -1) <= 0 {
			atomic.StoreInt64(p, 0)
		}
	}
}

func (e *Engine) ActiveLLMCalls(taskID string) int64 {
	if v, ok := e.llmCalls.Load(taskID); ok {
		return atomic.LoadInt64(v.(*int64))
	}
	return 0
}

func (e *Engine) touch(taskID string) { e.lastAct.Store(taskID, time.Now().Unix()) }

func NewEngine(m *Manager) *Engine {
	return &Engine{m: m, debounce: 800 * time.Millisecond, bc: NewBroadcaster(),
		execCancel: map[string]context.CancelCauseFunc{}, execCtx: map[string]context.Context{},
		work: map[int64]*workExecution{}, steerBox: map[int64][]string{},
		runtimes: map[string]*taskRuntime{}}
}

// registerWork는 지금 intentID를 돌리는 실행의 취소를 기록합니다.
func (e *Engine) registerWork(intentID int64, cancel context.CancelCauseFunc) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{cancel: cancel, done: make(chan error, 1)}
	e.workMu.Unlock()
}

// detachWork는 Execute가 돌아온 뒤 살아 있는 제어 손잡이를 뺍니다. complete는
// 최종 의도 상태를 쓴 뒤에 불러야 합니다. 기다리는 취소 처리기가
// 늦은 쓰기와 경주하지 않고 워커의 칠판 출력을 지울 수 있게 하려고요.
func (e *Engine) detachWork(intentID int64) (action string, complete func(error)) {
	e.workMu.Lock()
	run := e.work[intentID]
	if run != nil {
		delete(e.work, intentID)
		action = run.action
		run.cancel(agent.AbortWorkFinished) // 자원을 놓습니다(이미 취소됐으면 아무 일도 안 함).
	}
	e.workMu.Unlock()
	e.steerMu.Lock()
	delete(e.steerBox, intentID) // 끝난 실행에 아직 전달하지 않은 방향 수정은 버립니다.
	e.steerMu.Unlock()
	if run == nil {
		return action, func(error) {}
	}
	return action, func(err error) { run.done <- err }
}

// ControlWork는 화면에 보이는 일시정지나 취소를 요청하고, 워커가
// 쓰기를 완전히 멈출 때까지 기다립니다. 취소 정리는 이 함수가 돌아온 뒤
// API 처리기가 합니다. 일시정지 상태는 runWorkerStep이 직접 확정합니다.
// 초보용: 화면에서 의도 하나만 멈추고, 작업 전체의 플래너 루프는 그대로 둡니다.
func (e *Engine) ControlWork(ctx context.Context, intentID int64, action string) error {
	if action != "pause" && action != "cancel" {
		return fmt.Errorf("unsupported work action %q", action)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.workMu.Lock()
	run := e.work[intentID]
	if run == nil {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 의도 %d에 현재 실행 중인 work가 없음(이미 끝났거나 아직 할당되지 않았을 수 있음)", errWorkControlConflict, intentID)
	}
	if run.action != "" {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 의도 %d가 %s 작업을 실행 중", errWorkControlConflict, intentID, run.action)
	}
	run.action = action
	done := run.done
	cause := error(agent.AbortWorkPausedByUser)
	if action == "cancel" {
		cause = agent.AbortWorkCancelledByUser
	}
	run.cancel(cause)
	e.workMu.Unlock()

	timer := time.NewTimer(workControlWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("의도 %d %s 마무리 대기: %w", intentID, action, ctx.Err())
	case <-timer.C:
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("의도 %d %s 마무리 대기: %w", intentID, action, context.DeadlineExceeded)
	}
}

// releaseWorkControl은 대기가 취소된 뒤 이 호출자의 예약만 놓습니다.
// 실행 컨텍스트는 취소된 채로 둡니다. runWorkerStep은
// 이름이 있는 취소 원인을 알아채고, HTTP 호출자가 이미 나가도
// 의도를 다시 이을 수 있는 일시정지 상태로 정리합니다.
func (e *Engine) releaseWorkControl(intentID int64, run *workExecution, action string) {
	e.workMu.Lock()
	if current := e.work[intentID]; current == run && current.action == action {
		current.action = ""
	}
	e.workMu.Unlock()
}

func transitionIntentState(store *db.ExplorationStore, intentID int64, expected, state string) error {
	changed, err := store.CompareAndSetIntentState(intentID, expected, state)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 의도 %d가 더 이상 %s 상태가 아님", db.ErrIntentStateConflict, intentID, expected)
	}
	return nil
}

// SteerWork는 intentID를 돌리는 실행에 도중에 방향 수정을 넣습니다
// (플래너의 steer_work 도구). 워커는 다음 도구 호출 전에 전달하고
// 다시 계획합니다. 죽이지 않습니다. 그 의도를 지금 실행 중인 것이 없으면 오류입니다.
// 초보용: 플래너가 도는 워커의 다음 도구 앞에서 탐색 방향을 바꿉니다.
func (e *Engine) SteerWork(intentID int64, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("교정 메시지는 비울 수 없음")
	}
	e.workMu.Lock()
	running := e.work[intentID] != nil
	e.workMu.Unlock()
	if !running {
		return fmt.Errorf("의도 %d에 현재 실행 중인 work가 없음(이미 끝났거나 아직 할당되지 않았을 수 있음)", intentID)
	}
	e.steerMu.Lock()
	e.steerBox[intentID] = append(e.steerBox[intentID], msg)
	e.steerMu.Unlock()
	return nil
}

// drainSteer는 intentID에 쌓인 방향 수정 중 가장 오래된 것을 꺼냅니다(FIFO). 없으면 없습니다.
func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	msg := q[0]
	if len(q) == 1 {
		delete(e.steerBox, intentID)
	} else {
		e.steerBox[intentID] = q[1:]
	}
	return msg, true
}

// steerHooks는 가드의 훅 실행기를 감싸, 플래너가 도는 실행을 조종하게 합니다.
// 도구 호출 전에 쌓인 방향 수정이 있으면 꺼내 그 호출을 막고,
// 메시지를 모델에 돌려줍니다. 모델은 도구를 실행하는 대신 다음 단계를 다시 계획합니다.
// 쌓인 메시지가 없으면 가드는 예전과 똑같이 동작합니다.
// 이것은 「공회전 턴」의 이어 실행도 맡는다. Stop을 본다.
type steerHooks struct {
	inner harness.HookRunner
	drain func() (string, bool)
	// nudges는 이 의도에 이미 주입한 공회전 이어 실행 횟수이며, 상한은 limit이다. 포인터: harness가 보유한 것은
	// steerHooks의 값 복사본이므로, 카운트는 같은 한 벌을 공유해야 한다.
	nudges *atomic.Int64
	// limit은 공회전 이어 실행의 횟수 상한이며, Engine.emptyTurnNudgeLimit()가 「빈 응답 재시도
	// 횟수」에서 파싱해 온다. <=0 = 개입하지 않음(사용자가 이 층을 명시적으로 끔).
	limit int
	// label은 "worker-1 · #42" 형태이며, 로그에만 쓴다.
	label string
}

// 공회전 턴(생각만 있고 본문도 도구 호출도 없음) 이어 실행 횟수의 기본값이며, SDK 빈 응답 재시도의
// 내장 기본값(norma/llm/openai.go의 emptyResponseRetries)과 맞춘다——두 층이 같은
// 노브를 쓰며, 설정하지 않을 때의 동작도 맞아야 한다. 파싱은 Engine.emptyTurnNudgeLimit을 본다.
//
// 이 수는 「의도 하나의 총량」이며 「연속 몇 번」이 아니다: harness 자체의 stopHookActive가
// 이미 연속 공회전은 한 번만 밀도록 막아 두었다——그 라운드를 민 뒤에도 공회전이면 Stop 훅은 다시 호출되지 않고, run은 바로
// 마감한다. 도구 턴이 실제로 한 번 일어나야 할당량이 새로고침된다(norma/harness/query.go:534). 그래서 이
// 게이트가 막는 것은 「도구 → 공회전 → 밀기 → 도구 → 공회전」 같은 병적인 순환이며, 이것이 의도 예산을 소진하지 않게 한다.
const defaultEmptyTurnNudges = 2

// emptyTurnNudge는 공회전 턴에 주입하는 이어 실행 지시이다.
//
// 이런 턴은 harness 눈에는 한 번의 자연 종료이다(stop_reason=end_turn이고 tool_use가 없음). 다섯 층의
// LLM 재시도는 한 층도 적용되지 않는다——오류가 아니라 모델이 「생각은 끝났는데 손을 대지 않은」 것이다. SDK의 빈 응답 재시도도
// 닿지 못한다: 그것은 「이벤트를 yield한 적이 있는지」로 빔을 판정하는데, 생각 증분 자체가 이벤트이다(norma/llm/openai.go
// 의 SEThinkingDelta). 그래서 thinking-only는 빈 것으로 치지 않는다. 게다가 그 층은 prompt 전체를 그대로 다시 보내므로,
// 컨텍스트 모양이 정하는 이런 공회전에는 재전송이 모델에게 한 번 더 생각하게만 한다. 여기서는 지시를 하나 덧붙여, 그것이
// 이미 낸 생각을 가지고 이어 가게 한다. 입력이 바뀌어야 다른 행동을 낼 이유가 생긴다.
const emptyTurnNudge = "【공회전 알림】이전 라운드에서는 사고 과정만 출력했고, 본문 답도 주지 않았으며 어떤 도구도 호출하지 않았다," +
	"이 라운드는 산출이 없다. 방금 생각한 다음 단계를 바로 실행하라. 도구를 호출하거나 결론 문장을 제시하라. 생각을 반복하지 마라."

// isThinkingOnlyTurn은 최근 어시스턴트 턴이 글도 도구 호출도 안 냈는지 봅니다.
// 즉 모델이 그 라운드 내내 생각만 했는지입니다.
func isThinkingOnlyTurn(messages []llm.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		return strings.TrimSpace(m.Text()) == "" && len(m.ToolUses()) == 0
	}
	return false
}

func (h steerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drain(); ok {
		return true, "【플래너(의도만 생성) 실시간 교정】" + msg +
			"\n(이것은 플래너(의도만 생성)가 이 의도에 내린 즉시 지시이다. 이번 도구 호출은 실행되지 않았으니 이에 따라 다음 단계를 조정하라. 현재 계획과 충돌하면 이것을 따른다.)", nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h steerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop은 가드(guard)의 기존 의미 위에 「공회전 턴」 이어 실행을 한 층 보탠다: 모델이 생각만 출력하고 본문도 주지 않고
// 도구도 호출하지 않으면, harness는 그것을 자연 종료로 보고 빈 summary로 마감한다(query.go의
// ReasonCompleted + asst.Text()). 아직 끝내지 못한 의도 하나가 이렇게 중간에 끊긴다. 이때
// 이어 실행 지시를 하나 주입해, 모델이 이미 있는 생각을 가지고 계속 가게 한다.
func (h steerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	var (
		prevent  bool
		blocking []string
		msg      string
	)
	if h.inner != nil {
		prevent, blocking, msg = h.inner.Stop(ctx, messages)
	}
	// inner가 이미 강제 정지를 결정했거나, 이미 자기 이어 실행 메시지를 주입하려 하면 → 그것을 존중하고 더 겹치지 않는다.
	// limit<=0 = 사용자가 「빈 응답 재시도 횟수」를 -1로 설정한 것, 즉 이 층을 명시적으로 끈 것이다.
	if prevent || len(blocking) > 0 || h.nudges == nil || h.limit <= 0 || !isThinkingOnlyTurn(messages) {
		return prevent, blocking, msg
	}
	n := h.nudges.Add(1)
	if n > int64(h.limit) {
		log.Printf("[work %s] 공회전 라운드(사고만 있고 본문도 도구도 없음)가 이어 실행 상한 %d에 도달하여 마무리를 allow", h.label, h.limit)
		return prevent, blocking, msg
	}
	log.Printf("[work %s] 공회전 라운드(사고만 있고 본문도 도구도 없음), 이어 실행 지시를 주입 (%d/%d)", h.label, n, h.limit)
	return false, []string{emptyTurnNudge}, ""
}

// KillWork는 intentID를 돌리는 실행을 취소합니다(플래너의 kill_work 도구).
// 그 실행의 에이전트 코어 세션은 ctx 취소를 따르고 바로 끊깁니다.
func (e *Engine) KillWork(intentID int64) error {
	e.workMu.Lock()
	run := e.work[intentID]
	e.workMu.Unlock()
	if run == nil {
		return fmt.Errorf("의도 %d에 현재 실행 중인 work가 없음(이미 끝났거나 아직 할당되지 않았을 수 있음)", intentID)
	}
	run.cancel(agent.AbortKilledByPlanner)
	return nil
}

// Broadcaster는 엔진의 실시간 활동 발행/구독을 내보입니다(SSE 처리기가 씀).
func (e *Engine) Broadcaster() *Broadcaster { return e.bc }

// emitActivity는 잡은 단계 하나를 저장하고, 실시간 구독자에게도 뿌립니다.
// 한곳에서 하므로 저장소와 SSE 스트림이 어긋나지 않습니다.
// 초보용: 탐색 단계가 DB와 화면의 실시간 흐름에 같이 남습니다.
func (e *Engine) emitActivity(t *Task, r db.Activity) db.Activity {
	id, err := e.appendActivity(t, r)
	if err != nil {
		// 이제는 조용히 넘기지 않습니다. 기록을 버리면 흔적에서 명령과 결과의 짝이 깨집니다.
		// trace — tool_result가 유실된 tool_use는 영원히 "실행 중"으로 보이고,
		// 유실된 'result'/'round' 기록은 세션을 요약 없이 남긴다("요약 없음").
		// 원인 분석에 필요한 모든 것은 error 수준 한 줄에 들어간다: reason class,
		// 요약 미리보기, 이 작업에서 버린 누적 수, 그리고 외래 키일 때는
		// 부모 탐색 행이 왜 닿지 않는지 지금 DB를 살펴본 결과입니다.
		n := e.bumpDrop(t.ID)
		diag := ""
		// 부모 외래 키 실패(23503)면 지금 DB를 살펴, 로그에 탐색이 왜
		// 닿지 않는지(행이 없음 / expID가 틀림)를 남깁니다. 닿지 않는다는 사실만 남기지 않습니다.
		if isFKViolation(err) {
			storeID := t.Store.ID()
			if exists, refs, maxID, dErr := e.m.pg.ExplorationDiag(storeID); dErr != nil {
				diag = fmt.Sprintf(" | FK 진단 쿼리 실패(store.expID=%d task.ExpID=%d): %v", storeID, t.ExpID, dErr)
			} else {
				diag = fmt.Sprintf(" | FK 진단: store.expID=%d task.ExpID=%d exploration 존재=%v 이를 참조하는 task 수=%d MAX(exploration.id)=%d",
					storeID, t.ExpID, exists, refs, maxID)
			}
		}
		log.Printf("[activity] task %s 활동 기록 폐기(해당 작업 누적 %d번째) worker=%s kind=%s tool=%s tuid=%s reason=%s summary=%q: %v%s",
			t.ID, n, r.Worker, r.Kind, r.Tool, r.ToolUseID, dropReason(err), preview(r.Summary, 80), err, diag)
		e.touch(t.ID)
		return r
	}
	r.ID = id
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	e.bc.Publish(t.ID, r)
	e.touch(t.ID)
	return r
}

// appendActivity는 활동 행 하나를 저장하고, 쓰기가 실패하면 짧게 다시 시도합니다.
// 같은 탐색의 활동 로그에 플래너와 워커가 동시에 넣으면
// 가끔 실패합니다. 짧은 재시도 몇 번이면 대부분 회복됩니다. 중요한 점은, 이제
// 실패를 로그로 남긴다는 것입니다(예전에는 `if err == nil`에 삼켜졌습니다). 그래서
// 원인 DB 오류를 진단할 때 드디어 볼 수 있습니다.
func (e *Engine) appendActivity(t *Task, r db.Activity) (int64, error) {
	var id int64
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if id, err = t.Store.AppendActivity(r); err == nil {
			if attempt > 1 {
				log.Printf("[activity] task %s 쓰기 %d번째 재시도 성공 (worker=%s kind=%s tool=%s)",
					t.ID, attempt, r.Worker, r.Kind, r.Tool)
			}
			return id, nil
		}
		log.Printf("[activity] task %s 쓰기 실패 (%d/3번째, worker=%s kind=%s tool=%s expID=%d): %v",
			t.ID, attempt, r.Worker, r.Kind, r.Tool, t.Store.ID(), err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return 0, err
}

// SetReadiness는 전역 "LLM 제공자가 설정됨" 판정을 연결합니다(Ready()와
// llm_configured 표시가 읽음). 시작 때 한 번 부릅니다.
func (e *Engine) SetReadiness(fn func() bool) { e.readiness = fn }

// SetAgentResolver는 작업별 플래너/워커 결정 함수를 붙입니다(서버가 연결).
// 어떤 작업 루프보다 먼저, 시작 때 한 번 부릅니다. 읽을 때 잠금이 필요 없습니다.
func (e *Engine) SetAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = false
}

// SetAuthoritativeAgentResolver는 nil 결과가 전역 제공자로 떨어지면 안 되는
// 결정 함수를 붙입니다. 작업 수준 장애 조치 사슬이 이것을 써서, 바닥난
// 사슬이 설정된 경계를 조용히 넘지 못하게 합니다.
func (e *Engine) SetAuthoritativeAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = true
}

// snapshotFor는 작업이 돌릴 플래너/워커를 작업 라우터의 결정 함수에서 가져옵니다.
// nil,nil은 그 작업을 일부러 못 쓰게 한 것입니다(예: 바닥난
// 장애 조치 사슬). 전역 쌍으로 물러서지 않습니다.
func (e *Engine) snapshotFor(t *Task) (*agent.Planner, *agent.Worker) {
	if e.resolve != nil {
		p, w := e.resolve(t)
		if (p != nil && w != nil) || e.resolveAuthoritative {
			return p, w
		}
	}
	return nil, nil
}

// Ready는 전역 LLM 제공자가 설정됐는지 알립니다(시작 때 연결한
// 판정을 봄).
func (e *Engine) Ready() bool {
	return e.readiness != nil && e.readiness()
}

// ReadyFor는 특정 작업이 플래너/워커 쌍을 찾을 수 있는지 알립니다.
// 작업에 명시한 설정 사슬은 전역 기본 제공자가 없어도 돌 수 있어서,
// 작업 상태를 Ready만으로 판단하면 안 됩니다.
func (e *Engine) ReadyFor(t *Task) bool {
	p, w := e.snapshotFor(t)
	return p != nil && w != nil
}

// Run은 작업의 플래너 루프와 워커 루프 N개를 시작합니다. 루프는 항상 돌지만
// LLM이 설정되기 전에는 아무 일도 안 합니다. 쉬는 동안 만든 작업도 화면에서
// LLM을 정하면 알아서 이어집니다.
// 초보용: 이 함수가 작업의 플래너 루프와, 프론티어를 집는 워커 루프를 시작합니다.
func (e *Engine) Run(ctx context.Context, t *Task) {
	workers := e.m.Workers()
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		t.Notify() // 이미 돌고 있습니다. 계획 라운드만 한 번 밉니다.
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1+workers)
	e.deleteMu.RUnlock()
	e.touch(t.ID)
	runTaskRoutine(rt, func(loopCtx context.Context) { e.plannerLoop(loopCtx, t) })
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("work#%d", i+1)
		runTaskRoutine(rt, func(loopCtx context.Context) { e.workerLoop(loopCtx, t, name) })
	}
	e.startDeadlineCoordinator(ctx, t) // 작업 수준 타임아웃 타이머(timeout>0일 때만; 중복 제거)
	// 「활동 중인 의도(open+running)가 전혀 없을」 때만 첫 라운드 계획을 kick한다. 시드 의도가 있는 작업: 시드는 이미
	// open이거나, 위에서 방금 띄운 워커(의도 하나를 실행한 뒤 정지)가 먼저 claim하여 running이 되었다——둘 다 「할 일이 있음」이며, 예외 없이 건너뛰고
	// 첫 라운드 플래너(의도만 생성)를 건너뛴다. 워커가 시드 의도를 바로 받아 실행하고, 끝나면 NotifyDone/심박이 플래너를 깨운다.
	// ⚠️ 프론티어를 쓰면 안 된다(open만 센다): 워커 수령(open→running)과 이 검사 사이에 경쟁이 있어, 잘못 kick할 수 있다.
	// 재시작 자동 복구 때에도 running 의도만 남을 수 있으며, 마찬가지로 건너뛰어야 한다.
	if has, _ := t.Store.HasActiveIntent(); !has {
		t.Notify() // 첫 계획 라운드를 밉니다(LLM이 준비되면 실행됩니다).
	}
}

// plannerHeartbeatInterval은 작업의 플래너 심박 간격을 파싱한다. db.CreateTask가 이미 정규화했고
// (600 미만은 모두 600으로 올린다); 여기서 한 번 더 바닥을 받쳐, 메모리 상태의 이상값을 막는다.
func plannerHeartbeatInterval(t *Task) time.Duration {
	sec := t.PlanHeartbeatSeconds
	if sec < db.MinPlanHeartbeatSeconds { // 하한=기본값=600(10min)
		sec = db.MinPlanHeartbeatSeconds
	}
	return time.Duration(sec) * time.Second
}

// resetPlannerTimer는 이미 발화했을 수 있는 Timer를 안전하게 다시 장전한다(표준 Stop→drain→Reset 패턴).
func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := plannerHeartbeatInterval(t)
	// 하트비트 타이머는 loop 입구 갈래에서 건다 = 작업 start부터 시간을 잰다: 첫 라운드 플래너(의도만 생성)를 건너뛰는 seed 작업라도
	// (Run에서 프론티어가 비어 있지 않으면 첫 라운드를 kick하지 않음), 여기서 계속 막혀 있어도 하트비트는 「작업 start + interval」에
	// 첫 계획 라운드를 일으킨다. 그 뒤 매번 깨어날 때(에지/하트비트) 타이머를 다시 건다 = 지난번 아무 계획 트리거로부터 지난 시간.
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()

	// runRound는 계획 한 라운드를 돈다(debounce 병합 + 각 가드(guard) 포함). src는 로그에서 트리거 출처를 구분하는 데만 쓴다.
	runRound := func(src string) {
		// 디바운스: 몰린 변화를 계획 라운드 하나로 모읍니다.
		timer := time.NewTimer(e.debounce)
	drain:
		for {
			select {
			case <-t.notify:
			case <-timer.C:
				break drain
			}
		}
		planner, _ := e.snapshotFor(t)
		if planner == nil {
			return // LLM이 설정될 때까지 쉽니다.
		}
		if e.IsPaused(t.ID) {
			return // 사용자 일시정지: 계획하지 않습니다.
		}
		if e.IsDeleting(t.ID) {
			return
		}
		// 끝난 작업입니다(목표를 모두 이루면 완료, 아니면 실패). 실행은 끝났습니다.
		// 재개나 밀기(예: 재시작 때 활성 작업을 자동 재개)가 다시
		// 계획하면 안 됩니다. LLM 라운드를 낭비하고, 이미 정리된 결과를 다시 확인하게 됩니다.
		if isTerminalStatus(t.lifecycleSnapshot().Status) {
			return
		}
		// 작업 단위 타임아웃 마무리 중: 일반 깨우기는 버린다——워커(의도 하나를 실행한 뒤 정지) 마무리 쓰기와 Resume의 Notify는 더 이상
		// 일반 계획 라운드를 일으키지 않는다; 마지막 라운드는 코디네이터(settleTask)가 직접 돌리고, 여기를 타지 않는다.
		if e.isSettling(t.ID) {
			return
		}
		// goalless(사람이 직접 넣음) 분기: 작업에 open 목표가 없으면 플래너(의도만 생성)는 돌지 않는다——돌면 다시 판정해서
		// met→cancelExec가 사용자가 메인 에이전트로 직접 넣은 의도를 죽인다. 끝날지는 프론티어가 정한다:
		// open/running 의도가 아직 있으면 → running을 유지하고 조용히 기다린다; 의도가 모두 끝나면 → done으로 떨어진다.
		// 이 구간은 순수 Go이며, LLM 호출을 일으키지 않고 계획 라운드 marker도 찍지 않는다.
		if open, err := t.Store.HasOpenGoal(); err == nil && !open {
			t.drainTriggers() // 쌓인 done/발견(finding) 트리거를 버려, goalless 긴 세션에서 한없이 늘어나는 것을 막는다
			if active, err := t.Store.HasActiveIntent(); err == nil && !active {
				// 프론티어가 비고 실행 중인 의도가 없으면 → 마무리. Guarded 버전으로 CAS를 해서, 동시에 일어나는
				// pause/delete/타임아웃 마무리의 상태 전환을 밟지 않게 한다.
				if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil {
					log.Printf("[goalless] task %s 마감 상태를 done으로 기록 실패: %v", t.ID, err)
				} else if won {
					e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
						Summary: "목표가 모두 달성되었고, 직접 투입한 의도는 실행을 마쳤으며, 작업이 종료됨"})
				}
			}
			return // goalless 분기는 planner.Plan에 절대 들어가지 않는다
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		defer e.decInflight(t.ID)
		e.stampFirstRun(t) // 처음 진짜 계획 → first_run_at을 찍고 deadline을 계산한다(timeout이 있는 작업만)
		e.touch(t.ID)
		emit := func(r db.Activity) { e.emitActivity(t, r) }
		ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // Pause로 취소할 수 있다; 작업 deadline을 가진다
		if ectx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		log.Printf("[planner] task %s 계획 중…(%s 트리거)", t.ID, src)
		// 라운드 표시: Plan() 한 번이 플래너 라운드 하나입니다. 경계를 내보내
		// 화면이 대화 기록에서 라운드를 나누게 합니다(kind='round').
		e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
			Summary: fmt.Sprintf("%d번째 계획 라운드", e.nextPlannerRound(t.ID))})
		// 이 라운드를 깨운 것(워커 완료 / 발견. 여러 개일 수 있고, 디바운스가
		// 몰림을 모읍니다. 시간이나 심장박동으로 깨면 비어 있습니다).
		triggers := t.drainTriggers()
		taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
		e.BeginLLMCall(t.ID)
		met, reason, err := planner.Plan(ectx, taskIDInt, e.m.assets, t.Store, t.Goal, triggers, emit)
		e.EndLLMCall(t.ID)
		switch {
		case err != nil && ectx.Err() == nil:
			log.Printf("[planner] task %s 계획 오류: %v", t.ID, err)
		case met:
			log.Printf("[planner] task %s 목표 달성으로 판정: %s", t.ID, reason)
			// 모든 목표를 달성하면 → 작업 상태를 done으로 저장한다(프론트엔드 DTO가 이 종료 상태를 먼저 보여 준다).
			if err := e.m.SetTaskStatus(t.ID, "done"); err != nil {
				log.Printf("[planner] task %s 완료 표시 DB 기록 실패: %v", t.ID, err)
			}
			// 작업이 완료로 판정되면 → 돌고 있는 워커(의도 하나를 실행한 뒤 정지)를 바로 취소한다: 손에 든 의도를 끝내도 의미가 없다.
			// 다음 라운드 워커 루프는 종료 상태 문에 걸리면 새 의도를 받지 않는다; 취소된 묶음은 아래 \"작업 완료\" 분기로
			// stopped로 귀결된다(blocked가 아니다).
			e.cancelExec(t.ID, agent.AbortGoalMet)
		default:
			log.Printf("[planner] task %s 계획 완료", t.ID)
		}
		e.touch(t.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
			runRound("edge") // 워커 종료 / 발견(finding) / kill / resume / seed 첫 라운드
		case <-heartbeat.C:
			// 주기 안전망: 교착 안전망 + 깨워서 날고 있는 워커(의도 하나를 실행한 뒤 정지)를 감독(steer/kill) + 주기 재검사.
			runRound("heartbeat")
		}
		// 매번 깨어난 뒤(에지 또는 하트비트) 하트비트 타이머를 다시 건다: 아무 계획 트리거든 이 가만히 있는 시간 재기를 다시 계산한다.
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, worker := e.snapshotFor(t)
		if worker == nil {
			if sleepCtx(ctx, 1500*time.Millisecond) {
				return
			}
			continue
		}
		if e.IsPaused(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 사용자 일시정지: 의도를 집거나 실행하지 않습니다.
		}
		if e.IsDeleting(t.ID) {
			return
		}
		if e.isSettling(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 작업 타임아웃 마무리 중: 새 의도를 받지 않는다(돌고 있는 것은 스스로 마무리하고, 코디네이터는 drain을 기다린다)
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // 작업이 이미 종료 상태(done/failed/timeout): 남은 의도 받기를 멈추고, 완료 뒤에 프론티어를 빈 실행하지 않는다
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		claimed := e.runWorkerStep(ctx, t, name, worker)
		e.decInflight(t.ID)
		if !claimed && sleepCtx(ctx, 800*time.Millisecond) {
			return
		}
	}
}

// runWorkerStep은 프론티어에서 의도 하나를 집어 runIntent로 끝까지 정리합니다.
// 집을 것이 없으면 false입니다. 부르는 곳은 풀의 워커 루프
// 뿐입니다.
// 초보용: 워커 풀이 탐색 그래프 프론티어의 열린 의도 하나를 실행합니다.
func (e *Engine) runWorkerStep(ctx context.Context, t *Task, name string, worker *agent.Worker) bool {
	intent := e.claimNext(t, name)
	if intent == nil {
		return false
	}
	log.Printf("[worker %s] task %s 의도 #%d 수령", name, t.ID, intent.ID)
	return e.runIntent(ctx, t, name, worker, intent, "", "")
}

// runIntent는 이미 집은(state=running) 의도 하나를 실행하고 끝까지 정리합니다.
// 풀 워커 루프(runWorkerStep 경유)와 사람 메시지 처리기(runDetachedIntent,
// 워커 풀 밖의 전용 고루틴)가 같이 부릅니다. 그래서 실행, 재시도, 상태 쓰기는
// 정확히 한곳에만 있습니다. message가 비어 있지 않으면 ExecuteWithMessage로
// 이 턴의 입력에 넣습니다. requestID는 대화 기록 표시의 키로, model_error 재시도 때
// 같은 내용을 다시 넣지 않게 합니다. 호출자는 이 과정 전체에 대해
// 작업 조작 입장을 이미 하나 가지고 있어야 합니다. 그래야 삭제가
// LLM이 돌아온 시점과 마지막 DB 쓰기 사이를 잠잠하다고 보지 않습니다.
// 초보용: 집은 의도 하나를 워커가 실행하고, 결과를 탐색 그래프에 확정합니다.
func (e *Engine) runIntent(ctx context.Context, t *Task, name string, worker *agent.Worker, intent *db.Node, requestID, message string) bool {
	hasChatMessage := message != ""
	e.stampFirstRun(t) // 처음 진짜 실행 → first_run_at을 찍고 deadline을 계산한다(timeout이 있는 작업만)
	e.touch(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // Pause로 취소할 수 있다; 작업 deadline을 가진다
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 의도 #%d 수령 후 되돌리기 실패: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// 이 실행만의 자식 컨텍스트입니다. 플래너의 kill_work가 이 실행만 멈추게 합니다.
	workCtx, workCancel := context.WithCancelCause(ectx)
	e.registerWork(intent.ID, workCancel)
	// 가드 훅을 감싸, steer_work가 이 의도에 대해 도중에 방향 수정을
	// 넣게 합니다(워커의 다음 도구 호출 전에 꺼냄).
	iid := intent.ID
	taskEmit := func(a db.Activity) {
		nid := iid
		a.NodeID, a.Worker = &nid, name
		emit(a)
	}
	label := fmt.Sprintf("%s · #%d", name, iid)
	workCtx = intercept.WithTaskContext(workCtx, t.ID, label, taskEmit)
	// nudges는 일부러 model_error 재실행 루프 밖에 둔다: 빈 회전으로 이어 달리는 상한은 「이 의도」의 총량이고,
	// 한 라운드를 다시 돈다고 한도를 0부터 다시 주면 안 된다.
	hooks := steerHooks{
		inner:  t.Guard.Hooks(),
		drain:  func() (string, bool) { return e.drainSteer(iid) },
		nudges: &atomic.Int64{},
		limit:  e.emptyTurnNudgeLimit(),
		label:  label,
	}
	wTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	var reason harness.TerminalReason
	var wrote agent.WriteCounts
	var err error
	if hasChatMessage {
		reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
	} else {
		reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
	}
	e.EndLLMCall(t.ID)
	// model_error로 끝나면 → 몇 번 더 재실행한다(물러난 뒤 다시 시도). 의도가 아직 이 work에 속하고, 작업이
	// 일시정지/종료/취소되지 않았고【그리고 마무리에 들어가지 않았을】때만 재시도한다; 아니면 해당 분기에 맡긴다(마무리 기간에는
	// 다시 재시도하지 않아, 물러남이 다른 워커(의도 하나를 실행한 뒤 정지)의 부드러운 마무리 시간을 차지하지 않게 한다).
	maxRetries, retryBackoff := e.modelErrorRetryPolicy()
	for attempt := 1; attempt <= maxRetries &&
		retryableWorkerModelError(reason, err) &&
		workCtx.Err() == nil && ectx.Err() == nil && !e.IsPaused(t.ID) && !e.isSettling(t.ID); attempt++ {
		log.Printf("[worker %s] task %s 의도 #%d model_error로 마무리, %v 후 재시도 (%d/%d)",
			name, t.ID, intent.ID, retryBackoff, attempt, maxRetries)
		if sleepCtx(workCtx, retryBackoff) {
			break // 물러나 있는 동안 취소되면(종료/일시정지) → 아래 분기에 넘긴다
		}
		e.BeginLLMCall(t.ID)
		if hasChatMessage {
			reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
		} else {
			reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
		}
		e.EndLLMCall(t.ID)
	}
	// detachWork가 workCtx를 취소하기 전에 중단 상태를 잡아 둡니다. kill은 이 실행의
	// ctx가 취소된 경우입니다(플래너 kill_work). 그때 작업 ctx(ectx)는 계속 돕니다. 일시정지는
	// 작업 ctx(ectx)를 취소합니다. unregister 뒤에 workCtx.Err()를 보면
	// 항상 참입니다(unregister가 취소함). 그러면 끝난
	// 실행이 전부 잘못 중단으로 표시됩니다.
	workCause := context.Cause(workCtx)
	killed := workCtx.Err() != nil && ectx.Err() == nil
	action, completeWork := e.detachWork(intent.ID)
	// 호출자가 기다림을 멈추고, 에이전트가 취소를 따르기 전에 메모리 예약을 놓을 수 있습니다.
	// 이름이 있는 컨텍스트 원인이 기준이고,
	// 그래도 멈춘 실행을 다시 이을 수 있는 상태로 정리합니다.
	if action == "" {
		switch {
		case errors.Is(workCause, agent.AbortWorkPausedByUser):
			action = "pause"
		case errors.Is(workCause, agent.AbortWorkCancelledByUser):
			action = "cancel"
		}
	}
	var controlErr error
	defer func() { completeWork(controlErr) }()
	if action == "pause" {
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 의도 #%d 일시정지 상태 DB 기록 실패: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 의도 #%d 일시정지됨", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	if action == "cancel" {
		// 멈춘 실행을 API에 정리를 넘기기 전에 paused로 둡니다. 취소 뒤에
		// 요청이 끊겨도 의도는 다시 이을 수 있고,
		// 나중의 취소가 정리를 끝낼 수 있습니다. 유령 running 행으로 남지 않습니다.
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 의도 #%d 취소 펜스 DB 기록 실패: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 의도 #%d 정지됨, 취소 정리 대기", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// 일시정지가 실행 도중에 이 런을 취소하면, 의도를 프론티어로 돌려
	// 재개 때 다시 집히게 합니다. 워커는 처음부터 다시 시작하지 않고
	// 대화 기록으로 이전 LLM 대화를 이어 갑니다.
	if ectx.Err() != nil && taskExecutionPaused(context.Cause(ectx)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 의도 #%d 작업 일시정지 되돌리기 실패: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// 작업 타임아웃 마무리의 단단한 안전망 cancel(pause도 아니고 kill도 아님)이 이 run을 취소하면 → exhausted(마무리됨)로 귀결하고,
	// blocked로 잘못 표시하지 않는다. 이때 워커(의도 하나를 실행한 뒤 정지)는 보통 settlement 단계에서 결과를 이미 써 넣었다.
	if ectx.Err() != nil && e.isSettling(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "exhausted"); err != nil {
			log.Printf("[worker %s] task %s 의도 #%d 시간 초과 마감 상태 DB 기록 실패: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 의도 #%d 작업 시간 초과 마감으로 종료(exhausted), %s에 기록", name, t.ID, intent.ID, wrote)
		e.touch(t.ID)
		return true
	}
	// 작업이 완료로 판정되면(done via 일반 경로) → 위의 cancelExec가 이 run을 취소했다. 의도 결과는 의미가 없으니,
	// stopped로 표시한다(blocked가 아님), 이미 완료된 작업의 의도 상태를 더럽히지 않는다.
	if ectx.Err() != nil && isTerminalStatus(e.m.TaskStatus(t.ID)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 의도 #%d 종료 상태 정지 DB 기록 실패: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 의도 #%d 작업이 이미 완료되어 취소됨(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// 플래너가 죽임: stopped로 표시합니다(결과를 쓰지 않고, 자동으로 다시 집지 않음).
	if killed {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 의도 #%d planner 정지 DB 기록 실패: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 의도 #%d 종료됨(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		t.Notify()
		return true
	}
	if err != nil {
		log.Printf("[worker %s] intent %d: %v", name, intent.ID, err)
	}
	// terminal 분기: 걸음 수 상한에 부딪힘 ≠ 완료. max_turns→exhausted(플래너(의도만 생성)는 이것으로 이 방향이
	// 시도했지만 진짜 끝내지 못해 각도를 바꿔야 함을 알고, 이미 다뤄서 영원히 건너뛴 것으로 보지 않는다); 오류→blocked; 정상→done.
	state := "done"
	switch {
	case err != nil:
		state = "blocked"
	case reason == harness.ReasonMaxTurns:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 스텝 상한에 도달(exhausted), 이번 기록 %s", name, intent.ID, wrote)
	case reason == harness.ReasonTimeout:
		state = "exhausted"
		log.Printf("[worker %s] intent %d 실행 시간 초과(exhausted), 마감 후 %s에 기록", name, intent.ID, wrote)
	}
	if state == "blocked" && isTaskLLMChainExhausted(err) {
		_ = t.Store.SetIntentBlockedReason(intent.ID, db.IntentBlockedLLMQuota)
	} else {
		if stateErr := transitionIntentState(t.Store, intent.ID, "running", state); stateErr != nil {
			log.Printf("[worker %s] task %s 의도 #%d 종료 상태 %s DB 기록 실패: %v", name, t.ID, intent.ID, state, stateErr)
		}
	}
	log.Printf("[worker %s] task %s 의도 #%d 종료: %s (%s에 기록)", name, t.ID, intent.ID, state, wrote)
	e.touch(t.ID)
	t.NotifyDone(intent.ID) // 결과가 그래프를 바꿈 → 플래너를 깨웁니다(방금 끝난 의도 id와 함께).
	return true
}

// runDetachedIntent는 일시정지된 의도 하나를 워커 풀 밖에서, 자기
// 고루틴으로 돌립니다. 사람 메시지 경로입니다. 의도 상태를 스스로 paused에서 running으로
// 바꿉니다('open'은 거치지 않음). 그래서 'open'만 집는 풀은
// 경주할 수 없습니다. "의도마다 실행은 최대 하나"는 여전히 맞습니다. 이기는
// CAS가 유일한 입구이고, 일시정지가 정리될 때 work[intentID]가 비워졌기 때문입니다.
// 프론티어 자리를 놓고 다투지 않으므로, 풀 자리가 모두 바빠도 사용자 메시지는
// 워커를 바로 이어 갑니다(메인 에이전트 채팅 처리기가 실행을 직접
// 시작하는 것과 같습니다). 띄운 고루틴이 실행 전체의 작업 조작 입장을
// 하나 가지고, 컨텍스트의 뿌리는 ctx입니다(HTTP 요청이 아니라 서버 뿌리를 넘기세요.
// 연결이 끊겨도 실행이 중간에 갇히지 않고, 작업 일시정지, 삭제, 종료는
// 여전히 멈춥니다). 실행을 시작하지 못하면 오류를 돌려주고,
// 그때 의도는 그대로 둡니다.
// 초보용: 화면의 사람 메시지는 워커 풀 밖에서 그 의도의 대화를 바로 이어 갑니다.
func (e *Engine) runDetachedIntent(ctx context.Context, t *Task, intentID int64, requestID, message, agentMessage string) error {
	if !e.beginTaskOperation(t.ID) {
		return fmt.Errorf("작업을 삭제하는 중입니다")
	}
	release := true
	defer func() {
		if release {
			e.decInflight(t.ID)
		}
	}()
	_, worker := e.snapshotFor(t)
	if worker == nil {
		return fmt.Errorf("워커(의도 하나를 실행한 뒤 정지)가 아직 준비되지 않음")
	}
	node, err := t.Store.GetNode(intentID)
	if err != nil {
		return err
	}
	if node == nil || node.Kind != db.KindIntent {
		return fmt.Errorf("의도를 찾을 수 없습니다")
	}
	changed, err := t.Store.CompareAndSetIntentState(intentID, "paused", "running")
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 의도가 더 이상 paused 상태가 아닙니다", db.ErrIntentStateConflict)
	}
	node.State, node.Owner = "running", "chat"
	// 실행이 시작되기 전에 사람 턴을 보이는 활동으로 기록합니다. 그래서 어떤 워커 단계보다
	// 앞에 정렬되고, 실행 없이 혼자 나타나지 않습니다.
	// 화면 글은 짧게 둡니다. ExecuteWithMessage가 서버가 푼
	// 참조 스냅샷을 LLM 입력으로 의도 대화 기록에 씁니다.
	uid := intentID
	e.emitActivity(t, db.Activity{NodeID: &uid, Worker: "user", Kind: "user", Summary: message, Detail: message})
	release = false // 입장 허가의 소유는 고루틴으로 넘어갑니다.
	go func() {
		defer e.decInflight(t.ID)
		e.runIntent(ctx, t, "chat", worker, node, requestID, agentMessage)
	}()
	return nil
}

func taskExecutionPaused(cause error) bool {
	var abort *agent.AbortCause
	if !errors.As(cause, &abort) {
		return false
	}
	switch abort.Code {
	case "paused_by_user", "paused_by_orchestrator", "paused_on_reload", "paused_race_guard",
		"queued_for_admission", "llm_unavailable_queued", "task_deleted":
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) (done bool) {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func (e *Engine) claimNext(t *Task, name string) *db.Node {
	fr, _ := t.Store.Frontier(20)
	for _, in := range fr {
		if ok, _ := t.Store.ClaimIntent(in.ID, name); ok {
			return in
		}
	}
	return nil
}
