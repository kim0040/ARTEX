package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets 는 루프백 / 링크-로컬 주소로 메시지를 보낼 수 있는지를 정합니다.
//
// 기본은 거절입니다. 이 대역은 IM 로봇이나 공인 메일 서버가 있을 곳이 아니고,
// 닿는 대상은 민감합니다. 같은 기기의 다른 서비스 관리 포트, 클라우드 메타데이터
// 끝점(169.254.169.254, 인스턴스 자격 증명을 읽을 수 있음). 전달 주소는 관리자가
// 넣지만, XSS/CSRF로 빌린 관리 세션이나 같은 JWT를 쓰는 다른 사람이 설정을 바꿔
// 응답을 읽어 갈 수 있습니다. doJSON은 4xx/5xx 응답 본문 앞 200바이트를 last_error에
// 쓰고, 전달 이력 API가 그걸 다시 보여 줍니다. 반쯤 눈먼 읽기입니다.
//
// 다만 「이 기기의 SMTP 릴레이」(127.0.0.1:25의 postfix)는 자체 메일의 흔한 설정이라
// 일괄 차단하면 사람이 막힙니다. 하드코딩으로 열지 않고 명시적 탈출구를 둡니다.
// ARTEX_NOTIFY_ALLOW_LOCAL=1 이면 허용합니다.
//
// AllowLocalTargetsEnv 로 내보내는 이유는 테스트가 분명히 열 수 있게 하기 위해서입니다.
// 이 패키지와 server 패키지 테스트는 127.0.0.1의 httptest 수신단을 많이 쓰고,
// 열지 않으면 가드가 전부 막습니다.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP 는 대상 IP가 「기본적으로 전달하면 안 되는」 대역인지 보고합니다.
//
// 루프백, 링크-로컬(클라우드 메타데이터 169.254.169.254 포함), 미지정, 멀티캐스트만 거절합니다.
// RFC1918 사설망은 **거절하지 않습니다.** 내부망의 자체 Mattermost / SMTP 릴레이는
// 흔한 정상 용도이고, 같이 막으면 실제 환경에서 기능이 바로 죽습니다.
// 이 선택은 의도입니다. 정말 민감한 대상을 막고, 정상 배포를 같이 망가뜨리지 않습니다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6(::ffff:127.0.0.1)는 IPv4로 되돌린 뒤 판단합니다. 그렇지 않으면 검사를 우회합니다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial 은 http.Transport 다이얼러의 Control 훅입니다. **연결을 맺을 때**
// 대상 주소를 검사합니다.
//
// 설정을 저장할 때만 검사하지 않고 다이얼 단계에 두는 이유: 여기가 최종 적용 지점입니다.
// 설정 검사를 우회하는 두 경우를 같이 덮습니다. DNS 재바인딩(검사 때는 공인 IP,
// 실제 연결 때는 내부망)과 리다이렉트(다른 호스트 점프는 이미 거절하지만, 같은 호스트
// 점프는 경로를 다른 곳으로 가리킬 수 있습니다).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소를 해석할 수 없습니다 %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬/링크-로컬 주소 %s로는 전달하지 않습니다 (이 기기의 서비스로 보내야 하면 %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport 는 기본 Transport에 다이얼 가드만 더합니다.
// Clone으로 기본 조정(연결 풀, HTTP/2, 타임아웃, proxy 등)을 유지합니다.
// 검사 하나 때문에 다른 동작을 바꾸지 않습니다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient 는 모든 채널 전달이 공유하는 클라이언트입니다.
//
// 프로젝트의 전역 출구 프록시(server의 GlobalProxy)를 **재사용하지 않습니다.**
// 그 프록시는 점검 대상 트래픽용이고 종종 불안정한 터널입니다. 알림 가용성이
// 대상 네트워크의 흔들림에 묶이면 안 됩니다. IM 알림은 직접 연결이면 됩니다.
// 타임아웃은 15초입니다. 그보다 느린 상대는 이미 장애입니다.
//
// 다른 호스트로의 리다이렉트는 거절합니다. 이 기능의 전달 주소는 「고정 endpoint」
// 형태라 정상적으로는 다른 호스트로 가지 않습니다. 그런데 자격 증명(딩톡 access_token,
// 기업 위챗 key, Telegram bot token)이 **URL 안에** 있어, 다른 호스트를 따라가면
// 자격 증명을 리다이렉트 대상에게 주는 셈입니다. 같은 호스트 점프(끝 슬래시 보정 등)는 허용합니다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리다이렉트가 너무 많습니다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("다른 호스트로의 리다이렉트를 거절했습니다 (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit 은 응답 본문 읽기 상한입니다. 상대가 비정상이면 아주 큰 내용을 토할 수 있고,
// 우리에게 필요한 것은 오류 코드와 전달 이력에 보일 짧은 설명뿐입니다.
const respBodyLimit = 8 << 10

