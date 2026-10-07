package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 작업급 타임아웃 조정기(설계 문서 §4/§8.5 참고). 엔진이 작업을 마무리할 때 자산 그래프와 탐색 그래프 쪽 기록을 순서대로 닫게 한다.
// 절대 벽시계: timeout이 있는 작업마다 타이머 goroutine 하나. 시각이 되면 순서 있는 마무리 시퀀스를 돌린다:
//   ① settling → ② 워커는 새 의도 수령을 멈춤 / ③ 플래너는 일반 notify를 버림
//   ④ 실행 중인 워커 drain을 기다림(grace 한도) → ⑤ 마지막 한 바퀴 플래너 판정 → ⑥ 종료 상태 확정(가드 포함)

const (
	settleDrainGrace     = 90 * time.Second // 실행 중인 워커가 우아하게 마무리할 상한. 넘으면 강제 cancel
	deadlinePollInterval = 2 * time.Second  // deadline이 찍히지 않았거나 LLM이 준비되지 않았을 때의 폴링 간격
	deadlineMaxSleep     = 30 * time.Second // 한 번의 최대 수면(종료 상태를 주기적으로 다시 보기 쉽게)
)

// ---------- settling 상태 ----------

func (e *Engine) isSettling(taskID string) bool {
	v, _ := e.settling.Load(taskID)
	b, _ := v.(bool)
	return b
}

// markSettling은 settling을 켭니다. 처음 호출한 쪽만 true입니다.
func (e *Engine) markSettling(taskID string) bool {
	_, loaded := e.settling.LoadOrStore(taskID, true)
	return !loaded
}

// ---------- 실행 중 카운트(worker.Execute + planner.Plan), drain에 사용 ----------

func (e *Engine) inflightCounter(taskID string) *int64 {
	v, _ := e.inflight.LoadOrStore(taskID, new(int64))
	return v.(*int64)
}

// beginTaskOperation은 그 작업에 속한 조작을 원자적으로 등록합니다. 삭제가
// 이미 장벽을 세웠으면 등록하지 않습니다. 그래서 삭제 처리기는
// inflight가 0이 되길 기다릴 수 있고, 확인 뒤에 시작이 끼어 정리 후 파일을 다시 만들지 못합니다.
func (e *Engine) beginTaskOperation(taskID string) bool {
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(taskID) {
		return false
	}
	atomic.AddInt64(e.inflightCounter(taskID), 1)
	return true
}

func (e *Engine) decInflight(taskID string) { atomic.AddInt64(e.inflightCounter(taskID), -1) }
func (e *Engine) inflightCount(taskID string) int64 {
	return atomic.LoadInt64(e.inflightCounter(taskID))
}

// ---------- 마감 ----------

// taskDeadline은 작업의 절대 마감(유닉스)을 돌려줍니다. 이 프로세스의
// 맵을 우선합니다(이번 세션에 찍힘). 없으면 DB에 읽어 둔 값(재시작)으로 가고,
// 맵에 다시 넣습니다. 0은 시간 제한 없음, 또는 아직 안 찍힘입니다.
func (e *Engine) taskDeadline(t *Task) int64 {
	if v, ok := e.deadline.Load(t.ID); ok {
		return v.(int64)
	}
	deadlineAt := t.lifecycleSnapshot().DeadlineAt
	if deadlineAt > 0 {
		e.deadline.Store(t.ID, deadlineAt)
		return deadlineAt
	}
	return 0
}

// resetTimeoutRevival은 PostgreSQL이 timeout을 running으로 확정하고
// first_run_at/deadline_at을 지운 뒤에, 이번 실행의 시간 초과 상태만 지웁니다.
// 설정한 TimeoutSeconds는 Task에 남으므로, 다음 진짜 플래너/워커 실행이
// 예산 전체를 새로 찍습니다. coordStarted도 지웁니다. 시간 초과를 만든
// 조정자는 이미 끝났거나, 돌아오는 마지막 길에 있기 때문입니다.
func (e *Engine) resetTimeoutRevival(taskID string) {
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.coordStarted.Delete(taskID)
}

