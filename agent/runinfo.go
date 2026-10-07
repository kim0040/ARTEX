package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// RunInfo 는 도구 호출이 어느 실행에 속하는지 적습니다. 도구를 조립할 때는
// (ctx, agentKey)만 받고, 작업/탐색 id 는 호출자 인자에 있지 ctx 에는 없습니다.
// 그래서 조립 때 연결하는 것(지금 server/assembly.go 의 Skill 장부)은
// 호출을 어느 작업에 붙일지 모릅니다. 각 실행은 AugmentTools 를 부르기 전에
// 자기 RunInfo 를 붙이고, 연결 클로저가 그것을 한 번 읽어 붙잡습니다.
// 도구 층까지 인자를 꿰지 않아도 실행마다 귀속이 맞습니다. TaskClock 과 같은 방식입니다(taskclock.go).
//
// 영값이면 귀속을 모릅니다. 읽는 쪽은 항상 없어도 되게 다뤄야 합니다.
// 초보: 워커의 의도, 플래너의 탐색, 대화 세션 중 어디서 도구가 쓰였는지 장부가 여기서 구분합니다.
type RunInfo struct {
	TaskID        int64  // 작업 등록 id. 작업이 아닌 실행(대화 세션)은 0
	ExplorationID int64  // 탐색 id. 모르면 0
	IntentID      int64  // 워커의 의도 노드. 플래너/메인 에이전트/대화는 0
	SessionID     string // 대화 id. 작업 실행이면 비어 있음
}

// explorationID 는 저장소의 탐색 id 를 읽습니다. 저장소가 nil 이어도 괜찮습니다
// (테스트에서는 플래너와 워커를 저장소 없이 돌릴 수 있습니다).
func explorationID(ts *db.ExplorationStore) int64 {
	if ts == nil {
		return 0
	}
	return ts.ID()
}

type runInfoKey struct{}

// WithRunInfo 는 실행 귀속을 ctx 에 붙입니다.
func WithRunInfo(ctx context.Context, ri RunInfo) context.Context {
	return context.WithValue(ctx, runInfoKey{}, ri)
}

// RunInfoFrom 은 RunInfo 를 읽습니다. 없으면 영값입니다.
func RunInfoFrom(ctx context.Context) RunInfo {
	if v, ok := ctx.Value(runInfoKey{}).(RunInfo); ok {
		return v
	}
	return RunInfo{}
}
