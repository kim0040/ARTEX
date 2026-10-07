package llmrec

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

type captureContextKey struct{}

// Capture는 한 논리 LLM 호출의 손을 대지 않은 전송 원문을 모읍니다.
// Recorder가 만들어 컨텍스트에 넣고, norma가 거치는 HTTP 전송
// (agent.quotaAwareTransport)이 찾아 그 값을 채웁니다.
//
// 초보: PostgreSQL에 남기는 정규화 JSON과 달리, 프로바이더와 오간 HTTP 바이트입니다.
//
// Recorder가 직접 보는 값은 이미 정규화되어 있습니다. llm.CompletionRequest는
// buildBody()가 보낸 본문이 아니라 다시 직렬화한 것이고, 응답은 SSE 프레임이
// 아니라 풀린 StreamEvents입니다. 살아 있는 프로바이더를 볼 때의 기준은
// 실제로 오간 바이트뿐입니다.
//
// norma의 doStream은 요청이 붙는 단계를 재시도하므로, 스트림 하나가
// HTTP 시도를 여러 번 낼 수 있습니다. 버려진 시도(norma/llm/retry.go가
// 최종이 아닌 본문을 읽지 않고 닫음)가 속도 제한과 게이트웨이 실패를
// 진단할 수 있게 하는 바로 그 재료입니다.
//
// 전송 쪽은 norma의 스트림 읽기 고루틴에서 쓰고, Recorder는 스트림이 끝날 때
// 스냅샷을 뜨므로 상태는 모두 뮤텍스로 지킵니다.
type Capture struct {
	mu       sync.Mutex
	request  string
	attempts []*attempt
}

type attempt struct {
	status int
	body   strings.Builder
}

// NewCapture는 새 Capture를 담은 컨텍스트와 Capture 자신을 돌려줍니다.
func NewCapture(ctx context.Context) (context.Context, *Capture) {
	c := &Capture{}
	return context.WithValue(ctx, captureContextKey{}, c), c
}

// CaptureFrom은 ctx에 붙은 Capture를 돌려줍니다. 원문 기록이 꺼져 있으면 nil입니다.
// 호출자는 nil을 견뎌야 합니다. 기록은 스위치이고, 기록하지 않는 프로바이더도
// 같은 전송을 탑니다.
func CaptureFrom(ctx context.Context) *Capture {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(captureContextKey{}).(*Capture)
	return c
}

// SetRequest는 나가는 요청 본문을 저장합니다. 재시도는 같은 바이트를 다시 보내므로
// 첫 시도의 본문만 남깁니다.
func (c *Capture) SetRequest(body string) {
	if c == nil || body == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.request == "" {
		c.request = body
	}
}

// TeeResponse는 새 시도를 열고, rc에서 읽은 내용을 그 시도에 비춥니다.
// 통째로 읽지 않고 비추는 이유는, 성공 응답이 호출자에게 계속 흘러야 하는
// SSE 스트림이기 때문입니다.
func (c *Capture) TeeResponse(status int, rc io.ReadCloser) io.ReadCloser {
	if c == nil || rc == nil {
		return rc
	}
	a := &attempt{status: status}
	c.mu.Lock()
	c.attempts = append(c.attempts, a)
	c.mu.Unlock()
	return &teeBody{rc: rc, c: c, a: a}
}

// RawRequest는 보낸 요청 본문입니다. 캡처된 것이 없으면 빈 문자열입니다.
func (c *Capture) RawRequest() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.request
}

// RawResponse는 받은 응답 바이트입니다. 시도가 하나면 손을 대지 않은 원문이라
// 재생에 그대로 붙여 넣을 수 있습니다. 여러 번이면 시도마다 머리글을 붙인 뒤
// 이어 붙여, 재시도 순서를 읽을 수 있게 합니다.
func (c *Capture) RawResponse() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch len(c.attempts) {
	case 0:
		return ""
	case 1:
		return c.attempts[0].body.String()
	}
	var b strings.Builder
	for i, a := range c.attempts {
		fmt.Fprintf(&b, "===== attempt %d/%d — HTTP %d =====\n", i+1, len(c.attempts), a.status)
		body := a.body.String()
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Attempt는 HTTP 한 왕복의 상태 코드와 응답 바이트입니다.
type Attempt struct {
	Status int
	Body   string
}

// Attempts는 모든 왕복을 순서대로 돌려줍니다. RawResponse는 시도가 하나일 때
// 머리글을 빼 바이트를 재생할 수 있게 하지만, 이 값은 항상 상태 코드를 담습니다.
// HTTP 401과 본문을 함께 보고해야 하는 호출자용입니다.
func (c *Capture) Attempts() []Attempt {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Attempt, 0, len(c.attempts))
	for _, a := range c.attempts {
		out = append(out, Attempt{Status: a.status, Body: a.body.String()})
	}
	return out
}

// teeBody는 읽은 내용을 Capture 시도에 비추며, Capture의 뮤텍스로 지킵니다.
// 스트림 중간에 스냅샷을 떠도 쓰는 쪽과 경합하지 않습니다.
type teeBody struct {
	rc io.ReadCloser
	c  *Capture
	a  *attempt
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.c.mu.Lock()
		t.a.body.Write(p[:n])
		t.c.mu.Unlock()
	}
	return n, err
}

func (t *teeBody) Close() error { return t.rc.Close() }
