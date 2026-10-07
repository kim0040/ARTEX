package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit 은 기업 위챗 그룹 로봇 markdown content의 하드 상한(바이트, 문자 수 아님)입니다.
// 여섯 채널 중 가장 빡빡한 제한이고, TruncateBytes가 있는 주된 이유입니다.
const weComMarkdownLimit = 4096

// weComChannel 은 기업 위챗 그룹 로봇을 구현합니다.
//
// 플랫폼 특성:
//   - URL의 key로만 인증하고 서명은 없습니다. webhook 주소 자체가 자격 증명 전부입니다.
//   - markdown content 상한은 4096 **바이트**이고, 넘으면 통째로 거절됩니다(자르지 않음).
//     한글은 3바이트/글자라 본문은 천여 글자 정도만 들어가므로 클라이언트가 잘라야 합니다.
//   - 요청 한도는 20건/분이고, 역시 클라이언트 한도로 막습니다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// 기업 위챗의 자격 증명은 Webhook 하나(URL의 key)이고 서명을 지원하지 않습니다.
// 주소 전체가 자격 증명이라 가릴 다른 필드가 없습니다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// 기업 위챗은 Webhook 필드 하나뿐이고, 그것이 목적지이자 자격 증명입니다.
// 「주소를 바꾼 뒤 남는 자격 증명」은 없습니다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 요약은 길어질 수 있습니다(50건 × 한 줄 + 접두). 4096바이트를 넘기기 쉽습니다.
	// 여기서 자릅니다. 플랫폼이 거절하면 묶음 전체가 사라지고, 자르면 앞부분은 도착합니다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("기업 위챗 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009는 호출 한도 초과입니다. 플랫폼 창이 굴러가므로 기다린 뒤 재시도가 됩니다.
		// 여기까지 왔다면 클라이언트의 rate_per_min이 너무 공격적입니다.
		// 재시도는 안전망이고, 진짜 수정은 그 채널의 한도를 낮추는 것입니다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("기업 위챗 요청 한도 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000은 webhook key가 잘못된 영구 실패입니다. 재시도해도 스스로 낫지 않습니다.
		return 0, Permanent(fmt.Errorf("기업 위챗 오류 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