// doJSON 은 요청을 한 번 보내고 응답 본문(길이 제한됨)을 반환합니다.
//
// payload가 nil이면 빈 body를 보냅니다(GET이거나 플랫폼이 body를 요구하지 않을 때).
// headers의 키와 값은 그대로 붙입니다. 범용 Webhook의 사용자 헤더용입니다.
//
// 오류 분류가 이 함수의 핵심입니다. 네트워크 실패와 5xx/408/429는 「재시도 가능」,
// 나머지 4xx는 「영구 실패」입니다. 403을 재시도하면 같은 오류를 로그에 세 번 찍을 뿐입니다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 로컬 버그(설정 필드 타입이 틀림)라 재시도해도 나아지지 않습니다.
			return nil, Permanent(fmt.Errorf("요청 본문을 만들지 못했습니다: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL이 불법입니다. 대개 사용자가 주소를 잘못 넣은 영구 실패입니다.
		// 여기서도 err를 그대로 넘기면 안 됩니다. url.Parse 오류 텍스트에 전체 주소가 있습니다.
		return nil, Permanent(fmt.Errorf("요청 주소가 올바르지 않습니다: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거절, DNS 실패, 타임아웃은 대개 일시 장애라 백오프 재시도에 맡깁니다.
		//
		// 오류 텍스트는 가린 뒤에 밖으로 내야 합니다. http.Client.Do가 돌려주는 것은
		// *url.Error이고, Error()는 `Op "전체URL": 아래 오류`입니다. 이 기능의 자격 증명은
		// **URL 안에** 있습니다(딩톡 access_token, 기업 위챗 key, 페이슈 hook id, Telegram /bot<token>/).
		// 가리지 않으면 자격 증명이 네 곳으로 흐릅니다. notification_deliveries의
		// last_error(평문 저장), 전달 이력 API 응답(**채널 설정의 가리기를 우회**),
		// 서버 로그, 테스트 전송 API가 프론트에 주는 502 텍스트.
		return nil, fmt.Errorf("요청 실패: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답을 읽지 못했습니다: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429(요청 한도)와 408(시간 초과)는 재시도할 가치가 있습니다. 나머지 4xx는 설정이나 권한 문제라 재시도가 의미 없습니다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대가 요청을 제한했거나 시간이 초과되었습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대 서비스 이상 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대가 요청을 거절했습니다 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet 은 응답 본문을 한 줄의 짧은 텍스트로 접습니다. 오류 정보용입니다.
// 응답에 줄바꿈과 많은 공백이 있을 수 있고, 그대로 last_error에 넣으면 전달 이력 화면 배치가 깨집니다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget 은 전달 주소를 「scheme://host/…」로 접어 오류 정보에 씁니다.
//
// 이 패키지에서 주소를 가리는 유일한 기준이고, **일부러 거칠게** 합니다. scheme과 host 외에는
// 전부 버립니다. URL의 어느 조각이 자격 증명인지 「보편적이면서 안전한」 판단이 없기 때문입니다.
//
//	딩톡     자격 증명은 query      /robot/send?access_token=xxx
//	기업 위챗 자격 증명은 query      /cgi-bin/webhook/send?key=xxx
//	페이슈   자격 증명은 **경로 끝** /open-apis/bot/v2/hook/<hook_id>
//	Telegram 자격 증명은 **경로 중간** /bot<token>/sendMessage
//
// 「쓸모 있는 부분만 남기자」면 채널마다 패치가 필요하고, 한 곳을 빠뜨리면 자격 증명이 샙니다.
// host만 남겨도 조사에는 충분합니다(DNS 실패, 연결 실패, 인증서 불일치를 가릴 수 있음).
// 어느 로봇인지는 채널 설정의 가린 값 끝자리로 알아봅니다.
//
// 해석에 실패하면 고정 자리 표시를 반환합니다. 원문을 다시 보여 주지 않습니다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소를 해석할 수 없음)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError 는 전송 계층 오류에서 주소를 벗겨 아래 원인만 남깁니다.
//
// *url.Error 의 구조는 {Op, URL, Err}이고 Error()는 URL을 같이 찍습니다.
// 여기서 Err 필드를 직접 꺼내 Error()를 피합니다. 사후에 문자열을 바꾸는 것보다
// 확실합니다. 치환은 URL 인코딩/이스케이프 변형을 빠뜨리기 쉽습니다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error가 아닌 오류(리다이렉트 정책이 반환한 오류 등)도 주소를 가질 수 있어 같은 가리기를 탑니다.
	return redactURLsInText(err.Error())
}

// redactURLsInText 는 텍스트 안의 http(s) 주소를 가린 형태로 바꿉니다.
//
// 구조화 필드를 얻을 수 없는 오류(리다이렉트 정책 오류, 서드파티의 자체 오류)의 안전망입니다.
// http/https 접두만 알아보고, 공백과 따옴표로 자릅니다. 주소에는 그 문자가 들어가지 않습니다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
