package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/transcript"
)

// fakeRT 는 본 요청을 기록하고, 최소한의 200 응답을 돌려줍니다.
type fakeRT struct{ seen *http.Request }

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.seen = req
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func newReq(ctx context.Context) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.example.com/v1/messages", strings.NewReader("{}"))
	return req
}

func TestSessionHeaderInjectedFromContext(t *testing.T) {
	base := &fakeRT{}
	rt := quotaAwareTransport{base: base, sessionHeaderKey: "x-session-id"}
	ctx := transcript.WithSessionID(context.Background(), "conv-42")
	if _, err := rt.RoundTrip(newReq(ctx)); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := base.seen.Header.Get("x-session-id"); got != "conv-42" {
		t.Fatalf("x-session-id = %q, want conv-42", got)
	}
}

func TestSessionHeaderSkippedWhenKeyEmpty(t *testing.T) {
	base := &fakeRT{}
	rt := quotaAwareTransport{base: base} // 키가 설정되지 않음
	ctx := transcript.WithSessionID(context.Background(), "conv-42")
	if _, err := rt.RoundTrip(newReq(ctx)); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	// 헤더 이름은 사용자가 정한 것입니다. 키가 없으면 세션 관련 헤더를
	// 더하지 않습니다. 흔한 키가 없는지 확인합니다.
	if got := base.seen.Header.Get("x-session-id"); got != "" {
		t.Fatalf("unexpected session header %q with empty key", got)
	}
}

func TestSessionHeaderSkippedWhenNoSessionID(t *testing.T) {
	base := &fakeRT{}
	rt := quotaAwareTransport{base: base, sessionHeaderKey: "x-session-id"}
	// context 에 session id 가 없습니다(대화 기록 저장이 꺼짐).
	if _, err := rt.RoundTrip(newReq(context.Background())); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := base.seen.Header.Get("x-session-id"); got != "" {
		t.Fatalf("x-session-id = %q, want empty when no session id on context", got)
	}
}
