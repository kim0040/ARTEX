package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit 은 Telegram sendMessage의 text 필드 상한(문자 수)입니다.
const telegramTextLimit = 4096

// telegramChannel 은 Telegram Bot API를 구현합니다.
//
// 플랫폼 특성:
//   - 인증은 전부 URL path에 있습니다(/bot<token>/sendMessage). 별도 서명은 없습니다.
//   - MarkdownV2 대신 HTML을 씁니다. MarkdownV2는 `_*[]()~`>#+-=|{}.!` 18자를
//     이스케이프해야 하고 하나라도 빠지면 메시지 전체가 거절됩니다. HTML은 & < > 세 개만 이스케이프합니다.
//   - 업무 오류도 HTTP 200 안에 있고, ok 필드로 판단합니다.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram 1:1은 약 1건/초, 그룹은 20건/분입니다. 보수적인 값을 씁니다.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token이 완전한 자격 증명입니다. chat_id는 수신자일 뿐 비밀이 아닙니다
// (Token 없이는 메시지를 보낼 수 없습니다).
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url 이 Token을 어느 API로 보낼지 정합니다(자체 리버스 프록시 등). 바꾸면 Token을 다시 밝혀야 합니다.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token이 없습니다")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID가 없습니다")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API 주소가 올바르지 않습니다: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429는 요청 한도라 기다린 뒤 재시도가 됩니다. 나머지(400 인자 오류, 401 token 오류,
	// 403 차단, 404 chat 없음)는 설정 문제라 재시도해도 스스로 낫지 않습니다.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 요청 한도: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 오류 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint 는 sendMessage 주소를 만듭니다. base_url이 비면 공식 API를 쓰고,
// 비어 있지 않으면 자체 Bot API 리버스 프록시입니다.
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err를 그대로 넘기지 않습니다. 주소에 Bot Token이 있고, 이때는 addr도 보여 주면 안 됩니다.
		return "", fmt.Errorf("API 주소를 잇지 못했습니다 (API 주소: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML 은 HTML 본문을 그리고, 본문과 실제로 쓴 항목 수를 반환합니다(Channel.Send 참고).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram 상한은 **문자 수**라 포장도 문자로 잽니다(runeSize).
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 모두 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>요약</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">자세히 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes 는 제목과 잘림 안내를 위해 남겨 두는 문자 수입니다.
const telegramReservedRunes = 160

// telegramBatchLine 은 요약 안의 한 줄입니다(아직 이스케이프하지 않음. 호출자가 한 번에 처리).
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle 은 요약 제목입니다. 건수는 **이 메시지에 실제로 들어 있는** 수이지
// 묶음 전체가 아닙니다. 그렇지 않으면 머리의 숫자를 전부로 읽습니다.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("발견 요약 · 총 %d건", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("（앞 %d건만 표시, 나머지 %d건은 다음에서 계속）", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d분 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape 는 HTML 텍스트를 이스케이프합니다.
// Telegram은 이 세 엔티티만 알아봅니다. 이미 있는 &amp; 같은 엔티티는 두 번 이스케이프됩니다.
// 그게 맞습니다. 보여 줄 것은 원문이지, 사용자가 HTML을 주입하게 두는 것이 아닙니다.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr 는 HTML 속성 값을 이스케이프합니다. 텍스트 이스케이프에 더해 따옴표를
// 처리합니다. URL의 따옴표가 href를 미리 닫으면 뒤 내용이 주입 지점이 됩니다.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
