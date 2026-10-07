package agent

import (
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// TestConfigFromOpenAIResponses 는 openai-responses 형식 연결을 고정합니다.
// ConfigFrom 이 Responses 형식을 고르고, 전체 /responses 끝점을
// API 기반으로 되돌리며, Provider() 가 짧은 이름을 왕복합니다. NewProvider 는
// 동작하는 provider 를 만들어야 합니다.
func TestConfigFromOpenAIResponses(t *testing.T) {
	c := ConfigFrom("openai-responses", "gpt-5", "https://gw.example/v1/responses", "sk-x", "")
	if c.Format != llm.FormatOpenAIResponses {
		t.Fatalf("format=%v, want FormatOpenAIResponses", c.Format)
	}
	if c.BaseURL != "https://gw.example/v1" {
		t.Fatalf("base_url=%q, want the /responses suffix stripped", c.BaseURL)
	}
	if c.Provider() != "openai-responses" {
		t.Fatalf("Provider()=%q", c.Provider())
	}
	if !c.Stream { // 기본 스트리밍은 유지됩니다
		t.Fatal("Stream should default true")
	}
	if _, err := c.NewProvider(); err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
}
