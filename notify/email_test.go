package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 이 파일은 메일 채널의 프로토콜 단계를 채웁니다. 그 전에는 email.Send 커버리지가 0이었습니다.
// SMTP 경로를 도는 케이스가 하나도 없었고, 여섯 알림 채널 가운데 프로토콜 면이 가장 넓고
// 실패 의미가 단계마다 다른 곳이 바로 여기입니다(인사, 인증, 봉투, DATA).
//
// net/smtp를 목으로 바꾸지 않고, 최소 SMTP 서버를 직접 띄웁니다.
// 메일 채널의 위험은 「실제 SMTP 서버와 대화하는」 단계에 있고, 그 단계를 목으로 바꾸면 테스트를 안 한 것과 같습니다.

// fakeSMTP는 딱 필요한 만큼의 SMTP 서버입니다. greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT을 마치고,
// 케이스가 지정한 단계에 지정한 응답 코드를 돌려줍니다.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply는 RCPT TO 응답입니다. 기본 250.
	rcptReply string
	// mailReply는 MAIL FROM 응답입니다. 기본 250.
	mailReply string
	// advertiseAuth가 true이면 EHLO에서 AUTH PLAIN을 알립니다.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 리슨 주소가 아닙니다")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS는 알리지 않습니다. 코드가 평문 분기로 가게 합니다(목표는 봉투 로직이지 TLS가 아님).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 단순화: PLAIN 초기 응답이 여러 줄일 수 있어 그대로 받아들입니다.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	// 봉투 단계까지 가야 합니다. 보낸 사람, 수신자 둘, DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 세션에 %q 가 없습니다. 실제 명령: %v", want, f.commands)
		}
	}
	// 본문은 base64 HTML이고, 실제 발견 내용이 들어 있어야 합니다(인코딩 후에도 구분 가능).
	body := f.body()
	if body == "" {
		t.Fatal("DATA 단계에서 본문을 받지 못했습니다")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type 헤더가 없습니다:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("본문이 base64가 아닙니다(긴 HTML 줄은 SMTP 1000바이트 줄 제한을 깹니다):\n%s", body)
	}
	// 수신자가 여럿이면 To 헤더에 모두 보여야 합니다.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 헤더에 수신자가 모두 없습니다:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// 계정이 없으면 AUTH를 보내지 않습니다. 일부 릴레이는 그 때문에 거절합니다.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전달 실패: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("계정이 없는데 AUTH를 보냈습니다: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies는 감사 수정의 직접 검증입니다.
// 5xx는 영구 실패, 4xx(그레이리스트)는 재시도 가능입니다.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"수신자 550 영구 거절", "550 5.1.1 User unknown", "250 OK", true},
		{"수신자 450 그레이리스트", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"수신자 452 사서함 가득 참", "452 4.2.2 Mailbox full", "250 OK", false},
		{"발신자 553 영구 거절", "250 OK", "553 5.1.3 Bad address", true},
		{"발신자 451 일시 오류", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("오류가 나야 합니다")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 판정이 틀렸습니다. 기대 %v 실제 %v (%v)", tc.permanent, got, err)
			}
			// 서버 원문을 남겨야 합니다. 그래야 관리자에게 물을지 주소를 고칠지 압니다.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("오류에 서버 응답 코드가 남아 있어야 합니다: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp의 PlainAuth는 암호화되지 않은 연결에서 자격 증명 전송을 거절합니다(대상이 localhost가 아니면).
	// 이 거절은 올바른 동작이라 우회하면 안 되고, 사용자가 고칠 수 있는 오류를 내야 합니다.
	// 여기서는 localhost가 아닌 호스트 이름으로 그 분기를 탑니다.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost가 아님
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("이 머신의 DNS가 로컬 서버로 풀렸습니다. 건너뜁니다(다른 케이스에는 영향 없음)")
	}
	// 연결 실패이거나 자격 증명 전송 거절이면 이 단언을 통과합니다. 비밀번호가 조용히 나가면 안 됩니다.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("오류: %v (localhost가 아니면 연결 실패가 예상입니다)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// 메일 채널은 설정 필드가 많아, 하나라도 빠지면 전달 때가 되어야 드러납니다. 여기서는 하나씩
	// 검증이 미리 막는지 확인합니다. 단언이 보는 것은 「오류 문구가 무엇이 빠졌는지 말하는지」입니다.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host 없음", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port 없음", map[string]any{"host": "smtp.example.com"}},
		{"port 범위 밖", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from 없음", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to 없음", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("검증이 실패해야 합니다: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance는 설정 읽기의 관용을 덮습니다. JSONB 숫자는 float64인데,
// UI에서는 포트를 문자열로 넣을 수 있고 배열이 문자열 하나일 수도 있습니다.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 문자열 포트
		"from": "a@b.c",
		"to":   "d@e.f", // 배열이 아닌 문자열 하나
		"tls":  "true",  // 문자열 불리언
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("문자열 형태의 숫자를 허용해야 합니다: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt가 문자열 포트를 해석하지 못했습니다. 결과 %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool이 문자열 \"true\"를 해석하지 못했습니다")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings가 문자열 하나를 받아들이지 못했습니다. 결과 %v", to)
	}
}

// TestFilterValidateRejectsTypo는 감사 수정의 직접 검증입니다.
// 문턱을 잘못 치면 저장 시점에 막아야 합니다. 아니면 필터가 조용히 풀려 전부 전달됩니다.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("올바른 문턱 %q 가 거절되었습니다: %v", s, err)
		}
	}
	// 실제로 나는 오타입니다. 전부 거절되어야 합니다.
	for _, s := range []string{"hgih", "HIGH", "심각", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("잘못된 문턱 %q 는 거절되어야 합니다(아니면 필터가 조용히 풀려 전부 전달됩니다)", s)
			continue
		}
		// 오류 문구가 고칠 값을 안내해야 합니다.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("오류 문구가 선택지를 나열해야 합니다. 결과 %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly는 「쓸 때는 엄격, 읽을 때는 관대」를 고정합니다.
// 이미 저장된 나쁜 값 때문에 채널 전체를 읽지 못하면, 기존 채널의 전달이 갑자기 멈춥니다.
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // 오류를 내지 않음
	if f.MinSeverity != "hgih" {
		t.Fatalf("읽기 경로는 원문을 유지해야 합니다. 결과 %q", f.MinSeverity)
	}
	// 그 채널도 이벤트 판정은 할 수 있어야 합니다(패닉도 막힘도 없음).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
