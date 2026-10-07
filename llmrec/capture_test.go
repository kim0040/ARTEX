package llmrec

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestCaptureFromNilAndMissing(t *testing.T) {
	var nilCtx context.Context // 기록하지 않는 호출이면 전송이 여기까지 옵니다
	if CaptureFrom(nilCtx) != nil {
		t.Fatal("nil context should yield no capture")
	}
	if CaptureFrom(context.Background()) != nil {
		t.Fatal("bare context should yield no capture")
	}
}

// nil인 *Capture는 기록 꺼짐 경로입니다. 전송이 매 요청마다 이 메서드를 부르므로,
// 패닉이 아니라 모두 아무것도 하지 않아야 합니다.
func TestNilCaptureIsInert(t *testing.T) {
	var c *Capture
	c.SetRequest("body")
	if got := c.RawRequest(); got != "" {
		t.Fatalf("RawRequest()=%q want empty", got)
	}
	if got := c.RawResponse(); got != "" {
		t.Fatalf("RawResponse()=%q want empty", got)
	}
	rc := io.NopCloser(strings.NewReader("x"))
	if c.TeeResponse(200, rc) != rc {
		t.Fatal("nil capture must pass the body through untouched")
	}
}

func TestCaptureSingleAttemptKeepsBytesVerbatim(t *testing.T) {
	ctx, c := NewCapture(context.Background())
	if CaptureFrom(ctx) != c {
		t.Fatal("capture not retrievable from its own context")
	}
	c.SetRequest(`{"model":"x"}`)
	// 재시도는 같은 바이트를 다시 보냅니다. 첫 번째만 남깁니다.
	c.SetRequest(`{"model":"ignored"}`)

	const sse = "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {}\n\n"
	body := c.TeeResponse(200, io.NopCloser(strings.NewReader(sse)))
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != sse {
		t.Fatal("tee altered the stream delivered to the caller")
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if c.RawRequest() != `{"model":"x"}` {
		t.Fatalf("RawRequest()=%q", c.RawRequest())
	}
	// 시도가 하나면 바이트가 같아야 합니다. 머리글이나 프레임을 더하지 않아
	// 그대로 재생할 수 있습니다.
	if c.RawResponse() != sse {
		t.Fatalf("RawResponse()=%q want verbatim SSE", c.RawResponse())
	}
}

// norma의 doStream은 재시도한 시도의 본문을 버립니다(retry.go가 읽지 않고 닫음).
// 그래서 429 본문이 남는 곳은 이 캡처뿐입니다.
func TestCaptureRetriedAttemptsAreAllKept(t *testing.T) {
	_, c := NewCapture(context.Background())

	first := c.TeeResponse(429, io.NopCloser(strings.NewReader(`{"error":"rate_limit"}`)))
	if _, err := io.ReadAll(first); err != nil {
		t.Fatalf("read first: %v", err)
	}
	second := c.TeeResponse(200, io.NopCloser(strings.NewReader("data: ok\n")))
	if _, err := io.ReadAll(second); err != nil {
		t.Fatalf("read second: %v", err)
	}

	raw := c.RawResponse()
	if !strings.Contains(raw, `{"error":"rate_limit"}`) {
		t.Fatalf("dropped the retried attempt body: %q", raw)
	}
	if !strings.Contains(raw, "data: ok") {
		t.Fatalf("dropped the final attempt body: %q", raw)
	}
	if !strings.Contains(raw, "attempt 1/2 — HTTP 429") || !strings.Contains(raw, "attempt 2/2 — HTTP 200") {
		t.Fatalf("attempts not delimited: %q", raw)
	}
	if strings.Index(raw, "rate_limit") > strings.Index(raw, "data: ok") {
		t.Fatal("attempts stored out of order")
	}
}

func TestCaptureEmptyWhenNoHTTPHappened(t *testing.T) {
	_, c := NewCapture(context.Background())
	if c.RawRequest() != "" || c.RawResponse() != "" {
		t.Fatal("unused capture should be empty")
	}
}
