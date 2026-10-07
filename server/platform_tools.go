package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 플랫폼 조작 도구(내장 Auto agent용)다. 메인 에이전트가 엔진 안에서 작업을 돌릴 때 불러 쓴다: skill, 사용자 정의 도구, MCP를 만들거나 고친다. 모두 host 도구이고,
// tools 테이블에 seed하고 기본으로 auto에 묶으며, hostTools로 주입한다. 기존 db/파일 시스템 로직을 재사용한다.

func (s *Server) platformTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolCreateSkill(),
		s.toolUpdateSkillFile(),
		s.toolCreateCustomTool(),
		s.toolUpdateCustomTool(),
		s.toolCreateMCP(),
		s.toolUpdateMCP(),
		s.toolDeleteAssetsByHost(),
	}
}

// platformToolKeys는 Auto 에이전트에 기본으로 묶는 도구 키입니다.
var platformToolKeys = []string{
	"create_skill", "update_skill_file",
	"create_custom_tool", "update_custom_tool",
	"create_mcp", "update_mcp",
	"delete_assets_by_host",
}

// ---- 자산 ----

// toolDeleteAssetsByHost는 호스트 하나(정확히 일치)에 묶인 자산을 모두 하드 삭제합니다.
// 플랫폼 수준이다(작업별 도구가 아니다). 작업을 가로지르는 전역 자산 그래프를 다루며, 이 그래프는 탐색 그래프와 다른 PostgreSQL 그래프이고 UI가 이를 읽는다.
func (s *Server) toolDeleteAssetsByHost() actool.CoreTool {
	return wrTool("delete_assets_by_host",
		"host 기준으로 자산을 정확히 삭제합니다. 해당 host의 도메인/서브도메인과 그 아래 서비스(service), 인터페이스(endpoint)를 삭제합니다.\n"+
			"host는 완전 일치(소문자, 공백 제거)이며, 퍼지/와일드카드가 아닙니다.\n"+
			"루트 도메인(예: example.com)을 넘기면 그 서브도메인과 서비스/인터페이스까지 함께 삭제합니다. 서브도메인(예: a.example.com)이나 IP를 넘기면 해당 host 자신과 그 서비스/인터페이스만 삭제합니다.\n"+
			"⚠️ 하드 삭제이며, 전역 자산 라이브러리(작업 간 공유)에 작용하고, 되돌릴 수 없습니다.",
		objSchema(map[string]any{
			"host": strParam("삭제할 host: 도메인/서브도메인/IP. 완전 일치, 예: example.com 또는 a.example.com 또는 1.2.3.4"),
		}, "host"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			as := s.assetStore()
			if as == nil {
				return actool.Errorf("자산 라이브러리가 초기화되지 않았습니다"), nil
			}
			var a struct {
				Host string `json:"host"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host는 비워 둘 수 없습니다"), nil
			}
			counts, err := as.DeleteByHost(a.Host)
			if err != nil {
				return actool.Errorf("삭제 실패: " + err.Error()), nil
			}
			var total int64
			for _, n := range counts {
				total += n
			}
			return jsonResult(map[string]any{
				"host":            a.Host,
				"deleted":         total,
				"deleted_by_type": counts,
			})
		})
}

// ---- 스킬 ----

func (s *Server) toolCreateSkill() actool.CoreTool {
	return wrTool("create_skill",
		"새 skill을 만듭니다(SKILL.md를 작성하며, agentskills.io 규격). name은 소문자/숫자/하이픈입니다.",
		objSchema(map[string]any{
			"name":         strParam("skill 이름(소문자로 시작, 문자/숫자/하이픈)"),
			"description":  strParam("skill 설명(필수, 무엇을 하는지/언제 쓰는지)"),
			"instructions": strParam("Markdown 본문 설명(선택)"),
		}, "name", "description"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, Description, Instructions string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill 이름이 올바르지 않습니다(소문자로 시작, 문자/숫자/하이픈만, ≤64)"), nil
			}
			if strings.TrimSpace(a.Description) == "" {
				return actool.Errorf("description은 필수입니다"), nil
			}
			path := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(path); err == nil {
				return actool.Errorf("skill이 이미 있습니다: " + a.Name), nil
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var b strings.Builder
			b.WriteString("---\n")
			fmt.Fprintf(&b, "name: %s\n", a.Name)
			fmt.Fprintf(&b, "description: %s\n", a.Description)
			b.WriteString("---\n")
			if strings.TrimSpace(a.Instructions) != "" {
				b.WriteString(a.Instructions)
			} else {
				fmt.Fprintf(&b, "## %s\n\n1. \n2. \n3. \n", a.Name)
			}
			if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
				_ = os.RemoveAll(path)
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill created: " + a.Name), nil
		})
}

func (s *Server) toolUpdateSkillFile() actool.CoreTool {
	return wrTool("update_skill_file",
		"어떤 skill 안의 파일 하나를 쓰거나 덮어씁니다(기본 SKILL.md). 스킬 내용을 고치거나 스크립트/참조를 추가할 때 씁니다.",
		objSchema(map[string]any{
			"name":    strParam("skill 이름"),
			"file":    strParam("상대 경로(선택, 기본 SKILL.md, 예: scripts/run.py)"),
			"content": strParam("파일 전체 내용"),
		}, "name", "content"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Name, File, Content string }
			_ = json.Unmarshal(in, &a)
			if !validSkillName(a.Name) {
				return actool.Errorf("skill 이름이 올바르지 않습니다"), nil
			}
			skillPath := filepath.Join(s.skillDir, a.Name)
			if _, err := os.Stat(skillPath); os.IsNotExist(err) {
				return actool.Errorf("skill이 없습니다: " + a.Name), nil
			}
			rel := strings.TrimSpace(a.File)
			if rel == "" {
				rel = "SKILL.md"
			}
			clean, msg := skillRelPath(rel)
			if msg != "" {
				return actool.Errorf("잘못된 경로: " + msg), nil
			}
			full := filepath.Join(skillPath, clean)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("skill file written: " + a.Name + "/" + clean), nil
		})
}

// ---- 사용자 정의 도구 ----

type customToolToolInput struct {
	Key         string          `json:"key"`
	Description string          `json:"description"`
	Kind        string          `json:"kind"`
	Exec        json.RawMessage `json:"exec"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Deferred    bool            `json:"deferred"`
	Enabled     *bool           `json:"enabled"`
}

func customToolSchema(keyDesc string) map[string]any {
	return objSchema(map[string]any{
		"key":         strParam(keyDesc),
		"description": strParam("모델에 보내는 설명"),
		"kind":        strParam("shell | command | script(Python만) | http. shell=bash 환경 선언(모델에게 이 도구를 bash에서 직접 호출할 수 있다고만 알리며, exec/schema는 필요 없음). 나머지 세 가지는 exec를 제공해야 합니다"),
		"exec":        map[string]any{"type": "object", "description": "실행 명세(shell 유형은 필요 없음): command→{command}; script→{code}; http→{method,url,headers,body,proxy,use_recording_proxy}"},
		"schema":      map[string]any{"type": "object", "description": "매개변수 JSON-Schema(shell/command/script는 비워 둘 수 있음; http는 필수이며 properties를 포함해야 함)"},
		"agents":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "바인딩된 agent key(선택)"},
		"deferred":    map[string]any{"type": "boolean", "description": "지연 여부(shell 유형에는 무효; command/script/http 중 자주 쓰지 않는 도구에만 켭니다)"},
		"enabled":     map[string]any{"type": "boolean", "description": "사용 여부(기본 true)"},
	}, "key", "kind")
}

func toDBTool(a customToolToolInput) *db.Tool {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	return &db.Tool{
		Key: a.Key, Description: a.Description, Schema: a.Schema, Agents: a.Agents,
		Enabled: enabled, Kind: a.Kind, Exec: a.Exec, Deferred: a.Deferred,
	}
}

func (s *Server) toolCreateCustomTool() actool.CoreTool {
	return wrTool("create_custom_tool", "【중요】플랫폼에 없는 도구를 설치할 때, 이 도구를 호출해 설치한 도구를 플랫폼에 넣어 플랫폼이 호출할 수 있게 합니다. 사용자 정의 도구(shell/command/script/http)를 만듭니다. shell=bash 환경 선언이며, key+description+agents만 필요하고 exec/schema는 필요 없습니다.",
		customToolSchema("도구 key(소문자로 시작, 문자/숫자/밑줄)"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			a.Key = strings.TrimSpace(a.Key)
			if !reToolKey.MatchString(a.Key) {
				return actool.Errorf("key는 소문자로 시작해야 하며, 소문자/숫자/밑줄만 포함합니다"), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind는 shell / command / script / http여야 합니다"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http 도구는 매개변수 JSON Schema를 제공해야 합니다(비워 둘 수 없음)"), nil
			}
			if exist, _ := s.m.pg.GetTool(a.Key); exist != nil {
				return actool.Errorf("해당 key가 이미 있습니다: " + a.Key), nil
			}
			if err := s.m.pg.CreateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool created: " + a.Key), nil
		})
}

func (s *Server) toolUpdateCustomTool() actool.CoreTool {
	return wrTool("update_custom_tool", "이미 있는 사용자 정의 도구를 수정합니다(key 기준).",
		customToolSchema("수정할 사용자 정의 도구 key"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a customToolToolInput
			_ = json.Unmarshal(in, &a)
			existing, _ := s.m.pg.GetTool(a.Key)
			if existing == nil || existing.System {
				return actool.Errorf("사용자 정의 도구만 수정할 수 있습니다: " + a.Key), nil
			}
			if a.Kind != "shell" && a.Kind != "command" && a.Kind != "script" && a.Kind != "http" {
				return actool.Errorf("kind는 shell / command / script / http여야 합니다"), nil
			}
			if a.Kind == "http" && !hasSchemaProps(a.Schema) {
				return actool.Errorf("http 도구는 매개변수 JSON Schema를 제공해야 합니다(비워 둘 수 없음)"), nil
			}
			if err := s.m.pg.UpdateCustomTool(toDBTool(a)); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("custom tool updated: " + a.Key), nil
		})
}

// ---- MCP ----

type mcpToolInput struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url"`
	Enabled   *bool           `json:"enabled"`
	Insecure  *bool           `json:"insecure"`
}

