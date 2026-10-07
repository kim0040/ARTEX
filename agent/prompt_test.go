package agent

import (
	"strings"
	"testing"
)

func TestRenderSystemOverrideAndFallback(t *testing.T) {
	t.Cleanup(func() { PromptOverride = nil })

	// 덮어쓰기가 없으면 내장 기본입니다
	PromptOverride = nil
	if got := renderSystem("planner", "DEFAULT", PlannerVars{Goal: "g"}); got != "DEFAULT" {
		t.Fatalf("no override should give default, got %q", got)
	}

	// 덮어쓰기가 있으면 변수로 그립니다
	PromptOverride = func(k string) (string, bool) {
		if k == "planner" {
			return "目标:{{.Goal}} 范围:{{.Scope}}", true // han-allow 업스트림 프롬프트·픽스처
		}
		return "", false
	}
	if got := renderSystem("planner", "DEFAULT", PlannerVars{Goal: "拿下X", Scope: "*.x.com"}); got != "目标:拿下X 范围:*.x.com" { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("override render: %q", got)
	}

	// 카탈로그에 없는 변수를 가리키면 실행 오류가 나고 기본으로 돌아갑니다
	PromptOverride = func(k string) (string, bool) { return "{{.NotInCatalog}}", true }
	if got := renderSystem("planner", "DEFAULT", PlannerVars{Goal: "x"}); got != "DEFAULT" {
		t.Fatalf("bad var should fall back to default, got %q", got)
	}

	// plannerSystem 전체 경로입니다. DB 본문 [A] 를 지키고, 코드가 가진 꼬리
	// [C] (중간 산출물 출력 규약) 를 항상 붙입니다. 본문을 고쳐도 꼬리는 빠지지 않습니다.
	PromptOverride = func(k string) (string, bool) { return "PLANNER {{.Goal}}", true }
	got := plannerSystem("拿下X", "/data", "/data") // han-allow 업스트림 프롬프트·픽스처
	if !strings.HasPrefix(got, "PLANNER 拿下X") {   // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("plannerSystem body not honored: %q", got)
	}
	if !strings.Contains(got, "中间产物输出规约") || !strings.Contains(got, "/data") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("plannerSystem missing code-owned artifact tail: %q", got)
	}

	// 사용자 템플릿의 {{if .ProxyAddr}} 로 워커 글이 둘로 갈라지고, 코드 꼬리가 붙습니다.
	// [B] trafficTool 은 기록 중일 때만 있습니다(caCert 가 있음. MITM 이 켜져
	// traffic_* 도구가 있음). [C] 산출물 규약은 항상 있습니다. trafficTool
	// 블록은 CA(인자 2)에 묶입니다. ProxyAddr 가 아닙니다. 전역 출구 프록시는
	// 캡처가 꺼지면 트래픽을 보내기만 하고 기록하지 않습니다.
	PromptOverride = func(k string) (string, bool) {
		return "{{if .ProxyAddr}}走代理 {{.ProxyAddr}}{{else}}手动{{end}}", true // han-allow 업스트림 프롬프트·픽스처
	}
	recording := workerSystem("127.0.0.1:8080", "/ca.pem", "/data", "/data")
	if !strings.HasPrefix(recording, "走代理 127.0.0.1:8080") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("worker proxy branch body: %q", recording)
	}
	if !strings.Contains(recording, "traffic_search") {
		t.Fatalf("worker while recording should inject trafficTool: %q", recording)
	}
	if strings.Contains(recording, "traffic_refs") {
		t.Fatalf("worker bypassed shared optional evidence policy: %q", recording)
	}
	if !strings.Contains(recording, "中间产物输出规约") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("worker missing artifact tail: %q", recording)
	}
	// 출구 프록시는 있고 캡처는 꺼짐(CA 없음). ProxyAddr 템플릿 가지는 여전히
	// 그려지지만, trafficTool 블록은 없어야 합니다. 그 도구는 등록되지 않습니다.
	egressOnly := workerSystem("127.0.0.1:8080", "", "/data", "/data")
	if !strings.HasPrefix(egressOnly, "走代理 127.0.0.1:8080") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("worker egress-only branch body: %q", egressOnly)
	}
	if strings.Contains(egressOnly, "traffic_search") {
		t.Fatalf("worker without recording must NOT inject trafficTool: %q", egressOnly)
	}
	noProxy := workerSystem("", "", "/data", "/data")
	if !strings.HasPrefix(noProxy, "手动") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("worker no-proxy branch body: %q", noProxy)
	}
	if strings.Contains(noProxy, "traffic_search") {
		t.Fatalf("worker without proxy must NOT inject trafficTool: %q", noProxy)
	}
}
