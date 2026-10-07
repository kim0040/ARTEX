package traffic

import (
	"errors"
	"net/url"
	"testing"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"example.com:443": "example.com",
		"example.com":     "example.com",
		"10.0.0.1:8080":   "10.0.0.1",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q)=%q want %q", in, got, want)
		}
	}
}

func TestProxyCausedErr(t *testing.T) {
	proxy := []string{
		"protocol error: received DATA on a HEAD request",
		"http2: server sent GOAWAY",
		"malformed HTTP response",
	}
	target := []string{ // 대상 쪽 실패는 통과 모드를 켜면 안 됨
		"dial tcp 1.2.3.4:443: connect: connection refused",
		"read: connection reset by peer",
		"context deadline exceeded",
	}
	for _, s := range proxy {
		if !proxyCausedErr(errors.New(s)) {
			t.Errorf("expected proxy-caused: %q", s)
		}
	}
	for _, s := range target {
		if proxyCausedErr(errors.New(s)) {
			t.Errorf("expected NOT proxy-caused: %q", s)
		}
	}
}

func TestMaybePassthroughFlagsHostOnce(t *testing.T) {
	tr := &Traffic{}
	f := &mproxy.Flow{Request: &mproxy.Request{URL: &url.URL{Host: "target.test:443"}}}

	// 대상이 일으킨 오류는 표시하지 않습니다. MITM 과 기록을 유지합니다.
	tr.maybePassthrough(f, errors.New("connection refused"))
	if _, ok := tr.pass.Load("target.test"); ok {
		t.Fatal("target-caused error must not flag passthrough")
	}

	// 프록시가 일으킨 오류는 그 호스트를 투명 통과로 표시합니다.
	tr.maybePassthrough(f, errors.New("protocol error: received DATA on a HEAD request"))
	if _, ok := tr.pass.Load("target.test"); !ok {
		t.Fatal("proxy-caused error must flag passthrough")
	}

	// shouldIntercept 규칙은 hostOnly(req.Host) 를 씁니다. CONNECT 호스트에는
	// 포트가 붙으므로, 같은 표시 키로 풀려야 합니다. 그러면 intercept=false(터널)입니다.
	if _, tunnel := tr.pass.Load(hostOnly("target.test:443")); !tunnel {
		t.Fatal("flagged host must be recognized for the CONNECT form with port")
	}
}
