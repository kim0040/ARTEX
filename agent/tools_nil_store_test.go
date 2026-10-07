package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// buildDomainReg 뒤의 서버용 ToolSet 은 ExplorationStore 가 nil 입니다.
// 도구 표가 그 도구를 아무 에이전트에나 묶을 수 있습니다. 작업 안에서 돌지
// 않는 에이전트도 포함합니다. 그래서 모든 도메인 도구는 nil 저장소로 호출돼도
// 살아야 합니다. 여기서 nil 역참조가 나면 하네스 자신의 고루틴에서 돌고,
// 서버의 recover() 가 잡지 못해 프로세스 전체가 죽습니다.
func TestDomainToolsSurviveNilStores(t *testing.T) {
	inputs := []string{
		`{}`,
		`{"id":379,"asset_id":1,"goal_id":1,"evidence_id":1,"intent_id":1,"node_id":1,"work_id":1,` +
			`"summary":"x","reason":"x","name":"x","severity":"low","vulnclass":"x","text":"x","q":"x"}`,
	}
	for _, tool := range NewToolSet(nil, "").AllDomainTools() {
		for _, in := range inputs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s panicked with nil stores on %s: %v", tool.Name(), in, r)
					}
				}()
				if _, err := tool.Call(context.Background(), json.RawMessage(in), nil); err != nil {
					t.Fatalf("%s returned a transport error: %v", tool.Name(), err)
				}
			}()
		}
	}
}

// node_detail 이 프로세스를 죽인 도구입니다. 이제 죽지 않고, 쓸 수 있는 거절로 답해야 합니다.
func TestExplorationToolRefusesWithoutTask(t *testing.T) {
	ts := NewToolSet(nil, "")
	res, err := ts.NodeDetailTool().Call(context.Background(), json.RawMessage(`{"id":379}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Flatten(), "작업 맥락(탐색 그래프)") {
		t.Fatalf("want an explanatory tool error, got IsError=%v %q", res.IsError, res.Flatten())
	}
}

// 패닉이 나는 도구는 도구 오류로 내려가야 합니다. 실행은 이어지고 프로세스는
// 남습니다. 감독이 재생 때 같은 충돌로 다시 시작하지 않습니다.
func TestGuardPanicConvertsPanicToToolError(t *testing.T) {
	boom := actool.Build(actool.Spec{
		Name: "boom",
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			panic("nil map write")
		},
	})
	res, err := guardPanic(boom).Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Flatten(), "nil map write") {
		t.Fatalf("want the panic reported as a tool error, got IsError=%v %q", res.IsError, res.Flatten())
	}
}
