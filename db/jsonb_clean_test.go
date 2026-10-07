package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJsonbClean(t *testing.T) {
	// NUL 바이트가 든 직렬화 본문(예: 잡은 HTTP/도구 출력).
	b, err := json.Marshal(map[string]string{"body": "ab\x00cd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `\u0000`) {
		t.Fatalf("precondition: marshaled JSON should contain the NUL escape, got %s", b)
	}

	cleaned := jsonbClean(b)
	if strings.Contains(string(cleaned), `\u0000`) {
		t.Fatalf("jsonbClean left a NUL escape jsonb rejects: %s", cleaned)
	}

	// 결과는 NUL만 빠진 유효한 JSON이어야 한다.
	var out map[string]string
	if err := json.Unmarshal(cleaned, &out); err != nil {
		t.Fatalf("cleaned bytes are not valid JSON: %v (%s)", err, cleaned)
	}
	if out["body"] != "abcd" {
		t.Fatalf("expected NUL stripped to \"abcd\", got %q", out["body"])
	}

	// 원문 글에 적은 NUL 이스케이프(역슬래시 두 개)는 유지된다.
	lit := []byte(`{"body":"\\u0000"}`)
	if got := jsonbClean(lit); string(got) != string(lit) {
		t.Fatalf("literal \\\\u0000 must be untouched, got %s", got)
	}

	// NUL 이스케이프가 없으면 그대로 돌려준다.
	plain := []byte(`{"body":"hello"}`)
	if got := jsonbClean(plain); string(got) != string(plain) {
		t.Fatalf("plain JSON must be untouched, got %s", got)
	}
}
