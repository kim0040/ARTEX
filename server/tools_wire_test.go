package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func names(tools []actool.CoreTool) map[string]actool.CoreTool {
	m := map[string]actool.CoreTool{}
	for _, t := range tools {
		m[t.Name()] = t
	}
	return m
}

// TestWireTools는 살아있는 개발 PG로 끝까지 확인합니다. wireTools가 카탈로그를 심고,
// ToolResolve는 워커에게 record_fact를 남기고 플래너에게는 뺍니다(안
// 묶임). 고친 설명과 주입된 기본값이 통과하는지도 봅니다.
func TestWireTools(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer pg.Close()

	wireTools(pg, nil) // domainReg가 nil: 이 테스트는 거르기/장식만 보고, 주입은 안 봅니다.
	t.Cleanup(func() { agent.ToolResolve = nil })
	// 대화형 셸이 Bash 설명을 장식하지 못하게 합니다. DB에서 워커의
	// interactive_shell이 true면 ToolResolve가 Bash 설명에 문장을 붙입니다.
	// 이 테스트에서는 그 장식을 꺼, 장식 없는
	// 통과 확인이 DB 상태와 상관없이 성립하게 합니다.
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "1")

	// 심기 때문에 카탈로그가 채워졌습니다.
	rf, err := pg.GetTool("record_fact")
	if err != nil || rf == nil {
		t.Fatalf("record_fact not seeded: %v", err)
	}

	// 워커처럼 기본 목록을 만듭니다. 도메인 도구와 기본값. "worker"로 푼 뒤
	// "planner"로도 풉니다.
	ts := agent.NewToolSet(nil, "")
	base := append(ts.WorkerTools(), actool.DefaultTools()...)
	ctx := context.Background()

	worker := names(agent.ToolResolve(ctx, "worker", base))
	if _, ok := worker["record_fact"]; !ok {
		t.Error("worker lost record_fact")
	}
	// SDK 일반 도구는 심지 않으므로 ToolResolve가 그대로 통과시킵니다
	// (같은 객체, 원래 설명, 장식 없음). 시작 때 카탈로그 밖 행은
	// 안 지우므로, 나중에 사용자가 만든 도구도 남습니다.
	if bash, ok := worker["Bash"]; !ok {
		t.Error("worker lost Bash (should pass through)")
	} else if bash.Description() != actool.NewBash().Description() {
		t.Error("Bash should pass through undecorated, but description changed")
	}
	// 플래너는 record_fact에 안 묶입니다. 그것을 포함한 기본을 풀면 빠집니다.
	planner := names(agent.ToolResolve(ctx, "planner", base))
	if _, ok := planner["record_fact"]; ok {
		t.Error("planner should not get record_fact (not bound)")
	}

	// 설명을 고치고 스칼라 매개변수에 기본값을 더한 뒤, 풀린
	// 워커 도구에 둘 다 반영되는지 확인합니다. 끝날 때 코드 기본값으로 되돌립니다.
	t.Cleanup(func() { _ = pg.UpsertToolForce(rf.Key, rf.Description, rf.Schema, mustJSON(rf.Agents)) })
	var schema map[string]any
	_ = json.Unmarshal(rf.Schema, &schema)
	if props, ok := schema["properties"].(map[string]any); ok {
		if conf, ok := props["confidence"].(map[string]any); ok {
			conf["default"] = "inferred"
		}
	}
	edited := mustJSON(schema)
	if err := pg.UpdateTool(rf.Key, "EDITED DESC", edited, mustJSON([]string{"worker"}), true); err != nil {
		t.Fatal(err)
	}

	worker2 := names(agent.ToolResolve(ctx, "worker", base))
	got := worker2["record_fact"]
	if got == nil {
		t.Fatal("record_fact missing after edit")
	}
	if got.Description() != "EDITED DESC" {
		t.Errorf("description = %q, want EDITED DESC", got.Description())
	}
	// 기본값 주입: confidence를 빼면 처리기 입력에 기본값이 있어야 합니다.
	// 진짜 처리기는 못 돌립니다(저장소가 nil). InputSchema에 기본값이 보여야 합니다.
	sc := got.InputSchema()
	props := sc["properties"].(map[string]any)
	conf := props["confidence"].(map[string]any)
	if conf["default"] != "inferred" {
		t.Errorf("confidence.default = %v, want inferred", conf["default"])
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
