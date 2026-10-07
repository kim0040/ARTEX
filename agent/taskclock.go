package agent

import (
	"context"
	"time"
)

// TaskClock 은 작업의 절대 마감을 워커/플래너 실행에 실어, 실행이 자기 벽시계 예산을
// 작업에 남은 시간으로 맞추고 마무리 문구(한 번 실행인지, 작업 시간 초과인지)를 고르게 합니다.
// 엔진이 실행 ctx 에 붙입니다. 영값이면 작업 단위 시간 제한이 없고, 예전과 똑같이 동작합니다.
// 초보: 작업 화면의 제한 시간이 워커와 플래너가 얼마나 더 달릴지를 여기서 자릅니다.
type TaskClock struct {
	DeadlineUnix int64 // 절대 마감(unix 초). 0 이면 작업 시간 제한 없음
	Final        bool  // 조정자가 시키는 마지막 플래너 라운드(지금 작업을 끝냄)
}

type taskClockKey struct{}

// WithTaskClock 은 이번 실행의 ctx 에 TaskClock 을 붙입니다.
func WithTaskClock(ctx context.Context, tc TaskClock) context.Context {
	return context.WithValue(ctx, taskClockKey{}, tc)
}

// taskClockFrom 은 TaskClock 을 읽습니다. 없으면 영값입니다.
func taskClockFrom(ctx context.Context) TaskClock {
	if v, ok := ctx.Value(taskClockKey{}).(TaskClock); ok {
		return v
	}
	return TaskClock{}
}

// clampMaxDuration 은 작업 마감을 실행 자신의 벽시계 예산에 겹칩니다.
//   - ownBudget = 에이전트 자신의 run_seconds (0 = 무제한).
//   - eff = 쓸 MaxDuration 입니다. 1초 밑으로 내리지 않습니다. 0 이하는 하네스가 "무제한"으로 읽습니다.
//     clamped = 작업 마감이 이번 실행을 묶는지입니다(남은 시간 ≤ ownBudget, 또는 ownBudget 이 무제한).
//     마감이 없으면 (ownBudget, false) 를 그대로 돌려줍니다.
func clampMaxDuration(deadlineUnix int64, ownBudget time.Duration) (eff time.Duration, clamped bool) {
	if deadlineUnix <= 0 {
		return ownBudget, false
	}
	remaining := time.Until(time.Unix(deadlineUnix, 0))
	if remaining < time.Second {
		remaining = time.Second // max(1, …): 0 이하를 넘기지 않습니다(하네스는 0 을 무제한으로 봅니다)
	}
	// 작업 마감이 이번 실행 시간을 묶을 때 clamped 입니다. 남은 시간 ≤ 자기 예산이거나,
	// 에이전트에 자기 시간 예산이 없을 때입니다(그러면 남은 시간이 항상 묶습니다).
	clamped = ownBudget <= 0 || remaining <= ownBudget
	eff = remaining
	if ownBudget > 0 && ownBudget < remaining {
		eff = ownBudget
	}
	return eff, clamped
}
