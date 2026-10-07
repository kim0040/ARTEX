package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestPoolProfilesOrder는 장애 조치 사슬 쿼리를 고정한다. 키 없는 프로필은
// 요청을 처리할 수 없고, 제외된 프로필은 대체 대상이 아니다. 둘 다
// 사슬에 없다. 나머지는 우선순위가 높은 것부터 온다.
// is_default는 일부러 건드리지 않는다. 활성 프로필을 바꾸면
// 공유 개발 데이터베이스에 부수 효과가 난다.
func TestPoolProfilesOrder(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	mk := func(name string, priority int, exclude bool, key string) int64 {
		id, err := d.SaveProfile(&LLMProfile{
			Name: name, Format: "openai", Model: "m", APIKey: key,
			Priority: priority, PoolExclude: exclude,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
		return id
	}
	lo := mk("t-pool-lo", 1, false, "k1")
	hi := mk("t-pool-hi", 9, false, "k2")
	mk("t-pool-excluded", 99, true, "k3") // 우선순위가 맨 위여도 제외
	mk("t-pool-nokey", 50, false, "")     // 키 없음 → 아무것도 처리할 수 없다

	chain, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, p := range chain {
		switch p.Name {
		case "t-pool-lo", "t-pool-hi":
			got = append(got, p.ID)
		case "t-pool-excluded":
			t.Fatal("pool_exclude profile entered the failover chain")
		case "t-pool-nokey":
			t.Fatal("keyless profile entered the failover chain")
		}
	}
	if len(got) != 2 || got[0] != hi || got[1] != lo {
		t.Fatalf("chain order = %v, want [hi=%d lo=%d]", got, hi, lo)
	}
}

func TestDeleteProfileContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var d DB
	if err := d.DeleteProfileContext(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteProfileContext error=%v, want context cancellation", err)
	}
}

func TestConfigStores(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	// LLM 프로필: 저장, 활성, 키는 직렬화하지 않음
	pid, err := d.SaveProfile(&LLMProfile{Name: "t-default", Format: "openai", Model: "gpt-x", APIKey: "secret123"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, pid)
	if err := d.SetActiveProfile(pid); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteProfile(pid); !errors.Is(err, ErrActiveLLMProfileDelete) {
		t.Fatalf("deleting active profile error=%v, want %v", err, ErrActiveLLMProfileDelete)
	}
	act, err := d.ActiveProfile()
	if err != nil || act == nil || act.APIKey != "secret123" {
		t.Fatalf("active profile/key: %+v err=%v", act, err)
	}
	// 목록은 키를 숨기고 힌트만 보여야 한다
	list, _ := d.ListProfiles()
	for _, p := range list {
		if p.ID == pid {
			b, _ := json.Marshal(p)
			if string(b) == "" || contains(string(b), "secret123") {
				t.Fatalf("api key leaked in list json: %s", b)
			}
			if p.APIKeyHint != "…t123" {
				t.Fatalf("hint want …t123, got %q", p.APIKeyHint)
			}
		}
	}

	// 에이전트를 심고, 프롬프트 버전을 본다
	ag, err := d.GetAgentByKey("planner")
	if err != nil || ag == nil {
		t.Fatalf("planner agent: %v", err)
	}
	v1, err := d.SavePrompt(ag.ID, "당신은 플래너입니다 {{.Goal}}", "init", "test")
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := d.SavePrompt(ag.ID, "당신은 플래너 v2입니다 {{.Goal}} {{.Scope}}", "edit", "test")
	if v2 != v1+1 {
		t.Fatalf("version should increment: %d -> %d", v1, v2)
	}
	cur, _ := d.CurrentPrompt(ag.ID)
	if cur != "당신은 플래너 v2입니다 {{.Goal}} {{.Scope}}" {
		t.Fatalf("current prompt wrong: %q", cur)
	}
	vers, _ := d.ListPromptVersions(ag.ID)
	if len(vers) < 2 {
		t.Fatalf("want >=2 versions, got %d", len(vers))
	}
	pv, _ := d.PromptVars(ag.ID)
	// 플래너에는 적어도 심은 카탈로그 변수(Goal, AssetSummary)가 있어야 한다
	if len(pv) < 2 {
		t.Fatalf("planner catalog want >=2 vars, got %d", len(pv))
	}
	d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1`, ag.ID)
	d.Exec(`UPDATE agents SET current_prompt_id=NULL WHERE id=$1`, ag.ID)

	// mcp + 스킬 + 가시성(조인 하나로 양방향)
	// 이전 실행에 남은 MCP를 지워 이 테스트를 여러 번 돌려도 같게 한다.
	d.Exec(`DELETE FROM mcp_servers WHERE name = 't-gh'`)
	mid, err := d.SaveMCP(&MCPServer{Name: "t-gh", Transport: "stdio", Command: "npx", Args: json.RawMessage(`["server-github"]`), Env: json.RawMessage(`{"GITHUB_TOKEN":"x"}`), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// 에이전트 쪽 쓰기. MCP는 id 키다(일반 가시성 조인). 스킬은 이제
	// 파일시스템 기반이라 가시성 키는 스킬(디렉터리) 이름이고
	// 전용 표에 있다. 기존 MCP 가시성 행을 비워
	// 아래 단언이 이 테스트가 넣은 것만 보게 한다.
	d.Exec(`DELETE FROM agent_visibility WHERE agent_id = $1 AND resource_kind = 'mcp'`, ag.ID)
	if err := d.ToggleVisibility(ag.ID, "mcp", mid, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAgentSkillVisibility(ag.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.ToggleSkillVisibility(ag.ID, "t-skill", true); err != nil {
		t.Fatal(err)
	}
	// 에이전트 쪽 읽기
	vm, _ := d.AgentVisible(ag.ID, "mcp")
	if len(vm) != 1 || vm[0] != mid {
		t.Fatalf("agent visible mcp: %+v", vm)
	}
	// 리소스 쪽 읽기(같은 조인 행) → 양방향
	ra, _ := d.ResourceAgents("mcp", mid)
	if len(ra) != 1 || ra[0] != ag.ID {
		t.Fatalf("resource agents: %+v", ra)
	}
	// 끈다
	d.ToggleVisibility(ag.ID, "mcp", mid, false)
	vm2, _ := d.AgentVisible(ag.ID, "mcp")
	if len(vm2) != 0 {
		t.Fatalf("after toggle off: %+v", vm2)
	}
	// 스킬 가시성은 이름 키다. 읽기를 확인한 뒤 스킬의
	// 가시성 행을 지우면(스킬을 없앨 때 호출) 그게 사라진다.
	names, _ := d.AgentSkillNames(ag.ID)
	if len(names) != 1 || names[0] != "t-skill" {
		t.Fatalf("agent visible skills: %+v", names)
	}
	if err := d.DeleteSkillVisibility("t-skill"); err != nil {
		t.Fatal(err)
	}
	names2, _ := d.AgentSkillNames(ag.ID)
	if len(names2) != 0 {
		t.Fatalf("skill visibility should be cleared on delete: %+v", names2)
	}
	d.DeleteMCP(mid)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
