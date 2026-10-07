package agent

import (
	"encoding/json"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// deferredSystem 은 DeferredInfo 로 시스템 프롬프트 조각과 캐시 경계를 만듭니다.
// 전역으로 쓸 수 있는 MCP 도구가 있으면, 이름과 "핵심 도구를 먼저 쓰라"는 안내를
// <available-deferred-tools> 블록으로 만들어 시스템 프롬프트의 마지막 조각에 둡니다.
// DynamicBoundary 를 켜서, 세션 동안 고정된 시스템 프롬프트 전체(이 블록 포함)를 캐시합니다
// (설계 문서 §2.1 / C1). 스킬을 열어야 쓰는 MCP 이름은 이 블록에 없고, 그 스킬이
// 로드될 때 나타납니다. 전역 블록이 없으면 평범한 조각 하나와 경계 0 을 돌려줍니다.
func deferredSystem(sysText string, def DeferredInfo) (system []string, boundary int) {
	sysText += def.FindingGuidance
	block := actool.RenderDeferredToolsBlock(def.GlobalNames)
	if block == "" {
		return []string{sysText}, 0
	}
	system = []string{sysText, block}
	boundary = len(system) // b >= len 이면 시스템 프롬프트 전체를 캐시합니다(SDK 쪽 검사)
	return system, boundary
}

// seedUnlockFromHistory 는 대화에 있던 Skill() 호출을 다시 돌려, 재개된 세션에서
// 스킬에 묶인 MCP 를 다시 엽니다(설계 문서 C2). 메인 에이전트는 턴마다 세션을 새로 만들고,
// 메모리 안의 잠금 해제 집합을 그대로 두면 초기화됩니다. 그러면 모델은 스킬이 보여 준
// 도구 이름은 보지만 호출은 못 합니다. unlockSkill 이 nil 이면(미룬 도구가 없으면) 아무 일도 하지 않습니다.
// 초보: 메인 에이전트 대화를 이어서 열 때, 예전에 연 스킬 도구를 엔진이 여기서 다시 풀어 줍니다.
func seedUnlockFromHistory(msgs []llm.Message, unlockSkill func(string)) {
	if unlockSkill == nil {
		return
	}
	for _, m := range msgs {
		for _, b := range m.ToolUses() {
			if b.Name != "Skill" {
				continue
			}
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Name != "" {
				unlockSkill(in.Name)
			}
		}
	}
}
