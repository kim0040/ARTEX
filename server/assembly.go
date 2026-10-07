package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

// wireAgentAugment는 PG의 agent_visibility 표를 에이전트 실행에 연결합니다.
// 에이전트가 볼 수 있는 스킬은 파일에서 읽어 Skill 메타 도구 하나로 묶고,
// 볼 수 있는 stdio MCP 서버는 띄워 mcp__server__tool 로 펼칩니다.
// skillDir는 스킬 하위 디렉터리를 모두 담는 루트입니다.
// hostTools를 주면, 실행 중 host 도구를 돌려줍니다(지금은 캡처가 켜졌을 때의 트래픽 도구).
// 그 도구를 모든 에이전트의 기본 목록에 넣고, DB tools 표가 에이전트별 바인딩으로 거릅니다.
// 비었거나 nil이면 이번 실행에는 host 도구가 없습니다(캡처 꺼짐).
// 초보용: 엔진이 에이전트를 돌릴 때, 화면에서 켠 스킬·MCP·host 도구를 여기서 붙입니다.
func wireAgentAugment(pg *db.DB, skillDir string, hostTools func() ([]actool.CoreTool, map[string][]string)) {
	agent.ToolAugment = func(ctx context.Context, agentKey string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
		a, err := pg.GetAgentByKey(agentKey)
		if err != nil || a == nil {
			return nil, agent.DeferredInfo{}, nil
		}
		var extra []actool.CoreTool

		// --- 스킬: 보이는 스킬을 reg에 넣습니다(Skill 메타 도구에 쓰고,
		// 어느 MCP 서버가 스킬에 잠기는지 알 때도 씁니다). ---
		var reg *skill.Registry
		if names, _ := pg.AgentSkillNames(a.ID); len(names) > 0 {
			nameSet := make(map[string]bool, len(names))
			for _, n := range names {
				nameSet[n] = true
			}
			if allReg, err := skill.LoadDir(skillDir); err == nil && allReg != nil {
				reg = skill.NewRegistry()
				for _, s := range allReg.List() {
					// 표시 이름 Name이 아니라 디렉터리 이름(Dir의 Base)으로 맞춥니다.
					if s.Dir != "" && nameSet[filepath.Base(s.Dir)] {
						reg.Add(s)
					}
				}
				if len(reg.List()) == 0 {
					reg = nil
				}
			}
		}
		// 보이는 스킬의 `mcps:`에 적힌 서버는 스킬에 잠깁니다. 그 도구는
		// 그 스킬이 로드될 때까지 미루고 잠급니다(전역 블록에는 넣지 않음).
		gated := map[string]bool{}
		if reg != nil {
			for _, s := range reg.List() {
				for _, srv := range s.MCPs {
					gated[srv] = true
				}
			}
		}

		// --- mcp: 이 에이전트에 직접 보이거나
		// 스킬에 잠긴(보이는 스킬의 mcps에 적힌) 켜진 서버에 접속합니다. 직접 보이고
		// 잠기지 않으면 전역입니다(세션 시작부터 열림). 스킬에 잠기면
		// 직접 보이는지와 상관없이, 그 스킬이 불릴 때까지 미룹니다.
		var closers []io.Closer
		serverTools := map[string][]string{} // 서버 이름 → 그 도구 이름들
		var allNames, globalNames []string
		globalSet := map[string]bool{}
		{
			mcpIDs, _ := pg.AgentVisible(a.ID, "mcp")
			want := idSet(mcpIDs)
			all, _ := pg.ListMCP()
			for _, m := range all {
				if !m.Enabled {
					continue
				}
				directVisible := want[m.ID]
				skillGated := gated[m.Name]
				if !directVisible && !skillGated {
					continue // 직접 보이지도 않고, 보이는 스킬이 가리키지도 않습니다.
				}
				cl, err := connectMCP(ctx, m)
				if err != nil {
					log.Printf("[mcp] %s 연결 실패: %v", m.Name, err)
					continue
				}
				closers = append(closers, cl)
				ts, err := cl.Tools(ctx)
				if err != nil {
					log.Printf("[mcp] %s tools/list 실패: %v", m.Name, err)
					continue
				}
				for _, t := range ts {
					extra = append(extra, t)
					allNames = append(allNames, t.Name())
					serverTools[m.Name] = append(serverTools[m.Name], t.Name())
					if directVisible && !skillGated {
						// 직접 보이고 잠기지 않음 → 세션 시작부터 쓸 수 있습니다.
						globalNames = append(globalNames, t.Name())
						globalSet[t.Name()] = true
					}
					// 스킬에 잠긴 도구는 globalNames 밖에 둡니다. unlockSkill()로 엽니다.
				}
			}
		}

		// 공통 호출 문입니다. 전역 MCP 도구는 처음부터 열려 있고, 스킬에 잠긴
		// 도구는 그 스킬이 로드될 때, 또는 기록을 재생할 때(C2) 열립니다.
		unlock := actool.NewUnlockSet(globalNames...)
		unlockSkill := func(skillName string) {
			if reg == nil {
				return
			}
			if s, ok := reg.Get(skillName); ok {
				for _, srv := range s.MCPs {
					unlock.Add(serverTools[srv]...)
				}
			}
		}

		// Skill 메타 도구: 로드되면 그 스킬의 MCP를 열고 이름을 보여 줍니다
		// (이미 전역 블록에 있는 이름은 빼고요).
		if reg != nil {
			reg.OnInvoke = func(s skill.Skill) string {
				unlockSkill(s.Name)
				var reveal []string
				for _, srv := range s.MCPs {
					for _, n := range serverTools[srv] {
						if !globalSet[n] {
							reveal = append(reveal, n)
						}
					}
				}
				return actool.RenderDeferredToolsBlock(reveal)
			}
			// 사용 장부에 누구 것인지 적습니다. ToolAugment는 (ctx, agentKey)만 받으므로
			// 이번 실행의 작업/세션 id는 ctx의 agent.RunInfo로 들어옵니다. 여기서
			// 한 번만 읽습니다. 이 클로저는 실행마다 다시 만들므로, 잡은 값은 항상
			// 이번 실행의 것입니다.
			extra = append(extra, meterSkillTool(reg.Tool(), pg, reg, a.Key, agent.RunInfoFrom(ctx)))
		}

		// host 도구(트래픽 / 오케스트레이션 / 사용자 정의)를 모든 에이전트의 기본 목록에 넣습니다.
		// 그다음 ToolResolve가, 그 도구가 묶인 에이전트만 남깁니다. deferred로 표시된 사용자 정의
		// 도구는 이름을 deferred 배선에 넣어 스키마를 감춥니다(SearchExtraTools/ExecuteExtraTool).
		// MCP와 같습니다.
		if hostTools != nil {
			ht, deferredBinds := hostTools()
			extra = append(extra, ht...)
			for name, boundAgents := range deferredBinds {
				if !contains(boundAgents, a.Key) {
					continue // 이 에이전트에 실제로 묶인 이름만 미룹니다.
				}
				allNames = append(allNames, name)       // 프롬프트에는 스키마를 넣지 않습니다.
				globalNames = append(globalNames, name) // deferred 블록에는 이름을 알립니다.
				globalSet[name] = true
				if unlock != nil {
					unlock.Add(name) // 전역 deferred라서 처음부터 호출할 수 있습니다.
				}
			}
		}

		cleanup := func() {
			for _, c := range closers {
				_ = c.Close()
			}
		}
		def := agent.DeferredInfo{
			Deferred:    allNames,
			GlobalNames: globalNames,
			Unlock:      unlock,
			UnlockSkill: unlockSkill,
		}
		return extra, def, cleanup
	}
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// seedPrompts는 시작 때 내장 에이전트의 코드 기본 프롬프트 본문을
// agent_prompts에 씁니다. 처음 한 번만 넣습니다(버전이 있으면 SeedPromptIfEmpty는 아무 일도 안 함).
// 그래서 DB가 고칠 수 있는 기준이 되고,
// 사용자가 고친 내용은 재시작해도 남습니다. seedBuiltins가 에이전트 행을 만든 뒤에 돕니다.
func seedPrompts(pg *db.DB) {
	for key, tmpl := range agent.BuiltinPromptSeeds() {
		a, err := pg.GetAgentByKey(key)
		if err != nil || a == nil {
			log.Printf("[prompts] seed %s 건너뜀: agent가 존재하지 않음 (%v)", key, err)
			continue
		}
		if err := pg.SeedPromptIfEmpty(a.ID, tmpl); err != nil {
			log.Printf("[prompts] seed %s 실패: %v", key, err)
		}
	}
}

// wireTools는 내장 도구 목록을 심습니다. 여러 번 해도 되고, 처음만 넣어 화면에서
// 고친 내용이 재시작 뒤에 남게 합니다. DB tools 표를 에이전트 실행에 연결합니다.
// 도구를 조립할 때 내장 도구마다 에이전트 바인딩과 enabled
// 플래그로 거르고, 남기면 감싸서 모델이 DB에서 덮어쓴 설명/스키마를 보게 합니다.
// and 빠진 인자는 주입된다. MCP/skill/host 도구는 행이 없어 그대로 통과한다.
func wireTools(pg *db.DB, domainReg map[string]actool.CoreTool) {
	agent.FindingTrafficBindingEnabled = func() bool { return pg.GetBool(settingAgentTrafficBinding, false) }
	// 내장 도메인 도구를 심습니다(처음만 넣음. DO NOTHING이라 고친 내용은 남습니다).
	// 시작 때 행을 지우지 않습니다. 우리가 심지 않은 행은 그대로 두어, 나중에 사용자가
	// UI로 넣은 사용자 정의 도구(system=false)가 재시작 뒤에도 남게 합니다.
	for _, s := range agent.BuiltinToolSeeds() {
		schema, _ := json.Marshal(s.Schema)
		agents, _ := json.Marshal(s.Agents)
		if err := pg.SeedTool(s.Key, s.Desc, schema, agents); err != nil {
			log.Printf("[tools] seed %s 실패: %v", s.Key, err)
		}
	}
	// 트래픽 host 도구를 심어, 내장 도구처럼 에이전트마다 묶을 수 있게 합니다.
	// 기본 바인딩은 worker입니다(예전 동작을 유지). 실행 중에 실제로 나오는지는
	// 여전히 전역 캡처 스위치가 정합니다. hostTools()는 캡처가 켜져 있을 때만 돌려줍니다.
	// 그래서 캡처가 꺼진 채 묶기만 하면 그 도구는 나타나지 않습니다.
	trafficAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range traffic.SeedToolMetas() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := pg.SeedTool(t.Name(), t.Description(), schema, trafficAgents); err != nil {
			log.Printf("[tools] seed %s 실패: %v", t.Name(), err)
		}
	}
	// bashInteractiveShellNote는 interactive_shell이 켜진 에이전트의 Bash 설명에만 붙습니다.
	// 그래서 Bash가 대화형 프로그램은 shell_open을 가리키게 하고,
	// 주입되지 않은 도구는 언급하지 않습니다(§14.1/§14.2).
	const bashInteractiveShellNote = "\n\n【대화형 입력】이 필요한 프로그램(msfconsole / ssh 대화형 로그인 / mysql, psql, python 등의 REPL / 비밀번호 또는 yes/no 프롬프트 / nc 리버스 셸)은 Bash를 쓰지 말 것(stdin이 없어 멈춘다). shell_open으로 대화형 세션을 연다(끝나면 shell_close). 일회성이고 비대화형인 명령은 계속 Bash를 사용한다."
	agent.ToolResolve = func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool {
		rows, err := pg.ListTools()
		if err != nil {
			log.Printf("[tools] 도구 표를 읽지 못해, 코드 기본값으로 allow: %v", err)
			return tools
		}
		byKey := make(map[string]*db.Tool, len(rows))
		for _, t := range rows {
			byKey[t.Key] = t
		}
		runInfo := agent.RunInfoFrom(ctx)
		resolve := func(t actool.CoreTool, row *db.Tool) actool.CoreTool {
			var schema map[string]any
			if len(row.Schema) > 0 {
				_ = json.Unmarshal(row.Schema, &schema)
			}
			return meterTool(agent.DecorateTool(t, row.Description, schema), pg, row.Key, agentKey, runInfo)
		}
		out := tools[:0:0]
		for _, t := range tools {
			row, known := byKey[t.Name()]
			if !known { // MCP/스킬/host 도구는 행이 없으면 그대로 둡니다.
				out = append(out, t)
				continue
			}
			if !row.Enabled || !contains(row.Agents, agentKey) {
				continue // 전역으로 꺼졌거나 이 에이전트에 안 묶이면 뺍니다.
			}
			out = append(out, resolve(t, row))
		}
		// 주입: DB에서는 이 에이전트에 묶여 있는데
		// 들어온 목록에는 없는 도메인 도구입니다. 기본이 DefaultTools뿐인 에이전트
		// (Auto, 사용자 정의)를 위한 것입니다. 그 기본에는 ToolSet 도메인 도구가 없습니다. 작업별 인스턴스가
		// 기본 목록에 있으면 항상 그쪽이 이깁니다. inList는 원래 들어온 목록으로 만들어서,
		// 워커 자신의 upsert_asset이 서버 레지스트리 사본에 가리지 않습니다.
		if len(domainReg) > 0 {
			inList := make(map[string]bool, len(tools))
			for _, t := range tools {
				inList[t.Name()] = true
			}
			for _, row := range rows {
				if row.Kind == "shell" || !row.Enabled || !contains(row.Agents, agentKey) || inList[row.Key] {
					continue
				}
				inst, ok := domainReg[row.Key]
				if !ok {
					continue // 도메인 도구가 아닙니다. 사용자 정의/host 도구는 hostTools()로 넣습니다.
				}
				out = append(out, resolve(inst, row))
			}
		}
		// shell 힌트: 사용자가 만든 kind="shell" 도구는 호출할 수 없습니다. 이것은
		// 어떤 명령줄 도구가 설치돼 있는지 모델에게 알려 주는 환경 선언입니다.
		// 이 에이전트에 묶인 것을 모아 Bash 설명 뒤에 붙입니다.
		var shellHints []string
		for _, row := range rows {
			if row.Kind == "shell" && row.Enabled && contains(row.Agents, agentKey) {
				shellHints = append(shellHints, "- "+row.Key+": "+row.Description)
			}
		}
		if len(shellHints) > 0 {
			note := "\n\n다음 도구는 이 bash 환경에 설치되어 있어 Bash로 바로 호출할 수 있다:\n" + strings.Join(shellHints, "\n")
			for i, t := range out {
				if t.Name() == "Bash" {
					out[i] = agent.DecorateTool(t, t.Description()+note, t.InputSchema())
					break
				}
			}
		}
		// 대화형 셸은 tools 표 바인딩이 아니라, 에이전트의 interactive_shell 플래그만으로 정합니다(web_search와 같음).
		// 켜지면 shell_* 도구 5개를 넣고,
		// Bash 설명 추가문도 같이 붙여 shell_open을 가리키게 합니다. 없는 도구를 가리킨 채 두지 않습니다.
		// 꺼져 있으면 매달려 남는다. docs/대화형shell설계.md §14.2 참고.
		if !actool.InteractiveShellDisabled() {
			if a, err := pg.GetAgentByKey(agentKey); err == nil && a != nil && a.InteractiveShell {
				out = append(out, actool.ShellSessionTools()...)
				for i, t := range out {
					if t.Name() == "Bash" {
						out[i] = agent.DecorateTool(t, t.Description()+bashInteractiveShellNote, t.InputSchema())
						break
					}
				}
			}
		}
		return out
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// buildDomainReg는 서버 수준 ToolSet으로 이름에서 CoreTool로 가는 레지스트리를 만듭니다
// (진짜 AssetStore, ExplorationStore는 nil, taskID=0). ToolResolve가 이것으로
// 작업별 ToolSet이 없는 에이전트(Auto, 사용자 정의)에 도메인 도구를 넣습니다.
// as가 nil이면 nil을 돌려줍니다(넣지 않고, 기능을 조용히 줄입니다).
//
// ExplorationStore를 nil로 둔 것은 일부러입니다. 이 인스턴스는 처음부터 작업이 없어서,
// 여기 있는 도구는 모두 그걸 견뎌야 합니다. 자산/기업 도구는 견딥니다
// (AssetStore만 있으면 됩니다). 탐색 그래프 도구는 ToolSet.needExploration으로
// 분명한 말을 하고 거절합니다. 그런 도구를 작업 없는 에이전트의 tools 표에 묶으면
// 쓸 수 없는 도구일 뿐, 프로그램이 죽지는 않습니다.
// 초보용: 작업이 없는 에이전트에는 자산 그래프 도구만 넣고, 탐색 그래프 도구는 거절합니다.
func buildDomainReg(as *db.AssetStore) map[string]actool.CoreTool {
	if as == nil {
		return nil
	}
	serverTS := agent.NewToolSet(nil, "")
	serverTS.SetAssetStore(as, as.Companies())
	reg := make(map[string]actool.CoreTool)
	for _, t := range serverTS.AllDomainTools() {
		reg[t.Name()] = t
	}
	return reg
}

func jsonStrSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func jsonStrMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}
