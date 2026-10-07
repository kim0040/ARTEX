package notify

import (
	"fmt"
	"strings"
)

// 이 파일은 Markdown 계열 채널(딩톡, 기업 위챗)이 공유하는 메시지 렌더입니다.
// 페이슈는 카드 JSON, Telegram은 HTML, 메일은 HTML이라 각 어댑터에서 그립니다.

// maxAssetsShown 은 메시지에 나열하는 자산 상한입니다. 발견 하나가 자산을 수십 개
// 앵커할 수 있습니다. 전부 쓰면 메시지가 터지고 정보도 없습니다. 네 번째 이후 도메인은 IM에서 보지 않습니다.
const maxAssetsShown = 3

// maxSummaryRunes 는 요약을 몇 글자까지 줄일지입니다. IM 메시지는 「자세히 보러 가라」는
// 알림이지 보고서 본문이 아닙니다. 전문은 플랫폼에 있습니다.
const maxSummaryRunes = 120

// markdownReservedBytes 는 메시지 머리(요약 줄 + 심각도 분포 + 잘림 안내)와
// 꼬리(플랫폼 링크)를 위해 남겨 둡니다. 항목 단위로 포장할 때 예산에서 빼서
// 머리와 꼬리가 잘리지 않게 합니다. 머리가 잘리면 「어느 묶음인지, 몇 건이 빠졌는지」를 알 수 없습니다.
const markdownReservedBytes = 320

// markdownEscape 는 markdown 메타 문자를 이스케이프합니다.
//
// 해야 하는 이유: 발견 제목, 요약, 유형, 자산 표시 이름은 모두 **신뢰할 수 없는 출처**입니다.
// 제목과 요약은 모델 출력(모델이 읽은 것은 대상 응답)이고, 자산 url은 스캔으로 얻은
// 전체 URL(대상이 제어하는 쿼리 포함)입니다. 이스케이프하지 않으면 제목이
//
//	로그인 SQL 주입\n[긴급: 계정 확인](http://attacker.tld)
//
// 인 발견이 딩톡/페이슈에서 **클릭 가능한 외부 링크**로 그려집니다.
// `![](http://attacker.tld/beacon)` 는 클라이언트가 그릴 때 가져가므로,
// 「이 발견을 누가 봤는지」가 새고 읽는 사람의 IP가 나갑니다. 악의가 없어도
// 굵게나 인용 블록이 아래의 심각한 발견을 접힌 선 밖으로 밀어 낼 수 있습니다.
//
// 이스케이프 집합은 제목/링크/강조/목록/인용/취소선을 바꿔 구조나 클릭 요소가
// 생기는 문자입니다. `\` 를 가장 먼저 처리해야 뒤에 붙인 백슬래시가 다시 이스케이프되지 않습니다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText 는 신뢰할 수 없는 텍스트를 한 줄로 접고 이스케이프합니다. markdown 본문용입니다.
// 한 줄로 만드는 것은 이스케이프의 나머지 절반입니다. 줄바꿈만으로 새 목록 항목이나
// 인용 블록을 위조할 수 있고, 이스케이프 문자는 그걸 막지 못합니다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle 은 메시지 제목(IM 제목 줄/카드 제목)입니다. 내용은 **이스케이프하지 않은 원문**입니다.
//
// 여기서 일부러 이스케이프하지 않습니다. 이 제목은 네 곳의 렌더러가 공유합니다.
// markdown 본문, Telegram HTML, 페이슈 카드의 plain_text, 범용 Webhook JSON과 메일 제목.
// 문맥마다 이스케이프 규칙이 다릅니다(markdown 이스케이프를 HTML에 넣으면 백슬래시가
// 보이고, JSON에 넣으면 데이터가 오염됩니다). 이스케이프는 각 출력 쪽이 맡습니다.
// writeItem / feishuItemLines / telegramEscape를 보세요. 공유 함수에 markdown
// 이스케이프를 넣었다가 Telegram에 `\(1\)` 같은 백슬래시가 보인 적이 있습니다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("발견 요약 · 총 %d건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "발견 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody 는 본문을 그리고, 본문과 **실제로 쓴 항목 수**를 반환합니다.
//
// kept는 이번 전달에 실제로 도착한 항목 수입니다. 호출자는 앞 kept건만 전달됨으로
// 표시합니다. 채널 길이 상한 밖에 남은 항목은 다음 묶음으로 남겨야 하고, 같이
// 성공으로 표시하면 안 됩니다. 이것이 「조용한 유실」의 원인입니다. 메시지는 잘렸는데
// 전달 이력은 전부 성공이고, 뒤쪽을 보낸 적이 없다는 곳이 없습니다.
//
// maxBytes<=0이면 제한 없음.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단건은 길어도 보냅니다(최종 자르기가 받칩니다). 발견의 일부라도
		// 한 건도 안 보내는 것보다는 낫습니다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 모두 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro 는 요약 메시지의 시작입니다. 시간 창, 건수, 심각도 분포.
// 있으면 플랫폼에 들어가지 않아도 이 묶음을 당장 볼지 판단할 수 있습니다.
//
// items는 **실제로 넣은** 항목이고 total은 이 묶음이 가져야 할 전체입니다. 둘이 다르면
// 「다음 메시지에 몇 건이 남았는지」를 밝혀야 합니다. 그렇지 않으면 머리의 숫자를
// 전부로 읽고, 보내지 못한 항목은 화면에도 없습니다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분 동안 발견 %d건**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**새 발견 %d건**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "（이 메시지에는 앞 %d건만 보이고, 나머지 %d건은 다음 메시지에서 이어집니다）", len(items), extra)
	}
	// 심각도 분포를 줍니다. 심각한 항목이 있는지 한눈에 봅니다. **이 메시지에 실제로
	// 들어 있는** 항목만 셉니다. 「심각 3」과 아래에서 셀 수 있는 항목이 같아야 합니다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem 은 발견 한 건을 그립니다.
//
// prefix는 요약 목록의 번호입니다. single=true이면 전체(요약과 되돌아가는 링크)를 그리고,
// 요약 목록은 한 줄만 그립니다. 그렇지 않으면 50건 요약이 긴 문서가 됩니다.
//
// 외부에서 온 내용(제목/유형/자산/요약)은 모두 markdownText를 탑니다.
// 한 줄 + 이스케이프. 되돌아가는 링크는 관리자가 설정한 public_base_url로 만든 것이라
// 신뢰할 수 없는 내용이 아니고, 클릭 가능해야 하므로 그대로 출력합니다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 요약 모드: 한 줄. 자산과 요약을 줄여 뒤에 붙입니다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**：%s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**：%s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**：%s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**요약**：%s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[자세히 보기](%s)\n", it.DetailURL)
	}
}