// stampFirstRun은 시간 제한 작업의 첫 진짜 실행(LLM 준비됨)에
// first_run_at과 deadline_at을 남깁니다. 프로세스마다 한 번. 시간 제한이 없으면 아무 일도 안 합니다.
func (e *Engine) stampFirstRun(t *Task) {
	if t.TimeoutSeconds <= 0 {
		return
	}
	if _, loaded := e.stamped.LoadOrStore(t.ID, true); loaded {
		return
	}
	dl, err := e.m.StampTaskFirstRun(t.ID)
	if err != nil {
		log.Printf("[deadline] task %s first_run 도장 실패: %v", t.ID, err)
		e.stamped.Delete(t.ID) // 다음 재시도를 허용
		return
	}
	if dl > 0 {
		e.deadline.Store(t.ID, dl)
		log.Printf("[deadline] task %s 최초 실행, 마감 %s", t.ID, time.Unix(dl, 0).Format("2006-01-02 15:04:05"))
	}
}

// clockCtx는 실행 컨텍스트 위에 작업의 TaskClock(절대 마감)을 얹습니다.
// 워커/플래너가 벽시계 예산을 맞추고, 이번 실행용 말과 작업 시간 초과용
// 마무리 말을 고르게 합니다. final은 조정자가 시키는 마지막 플래너 라운드입니다.
func (e *Engine) clockCtx(base context.Context, t *Task, final bool) context.Context {
	dl := e.taskDeadline(t)
	if dl <= 0 && !final {
		return base // 시간 제한이 없으면 동작을 바꾸지 않습니다.
	}
	return agent.WithTaskClock(base, agent.TaskClock{DeadlineUnix: dl, Final: final})
}

// ---------- 조정기 ----------

// startDeadlineCoordinator는 작업마다 마감 타이머를 한 번만 띄웁니다(여러 번 해도 한 번).
// Run()과 재시작 복구 경로에서 부릅니다. 그래서 지금 돌지 않는 시간 제한 작업도
// 플래너/워커 루프가 없어도 마감 뒤에 정리됩니다.
// 초보용: 엔진이 작업의 시간 제한을 세고, 끝나면 마무리를 시작하게 합니다.
func (e *Engine) startDeadlineCoordinator(ctx context.Context, t *Task) {
	if t == nil || t.TimeoutSeconds <= 0 {
		return
	}
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.coordStarted.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1)
	e.deleteMu.RUnlock()
	runTaskRoutine(rt, func(loopCtx context.Context) { e.deadlineCoordinator(loopCtx, t) })
}

// deadlineCoordinator는 작업의 절대 마감까지 기다린 뒤 정리
// 절차를 돌립니다. 절대 벽시계라서, 일시정지 중에도 시간은 계속 갑니다.
func (e *Engine) deadlineCoordinator(ctx context.Context, t *Task) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			return // 이미 끝났습니다(목표 달성 또는 실패). 시간 초과로 돌릴 일이 없습니다.
		}
		dl := e.taskDeadline(t)
		if dl <= 0 {
			if sleepCtx(ctx, deadlinePollInterval) { // 아직 시각을 안 찍었습니다(작업이 진짜로 돌지 않음).
				return
			}
			continue
		}
		if remaining := time.Until(time.Unix(dl, 0)); remaining > 0 {
			nap := remaining
			if nap > deadlineMaxSleep {
				nap = deadlineMaxSleep
			}
			if sleepCtx(ctx, nap) {
				return
			}
			continue
		}
		e.settleTask(ctx, t)
		return
	}
}

