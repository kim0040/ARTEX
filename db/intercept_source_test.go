package db

import "testing"

// 사용자 안내 언어가 달라져도 승인 출처와 과거 기록의 분류를 유지해야 합니다.
func TestInterceptSourceKoreanAndLegacy(t *testing.T) {
	cases := []struct {
		ruleID       int64
		reason, want string
	}{
		{0, "[모델] 허용", "model"},
		{0, "[模型] allow", "model"}, // han-allow 이전 기록 호환
		{1, "[모델] 허용", "rule"},
		{0, "사용자 판정", "unknown"},
	}
	for _, c := range cases {
		if got := interceptSource(c.ruleID, c.reason); got != c.want {
			t.Errorf("출처(%d, %q) = %q, want %q", c.ruleID, c.reason, got, c.want)
		}
	}
}
