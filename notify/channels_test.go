package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg 는 따옴표와 줄바꿈이 있는 단건 메시지를 만듭니다. 제목/요약에 `"` 와 `\n` 을
// 일부러 넣습니다. 템플릿 보간이 잘못된 JSON을 만들기 가장 쉬운 입력입니다.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `로그인 지점 "SQL 주입" 위험`,
			VulnClass: "SQL 주입",
			Severity:  "high",
			Summary:   "매개변수 id\n필터링되지 않아 주입",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg 는 요약 메시지 한 묶음을 만듭니다.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "발견" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "반사형 크로스사이트 스크립트",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost 는 가짜 수신단을 띄우고, 받은 본문과 헤더를 단언 함수에 넘깁니다.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("요청 본문이 올바른 JSON이 아닙니다: %v\n원문: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("되돌아가는 링크가 있으면 actionCard여야 합니다. 결과 %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("되돌아가는 링크가 없습니다: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("요약 메시지는 markdown이어야 합니다. 결과 %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "최근 30분") {
			t.Errorf("요약 본문에 시간 창이 없습니다: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent 는 「HTTP 200이지만 errcode가 0이 아님」을 고정합니다.
// errcode를 안 보면 전달 실패를 성공으로 기록합니다. 국내 IM 플랫폼에 공통인 함정입니다.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode가 0이 아니면 오류여야 합니다")
	}
	if !IsPermanent(err) {
		t.Fatalf("키워드 불일치는 설정 오류라 영구 실패여야 합니다. 결과 %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("오류 정보에 플랫폼 오류 코드가 있어야 합니다. 결과 %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("자른 뒤 올바른 UTF-8이 아닙니다. 기업 위챗이 통째로 거절합니다")
		}
	})
	// 충분히 긴 한글 요약을 만듭니다. 4096바이트를 반드시 넘습니다.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("본문 %d 바이트가 기업 위챗 상한 %d 을 넘음", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("본문이 비었습니다")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009는 굴러가는 창의 요청 한도라 재시도 가능해야 합니다. 결과 %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000은 key가 잘못되어 재시도해도 스스로 낫지 않습니다. 영구 실패여야 합니다. 결과 %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("대화형 카드여야 합니다. 결과 %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high는 orange 색이어야 합니다. 결과 %v", header["template"])
		}
		// secret이 있으면 서명 인자가 있어야 합니다. 없으면 페이슈가 19021로 거절합니다.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("서명 인자가 없습니다: %v", body)
		}
		// 카드 요소에 발견 상세를 가리키는 버튼이 있어야 합니다.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("카드에 상세 페이지를 가리키는 버튼이 없습니다")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret이 없으면 서명 인자를 붙이면 안 됩니다: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML 해석 모드여야 합니다. 결과 %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// 제목과 요약은 대상/모델 출력이라 신뢰할 수 없습니다.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML을 이스케이프하지 않아 주입이 가능합니다: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔티티를 기대했습니다. 결과 %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& 가 이스케이프되지 않았습니다. 결과 %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429는 재시도 가능해야 합니다. 결과 %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403은 설정 문제라 영구 실패여야 합니다. 결과 %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// 기본 템플릿이 있는 이유입니다. 제목에 따옴표와 줄바꿈이 있으면
	// `"title": "{{.Title}}"` 같은 순진한 쓰기는 잘못된 JSON이 됩니다. {{json .}} 만 안전합니다.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 높음] 로그인 지점 "SQL 주입" 위험` {
			t.Errorf("제목이 그대로 복원되지 않았습니다: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items 수는 1이어야 합니다. 결과 %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "매개변수 id\n필터링되지 않아 주입" {
			t.Errorf("요약이 그대로 복원되지 않았습니다: %v", it["summary"])
		}
		// 숫자는 JSON 숫자여야 하고 문자열이 아니어야 합니다(json:"...,string" 같은 쓰기가 이 함정에 빠집니다).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id는 숫자여야 합니다. 결과 %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("사용자 헤더가 없습니다: %v", r.Header)
		}
		if body["msg"] != "3건" {
			t.Errorf("사용자 템플릿 렌더가 틀렸습니다: %v", body["msg"])
		}
		if body["first"] != "발견1" {
			t.Errorf("range 추출이 틀렸습니다: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d건" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("렌더 결과가 JSON이 아니면 영구 실패여야 합니다(템플릿이 틀림, 재시도 무의미). 결과 %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("%d번째 설정은 거절되어야 합니다: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("메일 조립 실패: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From 헤더가 틀렸습니다:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To 헤더가 틀렸습니다:\n%s", msg)
	}
	// 한글 제목은 RFC 2047로 인코딩해야 합니다. 그렇지 않으면 클라이언트가 깨진 글자로 보여 줍니다.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("제목이 RFC 2047로 인코딩되지 않았습니다:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("제목을 디코딩할 수 없습니다: %v", err)
	} else if !strings.Contains(dec, "SQL 주입") {
		t.Fatalf("제목을 디코딩한 내용이 틀렸습니다: %q", dec)
	}

	// 본문은 base64이고, 풀면 올바른 HTML이어야 합니다.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("메일에 헤더/본문 구분이 없습니다")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("본문 base64 디코딩 실패: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("본문이 HTML이 아닙니다: %.80s", html)
	}
	// 제목은 텍스트 위치에 그대로 나옵니다. HTML 텍스트의 큰따옴표는 올바른 문자라 이스케이프가 필요 없습니다.
	// 「그대로 유지」를 단언하는 이유는, 나중에 따옴표 이스케이프를 한 겹 더 넣어
	// 따옴표가 &quot; 로 보이는 실수를 막기 위해서입니다.
	if !strings.Contains(html, `"SQL 주입"`) {
		t.Fatalf("제목의 따옴표는 텍스트 위치에서 그대로여야 합니다: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection 은 메일 본문이 실제로 막아야 하는 주입을 덮습니다.
// 발견 제목과 요약은 대상과 모델 출력이라 신뢰할 수 없습니다. 텍스트 위치는
// & < > 를 이스케이프해야 하고(아니면 태그를 주입할 수 있음), 속성 위치는 따옴표도
// 이스케이프해야 합니다(아니면 href를 닫을 수 있음).
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("제목이 이스케이프되지 않아 태그를 주입할 수 있습니다: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔티티를 기대했습니다: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& 와 > 가 이스케이프되지 않았습니다: %s", html)
	}
	// 되돌아가는 링크는 관리자가 정하는 public_base_url이라 신뢰도가 높지만, 속성 위치에서는
	// 따옴표를 이스케이프해야 합니다. 그렇지 않으면 따옴표가 있는 주소가 href를 닫고 이벤트 핸들러를 주입합니다.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 속성이 제대로 이스케이프되지 않았습니다: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("속성 위치의 따옴표는 이스케이프되어야 합니다: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s 헤더를 찾지 못했습니다", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 검사 오류는 설정하는 사람에게 그대로 보입니다. 무엇이 빠졌는지 말해야 하고, 막연한 「설정이 잘못됨」은 안 됩니다.
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "포트"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "수신자"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("채널 %s 이(가) 등록되지 않았습니다", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s 설정 %v 은(는) 검사에 실패해야 합니다", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s 오류 정보가 %q 을(를) 언급해야 합니다. 결과 %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification 은 SMTP 4xx/5xx 의미 구분을 고정합니다.
// 4xx도 영구 실패로 보면, 그레이리스트를 켠 메일 서버는 모든 알림이 첫 시도 뒤
// failed로 떨어집니다. 그레이리스트야말로 자동 재시도가 가장 필요한 경우입니다.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 응답 코드를 못 읽으면 「재시도 가능」으로 봅니다. 한 번 더 시도하는 편이
		// 일시 장애를 죽은 것으로 보는 것보다 낫습니다.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("수신자 거절", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("응답 %q: 기대 permanent=%v 결과 %v", tc.reply, tc.permanent, got)
		}
		// 어떻게 분류하든 원문은 사용자가 조사할 수 있게 남겨야 합니다.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("응답 %q 의 원문이 버려졌습니다: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 여섯 채널은 하나라도 빠지면 안 됩니다. 하나 빠지면 UI 드롭다운에서 조용히 사라집니다.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("채널 수는 %d 이어야 합니다. 결과 %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("채널 %s 이(가) 등록되지 않았습니다", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("채널 %s 의 Kind()가 등록 키와 다릅니다", k)
		}
	}
	if ValidKind("nope") {
		t.Error("등록되지 않은 종류는 검사를 통과하면 안 됩니다")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("영구 실패로 알아봐야 합니다")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("오류 정보는 아래 원인을 그대로 전해야 합니다: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) 은 nil을 반환해야 합니다")
	}
	if IsPermanent(nil) {
		t.Fatal("nil은 영구 실패가 아닙니다")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
