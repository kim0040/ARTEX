package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause 는 에이전트 실행의 context 가 왜 취소됐는지 이름입니다. 취소하는 곳은
// 모두 이것을 붙여, 활동 기록이 누가 멈췄는지 말할 수 있게 합니다.
//
// 초보용: 플래너, 워커, 메인 에이전트 실행이 중간에 끊기면 UI 활동 기록에
// 누가 왜 멈췄는지 이 문장으로 보여 줍니다. 코드 값은 바꾸지 않습니다.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef 는 실행 중 세부 내용이 들어간 취소 이유를 만듭니다.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// 작업 전체의 실행 context.
	AbortPausedByUser = cause("paused_by_user", "사용자가 작업을 일시정지했습니다",
		"사용자가 작업 제어 인터페이스(POST /api/tasks/{id}/control, action=pause)로 작업을 일시정지했습니다. 이번 플래너/워커 실행은 능동적으로 취소됩니다. 실행 중이던 인텐트는 frontier(open)로 돌아가고, 작업을 재개하면 다시 가져가며, 저장된 대화 기록이 있으면 이어서 실행합니다")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "오케스트레이션 에이전트가 작업을 일시정지했습니다",
		"오케스트레이션 에이전트가 pause_task 도구로 이 작업을 일시정지했습니다. 이번 플래너/워커 실행은 능동적으로 취소됩니다. 실행 중이던 인텐트는 frontier(open)로 돌아가고, 재개하면 다시 실행합니다")
	AbortTaskDeleted = cause("task_deleted", "작업이 삭제되었습니다",
		"작업이 삭제되는 중입니다(DELETE /api/tasks/{id}). 삭제 장벽이 이 작업에서 돌고 있던 플래너, 워커, 메인 에이전트를 취소했습니다. 이번 실행 결과는 더 이상 쓰이지 않습니다")
	AbortPausedOnReload = cause("paused_on_reload", "백엔드가 일시정지 상태를 복구했습니다",
		"백엔드가 시작할 때 데이터베이스에 저장된 상태로 작업 일시정지를 복구했습니다. 이번 실행은 취소됩니다. 정상적이면 복구 단계에는 돌고 있는 에이전트가 없습니다")
	AbortGoalMet = cause("goal_met", "플래너가 목표 달성을 판정했습니다",
		"플래너가 작업 목표 달성을 판정하고 작업을 done으로 둔 뒤, 아직 돌고 있던 워커를 취소합니다. 그 인텐트는 실패가 아니라 stopped로 표시됩니다")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "시간 초과 마무리 대기가 끝났습니다",
		"작업이 timeout에 도달한 뒤 돌고 있던 워커가 부드럽게 마치기를 기다렸지만, 90초 drain 여유가 부족해 강제 취소합니다. 인텐트는 exhausted로 표시되고, 마무리 중에 이미 쓴 사실과 자산은 남습니다")

	// 워커가 집어 든 의도 하나의 context.
	AbortKilledByPlanner = cause("killed_by_planner", "플래너가 이 인텐트를 종료했습니다",
		"플래너가 kill_work로 이 인텐트를 능동적으로 종료했습니다. 보통 방향이 빗나갔거나 더 볼 가치가 없다는 뜻입니다. 인텐트는 stopped로 표시되고 자동으로 다시 집어가지 않습니다")
	AbortWorkPausedByUser = cause("work_paused_by_user", "사용자가 이 워커 인텐트를 일시정지했습니다",
		"사용자가 돌고 있던 워커를 일시정지했습니다. 이번 호출은 취소되고 인텐트는 paused가 됩니다. 이미 등록한 인텐트, 사실, 취약점, 활동 기록은 모두 남고, 재개하면 저장된 대화 기록이 있을 때 이어서 실행합니다")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "사용자가 이 워커 인텐트를 삭제했습니다",
		"사용자가 돌고 있던 워커를 삭제했습니다. 이번 호출은 취소됩니다. 워커가 쓰기 구간을 빠져나간 뒤, 서버는 사용자가 고른 삭제 방식대로 이 인텐트를 처리합니다. 가짜 삭제는 삭제됨으로만 표시하고 산출을 모두 남깁니다. 진짜 삭제는 이 인텐트와, 오직 이것만 받치고 있던 하위 노드를 함께 지웁니다")
	AbortWorkFinished = cause("work_finished", "워커가 정상 종료되어 context를 해제했습니다",
		"워커가 정상 종료되어, 엔진이 detachWork에서 그 context 자원을 해제합니다. 이것은 실행 중단이 아닙니다. 중단 메시지에 보인다면 취소와 마무리 이벤트가 겹친 것입니다")
	AbortPausedRaceGuard = cause("paused_race_guard", "일시정지 중에는 새 실행을 거부합니다",
		"작업이 일시정지일 때 엔진은 새 실행 context를 내주지 않습니다. claim과 일시정지 사이의 경합으로 워커가 계속 시작되는 것을 막습니다. 이미 집어 든 인텐트는 frontier로 돌아갑니다")

	// 메인 에이전트와 독립 대화의 context.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "사용자가 이번 대화를 중지했습니다",
		"사용자가 중지를 눌러 이번 메인 에이전트 또는 세션 에이전트 실행을 끊었습니다. 이미 생긴 활동 기록은 남고, 다음 메시지를 계속 보낼 수 있습니다")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "작업 일시정지가 메인 에이전트 대화도 끊었습니다",
		"사용자가 작업을 일시정지할 때, 돌고 있던 메인 에이전트 대화도 함께 취소됩니다. 이미 생긴 활동 기록은 남습니다. 작업을 재개해도 이번 메시지는 자동으로 다시 돌리지 않습니다")
	AbortChatTurnFinished = cause("chat_turn_finished", "이번 대화가 정상 종료되어 context를 해제했습니다",
		"이번 대화가 정상 종료되어, 서버가 그 회의 context 자원을 해제하는 중입니다. 이것은 실행 중단이 아닙니다. 중단 메시지에 보인다면 취소와 마무리 이벤트가 겹친 것입니다")

	// 프로세스 종료와, 한 번 실행의 하드 타임아웃.
	AbortShutdown = cause("shutdown", "백엔드 프로세스가 종료되는 중입니다",
		"백엔드 프로세스가 SIGINT 또는 SIGTERM을 받아 재시작, 업데이트, 또는 종료 중입니다. 돌고 있던 에이전트는 모두 취소됩니다. 재시작 후 남아 있던 running 인텐트는 open으로 돌아가 다시 실행됩니다")
	AbortRunHardTimeout = cause("run_hard_timeout", "한 번 실행의 하드 타임아웃이 켜졌습니다",
		"한 번 실행이 부드러운 벽시계 예산과 추가 여유를 넘었습니다. 모델 요청이나 어떤 도구가 오래 돌아오지 않아, 보통의 턴 경계 마무리를 할 수 없습니다. 중단 직전, 결과를 아직 반환하지 않은 마지막 도구 호출을 확인하세요")
)

// AbortReason 은 취소된 실행 context 에 붙은 이름 있는 이유를 꺼냅니다.
// 초보: 엔진이 멈춘 실행을 UI 활동 기록에 어떤 문장으로 보여줄지 여기서 고릅니다.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "상위 context가 deadline에 도달했습니다",
			"상위 context가 deadline에 도달했지만, 설정 쪽이 WithTimeoutCause로 이름 있는 이유를 붙이지 않았습니다: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "취소 쪽에 이름 있는 이유가 없습니다",
			"상위 context가 취소됐지만, 취소 쪽이 context.WithCancelCause로 이름 있는 이유를 붙이지 않았습니다. agent/cancelcause.go에 이유를 등록하고 그 취소 지점에 연결하세요", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
