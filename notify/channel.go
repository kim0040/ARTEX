package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel 은 알림 채널 어댑터입니다. 구현은 **상태가 없어야** 합니다. 같은 인스턴스를
// 여러 채널 설정이 동시에 재사용하고, 자격 증명은 항상 cfg로만 받습니다.
type Channel interface {
	// Kind 는 채널 종류 식별자를 반환합니다. 레지스트리 키와 같아야 합니다.
	Kind() string
	// Validate 는 설정을 저장할 때 호출되어 필수 필드와 형식을 검사합니다. 반환 오류는
	// 설정하는 사람에게 그대로 보이므로, 「어느 필드가 없는지」를 말하고 막연한 「설정이 잘못됨」은 피합니다.
	Validate(cfg map[string]any) error
	// Send 는 메시지를 한 번 보내고, **실제로 도착한 항목 수**와 오류를 반환합니다.
	//
	// 건수를 반환하는 이유: 플랫폼마다 메시지 길이 상한이 있어 요약이 한 묶음을 다 담지 못하면 잘립니다.
	// 호출자가 묶음 전체를 전달됨으로 표시하면 잘린 항목은 사라집니다. 메시지에도 없고
	// 전달 이력은 성공입니다. 발견이 나간 적이 없다는 곳이 없습니다. kept를 반환하면
	// 호출자는 앞 kept건만 표시하고 나머지는 다음 묶음으로 남깁니다.
	//
	// 오류는 전달 실패입니다. *PermanentError 는 재시도하면 안 됩니다.
	// 실패하면 kept는 의미 없고 호출자는 무시해야 합니다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin 은 그 채널이 권하는 분당 전달 상한입니다. 채널을 새로 만들 때
	// 기본 한도로 씁니다. 0은 알려진 제한이 없다는 뜻입니다.
	DefaultRatePerMin() int
	// SecretKeys 는 이 채널 설정에서 자격 증명인 키 이름입니다. API가 돌려줄 때 값을 가리고,
	// 갱신 때 가린 값이 오면 저장소의 원값을 유지합니다. 어느 필드가 자격 증명인지는
	// 구현만 압니다(기업 위챗은 Webhook 주소 전체가 자격 증명이고, 딩톡은 그중 secret만).
	// 이 지식은 채널이 제공해야 하고 위층이 추측하면 안 됩니다.
	SecretKeys() []string
	// DestinationKeys 는 「메시지를 어디로 보내는지」를 정하는 키 이름입니다.
	//
	// SecretKeys와 같이 보안과 관련됩니다. 목적지와 자격 증명은 별도 필드입니다.
	// 「주소만 바꾸고 자격 증명은 그대로」를 허용하면, 채널 설정을 고칠 수 있는 사람이
	// 저장소의 진짜 자격 증명을 자기가 통제하는 서버로 보낼 수 있습니다. 가리기의 의미가 사라집니다.
	// PrepareConfigUpdate를 보세요.
	DestinationKeys() []string
}

// registry 는 채널 레지스트리입니다. init() 자가 등록 대신 명시적 리터럴을 씁니다.
// 「어떤 채널이 있는지」를 한곳에서 보고, 채널을 추가하면 컴파일 때 빠짐이 드러납니다.
// 런타임 부작용에 기대지 않습니다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get 은 종류로 채널 구현을 가져옵니다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind 는 kind가 지원하는 채널 종류인지 보고합니다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds 는 지원하는 채널 종류를 사전순으로 반환합니다(UI 드롭다운이 안정적으로 보이게).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError 는 재시도하면 안 되는 전달 실패입니다. 자격 증명 오류, 대상 거절, 잘못된 본문 등.
// 재시도는 일시 장애(네트워크 흔들림, 요청 한도, 상대 5xx)에만 의미가 있습니다.
// 영구 실패를 반복하면 성공하지 않고, 진짜 오류가 재시도 로그에 묻힙니다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent 는 err를 영구 실패로 표시합니다. err가 nil이면 nil을 반환해
// `return Permanent(someCheck())` 로 쓸 수 있습니다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent 는 err 사슬에 영구 실패 표시가 있는지 보고합니다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기 helper ----
//
// 채널 설정은 데이터베이스 JSONB 열이고, encoding/json으로 풀면 map[string]any입니다.
// 숫자는 전부 float64, 배열은 []any입니다. 아래 helper가 이 변환을 모으고,
// UI에서 비워 둔 칸의 타입 어긋남(포트를 문자열로 넣는 등)을 허용합니다.

// cfgString 은 문자열 설정 항목을 가져오고 앞뒤 공백을 자릅니다. 웹 폼 붙여넣기에 공백이 붙기 쉽습니다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt 는 정수 설정 항목을 가져옵니다. float64(JSON 기본)와 문자열을 모두 받습니다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool 은 불리언 설정 항목을 가져옵니다. 문자열 "true"/"1"도 받습니다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings 는 문자열 배열 설정 항목을 가져옵니다. 공백을 자르고 빈 문자열은 버립니다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 값이 하나일 때 폼이 문자열 하나만 보내도 받습니다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap 은 문자열 맵 설정 항목(사용자 HTTP 헤더 등)을 가져옵니다. 키와 값의 공백을 자르고 빈 키는 버립니다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
