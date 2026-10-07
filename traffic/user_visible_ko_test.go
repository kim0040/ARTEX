package traffic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTrafficMessagesKorean은 기록 프록시 도구가 사람에게 돌려주는 짧은 문장을 검사한다.
// 초보: 트래픽 저장소(SQLite)를 열지 않는다. host 가 비면 조회 전에 거절한다.
func TestTrafficMessagesKorean(t *testing.T) {
	tools := (&Traffic{}).Tools()
	if len(tools) == 0 {
		t.Fatal("no traffic tools")
	}
	res, err := tools[0].Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !strings.Contains(res.Flatten(), "host 는 필수입니다") {
		t.Fatalf("empty host: %v %s", err, res.Flatten())
	}
	if got := clip("abc", 1); !strings.Contains(got, "잘림, 총") {
		t.Fatalf("clip = %q", got)
	}
	body := []byte{0x89, 0x50, 0x4e, 0x47}
	if got := binaryTag("image/png", body); !strings.HasPrefix(got, "[이진 image/png,") || !strings.Contains(got, "magic=89504e47") {
		t.Fatalf("binary tag = %q", got)
	}
}
