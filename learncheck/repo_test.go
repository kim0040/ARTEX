package learncheck

import "testing"

// TestRepoHasNoUnexpectedHan는 학습 문서, 주석, 사람에게 보이는 문자열에
// 허용 목록 밖 한자가 없는지 저장소 전체를 검사한다.
// 업스트림 프롬프트 본문과 프로토콜 토큰은 scan.go 의 이름 목록과
// 줄의 han-allow 표시로만 남긴다.
func TestRepoHasNoUnexpectedHan(t *testing.T) {
	hits, err := Scan("..")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("hits %d", len(hits))
	if len(hits) == 0 {
		return
	}
	limit := len(hits)
	if limit > 40 {
		limit = 40
	}
	for _, h := range hits[:limit] {
		t.Errorf("%s", h.String())
	}
	t.Fatalf("han hits %d", len(hits))
}
