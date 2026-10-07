package notify

import (
	"net/url"
	"testing"
	"time"
)

// 서명 기준값은 OpenSSL이 따로 계산한 것입니다. 이 패키지 구현으로 만들면
// 「코드가 안 바뀌었다」만 증명하고 「알고리즘이 맞다」는 증명하지 못합니다.
//
//	시각 TS=1700000000000, 비밀 SECRET=SECtest123
//	딩톡: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      딩톡 기대값 -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	페이슈: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      페이슈 기대값 -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("만든 주소를 해석할 수 없습니다: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("서명이 다릅니다\n기대 %s\n결과 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("타임스탬프는 밀리초이고 그대로 붙어야 합니다. 결과 %q", q.Get("timestamp"))
	}
	// 기존 query 인자(access_token)를 서명이 덮으면 안 됩니다.
	if q.Get("access_token") != "tok" {
		t.Errorf("기존 query 인자가 사라졌습니다. 결과 %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("서명이 다릅니다\n기대 %s\n결과 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer 는 두 알고리즘의 차이를 고정합니다. 인자 순서가 서로 반대입니다
// (딩톡 key=secret, 페이슈 key=서명할 문자열). 다른 쪽을 베끼면 검증이 실패합니다.
// 이 케이스는 나중에 리팩터할 때 둘을 한 함수로 합치지 못하게 합니다.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("딩톡과 페이슈 서명이 같습니다. 한쪽 알고리즘 구현이 틀렸습니다")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 서명을 켜지 않은 로봇: timestamp/sign 인자를 멋대로 붙이면 안 됩니다.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret이 없으면 주소가 바뀌면 안 됩니다. 결과 %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q 은(는) 허용되어야 합니다: %v", s, err)
		}
	}
	// file:// 같은 것은 통과시키면 안 됩니다. http.Client가 그것들을 처리하는 방식은 예상 범위 밖입니다.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q 은(는) 거절되어야 합니다", s)
		}
	}
}
