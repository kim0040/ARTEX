package agent

import (
	"strings"
	"testing"
	"time"
)

// TestChatNowVarRenders 는 공통 {{.Now}} 실행 변수를 확인합니다. 사용자 정의
// (대화) 에이전트 프롬프트가 이것을 쓰면, 매 턴 서버의 현재 시각을 그립니다.
// 템플릿 실행이 실패해 DefaultAssistantPrompt 로 돌아가지 않습니다.
func TestChatNowVarRenders(t *testing.T) {
	prev := PromptOverride
	defer func() { PromptOverride = prev }()

	PromptOverride = func(key string) (string, bool) {
		if key == "tec_benchmark" {
			return "当前时间：{{.Now}}", true // han-allow 업스트림 프롬프트·픽스처
		}
		return "", false
	}

	out := chatSystem("tec_benchmark", "/app/data", "/tmp/x")
	if !strings.Contains(out, "当前时间：") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("custom prompt body missing, likely fell back to default: %q", out)
	}
	year := time.Now().Format("2006")
	if !strings.Contains(out, year) {
		t.Fatalf("{{.Now}} did not render the live time (want year %s): %q", year, out)
	}
}

// TestChatDataDirVarRenders 는 공통 {{.DataDir}} 실행 변수를 확인합니다.
// 사용자 정의 프롬프트가 이것을 쓰면 서버 데이터 뿌리(s.m.dir)를 그립니다.
// 템플릿 실행이 실패해 DefaultAssistantPrompt 로 돌아가지 않습니다.
func TestChatDataDirVarRenders(t *testing.T) {
	prev := PromptOverride
	defer func() { PromptOverride = prev }()

	PromptOverride = func(key string) (string, bool) {
		return "数据根目录：{{.DataDir}}", true // han-allow 업스트림 프롬프트·픽스처
	}

	out := chatSystem("tec_benchmark", "/app/data", "/tmp/x")
	if !strings.Contains(out, "数据根目录：/app/data") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("{{.DataDir}} did not render the data root: %q", out)
	}
}

// TestChatUnknownVarFallsBack 은 카탈로그에 없는 {{.X}} 가 기본 도우미
// 프롬프트로 안전하게 내려가는지 봅니다(반쯤 그린 프롬프트는 없습니다).
func TestChatUnknownVarFallsBack(t *testing.T) {
	prev := PromptOverride
	defer func() { PromptOverride = prev }()

	PromptOverride = func(key string) (string, bool) {
		return "引用了不存在的变量：{{.Bogus}}", true // han-allow 업스트림 프롬프트·픽스처
	}

	out := chatSystem("whatever", "/app/data", "/tmp/x")
	if strings.Contains(out, "引用了不存在的变量") { // han-allow 업스트림 프롬프트·픽스처
		t.Fatalf("broken template should have fallen back, got custom body: %q", out)
	}
	if !strings.Contains(out, DefaultAssistantPrompt) {
		t.Fatalf("expected fallback to DefaultAssistantPrompt, got: %q", out)
	}
}
