package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 서로 붙은 두 보강을 덮습니다.
//
//	① 전달 주소가 서버를 발판으로 내부망 / 클라우드 메타데이터에 닿으면 안 됩니다(SSRF)
//	② 주소 검증 오류에 주소 안의 자격 증명이 나가면 안 됩니다
//
// 이 패키지의 많은 케이스는 127.0.0.1의 httptest 수신단을 씁니다. 가드는 기본적으로 그것을 막습니다.
// 그래서 TestMain에서 AllowLocalTargetsEnv를 켜고, 아래 SSRF 케이스는 그 값을 직접 비워
// **기본 거절**을 단언합니다.

func TestMain(m *testing.M) {
	// 일반 케이스가 로컬 수신단에 붙게 합니다. SSRF 케이스는 스스로 잠시 비웁니다.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault는 SSRF 방어의 핵심 단언입니다.
// 기본 설정에서 루프백으로의 전달은 **연결 계층**에서 거절되어야 합니다.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구를 끔 = 기본 동작
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("기본적으로 루프백 주소로는 전달하면 안 됩니다")
	}
	if hit {
		t.Fatal("요청이 이 기기의 서비스에 닿았습니다. 가드가 동작하지 않았습니다")
	}
	// 오류는 어떻게 푸는지 안내해야 합니다(이 기기의 SMTP 릴레이는 합법적인 설정입니다).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거절 문구가 명시적으로 푸는 방법을 말해야 합니다: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn은 반대 케이스입니다. 명시적으로 열면 쓸 수 있어야 합니다.
// 그렇지 않으면 이 기기의 postfix / 내부망 릴레이 같은 합법 배포가 통째로 막힙니다.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("명시적으로 연 뒤에는 전달되어야 합니다: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 주소. 이 함수가 있는 주된 이유
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped는 되돌린 뒤에 판정해야 합니다. 아니면 우회로가 됩니다
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 는 거절되어야 합니다", s)
		}
	}
	// RFC1918 사설망은 **일부러 통과**시킵니다. 내부망의 자체 Mattermost / SMTP 릴레이는 흔한 합법 용도입니다.
	// 이 단언이 그 선택을 고정합니다. 나중에 사설망 판정이 슬쩍 들어가면 여기서 실패하고,
	// 조용히 배포를 망가뜨리는 대신 의식적인 결정을 요구합니다.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 는 통과해야 합니다(사설망은 흔한 합법 전달 대상입니다)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets는 설정 단계의 사전 안내를 덮습니다.
// 리터럴 IP는 저장할 때 거절해야 하고, 첫 전달이 실패한 뒤에 알게 하면 안 됩니다.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s 는 설정 단계에서 거절되어야 합니다", raw)
		}
	}
	// 공인 주소와 사설 주소는 그대로 통과합니다(사설망은 다이얼 단계에 맡기고, 거기서는 막지 않음).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s 는 검증을 통과해야 합니다: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials는 감사에서 지적된, 이전 라운드가 빠뜨린 분기입니다.
//
// url.Parse가 **실패**하면 *url.Error를 반환하고, 그 Error()에는 원본 주소 전체가 들어 있습니다.
// 이전에는 http.Client.Do의 반환 오류만 가렸고 여기를 빠뜨렸습니다. 그때 보강한 「영구 실패 경로」
// 케이스(file://, gopher://, ftp://)는 url.Parse가 성공해 scheme 분기로 가므로,
// 전부 통과해도 이 경로가 안전하다는 증거가 되지 않습니다. 가짜 보증이었습니다.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 잘못된 퍼센트 이스케이프
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 포트가 숫자가 아님
		"http://[::1?access_token=" + leakProbeToken,                  // 대괄호가 짝이 아님
	}
	for _, raw := range cases {
		// 이 입력이 **정말로** url.Parse를 실패시키는지 먼저 확인합니다. 이 단계가 없으면
		// 케이스가 다른 분기로 새어 나갈 수 있습니다(이전 라운드의 가짜 보증이 그렇게 생겼습니다).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q 는 해석에 실패해야 합니다. 아니면 이 케이스가 목표 분기를 덮지 않습니다", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q 는 검증에 실패해야 합니다", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 채널 층의 포장도 주소를 밖으로 내보내지 않는지 확인합니다.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("잘못된 주소는 검증에 실패해야 합니다")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault는 SMTP 채널의 다이얼 가드를 덮습니다.
//
// 메일 채널은 한때 맨 net.Dialer를 썼고, SSRF 방어에서 유일한 빈틈이었습니다. host를
// 169.254.169.254나 127.0.0.1로 두면 바로 붙고, smtp.NewClient 핸드셰이크가 실패하면
// 상대가 돌려준 한 줄을 오류에 넣어 last_error로 전달 이력 API에 다시 보여 줍니다.
// 다른 채널이 이미 닫은, 반쯤 눈먼 읽기입니다. 「연결 거절 vs 타임아웃」의 시간 차이로
// 포트를 짚을 수도 있습니다.
//
// 이 패키지의 TestMain은 AllowLocalTargetsEnv를 전역으로 켭니다(많은 케이스가 127.0.0.1
// 수신단을 씀). 그래서 이 케이스는 그 값을 직접 비워야 합니다. 그렇지 않으면 가드가 있든
// 없든 통과하고, 빈틈이 어떤 테스트에도 안 잡힌 이유가 바로 그것입니다.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구를 끔 = 기본 동작
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("기본적으로 루프백 주소로 메일을 전달하면 안 됩니다")
	}
	// 연결 자체가 맺어지면 안 됩니다. 가드가 Control 훅에서 막고, EHLO는 나가지 않습니다.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 세션이 이미 맺어졌습니다. 가드가 동작하지 않았습니다")
	}
	// 오류는 어떻게 푸는지 안내해야 합니다(이 기기의 postfix 릴레이는 합법적인 설정입니다).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거절 문구가 명시적으로 푸는 방법을 말해야 합니다: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn은 짝이 되는 반대 케이스입니다.
// 명시적으로 열면 정상 전달되어야 합니다. 내부망 SMTP / 이 기기의 릴레이는 매우 흔한 배포라, 가드가 전부 막으면 안 됩니다.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("명시적으로 연 뒤 이 기기의 SMTP는 전달되어야 합니다: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO가 보이지 않습니다. 세션이 실제로 맺어지지 않았습니다")
	}
}
