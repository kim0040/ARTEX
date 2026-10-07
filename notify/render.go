package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes 는 s를 max 바이트 이하로 자르되, 결과는 올바른 UTF-8이고 문자를 반으로 자르지 않습니다.
//
// 문자 경계로 잘라야 하는 이유: 기업 위챗 로봇 markdown은 4096 **바이트** 상한입니다
// (문자 수가 아닙니다). 한글 한 글자는 3바이트입니다. 바이트로 자르면 글자가 반쪽이 되어
// 잘못된 UTF-8이 되고, 플랫폼은 통째로 거절하거나 깨진 네모로 보여 줍니다. 예산 위치에서
// 가장 가까운 rune 시작 바이트로 물러납니다(utf8.RuneStart가 연속 바이트 0b10xxxxxx를 구분).
//
// max<=0이면 제한 없음. 자른 뒤에는 말줄임표를 붙이되, max가 말줄임표보다 짧으면 붙이지 않습니다.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max가 말줄임표보다 짧음: 말줄임표를 포기하고 순수 절단. 결과가 max를 넘지 않게 합니다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine 은 여러 줄을 한 줄로 접습니다. 공백을 모두 접은 뒤 문자 수로 자릅니다.
// IM 제목용입니다. 요약에 줄바꿈이 있으면 표나 제목이 깨집니다.
// max<=0이면 길이 제한 없음.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes 는 s를 max **문자**(바이트가 아님) 이하로 자르고, 넘치면 말줄임표를 붙입니다.
// max<=0이면 제한 없음.
//
// TruncateBytes와 다른 점: 기업 위챗은 바이트, Telegram은 문자 수입니다.
// 기준을 바꾸면 오류는 나지 않고 메시지만 예상보다 훨씬 짧아집니다(한글 1자 = 3바이트,
// 4096바이트면 약 1365자). 두 함수를 모두 두고 채널에 맞게 고릅니다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML 은 HTML 조각을 문자 수로 자르되 태그를 반쪽으로 남기지 않습니다.
//
// HTML을 문자로만 자르면 `<a href="htt` 같은 잘린 태그가 생깁니다. 플랫폼 파서는
// 통째로 거절하거나 뒤 본문을 속성 값으로 삼킵니다. 먼저 문자로 자른 뒤,
// 꼬리에 닫히지 않은 `<`가 있으면 그 앞으로 물러납니다.
//
// 태그 짝을 맞추지 않습니다(</b>를 채우지 않음). Telegram HTML 파서는 닫히지 않은
// 태그를 스스로 닫습니다. 속성 따옴표, 주석, 자체 닫힘 태그까지 맞추려면
// 복잡도만 커지고 이득이 작습니다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 꼬리가 `<`로 시작하는 조각이면(마지막 `<` 뒤에 `>`가 없으면) `<` 앞으로 물러납니다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 꼬리가 잘린 HTML 엔티티면(`&amp;`가 `&amp`가 된 경우) 역시 물러납니다.
	// 엔티티만 아는 파서는 그 조각 때문에 **메시지 전체**를 거절할 수 있습니다.
	// 길이 상한을 넘는 요약은 흔해서, 이 때문에 알림 전체를 잃을 가치가 없습니다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount 는 예산 안에 **통째로** 들어가는 항목 수를 셉니다. 요약은 항목 단위로 포장합니다.
//
// 통째로 자르지 않고 다 그린 뒤 자르면 뒤 항목이 사라지는데, 전달 이력은 성공으로
// 남습니다. 메시지에도 전달 이력에도 흔적이 없어 발견이 그냥 없어집니다.
// 통째로 포장하면 못 넣은 항목은 다음 묶음으로 남고, 호출자가 받는 kept가
// 이 메시지에 실제로 실린 건수입니다.
//
// 인자: maxSize<=0이면 제한 없음. reserve는 머리/꼬리 여유. size는 재는 함수
// (기업 위챗/딩톡은 바이트, Telegram은 문자 수. 기준을 바꾸면 오류는 없고
// 한글 메시지만 상한보다 훨씬 짧아집니다). render는 idx번째를 실제 텍스트로
// 바꿉니다. 길이는 내용마다 달라 추정으로 대체할 수 없습니다.
//
// 항목이 있으면 최소 1을 반환합니다. 한 건이 극단적으로 길어도 그 한 건은 보내고
// 호출자의 최종 자르기가 받칩니다. 그렇지 않으면 긴 발견 하나가 묶음 전체를 영원히 막습니다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize 는 packItemCount의 두 계량입니다. 이름을 붙여 호출부에
// 맨 func(s string) int 클로저가 남지 않게 합니다. 어떤 기준인지 한눈에 보입니다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine 은 자산 목록을 한 줄로 그립니다. limit를 넘으면 나머지를 생략하고 총수를 적습니다.
// 발견 하나가 자산을 수십 개 앵커할 수 있어, 전부 나열하면 메시지가 터집니다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " 외 " + itoa(len(assets)) + "개"
}

// itoa 는 strconv.Itoa의 짧은 별칭입니다. 표시 문구를 붙일 때만 쓰고, strconv import를 줄입니다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
