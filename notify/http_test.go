package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 이 파일은 doJSON의 HTTP 계층 오류 분류를 덮습니다.
//
// 따로 테스트하는 이유: 채널 어댑터는 플랫폼 자신의 업무 오류 코드(딩톡 errcode,
// 페이슈 code, Telegram ok 필드)만 보고, **HTTP 계층** 분류는 doJSON이 한곳에서 합니다.
// 둘은 독립된 방어선입니다. 이 선이 없으면 503을 돌려주는 중계 게이트웨이가 영구 실패가 되어
// 재시도를 바로 포기하고, 403은 재시도 가능이 되어 백오프를 세 바퀴 헛돕니다.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 성공은 오류가 아님", 200, false},
		{"429 요청 한도는 재시도 가능", 429, false},
		{"408 요청 시간 초과는 재시도 가능", 408, false},
		{"500 서버 오류는 재시도 가능", 500, false},
		{"502 게이트웨이 오류는 재시도 가능", 502, false},
		{"503 서비스 이용 불가는 재시도 가능", 503, false},
		{"400 인자 오류는 영구 실패", 400, true},
		{"401 인증 실패는 영구 실패", 401, true},
		{"403 접근 금지는 영구 실패", 403, true},
		{"404 주소 없음은 영구 실패", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx는 오류가 아니어야 합니다: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx가 아니면 오류여야 합니다")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d 의 permanent 판단이 틀렸습니다. 기대 %v 결과 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 상태 코드는 오류에 있어야 합니다. 없으면 사용자가 자기 설정 문제인지 상대 장애인지 모릅니다.
			// Go의 영어 StatusText가 아니라 숫자를 단언합니다. 문구는 한국어이고,
			// 숫자만 언어와 상관없이 안정적으로 단언할 수 있습니다.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("오류 정보에 HTTP 상태 코드 %d 이(가) 있어야 합니다. 결과 %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet 은 snippet을 덮습니다. 상대가 돌려준 오류 설명을
// 가져와야 합니다. 그렇지 않으면 사용자는 「실패했다」만 알고 왜 거절했는지 모릅니다.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 합니다")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("오류 정보에 상대의 설명이 있어야 합니다. 결과 %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded 는 snippet 형태를 제약합니다.
// 상대 응답은 last_error 열과 프론트 표에 그대로 들어가므로, 여러 줄이나 과도한 길이는 배치와 적재물을 망가뜨립니다.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈, 탭, 5000자의 긴 내용이 있는 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 합니다")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("오류 정보는 한 줄이어야 합니다. 결과 %q", msg)
	}
	// snippet 상한 200자 + 고정 접두. 전체는 원본 응답보다 훨씬 짧아야 합니다.
	if len(msg) > 400 {
		t.Errorf("오류 정보가 너무 깁니다(%d 바이트). snippet으로 잘려야 합니다: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse 는 읽기 상한을 확인합니다. 상대가 비정상적으로
// 큰 내용을 돌려줄 때 응답 전체를 메모리에 읽으면 안 됩니다(전달 이력의 각 행이 last_error 한 부를 저장합니다).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("오류여야 합니다")
	}
	if len(err.Error()) > 400 {
		t.Errorf("큰 응답은 길이 제한으로 읽고 잘라야 합니다. 오류 정보 길이 %d", len(err.Error()))
	}
}
