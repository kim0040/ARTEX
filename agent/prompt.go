package agent

import (
	"bytes"
	"text/template"
	"time"
)

// PromptOverride 가 설정되면, 에이전트 key 의 저장된 시스템 프롬프트 틀과
// 그것이 있는지를 돌려줍니다. 서버가 PG agent_prompts 표에 연결합니다.
// nil 이거나 덮어쓰기가 없으면 에이전트는 내장 기본 프롬프트를 씁니다.
// 사용자가 UI 에서 프롬프트를 고치기 전까지 동작은 같습니다.
var PromptOverride func(agentKey string) (string, bool)

// 프롬프트 변수 구조체입니다. 필드는 각 에이전트 목록(문서 §5a)과 같습니다.
// 사용자가 목록에 있는 변수를 쓰면 렌더되고, 그 밖을 쓰면 템플릿 실행이 실패해
// 내장 기본값으로 돌아갑니다.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr 은 서버 현지 시각 문자열이고, 공통 프롬프트 변수 {{.Now}} 로 나갑니다.
// renderSystem 은 에이전트 턴/라운드마다 돌므로 실행마다 새 값입니다.
// 프롬프트는 고정된 시작 시각에서 이것을 빼 경과 시간을 생각할 수 있습니다
// (예: 시간 제한 벤치마크의 "최근 N시간" 창).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem 은 agentKey 의 시스템 프롬프트 본문(구간 [A])을 렌더해 돌려줍니다.
// 우선순위: DB 에 저장된 틀이 있으면 그것을, 없으면 내장 기본 틀(def)을 씁니다.
// 둘 다 이제 Go 템플릿입니다. 내장 기본은 DB 에 그대로 심기므로, 사용자가
// 프롬프트를 고치기 전에는 두 경로가 같게 렌더됩니다.
// 렌더는 항상 합니다(def 는 예전엔 미리 치환된 평문이었다가, 이제 DB 쪽처럼 {{.Var}} 틀입니다).
// 렌더가 실패하면 기본 틀로, 그것도 실패하면 기본 원문으로 돌아갑니다.
// 에이전트는 반쯤 렌더된 프롬프트로 시작하지 않습니다. 호출자는 이 뒤에
// 코드가 소유한 꼬리(trafficTool / 중간 산출물 출력 규약)를 붙이므로,
// DB 본문을 고쳐도 그 꼬리는 지워지지 않습니다.
// 초보: UI 에서 고친 플래너·워커·메인 에이전트 프롬프트 본문이 여기서 실제 문장이 됩니다.
func renderSystem(agentKey, def string, vars any) string {
	tmpl := def
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
		}
	}
	if out, err := renderTmpl(tmpl, vars); err == nil {
		return out
	}
	// DB 틀이 깨졌습니다(예: 목록에 없는 변수). 코드 기본값으로 돌아갑니다.
	if out, err := renderTmpl(def, vars); err == nil {
		return out
	}
	return def
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
