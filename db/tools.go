package db

import (
	"database/sql"
	"encoding/json"
)

// Tool은 내장 도구 목록의 한 줄이다. key와 핸들러는 코드에 있고, 이 행은
// 화면에서 고칠 수 있는 면만 담는다. 설명, 매개변수 스키마
// (구조는 읽기 전용, 매개변수별 설명·기본값은 고칠 수 있음), 에이전트 연결,
// 켜기/끄기다. schema.sql §H와 agent/toolcatalog.go를 본다.
type Tool struct {
	Key         string          `json:"key"`
	System      bool            `json:"system"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Agents      []string        `json:"agents"`
	Enabled     bool            `json:"enabled"`
	Kind        string          `json:"kind"`     // builtin(내장) | command(명령) | script(스크립트) | http
	Exec        json.RawMessage `json:"exec"`     // 사용자 정의 도구 실행 명세(kind!=builtin)
	Deferred    bool            `json:"deferred"` // schema 지연(SearchExtraTools/ExecuteExtraTool을 거침)
	Calls       int             `json:"calls"`    // 실행 중 장부 합계. tools 테이블에는 저장하지 않는다
}

const toolCols = `key, system, description, schema, agents, enabled, kind, exec, deferred`

// SeedTool은 내장 도구의 코드 기본값을 한 번만 넣는다. ON CONFLICT DO
// NOTHING이라, 이미 있는 행(화면에서 고쳤을 수 있음)은 시작 때 덮어쓰지 않는다.
// 그래서 재시작할 때마다 화면 수정이 지워지지 않는다.
// 코드 기본값으로 되돌리려면 UpsertToolForce를 쓴다.
func (d *DB) SeedTool(key, desc string, schema, agents json.RawMessage) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
INSERT INTO tools(key, system, description, schema, agents, enabled)
VALUES ($1, true, $2, $3, $4, true)
ON CONFLICT (key) DO NOTHING`, key, desc, schema, agents)
	return err
}

