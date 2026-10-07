package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 이 패키지에서 가장 중요한 불변입니다. 기업 위챗은 **바이트**로 길이를 제한하고
	// 한글은 3바이트/글자입니다. 바이트로 잘라 버리면 글자가 반쪽이 되어 잘못된 UTF-8이 되고
	// 플랫폼이 거절합니다. 길이가 서로소에 가까운 한·영 혼합 입력으로 가능한 절단점을 모두 칩니다.
	inputs := []string{
		"한글 테스트 내용",
		"혼합 mixed 내용 content",
		"a한b글c테d스e",
		"🔴🟠🟡🔵", // 4바이트 이모지. 잘못 자르면 더 눈에 띕니다
		strings.Repeat("발견", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("입력 %q max=%d: 잘못된 UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("입력 %q max=%d: 결과 %d 바이트가 상한을 넘음", in, max, len(got))
			}
			// 잘리지 않았으면 내용을 바꾸면 안 됩니다.
			if len(in) <= max && got != in {
				t.Fatalf("입력 %q max=%d: 상한을 안 넘었는데 내용이 바뀜 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 은 제한 없음이어야 합니다")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 은 제한 없음이어야 합니다")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max가 말줄임표보다 짧으면, 말줄임표를 붙여 오히려 상한을 넘기면 안 됩니다.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 일 때 결과 %q 길이 %d 가 상한을 넘음", got, len(got))
	}
	// 보통은 말줄임표가 붙어야 합니다.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("말줄임표를 기대했는데 결과 %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes와의 기준 차이는 유지해야 합니다. Telegram은 문자로 제한하고,
	// 바이트 기준이면 한글 메시지가 약 3분의 1로 잘립니다.
	s := "가나다라마바사아자차"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("문자 5개를 기대했는데 %d개 (%q)", n, got)
	}
	// 같은 문자열을 바이트 기준으로 자르면 문자 수가 분명히 더 적어야 합니다.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("바이트 기준은 문자 기준과 같은 문자 수를 내면 안 됩니다")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("첫줄\n\n둘째줄\t탭   여러공백", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("모든 공백을 접어야 합니다. 결과 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("연속 공백을 남기면 안 됩니다. 결과 %q", got)
	}
	// 자른 뒤에도 읽을 수 있고 올바른 문자열이어야 합니다.
	got = OneLine("가나다라마바사아자차", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("문자 4개를 기대했는데 %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML을 그대로 자르면 `<a href="htt` 같은 조각이 생기고, 플랫폼이 메시지 전체를 거절합니다.
	s := `<b>제목</b>본문본문본문<a href="https://example.com/very/long/path">자세히 보기</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 결과 %d 문자가 상한을 넘음", max, n)
		}
		// 꼬리에 닫히지 않은 `<`가 있으면 안 됩니다(마지막 구간에 `<`는 있고 `>`는 없음).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 꼬리 태그가 잘림 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("자산이 없으면 빈 문자열이어야 합니다. 결과 %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("상한을 안 넘으면 전부 나열해야 합니다. 결과 %q", got)
	}
	// 상한을 넘으면 총수를 적어야 합니다. 그렇지 않으면 빠뜨린 자산이 몇 개인지 모릅니다.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "외 5개") {
		t.Fatalf("총수 5를 적어야 합니다. 결과 %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("빈 심각도의 순서는 0이라 어떤 문턱에도 막혀야 합니다")
	}
	if !AtLeast("critical", "") {
		t.Fatal("빈 문턱은 통과시켜야 합니다")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("상태 매핑이 다릅니다. 결과 %q", got)
	}
	// 모르는 상태는 그대로 돌려주고, 없는 라벨을 지어내지 않습니다.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("모르는 상태는 그대로 보여야 합니다. 결과 %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity 는 감사에서 지적된 누락을 덮습니다. 자를 때
// 반쪽 태그만이 아니라 잘린 HTML 엔티티도 피해야 합니다.
//
// `&amp;` 가 `&amp` 로 잘리면, 엔티티만 아는 파서가 **메시지 전체**를 거절할 수 있습니다.
// 긴 요약 메시지는 흔해서 대가가 큽니다.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 꼬리에 「& 는 있는데 대응하는 ; 가 없는」 엔티티 조각을 남기면 안 됩니다.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 꼬리에 엔티티 조각 %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 기형 엔티티", max)
		}
	}
}
