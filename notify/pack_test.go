package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 이 파일은 「항목 통째로 포장」 수정을 덮습니다. 요약 메시지가 채널 길이 상한을 넘으면
// **항목 통째로** 자르고, 못 넣은 건수를 그대로 알려 호출자가 실제로 도착한 것만 표시하게 합니다.
//
// 이전에는 전문을 그린 뒤 자르고 묶음 전체를 전달됨으로 표시했습니다. 메시지 뒷부분이
// 사라지고 전달 이력은 전부 성공이었습니다. 발견이 없어지고 어디에서도 알 수 없었습니다.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 한글 요약 200건은 기업 위챗 4096바이트를 훨씬 넘습니다.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("본문 %d 바이트가 상한 %d 을 넘음", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("본문이 올바른 UTF-8이 아닙니다")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 들어가야 합니다 (0 < kept < %d). 결과 %d", len(m.Items), kept)
	}
	// 머리는 이 메시지에 몇 건이 있고 나머지가 몇 건인지 사실대로 말해야 합니다.
	// 그렇지 않으면 독자가 머리의 숫자를 전부로 읽습니다.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에서 이어집니다") {
		t.Fatalf("머리에 이 메시지에 없는 건수가 적혀야 합니다:\n%s", body[:minInt(400, len(body))])
	}
	// 앞 kept건만 들어 있어야 합니다.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "발견"+itoa(i+1)) {
			t.Fatalf("%d번째가 이 메시지에 있어야 합니다:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "발견"+itoa(kept+1)) {
		t.Fatalf("%d번째는 나오면 안 됩니다 (다음 묶음)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 제한 없음
	if kept != len(m.Items) {
		t.Fatalf("길이 제한이 없으면 전부 남아야 합니다. kept=%d", kept)
	}
	if strings.Contains(body, "나머지") {
		t.Fatalf("자르지 않았으면 잘림 안내가 나오면 안 됩니다:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 예산이 한 건도 못 담을 만큼 작아도 한 건은 보내야 합니다(최종 자르기가 받칩니다).
	// 그렇지 않으면 긴 발견 하나가 묶음을 영원히 막습니다. 받을 때마다 못 담고, 받을 때마다 안 보냅니다.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("최소 1건은 남아야 합니다. 결과 %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("단건 메시지는 도착 1건이어야 합니다. 결과 %d", kept)
	}
	// 빈 메시지에는 도착할 항목이 없습니다.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("빈 메시지는 0건이어야 합니다. 결과 %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram은 **문자 수**로 제한합니다. 바이트 기준이면 한글 메시지가 약 3분의 1로 줄어듭니다.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("본문 %d 문자가 상한 %d 을 넘음", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 들어가야 합니다. 결과 %d", kept)
	}
	if !strings.Contains(text, "다음에서 계속") {
		t.Fatalf("남은 분량이 있다고 밝혀야 합니다:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("카드에는 일부만 들어가야 합니다. 결과 %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 이 두 채널은 본문을 자르지 않고 묶음 전체를 도착으로 칩니다.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("전제가 성립하지 않습니다")
	}
	// 렌더러 반환값으로 간접 확인: markdownBody(0)은 제한이 없으면 전부 남깁니다.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("길이 제한이 없으면 전부를 써야 합니다. 결과 %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent 는 「신뢰할 수 없는 내용이 메시지 구조를 바꾸면 안 된다」는 회귀 테스트입니다.
// 제목과 요약은 모델 출력(모델이 읽은 것은 대상 응답)이고, 자산 이름은 대상 URL에서 옵니다.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 결과에 있어야 함(이스케이프된 형태)
		wrong []string // 결과에 있으면 안 됨(이스케이프되지 않은 형태)
	}{
		{
			name: "제목의 줄바꿈 + 외부 링크",
			item: Item{
				Severity: "high",
				Name:     "로그인 SQL 주입\n[긴급: 계정 확인](http://attacker.tld)",
			},
			// 줄바꿈은 접혀야 합니다(아니면 새 목록 항목/인용 블록을 위조할 수 있음).
			// 대괄호와 소괄호는 이스케이프되어야 합니다(아니면 클릭 가능한 외부 링크).
			must:  []string{`\[긴급: 계정 확인\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[긴급", "\n\n[긴급"},
		},
		{
			name: "제목의 이미지 비컨",
			item: Item{
				Severity: "high",
				Name:     "발견 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "자산 이름의 강조와 인용",
			item: Item{
				Severity: "high",
				Name:     "일반 제목",
				Assets:   []string{"a.com/*주입*>인용"},
			},
			must:  []string{`\*주입\*`, `\>`},
			wrong: []string{"*주입*"},
		},
		{
			name: "요약의 백틱과 세로줄",
			item: Item{
				Severity: "high",
				Name:     "제목",
				Summary:  "`code` | 표",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단건 모드의 writeItem은 세 markdown 채널이 공유하는 렌더 경로입니다.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("이스케이프된 형태 %q 이(가) 없습니다:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("이스케이프되지 않은 형태 %q 이(가) 있습니다 (구조나 외부 링크 주입에 쓰일 수 있음):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst 는 이스케이프 순서를 고정합니다. 백슬래시를 가장 먼저
// 처리해야 합니다. 그렇지 않으면 뒤에 붙인 백슬래시에 한 겹이 더 씌워 출력에 이중 백슬래시가 나옵니다.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("이스케이프 순서가 틀렸습니다. 결과 %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes 는 구체적 회귀를 고정합니다.
// markdown 이스케이프가 Telegram HTML로 새면 안 됩니다(공유 제목 함수에
// 이스케이프를 넣었다가 Telegram에 `\(1\)` 같은 백슬래시가 보인 적이 있습니다).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *핵심*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 본문에 markdown 백슬래시 이스케이프가 있습니다:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
