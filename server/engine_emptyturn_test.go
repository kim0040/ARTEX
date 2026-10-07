package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 공회전 라운드(생각만 있고, 본문도 도구도 없음)의 식별과 이어서 실행은 steerHooks.Stop을 본다.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "먼저 포트 스캔"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"사고만", []llm.Message{llm.UserText("시작"), assistantThinking("생각하기")}, true},
		{"사고+도구", []llm.Message{llm.UserText("시작"), toolUse}, false},
		{"사고+본문", []llm.Message{assistantThinking("생각하기"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("결론")},
		}}, false},
		{"본문에 공백 문자만 있음", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"완전히 빈 assistant 턴", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// 도구 결과는 user 역할이므로, 판정은 그 앞의 assistant까지 거슬러 올라가야 하며, 가까운 항목으로 잘못 판정하면 안 된다.
		{"마지막 항목이 도구 결과임", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant 메시지가 없음", []llm.Message{llm.UserText("시작")}, false},
		{"빈 기록", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks는 프로그래밍 가능한 inner HookRunner로, steerHooks가 inner 결정을 존중하는지 검증하는 데 쓴다.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("서브도메인부터 열거해야 한다")}

	t.Run("공회전 턴에 이어 실행 지시를 주입", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("공회전 턴은 강제 정지하면 안 됨")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("본문이나 도구가 있으면 개입하지 않음", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("스캔을 완료했고, 열린 포트를 찾지 못함")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("정상 마무리가 공회전으로 오판됨: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("개입하지 않았을 때 카운트하면 안 됨, got %d", n)
		}
	})

	t.Run("상한에 도달한 뒤 마무리를 허용", func(t *testing.T) {
		const limit = 5 // 사용자가 「빈 응답 재시도 횟수」를 5로 설정함
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d번째에는 아직 할당량 안이어야 함, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("상한을 넘었는데도 주입 중: %v", blocking)
		}
	})

	// 「빈 응답 재시도 횟수」를 -1로 두면 이 층을 끄며, emptyTurnNudgeLimit은 0으로 해석된다.
	t.Run("설정이 꺼져 있으면 개입하지 않음", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("꺼진 상태인데도 주입 중: %v", blocking)
		}
	})

	t.Run("inner가 강제 정지를 결정하면 덧붙이지 않음", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard가 마무리를 거부"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard가 마무리를 거부" || blocking != nil {
			t.Fatalf("inner의 강제 정지가 다시 쓰임: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner에 양보할 때 할당량을 소모하면 안 됨, got %d", n)
		}
	})

	t.Run("inner가 이미 이어 실행을 요구하면 덧붙이지 않음", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard의 이어 실행 이유"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard의 이어 실행 이유" {
			t.Fatalf("inner의 이어 실행 메시지가 다시 쓰임: %v", blocking)
		}
	})

	t.Run("카운터를 장착하지 않으면 동작이 그대로임", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 예를 들어 나중에 다른 호출 지점이 nudges를 넘기는 것을 잊은 경우
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("카운터가 없을 때 주입하면 안 됨: %v", blocking)
		}
	})
}