// settleTask는 정해진 정리 절차를 한 번 돌립니다(§4의 ①–⑥).
// 초보용: 작업 시간이 다 되면 엔진이 이 순서로 마무리를 한 번만 합니다.
func (e *Engine) settleTask(ctx context.Context, t *Task) {
	if !e.markSettling(t.ID) {
		return
	}
	log.Printf("[deadline] task %s 시간 초과 상한에 도달, 마무리 시퀀스 진입", t.ID)

	// ④ 실행 중인 워커/플래너 drain을 기다림(실행 중 run은 상한에 맞춘 MaxDuration 때문에 스스로 마무리에 들어감);
	// grace를 넘어도 비지 않으면 → 그 작업 exec ctx를 강제 cancel(settling-aware 분기가 올바르게 분류).
	hardStop := time.Now().Add(settleDrainGrace)
	for e.inflightCount(t.ID) > 0 {
		if time.Now().After(hardStop) {
			log.Printf("[deadline] task %s drain 시간 초과(%s), 실행 중인 run을 강제 취소", t.ID, settleDrainGrace)
			e.cancelExec(t.ID, agent.AbortSettleDrainTimeout)
			_ = sleepCtx(ctx, 3*time.Second) // 워커 분기에 저장/분류할 시간을 조금 줌
			break
		}
		if sleepCtx(ctx, 500*time.Millisecond) {
			return // 엔진 전체 종료
		}
	}

	// ⑤ 마지막 한 바퀴 플래너(작업 타임아웃 문구, 최종 목표 판정, 새 의도는 만들지 않음).
	met := e.runFinalPlannerRound(ctx, t)
	if !e.beginTaskOperation(t.ID) {
		return
	}
	defer e.decInflight(t.ID)

	// ⑥ 종료 상태 확정(가드 포함): met → done(completed); 아니면 timeout. 일반 경로가 먼저 done을 기록했으면,
	// 가드(SetTaskStatusGuarded)가 덮어쓰기를 거부해 completed 의미를 유지한다.
	status := "timeout"
	if met {
		status = "done"
	}
	won, err := e.m.SetTaskStatusGuarded(t.ID, status)
	switch {
	case err != nil:
		log.Printf("[deadline] task %s 종료 상태 기록 실패: %v", t.ID, err)
	case won:
		log.Printf("[deadline] task %s 마무리 완료, 종료 상태=%s", t.ID, status)
	default:
		log.Printf("[deadline] task %s 마무리 시점에 이미 종료 상태, 원래 상태 유지", t.ID)
	}
}

// runFinalPlannerRound는 마지막 플래너 라운드를 정확히 한 번 돌립니다.
// 작업 시간 초과용 플래너 말을 씁니다(마지막 목표 판정, 새 의도는 없음). LLM이
// 준비될 때까지 기다립니다(ctx 한도 안). 끝낼 수 있는 작업을 시간 초과로 잘못 보지 않으려고요.
func (e *Engine) runFinalPlannerRound(ctx context.Context, t *Task) (met bool) {
	if e.IsDeleting(t.ID) {
		return false
	}
	planner, _ := e.snapshotFor(t)
	for planner == nil {
		if sleepCtx(ctx, deadlinePollInterval) {
			return false
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			return false
		}
		if e.IsDeleting(t.ID) {
			return false
		}
		planner, _ = e.snapshotFor(t)
	}
	// 독립 ctx(execCancel에 매달지 않아 pause/강제 cancel이 이 마지막 라운드를 끊지 않게 함). Final과 함께 작업 타임아웃 문구를 넣는다.
	fctx := e.clockCtx(ctx, t, true)
	if !e.beginTaskOperation(t.ID) {
		return false
	}
	defer e.decInflight(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
		Summary: fmt.Sprintf("작업 시간 초과 마무리·최종 판정(%d라운드)", e.nextPlannerRound(t.ID))})
	tTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	met, reason, err := planner.Plan(fctx, tTaskID, e.m.assets, t.Store, t.Goal, t.drainTriggers(), emit)
	e.EndLLMCall(t.ID)
	if err != nil {
		log.Printf("[deadline] task %s 최종 계획 오류: %v", t.ID, err)
	} else if met {
		log.Printf("[deadline] task %s 최종 판정 목표 달성: %s", t.ID, reason)
	}
	return met
}
