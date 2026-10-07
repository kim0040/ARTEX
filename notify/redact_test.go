package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 이 파일은 불변 조건입니다. 채널 구현에서 나오는 **어떤** 오류 텍스트에도 자격 증명이 있으면 안 됩니다.
//
// 파일을 나눈 이유: 처음 채널 케이스는 성공 경로와 플랫폼 업무 오류만 보고,
// 전송 계층 실패는 전혀 보지 않았습니다. 그런데 위험한 것은 바로 전송 계층 오류(연결 거절/DNS 실패/시간 초과)입니다.
// http.Client.Do가 반환하는 *url.Error는 **전체 URL**을 오류 텍스트에 넣고, 이 기능의
// 여러 채널은 자격 증명이 URL 안에 있습니다. 그 문자열이 네 출구로 흘렀습니다.
//
//	notification_deliveries.last_error  → 평문으로 저장
//	GET /api/notify/deliveries 응답     → 채널 설정의 가리기를 우회해 브라우저에 표시
//	서버 로그                            → 밖으로 보관되는 일이 많음
//	테스트 전송 API의 502 응답           → 프론트에 그대로 뜸
//
// 그래서 함수 하나만 보지 않고, 채널마다 반드시 실패하는 요청을 실제로 보내
// 오류 텍스트에서 그 자격 증명을 찾지 못하는지 단언합니다.

// credentialCases는 「자격 증명이 URL 안에 있는」 채널 형태를 모두 덮습니다.
// 딩톡/기업 위챗은 쿼리, 페이슈는 경로 끝, Telegram은 경로 중간입니다.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "딩톡 access_token이 쿼리에 있음",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "기업 위챗 key가 쿼리에 있음",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "페이슈 hook id가 경로 끝에 있음",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token이 경로 중간에 있음",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "딩톡 서명 비밀",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken은 실제 자격 증명일 수 없는 센티널입니다. 오류 텍스트에서 이 값을 찾습니다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials는 핵심 불변 조건입니다.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 반드시 실패하는 상대: 127.0.0.1:1은 아무도 듣지 않아 연결 거절 경로로 갑니다.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "유출 탐침"}},
			})
			if err == nil {
				t.Fatal("닿을 수 없는 주소는 오류가 나야 합니다")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath는 영구 실패 분기를 덮습니다.
// URL 검증 실패, 플랫폼 업무 오류도 오류 텍스트를 밖으로 보내므로 자격 증명이 있으면 안 됩니다.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 주소에 자격 증명이 있지만 형식이 잘못됨 → validateHTTPURL / url.Parse 분기.
		{"딩톡 주소가 잘못됨", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"기업 위챗 주소가 잘못됨", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"페이슈 주소가 잘못됨", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API 주소가 잘못됨", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"일반 Webhook 주소가 잘못됨", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("잘못된 설정은 오류가 나야 합니다")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("오류 텍스트가 자격 증명 %q 를 흘렸습니다:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, 기대 %q", in, got, want)
		}
		// 가린 결과에 원래 주소의 경로/쿼리 조각이 남아 있으면 안 됩니다.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("가린 뒤에도 경로/쿼리 조각 %q 가 남았습니다: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 해석할 수 없는 입력은 원문을 다시 보여 주지 않습니다.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("해석할 수 없는 입력 %q 가 %q 로 다시 보였습니다", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL은 *url.Error라는 구체 타입을 직접 봅니다.
// http.Client.Do의 반환 타입이고, 유출의 첫 현장입니다.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("조사할 수 있게 host는 남아야 합니다. 결과 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("조사할 수 있게 하위 원인은 남아야 합니다. 결과 %q", got)
	}
	// Op도 남깁니다(POST인지 GET인지는 조사에 의미가 있습니다).
	if !strings.Contains(got, "Post") {
		t.Errorf("동작 이름은 남아야 합니다. 결과 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback은 폴백 경로입니다. *url.Error가 아닌 사용자 정의 오류
// (리다이렉트 정책이 반환하는 오류 등) 안의 주소도 같은 방식으로 걷어 냅니다.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("다른 호스트로의 리다이렉트를 거절했습니다 (a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("주소를 가린 형태로 바꿔야 합니다. 결과 %q", got)
	}
	// 주소가 없는 텍스트는 그대로 둡니다.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("주소가 없는 텍스트는 바뀌면 안 됩니다")
	}
}

// TestCrossHostRedirectRefused는 「자격 증명이 URL 안에 있고, 다른 호스트 점프를 따라가면 자격 증명을 넘긴다」를 덮습니다.
// httptest 서버 둘은 127.0.0.1의 다른 포트를 듣고, 포트가 다르면 Host가 다릅니다.
// 그게 바로 다른 호스트로의 점프입니다.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("다른 호스트로의 리다이렉트는 거절되어야 합니다")
	}
	if hit {
		t.Fatal("점프 대상이 호출되었습니다. 자격 증명이 리다이렉트와 함께 나갔습니다")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed는 반대 케이스입니다. 같은 호스트 점프(끝 슬래시 보정 등)는 계속 동작해야 합니다.
// 그렇지 않으면 정상 흐름까지 막습니다.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 같은 호스트, 같은 포트의 점프.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("같은 호스트 리다이렉트는 거절되면 안 됩니다: %v", err)
	}
}
