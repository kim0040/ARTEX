package db

import "testing"

// TestStripHostPort는 ip/cidr 범위가 쓰는 ip:port / [ipv6]:port 벗기기를 덮는다.
// "10.0.188.136:3000" 같은 대상이 더 이상 404가 되지 않게 한다.
func TestStripHostPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.0.188.136:3000", "10.0.188.136"},       // 보고된 IP 사례
		{"10.0.188.136", "10.0.188.136"},            // 맨 IPv4는 그대로
		{"[2001:db8::1]:8080", "2001:db8::1"},       // 대괄호 IPv6 + 포트
		{"2001:db8::1", "2001:db8::1"},              // 맨 IPv6는 그대로(콜론이 있음)
		{" 1.2.3.4:80 ", "1.2.3.4"},                 // 앞뒤 공백을 자른다
		{"example.com:443", "example.com"},          // 도메인 + 포트 → 맨 호스트
		{"api.example.com:8080", "api.example.com"}, // 서브도메인 + 포트
		{"example.com", "example.com"},              // 맨 도메인은 그대로
	}
	for _, c := range cases {
		if got := stripHostPort(c.in); got != c.want {
			t.Errorf("stripHostPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
