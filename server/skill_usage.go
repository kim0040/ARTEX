package server

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// maxLedgerSkillName은 MISS로 저장하는 스킬 이름 길이의 상한입니다. 그 문자열은
// 모델이 그대로 보낸 것이라, 상한이 없으면 길이가 한없이 커질 수 있습니다.
const maxLedgerSkillName = 128

// meteredSkill은 Skill 메타 도구를 감싸, 호출마다 skill_usage 행을 하나 붙입니다.
// 계량을 Registry.OnInvoke에 두지 않습니다. OnInvoke는 해석이 끝난 Skill만
// 받기 때문입니다. 모르는 스킬 이름에는 불이 안 붙고(norma가 훅보다 먼저
// 도구 오류를 돌려줌), 호출자의 인자도 못 봅니다. 둘 다 중요합니다.
// 실패(miss)를 보면, 에이전트가 어떤 절차를 바랐는지 알 수 있습니다.
type meteredSkill struct {
	actool.CoreTool
	pg       *db.DB
	reg      *skill.Registry
	agentKey string
	ri       agent.RunInfo
}

// meterSkillTool은 기록할 DB가 없으면 t를 그대로 돌려줍니다.
func meterSkillTool(t actool.CoreTool, pg *db.DB, reg *skill.Registry, agentKey string, ri agent.RunInfo) actool.CoreTool {
	if pg == nil {
		return t
	}
	return &meteredSkill{CoreTool: t, pg: pg, reg: reg, agentKey: agentKey, ri: ri}
}

func (m *meteredSkill) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	m.record(input)
	return m.CoreTool.Call(ctx, input, tc)
}

// record는 장부 행을 붙입니다. 일부러 실패해도 흐름은 계속합니다. 계량 실패가
// 스킬 호출 실패가 되면 안 되므로, 오류는 모두 로그만 남깁니다.
func (m *meteredSkill) record(input json.RawMessage) {
	var in struct {
		Name string `json:"name"`
		Args string `json:"args"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return // 형식이 잘못된 호출입니다. norma도 거절합니다.
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return
	}
	// 디렉터리 이름으로 맞춥니다. 그래야 장부 키가 agent_skill_visibility와
	// 스킬 페이지와 같습니다(표시용 Name은 디렉터리 이름과 다를 수 있습니다).
	found := false
	if m.reg != nil {
		if s, ok := m.reg.Get(name); ok {
			found = true
			if s.Dir != "" {
				name = filepath.Base(s.Dir)
			}
		}
	}
	if len(name) > maxLedgerSkillName {
		name = name[:maxLedgerSkillName]
	}
	err := m.pg.InsertSkillUsage(&db.SkillUsage{
		Skill:         name,
		AgentKey:      m.agentKey,
		TaskID:        m.ri.TaskID,
		ExplorationID: m.ri.ExplorationID,
		IntentID:      m.ri.IntentID,
		SessionID:     m.ri.SessionID,
		ArgsLen:       len(in.Args),
		Found:         found,
	})
	if err != nil {
		log.Printf("[skillusage] insert: %v", err)
	}
}
