package traffic

import "testing"

func TestValidateProxyURL(t *testing.T) {
	ok := []string{
		"http://127.0.0.1:8080",
		"https://proxy.example.com:3128",
		"socks5://10.0.0.1:1080",
		"socks5://user:pass@10.0.0.1:1080",
	}
	for _, raw := range ok {
		if _, err := ValidateProxyURL(raw); err != nil {
			t.Errorf("ValidateProxyURL(%q) unexpected error: %v", raw, err)
		}
	}
	bad := []string{
		"127.0.0.1:8080",         // 스킴 없음
		"ftp://host:21",          // 지원하지 않는 스킴
		"http://",                // 호스트 없음
		"socks4://10.0.0.1:1080", // 지원하지 않는 스킴
	}
	for _, raw := range bad {
		if _, err := ValidateProxyURL(raw); err == nil {
			t.Errorf("ValidateProxyURL(%q) expected error, got nil", raw)
		}
	}
}

func TestSetUpstreamProxyStoreClear(t *testing.T) {
	tr := &Traffic{}
	if got := tr.upstream.Load(); got != nil {
		t.Fatalf("initial upstream = %v, want nil", got)
	}
	if err := tr.SetUpstreamProxy("socks5://user:pass@10.0.0.1:1080"); err != nil {
		t.Fatalf("SetUpstreamProxy: %v", err)
	}
	u := tr.upstream.Load()
	if u == nil || u.Scheme != "socks5" || u.Host != "10.0.0.1:1080" {
		t.Fatalf("stored upstream = %v, want socks5://10.0.0.1:1080", u)
	}
	if pw, _ := u.User.Password(); u.User.Username() != "user" || pw != "pass" {
		t.Fatalf("stored upstream lost credentials: %v", u)
	}
	// 빈 값은 직접 연결로 되돌립니다.
	if err := tr.SetUpstreamProxy("  "); err != nil {
		t.Fatalf("SetUpstreamProxy(clear): %v", err)
	}
	if got := tr.upstream.Load(); got != nil {
		t.Fatalf("after clear upstream = %v, want nil", got)
	}
	// 잘못된 값은 거부되고, 지금 상태를 바꾸지 않습니다.
	if err := tr.SetUpstreamProxy("nope://x"); err == nil {
		t.Fatal("SetUpstreamProxy(invalid) expected error")
	}
	if got := tr.upstream.Load(); got != nil {
		t.Fatalf("invalid set mutated upstream to %v, want nil", got)
	}
}

func TestProxyAddr(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		// 맨 :포트 는 "모든 인터페이스에 바인드"입니다. 예전 기본값입니다.
		// 에이전트가 쓰는 URL 은 여전히 루프백을 가리켜 로컬 프록시에 닿아야 합니다.
		{":8788", "http://127.0.0.1:8788"},
		// 명시적 루프백. #129 이후의 현재 기본값입니다(열린 프록시 노출).
		{"127.0.0.1:8788", "http://127.0.0.1:8788"},
		// 모든 인터페이스 바인드도 여전히 됩니다(SSH 로 원격 캡처).
		{"0.0.0.0:8788", "http://0.0.0.0:8788"},
	}
	for _, c := range cases {
		got := (&Traffic{addr: c.addr}).ProxyAddr()
		if got != c.want {
			t.Errorf("ProxyAddr(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
