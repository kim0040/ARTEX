package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel 은 범용 Webhook 어댑터입니다. URL, 메서드, 요청 헤더, JSON 템플릿을 사용자가 정합니다.
// Slack / Mattermost / Discord / 자체 시스템을 각각 구현하지 않아도 됩니다.
// 그 플랫폼은 설정 가능한 템플릿 하나로 덮입니다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 범용 Webhook에는 공식 제한이 없습니다. 0은 기본으로 한도를 두지 않는다는 뜻이고, 상대 능력에 맞게 사용자가 정합니다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url과 headers를 가립니다. 목적지 주소에 token이 있는 경우가 많고, 사용자 헤더에는
// 보통 인증 자격 증명이 있습니다. 둘 다 API 응답에 나오므로 막아야 합니다.
// 대가는 헤더 하나를 고치려면 헤더 묶음을 다시 넣어야 한다는 점입니다(가린 값은 「원값 유지」).
// 이 선택은 의도입니다. 한 번 더 입력하는 편이 자격 증명을 브라우저에 보여 주는 것보다 낫습니다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 목적지는 url입니다. url을 바꿀 때 headers를 다시 밝혀야 합니다. 그렇지 않으면
// 원래 Authorization 헤더가 새 주소로 그대로 나갑니다. 가리기를 우회하는 주된 경로입니다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate 은 템플릿을 비웠을 때의 본문입니다. 단순한 JSON이라
// 「JSON 한 건을 받아 저장」하는 자체 수신단 대부분을 덮습니다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData 는 사용자 템플릿에 열리는 문맥입니다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt 은 이번 전달 시각(RFC3339)입니다. 수신단이 기록할 때 씁니다.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel 은 상태 변경의 읽기 쉬운 설명입니다. 예: 「대기 → 수정됨」. 상태 변경이 아니면 빈 값입니다.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL이 없습니다")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("대상 URL이 올바르지 않습니다: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드 %s (사용 가능: GET/POST/PUT/PATCH)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 문법 오류: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET은 본문이 없습니다. 내용을 query에 넣는 것은 템플릿 범위를 넘고 GET 의미와도 맞지 않습니다.
	// GET은 「맞으면 훅을 친다」는 수신단에 맞습니다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 템플릿 결과는 문자열 형태의 JSON입니다. json.RawMessage로 그대로 보냅니다.
		// 다시 이스케이프하면 사용자가 만든 구조가 JSON 문자열 안에 한 번 더 감깁니다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿 결과가 올바른 JSON이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 덮어쓰기를 허용하되 headers 뒤에 적용해, 명시적 설정이 우선하게 합니다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 범용 Webhook은 본문을 자르지 않습니다(수신단은 사용자 서비스이고 크기는 body_template이 정합니다).
	// 따라서 묶음 전체가 도착한 것으로 칩니다.
	return len(m.Items), nil
}

// renderWebhookBody 는 사용자 템플릿(또는 기본 템플릿)으로 요청 본문을 그립니다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 문법 오류: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿을 그리지 못했습니다: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate 은 템플릿을 해석합니다.
//
// missingkey=zero 는 없는 map 키를 오류 대신 영 값으로 그립니다. 이 파일의 문맥은
// 구조체라, 주된 효과는 .Items가 비었을 때 range가 실패하지 않게 하는 것입니다.
// 정말 막아야 하는 것은 .Items가 nil인 경우입니다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs 는 템플릿에 열리는 보조 함수입니다.
var webhookTemplateFuncs = template.FuncMap{
	// json 은 임의의 값을 JSON으로 직렬화합니다.
	//
	// 장식이 아니라 필수입니다. 없으면 사용자는 {{.Title}}을 그대로 넣고,
	// 발견 제목에 따옴표나 줄바꿈이 있으면 요청 본문 전체가 올바른 JSON이 아닙니다.
	// 수신단은 거절하고, 오류는 「JSON 해석 실패」를 가리켜 제목의 따옴표를 떠올리기 어렵습니다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons 는 JSON 조각을 다른 JSON 문자열 값 안에 넣을 때 한 겹 문자열 이스케이프를 합니다.
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥 따옴표를 뺍니다. 호출자가 따옴표를 붙일지 정합니다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