func mcpSchema(withID bool) map[string]any {
	props := map[string]any{
		"name":      strParam("MCP 서버 이름"),
		"transport": strParam("stdio | http / sse"),
		"command":   strParam("stdio 시작 명령(예: npx)"),
		"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "명령 인자 배열"},
		"env":       map[string]any{"type": "object", "description": "환경 변수 {KEY:VALUE}"},
		"url":       strParam("http/sse의 URL"),
		"enabled":   map[string]any{"type": "boolean", "description": "사용 여부(기본 true)"},
		"insecure":  map[string]any{"type": "boolean", "description": "http: TLS 인증서 검증 건너뛰기(자체 서명 인증서일 때 true, 기본 false)"},
	}
	required := []string{"name", "transport"}
	if withID {
		props["id"] = map[string]any{"type": "integer", "description": "수정할 MCP 서버 id"}
		required = []string{"id", "name", "transport"}
	}
	return objSchema(props, required...)
}

func (a mcpToolInput) toDB() *db.MCPServer {
	enabled := true
	if a.Enabled != nil {
		enabled = *a.Enabled
	}
	insecure := false
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	return &db.MCPServer{
		ID: a.ID, Name: a.Name, Transport: a.Transport, Command: a.Command,
		Args: a.Args, Env: a.Env, URL: a.URL, Enabled: enabled, Insecure: insecure,
	}
}

func (s *Server) toolCreateMCP() actool.CoreTool {
	return wrTool("create_mcp", "MCP 서버를 만듭니다(stdio/http/sse). 만든 뒤 그 도구는 agent 가시성에 따라 권한을 부여해야 합니다.",
		mcpSchema(false),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			a.ID = 0
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Transport) == "" {
				return actool.Errorf("name / transport는 필수입니다"), nil
			}
			id, err := s.m.pg.SaveMCP(a.toDB())
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp created: id=%d name=%s", id, a.Name)), nil
		})
}

func (s *Server) toolUpdateMCP() actool.CoreTool {
	return wrTool("update_mcp", "이미 있는 MCP 서버를 수정합니다(id 기준).",
		mcpSchema(true),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a mcpToolInput
			_ = json.Unmarshal(in, &a)
			if a.ID == 0 {
				return actool.Errorf("id는 필수입니다"), nil
			}
			if _, err := s.m.pg.SaveMCP(a.toDB()); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("mcp updated: id=%d", a.ID)), nil
		})
}
