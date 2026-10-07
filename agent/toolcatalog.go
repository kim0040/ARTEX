package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 이 파일은 내장 도구를 순수 코드에서, 열거할 수 있고 DB 가 덮어쓸 수 있는 목록으로 만듭니다.
//   - BuiltinToolSeeds(): 실행 에이전트 세 곳의 내장 도구 묶음을 seed 기록으로 펼칩니다(key +
//     설명 + 인자 schema + 기본으로 묶인 에이전트). 서버가 켤 때 tools 표에 멱등으로 심습니다.
//   - ToolResolve 훅: 실행 때 DB 의 tools 행으로 이미 조립된 도구를 에이전트별 필터와
//     설명/schema 덮어쓰기, 인자 기본값 주입으로 조정합니다. key/handler 는 코드에 남고, DB 는 문장과 기본값만 바꿉니다.
// handler(Call 동작)는 항상 코드에서 옵니다. DB 는 그것을 못 바꾸고, 모델이 보는 설명과 기본 인자만 바꿉니다.

// ToolSeed 는 내장 도구의 심을 수 있는 스냅샷입니다. key 는 CoreTool.Name() 입니다(handler 와 단단히 묶이고,
// UI 는 읽기 전용). Desc/Schema 는 코드의 도구 정의에서 오고, Agents 는 코드가 기본으로 어느 에이전트에 줬는지입니다.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), 기본 키, 바꿀 수 없음
	Desc   string         // 최상위 설명(UI 에서 덮어쓸 수 있음)
	Schema map[string]any // 인자 JSON-Schema(구조는 읽기 전용, description/default 는 UI 에서 바꿀 수 있음)
	Agents []string       // 기본으로 묶인 에이전트 key(worker/planner/mainagent)
}

// builtinToolsByAgent 는 읽기 전용 빈 껍질 ToolSet(nil stores)으로 각 실행 에이전트의
// 도메인 도구 묶음을 만듭니다. 도구 생성자는 클로저를 Spec 에만 넣고, 만드는 동안 store 를 역참조하지 않아 nil 이 안전합니다.
// 여기서 이 도구들은 Name()/Description()/InputSchema() 만 읽고, 절대 Call 하지 않습니다.
//
// 일부러 SDK 공통 도구 actool.DefaultTools()(Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash)는 넣지 않습니다. 모든 에이전트가 고정으로 가지고, 누구에게 묶을지 선택이 없으며, 설명은 대부분 Prompt()
// 안에 있습니다(이 표는 Description() 만 덮어 반쪽 덮어쓰기가 오해를 만듭니다). seed 하지 않으면 DB 행이 없고 ToolResolve 가
// 그대로 통과시키고 덮어쓰지 않아, 이전과 동작이 같습니다. artex 자신의 도메인 도구만 표에 넣어 관리합니다.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals(목표 분해기)는 기본으로 set_goals + set_constraints 에 묶입니다. 그것으로 쪼갠 목표와
		// 뽑은 조작 제약을 저장소에 씁니다. mainagent 와 같은 관리 도구를 쓰고, 웹에서 설명/schema 를 고치고 에이전트별로 고를 수 있습니다.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto 는 기본으로 발견 보고와 자산 관리 도구에 묶입니다. 다른 도메인 도구는 UI 에서 필요할 때 고릅니다.
		// 새 저장소는 이 seed 로 씁니다. 옛 저장소는 seedAutoDefaultBindings 가 이전합니다.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest(독립 침투 에이전트) 기본 묶음: 자산 조회 / 자산 삽입 / 발견 보고 / 발견 조회 / 기업 조회.
		// 새 저장소는 이 seed 로 씁니다. 옛 저장소는 seedPentestDefaultBindings 가 이전합니다.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: 이 system 도구는 목록에 그대로 들어갑니다(웹에서 보이고, 에이전트별로 수동으로 고를 수 있음). 하지만
// 기본으로는 아무 에이전트에도 묶지 않습니다. ToolResolve 는 빈 묶음의 도구를 모든 에이전트에서 버립니다. 명시적 opt-in 이 필요합니다.
// 그래도 어떤 에이전트의 base 도구 묶음에 남겨 두는 이유(예: goal_met 가 PlannerTools 에 있음)는 둘입니다. seed 가
// 그것을 만들어 desc/schema 를 얻게 하고, 사용자가 다시 묶은 뒤 실행 때 base 에 있어야 ToolResolve 가 남길 수 있습니다.
//
// goal_met 는 prove_goal 을 하나씩 거치지 않고 전역에서 작업 전체 완료를 바로 선언합니다. 무게가 크고 오판 위험이 있으며
// prove_goal 이 마지막 목표를 표시하면 자동 종료되는 경로와 겹칩니다. 그래서 기본으로는 아무 에이전트에도 주지 않고, 필요할 때 수동으로 묶습니다.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds 는 각 에이전트의 내장 도구 묶음을 중복 없이 seed 목록으로 합칩니다. 같은 이름(예: list_assets 를
// 여러 에이전트가 가짐)은 한 줄로 합치고 Agents 는 합집합입니다. defaultUnbound 의 도구는 묶음을 강제로 비웁니다.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 목록에 넣고 수동으로 묶을 수 있지만, 기본으로는 아무 에이전트에도 주지 않습니다([] 로 저장하고 null 이 아님. 다른 도구와 같음)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve 가 설정되면, 이미 조립된 도구 목록을 DB tools 표와 맞춰 후처리합니다.
// 이 에이전트에 묶이지 않았거나 전역으로 꺼진 도구는 빼고, 나머지는 감싸서
// 모델이 DB 에서 덮어쓴 설명/schema 와 기본 인자를 보게 합니다.
// DB 행이 없는 도구(MCP/skill/호스트 도구, 예: 기록 프록시의 traffic)는 그대로 통과합니다.
// nil 이면 도구를 바꾸지 않습니다. server/assembly.go 에서 연결합니다.
// 초보: 웹에서 고친 도구 문구가 플래너, 워커, 메인 에이전트가 실제로 쓰는 목록에 여기서 반영됩니다.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool 은 t 를 감싸 Description()/InputSchema() 가 DB 덮어쓰기를 보고하게 하고,
// 모델이 빠뜨린 스칼라 인자에는 schema 의 "default" 를 Call() 이 넣게 합니다.
// Name/Prompt/권한/스케줄러 플래그는 t 에 맡기므로 도구의 정체와 handler 는 그대로입니다.
// desc 나 schema 가 비면 t 의 값으로 돌아갑니다.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool 은 CoreTool 장식자입니다. 원본을 품어 동작 메서드
// (Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name)는 원본에 맡기고,
// 모델이 보는 설명/schema 와 기본값 주입만 바꿉니다.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults 는 (고쳐졌을 수도 있는) schema 에 적힌 스칼라 인자 기본값을,
// 모델이 그 필드를 빼먹었거나 빈 값/null 로 둔 입력 JSON 에 채웁니다.
// 구조(이름/타입/required)는 그대로 두고 기본값만 합칩니다.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // 객체가 아닌 입력은 건드리지 않습니다
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults 는 스칼라 인자(string/integer/number/boolean)의
// properties[k]["default"] 만 꺼냅니다. 배열이나 객체 기본값은 합치는 기준이
// 애매해서 건너뜁니다.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
