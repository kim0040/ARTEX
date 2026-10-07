package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout 은 각각 연결 수립과 SMTP 세션 전체를 제한합니다.
// net/smtp 자체에는 타임아웃이 없습니다. 이 두 칸이 없으면 멈춘 상대가
// 전달 goroutine을 영원히 붙잡습니다. dispatcher는 고루틴 하나가 직렬로 처리하므로
// 알림 시스템 전체가 멈춥니다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel 은 SMTP 메일 전달을 구현합니다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 메일에는 플랫폼 한도가 없지만 도배하면 안 됩니다. 느슨한 기본값을 줍니다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 가립니다. SMTP 호스트, 계정, 수신자는 비밀이 아니고, 가리면 편집만 번거롭습니다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port 는 비밀번호를 어느 서버에 줄지 정하고, tls는 전송을 암호화할지 정합니다.
// 셋 중 하나라도 바뀌면 비밀번호를 다시 밝혀야 합니다. 「TLS 끄기」도 자격 증명을
// 명시적으로 같이 내야 하고, 슬쩍 바꾸기만 해서는 안 됩니다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소가 없습니다")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트가 올바르지 않습니다 (1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("보낸 사람 주소가 없습니다")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 하나 이상 필요합니다")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: 상대가 지원하면 올립니다. 평문 세션에서는 자격 증명을 보내지 않습니다(아래 auth 설명).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth 는 암호화되지 않은 연결에서 자격 증명 전송을 거절합니다(대상이 localhost가 아니면).
			// 이것은 **올바른** 보안 동작이라 우회하면 안 됩니다. 다만 이유를 알려 줘야 합니다.
			// 그렇지 않으면 사용자는 「unencrypted connection」만 보고 어떻게 고칠지 모릅니다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 전송을 거절했습니다. 연결이 암호화되지 않았습니다. TLS를 켜거나, 465 포트(암시적 TLS)를 쓰거나, 「TLS 사용」을 체크하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("보낸 사람 %s 이(가) 거절됨", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 이(가) 거절됨", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문을 쓰지 못했습니다: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일을 제출하지 못했습니다: %w", err)
	}
	// Quit 실패는 「서버가 메일을 받았다」는 사실을 바꾸지 않으므로 오류를 무시합니다.
	_ = client.Quit()
	// 메일은 길이로 자르지 않습니다(HTML 본문을 전부 보냄). 묶음 전체가 도착한 것으로 칩니다.
	return len(m.Items), nil
}

// emailDial 은 SMTP 연결을 맺습니다.
//
// implicitTLS=true 는 465처럼 「붙자마자 TLS」입니다. false는 25/587 평문으로 붙인 뒤
// STARTTLS입니다. 섞으면 안 됩니다. 465에 평문 greeting을 보내면 바로 끊깁니다.
//
// 세션 기한은 **연결을 맺을 때** 미리 겁니다(나중에 보강하지 않음). net/smtp Client는
// 아래 연결을 내보내지 않는 필드에 숨겨 밖에서 꺼낼 수 없습니다. 연결을 넘긴 뒤에는
// 미리 건 deadline만 안전망입니다. 핸드셰이크가 막히는 경우도 같이 덮습니다.
// Control에 blockInternalDial을 걸어 HTTP 계열 채널과 같은 가드를 씁니다. 안 걸면 SMTP가
// SSRF 방어의 구멍입니다. host에 169.254.169.254나 127.0.0.1을 넣으면 바로 붙고,
// smtp.NewClient 핸드셰이크 실패 시 상대가 돌려준 한 줄이 오류에 들어가 last_error를 거쳐
// 전달 이력 API로 다시 보입니다. 반쯤 눈먼 읽기입니다. 「연결 거절 vs 타임아웃」
// 시간 차이로 포트를 엿볼 수도 있습니다. 다이얼 단계가 최종 적용 지점이고 DNS 재바인딩도 덮습니다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버에 연결하지 못했습니다: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError 는 SMTP 응답 코드로 그 단계의 실패를 「재시도 가능」과 「영구 실패」로 나눕니다.
//
// 구분해야 하는 이유: SMTP 4xx와 5xx는 의미가 완전히 다릅니다.
//   - 4xx(450 그레이리스트, 451 로컬 오류, 452 저장 공간 부족)는 **일시** 거절입니다.
//     정석은 나중에 재시도입니다. 그레이리스트는 거의 첫 전달마다 만납니다.
//   - 5xx(550 사용자 없음, 553 주소 불법)는 영구 거절이라 재시도가 의미 없습니다.
//
// 전부 영구 실패로 보면, 그레이리스트를 켠 메일 서버는 **모든** 알림이 첫 시도 뒤
// failed로 떨어집니다. 바로 자동 재시도가 가장 필요한 경우입니다.
// 응답 코드는 오류 텍스트 앞 세 자리입니다. 코드를 못 읽으면 재시도 가능으로 봅니다
// (한 번 더 시도하는 편이, 해석 실패로 일시 장애를 죽은 것으로 보는 것보다 낫습니다).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode 는 SMTP 오류 텍스트에서 앞의 세 자리 응답 코드를 꺼냅니다. 없으면 0입니다.
// net/smtp 는 오류 코드 필드를 내보내지 않아 텍스트에서만 꺼낼 수 있습니다. 형식은 「450 4.7.1 ...」입니다.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage 는 완전한 RFC 5322 메일을 조립합니다.
//
// 본문을 base64로 인코딩하는 이유 두 가지: SMTP는 한 줄 1000바이트를 넘기면 안 되는데
// HTML 본문(특히 요약 메일)은 긴 줄이 흔합니다. base64는 자연히 "."으로 시작하는
// 줄이 없어 SMTP 점 이스케이프를 피할 수 있습니다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 한글 제목은 RFC 2047로 인코딩해야 클라이언트가 깨진 글자로 보여 주지 않습니다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 메일에는 길이 하드 상한이 없어 본문을 자르지 않습니다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64는 76자마다 줄을 접습니다. RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
