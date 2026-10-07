package agent

import (
	"context"
	"encoding/json"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestBuiltinToolSeeds 는 저장소가 nil 인 ToolSet 으로도 카탈로그가 패닉 없이
// 만들어지고, 키가 안정적이며, 에이전트 연결을 합치고, 진짜 설명을 담는지 봅니다.
func TestBuiltinToolSeeds(t *testing.T) {
	seeds := BuiltinToolSeeds()
	if len(seeds) == 0 {
		t.Fatal("no seeds")
	}
	byKey := map[string]ToolSeed{}
	for _, s := range seeds {
		if s.Key == "" || s.Desc == "" {
			t.Errorf("seed %q missing key/desc", s.Key)
		}
		byKey[s.Key] = s
	}
	// record_fact 는 워커에 묶입니다(확인된 사실을 남길 수 있는 메인 에이전트에도).
	rf, ok := byKey["record_fact"]
	hasWorker := false
	for _, a := range rf.Agents {
		if a == "worker" {
			hasWorker = true
		}
	}
	if !ok || !hasWorker {
		t.Errorf("record_fact agents = %v, want to include worker", rf.Agents)
	}
	// add_task_scope 는 플래너에 묶입니다(범위를 일부러 넓히는 일).
	if ts, ok := byKey["add_task_scope"]; !ok || len(ts.Agents) == 0 {
		t.Errorf("add_task_scope not seeded / has no agent binding: %v", ts.Agents)
	}
	// SDK 공통 도구(sleep 포함, 이제 DefaultTools)는 일부러 심지 않습니다.
	// 모든 에이전트가 갖고, ToolResolve 를 그대로 탑니다.
	for _, k := range []string{"Bash", "Read", "Write", "Edit", "Grep", "sleep"} {
		if _, ok := byKey[k]; ok {
			t.Errorf("SDK tool %q should not be seeded", k)
		}
	}
}

// TestDecorateToolInjectsDefaults 는 schema 의 "default" 가 빠진 파라미터를
// 핸들러 전에 채우고, 명시적으로 준 값은 유지하는지 봅니다.
func TestDecorateToolInjectsDefaults(t *testing.T) {
	var seen map[string]any
	base := actool.Build(actool.Spec{
		Name: "probe", Description: "orig",
		Run: func(_ context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			_ = json.Unmarshal(in, &seen)
			return actool.Text("ok"), nil
		},
	})
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"limit": map[string]any{"type": "integer", "description": "n", "default": float64(3)},
		"q":     map[string]any{"type": "string", "description": "query"},
	}}
	dec := DecorateTool(base, "new desc", schema)
	if dec.Description() != "new desc" {
		t.Errorf("description = %q", dec.Description())
	}

	// limit 을 빼면 기본 3 이 들어가고, q 는 유지됩니다.
	if _, err := dec.Call(context.Background(), json.RawMessage(`{"q":"x"}`), nil); err != nil {
		t.Fatal(err)
	}
	if seen["limit"] != float64(3) || seen["q"] != "x" {
		t.Errorf("injected = %v, want limit=3 q=x", seen)
	}

	// limit 을 주면 기본값이 덮어쓰지 않습니다.
	if _, err := dec.Call(context.Background(), json.RawMessage(`{"limit":9}`), nil); err != nil {
		t.Fatal(err)
	}
	if seen["limit"] != float64(9) {
		t.Errorf("limit = %v, want 9 (no override)", seen["limit"])
	}
}
