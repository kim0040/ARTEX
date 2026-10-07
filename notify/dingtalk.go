package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel 은 딩톡 커스텀 로봇을 구현합니다.
//
// 플랫폼 특성(구현 선택의 이유):
//   - 로봇 하나당 분당 20건. 넘기면 조용히 버려질 수 있습니다(HTTP는 200일 수도 있음).
//     그래서 한도는 클라이언트에서 합니다. DefaultRatePerMin을 보세요.
//   - 보안 설정은 셋 중 하나입니다. 서명 / 사용자 키워드 / IP 허용 목록. 서명은
//     메시지 내용에 기대지 않는 유일한 방식이라 서명만 지원합니다(셋 다 끈 맨 webhook도 됩니다).
//   - 성공과 실패 모두 HTTP 200이고 body의 errcode로 구분합니다. errcode를 안 보면
//     전달 실패를 성공으로 기록합니다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// 딩톡 Webhook 주소에 access_token이 들어 있어 그 자체가 자격 증명입니다. 통째로 가립니다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 목적지는 딩톡 Webhook 주소입니다. 주소를 바꾸면 새 주소의 서명 비밀도 다시 밝혀야 합니다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

// Send 는 메시지를 한 번 보냅니다. 되돌아가는 링크가 있고 단건이면 ActionCard(버튼), 아니면 markdown입니다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// 딩톡 markdown 본문에 명확한 바이트 상한은 없지만, 증거 필드가 비정상적으로 커지지 않게 상한을 둡니다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "자세히 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// 딩톡은 업무 오류를 200 응답 안에 숨깁니다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("딩톡 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000은 서명 검증 실패, 310000은 키워드 불일치입니다. 둘 다 설정 오류라
		// 재시도해도 스스로 낫지 않습니다.
		return 0, Permanent(fmt.Errorf("딩톡 오류 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL 은 공식 서명 규칙으로 webhook에 timestamp와 sign을 붙입니다.
//
// 규칙: 서명할 문자열 = timestamp + "\n" + secret. HMAC-SHA256의 **키도 secret**이고,
// 결과를 base64한 뒤 URL 인코딩합니다. timestamp는 밀리초입니다. secret이 비면
// 그대로 돌려 서명을 켜지 않은 로봇을 지원합니다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err를 그대로 넘기지 않습니다. url.Parse의 오류 텍스트에 전체 주소(access_token 포함)가 있습니다.
		return "", fmt.Errorf("Webhook 주소를 해석하지 못했습니다: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL 은 주소를 쓸 수 있는지, 스킴이 지원되는지 보고, 리터럴 IP면 내부망인지 봅니다.
//
// 두 가지를 지킵니다.
//
//  1. **오류 메시지는 가려야 합니다.** url.Parse가 돌려주는 것은 *url.Error이고, Error()에
//     **원본 주소 전체**가 있습니다. 이 기능의 주소에는 자격 증명이 박혀 있습니다
//     (딩톡 access_token, 기업 위챗 key, Telegram bot token, 페이슈 hook id).
//     여기서 `return err`를 그대로 두면 「주소 형식이 잘못됨」 오류가 자격 증명을 데리고
//     테스트 API의 400, 매번 전달 때 저장되는 last_error, 서버 로그, 전달 이력 API로 흘렀습니다.
//
//  2. **리터럴 IP는 바로 내부망으로 판단**하고, 도메인은 다이얼 단계에 맡깁니다
//     (blockInternalDial이 최종 적용 지점이고 DNS 재바인딩도 덮습니다). 여기서 한 번
//     보는 이유는 설정을 저장할 때 안내를 주고, 첫 전달이 실패한 뒤에야 알게 하지 않기 위해서입니다.
//
// 스킴을 제한하는 것은 방어입니다. file:///gopher:// 같은 것은 http.Client가 예상 밖
// 동작을 합니다(스킴 검사가 막지만, 이 면을 열어 둘 이유가 없습니다).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다 (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https만 지원합니다. 받은 값: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트 이름이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬/링크-로컬 주소 %s로는 전달하지 않습니다 (이 기기의 서비스로 보내야 하면 %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