// AddAgentToToolBinding은 지정한 도구의 agents 배열에 에이전트 키를 더한다.
// 이미 있으면 그대로 둔다. 기존 DB에서 내장 Auto 에이전트에 기본 도구 묶음을 줄 때 쓴다.
func (d *DB) AddAgentToToolBinding(agentKey string, keys []string) error {
	for _, k := range keys {
		if _, err := d.Exec(`UPDATE tools SET agents = agents || to_jsonb($1::text) WHERE key=$2 AND NOT (agents ? $1)`, agentKey, k); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAgentFromToolBindings는 모든 도구의 agents JSONB 배열에서 에이전트 키를 뺀다.
// 사용자 에이전트를 지울 때 불러, 도구에 끊긴 연결이 남지 않게 한다.
// jsonb `-`(배열 원소 삭제)를 쓰고, `?`(포함 여부)로 지킨다.
func (d *DB) RemoveAgentFromToolBindings(agentKey string) error {
	_, err := d.Exec(`UPDATE tools SET agents = agents - $1 WHERE agents ? $1`, agentKey)
	return err
}

// RemoveAgentFromTool은 도구 하나의 agents 배열에서 에이전트 키 하나를 뺀다.
// 기존 DB에서 도구의 기본 연결을 바꾸는 일회성 이관이 쓴다.
// SeedTool은 처음 넣을 때만 동작하므로, 바뀐 기본값이 이미 심긴 행에는 닿지 않는다.
func (d *DB) RemoveAgentFromTool(agentKey, toolKey string) error {
	_, err := d.Exec(`UPDATE tools SET agents = agents - $1 WHERE key=$2 AND agents ? $1`, agentKey, toolKey)
	return err
}

// UpsertToolForce는 도구 행을 준 코드 기본값으로 덮어쓴다(도구별 「초기화」 동작).
// 설명·스키마·에이전트를 되돌리고 도구를 다시 켜지만, system=true는 유지한다.
func (d *DB) UpsertToolForce(key, desc string, schema, agents json.RawMessage) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
INSERT INTO tools(key, system, description, schema, agents, enabled)
VALUES ($1, true, $2, $3, $4, true)
ON CONFLICT (key) DO UPDATE
  SET description = EXCLUDED.description,
      schema      = EXCLUDED.schema,
      agents      = EXCLUDED.agents,
      enabled     = true`, key, desc, schema, agents)
	return err
}

// RefreshToolDefaults는 시스템 도구의 모델용 설명과 스키마를 코드 기본값으로 고친다.
// 사용자가 고른 에이전트 연결과 켜짐 표시는 남긴다. 일회성 이관이 코드의 스키마 변경을 퍼뜨릴 때 쓴다.
// SeedTool은 처음 넣을 때만 동작하므로, 코드에 새로 더한 매개변수가 이미 심긴 행에는 닿지 않는다.
// 사용자 도구나 모르는 키에는 아무 일도 하지 않는다.
func (d *DB) RefreshToolDefaults(key, desc string, schema json.RawMessage) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	_, err := d.Exec(`UPDATE tools SET description=$2, schema=$3, updated_at=now() WHERE key=$1 AND system`, key, desc, schema)
	return err
}

func scanTool(rows interface{ Scan(...any) error }) (*Tool, error) {
	var t Tool
	var agents []byte
	if err := rows.Scan(&t.Key, &t.System, &t.Description, &t.Schema, &agents, &t.Enabled, &t.Kind, &t.Exec, &t.Deferred); err != nil {
		return nil, err
	}
	if len(agents) > 0 {
		_ = json.Unmarshal(agents, &t.Agents)
	}
	if t.Agents == nil {
		t.Agents = []string{}
	}
	return &t, nil
}

// ListTools는 도구 목록 전체를 키 순으로 돌려준다.
func (d *DB) ListTools() ([]*Tool, error) {
	rows, err := d.Query(`SELECT ` + toolCols + ` FROM tools ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTool은 도구 한 줄을 가져온다. 없으면 nil이다.
func (d *DB) GetTool(key string) (*Tool, error) {
	row := d.QueryRow(`SELECT `+toolCols+` FROM tools WHERE key=$1`, key)
	t, err := scanTool(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// CreateCustomTool은 실행 명세를 가진 사용자 도구(system=false)를 넣는다.
// 키가 이미 있으면 실패한다.
func (d *DB) CreateCustomTool(t *Tool) error {
	schema := t.Schema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	exec := t.Exec
	if len(exec) == 0 {
		exec = json.RawMessage("{}")
	}
	agents, _ := json.Marshal(t.Agents)
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
INSERT INTO tools(key, system, description, schema, agents, enabled, kind, exec, deferred)
VALUES ($1, false, $2, $3, $4, $5, $6, $7, $8)`,
		t.Key, t.Description, schema, agents, t.Enabled, t.Kind, exec, t.Deferred)
	return err
}

// UpdateCustomTool은 사용자 도구에서 고칠 수 있는 필드를 바꾼다
// (kind/exec/deferred와 설명·스키마·에이전트·켜짐). system=false 행만 건드린다.
func (d *DB) UpdateCustomTool(t *Tool) error {
	schema := t.Schema
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	exec := t.Exec
	if len(exec) == 0 {
		exec = json.RawMessage("{}")
	}
	agents, _ := json.Marshal(t.Agents)
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
UPDATE tools SET description=$2, schema=$3, agents=$4, enabled=$5, kind=$6, exec=$7, deferred=$8
WHERE key=$1 AND system=false`,
		t.Key, t.Description, schema, agents, t.Enabled, t.Kind, exec, t.Deferred)
	return err
}

// DeleteCustomTool은 사용자 도구를 지운다. system=false만 대상이고, 내장 도구는 지킨다.
func (d *DB) DeleteCustomTool(key string) error {
	_, err := d.Exec(`DELETE FROM tools WHERE key=$1 AND system=false`, key)
	return err
}

// ListCustomTools는 사용자가 만든 도구(system=false)만 돌려준다.
func (d *DB) ListCustomTools() ([]*Tool, error) {
	rows, err := d.Query(`SELECT ` + toolCols + ` FROM tools WHERE system=false ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTool은 화면에서 고칠 수 있는 필드를 저장한다. key는 바꾸지 않는다(Go 핸들러에 붙어 있다).
// 시스템 도구는 호출자가 스키마 구조를 유지해야 한다. 매개변수별 설명·기본값과 에이전트 연결만 움직인다.
func (d *DB) UpdateTool(key, desc string, schema, agents json.RawMessage, enabled bool) error {
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	if len(agents) == 0 {
		agents = json.RawMessage("[]")
	}
	_, err := d.Exec(`
UPDATE tools SET description=$2, schema=$3, agents=$4, enabled=$5 WHERE key=$1`,
		key, desc, schema, agents, enabled)
	return err
}
