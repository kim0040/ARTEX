package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/llmrec"
)

// 이 전송 층만 아직 wire 본문을 봅니다. norma 는 요청 본문을 안에서 만들고,
// SSE 응답을 푼 뒤에야 recorder 에 줍니다. 왕복이 양쪽을 건드리지 않고
// 보존하는지 확인합니다.
func TestRoundTripCapturesRawBodies(t *testing.T) {
	const sse = "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	const reqBody = `{"model":"claude","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t","input_schema":{}}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ctx, capt := llmrec.NewCapture(req.Context())
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = resp.Body.Close()

	if string(got) != sse {
		t.Fatal("capture altered the response delivered to norma")
	}
	if capt.RawRequest() != reqBody {
		t.Fatalf("RawRequest()=%q want %q", capt.RawRequest(), reqBody)
	}
	if capt.RawResponse() != sse {
		t.Fatalf("RawResponse()=%q want the raw SSE frames", capt.RawResponse())
	}
}

// 429 본문은 할당량 검사가 읽고 그 자리에서 바꿉니다. 캡처는 그 본문을
// 여전히 봐야 하고, 바꾼 본문은 뒤에서 읽을 수 있어야 합니다.
func TestRoundTripCaptures429BodyAlongsideQuotaRewrite(t *testing.T) {
	const body = `{"error":{"message":"insufficient_quota"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ctx, capt := llmrec.NewCapture(req.Context())
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	// 할당량 소진은 402 로 바꿉니다. 라우터가 다른 provider 로 넘어가게 합니다.
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status=%d want 402", resp.StatusCode)
	}
	if capt.RawResponse() != body {
		t.Fatalf("RawResponse()=%q want %q", capt.RawResponse(), body)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read replaced body: %v", err)
	}
	if string(rest) != body {
		t.Fatalf("replaced body=%q want it still readable", rest)
	}
}

// 기록이 꺼지면 context 에 Capture 가 없습니다. 전송은 예전과 같아야 합니다.
// 할당량 다시 쓰기도 포함합니다.
func TestRoundTripWithoutCaptureIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("body=%q", got)
	}
}
