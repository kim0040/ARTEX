package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"

	actool "github.com/Autumn-27/norma/tool"
)

// DeferredInfo 는 에이전트가 Options 를 만들 때 필요한, 나중에 여는 도구의 연결 정보입니다.
// schema 를 아직 안 보여주는 MCP 도구 이름, 전역 시스템 프롬프트 블록에 적는 이름
// (스킬을 열어야만 보이는 것은 제외), 그리고 세션이 공유하는 잠금 해제 집합입니다.
// UnlockSkill 은 이름 있는 스킬의 MCP 를 엽니다. 호스트는 재개된 세션에서
// 기록으로 잠금 해제 집합을 다시 만들 때 이것을 부릅니다(설계 문서 C2).
type DeferredInfo struct {
	FindingGuidance string            // 최종으로 허용된 도구(DB 덮어쓰기 포함)에서 만든 안내
	Deferred        []string          // MCP 도구 이름 전체(schema 는 숨김)
	GlobalNames     []string          // 시스템 프롬프트 블록에 적을 MCP 이름
	Unlock          *actool.UnlockSet // 공유 호출 문. MCP 도구가 없으면 nil
	UnlockSkill     func(skillName string)
}

// ToolAugment 가 설정되면, 내장 기본 집합 밖에 에이전트가 볼 추가 도구를 돌려줍니다.
// 보이는 스킬(Skill 메타 도구 하나로 묶음)과 보이는 MCP 서버(mcp__server__tool 로 펼침)입니다.
// 그 MCP 도구를 어떻게 미루고 여닫는지도 DeferredInfo 로 돌려줍니다. 서버는 이것을
// PG agent_visibility 표에 연결합니다. cleanup 은 띄운 MCP 클라이언트를 놓아줍니다.
//
// nil 이면 에이전트는 내장 도구만 씁니다. 사용자가 UI 에서 스킬이나 MCP 를 붙이기 전까지
// 동작은 같습니다.
// 초보: 웹에서 에이전트에 붙인 스킬과 MCP 가 플래너, 워커, 메인 에이전트 도구 목록에 여기서 합쳐집니다.
var ToolAugment func(ctx context.Context, agentKey string) (extra []actool.CoreTool, def DeferredInfo, cleanup func())

// AugmentTools 는 기본 도구에 에이전트가 보는 스킬/MCP 도구를 더하고, DeferredInfo 와
// 호출자가 defer 해야 하는 cleanup(MCP 클라이언트를 닫음)을 돌려줍니다.
// 내장 기본 도구는 그대로 둡니다. 거르지 않습니다(내장 도구는 코드 층에 두고, 보이기 필터를 하지 않습니다).
func AugmentTools(ctx context.Context, agentKey string, base []actool.CoreTool) ([]actool.CoreTool, DeferredInfo, func()) {
	var (
		def     DeferredInfo
		cleanup = func() {}
		out     = base
	)
	if ToolAugment != nil {
		var extra []actool.CoreTool
		var cl func()
		extra, def, cl = ToolAugment(ctx, agentKey)
		if cl != nil {
			cleanup = cl
		}
		if len(extra) > 0 {
			out = append(append([]actool.CoreTool{}, base...), extra...)
		}
	}
	// DB tools 표가 내장 도구의 마지막 말입니다. 이 에이전트에 묶이지 않았거나
	// 꺼진 도구는 빼고, 덮어쓴 설명/schema 와 기본값 주입으로 바꿉니다.
	// MCP/skill/호스트 도구는 행이 없어 그대로 통과하므로, 미루기와 잠금 해제 연결이 어긋나지 않습니다.
	if ToolResolve != nil {
		out = ToolResolve(ctx, agentKey, out)
	}
	out, def.FindingGuidance = findingWorkflowTools(agentKey, out)
	for i, t := range out {
		out[i] = guardPanic(t)
	}
	return out, def, cleanup
}

// guardPanic 은 패닉이 난 도구 handler 를 평범한 도구 오류로 바꿉니다.
// 하네스는 도구마다 고루틴을 따로 쓰므로, handler 안의 패닉은 실행을 시작한
// 호출자가 recover 할 수 없습니다. 프로세스 전체가 죽고, 재시작하면 에이전트가
// 같은 호출을 다시 해 또 죽습니다. 맨 마지막에 씌우므로 에이전트가 닿는
// 모든 도구(도메인, SDK, MCP, 스킬)를 덮습니다.
// 초보: 도구 하나가 죽어도 엔진 전체가 내려가지 않게 하는 안전망입니다.
func guardPanic(t actool.CoreTool) actool.CoreTool { return &guardedTool{CoreTool: t} }

type guardedTool struct{ actool.CoreTool }

func (g *guardedTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (res actool.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tools] %s panic: %v\n%s", g.Name(), r, debug.Stack())
			res, err = actool.Errorf(fmt.Sprintf("도구 %s 내부 오류: %v (이번 호출은 실패했습니다. 인자를 바꾸거나 다른 도구를 쓰세요)", g.Name(), r)), nil
		}
	}()
	return g.CoreTool.Call(ctx, in, tc)
}
