package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
)

// 진짜 norma provider 를 끝까지 탑니다. Capture 가 context 를 타고 norma 로
// 들어가, 안의 요청 만들기를 견디고, buildBody() 가 전선에 올린 바이트를
// 그대로 들고 돌아옵니다. 이 기능의 전제입니다. norma 는 호출자 context 를
// http.NewRequestWithContext 까지 넘겨야 합니다.
func TestCapturePropagatesThroughNormaProvider(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":11,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	// 서버가 받은 것을 그대로 기록합니다. 캡처를 필드만 보지 않고
	// 바이트 단위로 비교합니다.
	var gotPath, serverSaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("server read body: %v", err)
		}
		serverSaw = string(b)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	cfg := Config{
		Format:  llm.FormatAnthropic,
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "claude-test",
	}
	prov, err := cfg.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	ctx, capt := llmrec.NewCapture(context.Background())
	req := llm.CompletionRequest{
		System:   []string{"you are a scanner"},
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools: []llm.ToolSchema{{
			Name:        "bash",
			Description: "run a shell command",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}},
		}},
		MaxTokens: 1024,
	}

	var text strings.Builder
	for ev, err := range prov.Stream(ctx, req) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Type == llm.SETextDelta {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "hello" {
		t.Fatalf("stream text=%q, capture interfered with delivery", text.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path=%q", gotPath)
	}

	// 원본 요청은 recorder 가 다시 직렬화한 것이 아니라, norma 가 실제로
	// 보낸 것이어야 합니다. 그래서 정규화 뷰가 버린 필드도 있습니다.
	raw := capt.RawRequest()
	if raw == "" {
		t.Fatal("no raw request captured — context did not reach the transport")
	}
	// 핵심 주장입니다. 저장한 것이 서버가 받은 것과 바이트까지 같습니다.
	// "필드가 맞다"만으로는 부족합니다.
	if raw != serverSaw {
		t.Fatalf("captured request != what the server received:\n got: %q\nsaw: %q", raw, serverSaw)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("raw request is not valid JSON: %v\n%s", err, raw)
	}
	if body["model"] != "claude-test" {
		t.Errorf("model=%v want claude-test (absent from CompletionRequest)", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream=%v want true", body["stream"])
	}
	// 도구 schema 전체가 핵심 이득입니다. 정규화 뷰는 이름만 남깁니다.
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["description"] != "run a shell command" {
		t.Errorf("tool description missing: %v", tool)
	}
	if tool["input_schema"] == nil {
		t.Errorf("tool input_schema missing: %v", tool)
	}

	// 응답은 손대지 않은 SSE 프레임입니다. recorder 가 저장 출력으로
	// 바꾸지 않는 사건도 포함합니다.
	if capt.RawResponse() != sse {
		t.Errorf("RawResponse mismatch:\n got: %q\nwant: %q", capt.RawResponse(), sse)
	}
}
