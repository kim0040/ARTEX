package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel 은 페이슈(Lark 포함) 커스텀 로봇을 구현합니다. 대화형 카드를 씁니다.
//
// 플랫폼 특성:
//   - 서명 알고리즘이 딩톡과 **다르고** 틀리기 쉽습니다. feishuSign 주석을 보세요.
//   - 딩톡처럼 업무 오류를 HTTP 200 body에 넣습니다(code != 0).
//   - 카드 header는 색 템플릿을 지원합니다. 심각도로 색을 매겨 목록에서 한눈에 보게 합니다.
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// 페이슈 커스텀 로봇은 약 5회/초, 분당 100회입니다.
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook 주소 끝부분이 로봇의 고유 식별자라 자격 증명입니다.
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 같은 이유: Webhook 주소를 바꾸면 새 주소의 서명 비밀을 다시 밝혀야 합니다.
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 서명 인자는 메시지와 같은 층이고, secret을 설정했을 때만 나타납니다.
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 일부 페이슈 hook 버전은 이 필드 이름을 씁니다. 같이 받습니다.
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("페이슈 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("페이슈 오류 %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("페이슈 오류 %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign 은 페이슈 공식 규칙으로 서명을 계산합니다.
//
// 여기서 틀리기 쉽습니다. 공식 예제는
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// 즉 **key = timestamp + "\n" + secret, message는 빈 값**입니다. 직관적인
// 「key=secret, message=stringToSign」이 아닙니다. 그건 딩톡 알고리즘입니다.
// 둘은 인자 순서가 반대라, 다른 쪽을 베끼면 서명 검증이 실패합니다(19021).
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate 는 발견 심각도를 카드 header 색 템플릿에 대응시킵니다.
// 모르는 심각도는 grey입니다. blue를 쓰면 low와 헷갈립니다.
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes 는 카드 내용의 보수적 상한입니다. 페이슈는 카드 크기를 제한하고
// 넘으면 통째로 거절합니다. 공식 상한보다 분명히 낮게 잡아 JSON 포장 비용도 포함합니다.
const feishuMaxCardBytes = 24000

// feishuCard 는 대화형 카드를 만들고, 카드와 **실제로 쓴 항목 수**를 반환합니다.
// kept의 용도는 markdownBody와 같습니다. 카드에 실제로 들어간 항목만 전달됨으로 표시해야 합니다.
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 항목 단위로 포장한 뒤 머리를 붙입니다. 머리에는 「나머지 N건은 다음 메시지에서
		// 이어집니다」가 들어가고, N은 실제로 넣은 건수에서 나와야 합니다.
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("플랫폼에서 모두 보기", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("자세히 보기", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines 는 발견 하나의 lark_md 본문입니다.
//
// lark_md는 markdown과 같은 계열이라 링크와 강조를 해석합니다. 외부 필드는
// 모두 markdownText(한 줄 + 이스케이프)를 탑니다. 그렇지 않으면 발견 제목 하나가
// 페이슈에서 클릭 가능한 외부 링크가 됩니다.
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**상태 변경**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**유형**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**자산**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**요약**: %s", s)
		}
	}
	return out
}

// feishuBatchLine 은 요약 카드의 한 줄입니다.
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
