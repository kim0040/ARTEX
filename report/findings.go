package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 발견 페이지의 「내보내기」가 쓰는 렌더입니다. findings 표의 행을 요약 Markdown,
// 건별 Markdown, 또는 CSV로 만듭니다. JSON은 server 층이 DTO로 바로 직렬화하므로
// 여기 두지 않습니다.
//
// 초보용: 사람이 다운로드하는 글입니다. 원본은 탐색 그래프의 발견 기록이고,
// 공격 절차를 새로 적지 않습니다.

// sortFindingsForExport는 심각도가 높은 순, 그다음 시간이 최신인 순으로 정렬합니다.
// 요약 보고서의 묶음과 같습니다.
func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj // sevRank가 작을수록 더 심각
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

// findingTitle은 사람이 읽는 제목입니다. 이름 → 분류 → 「미분류」 순입니다.
func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "미분류"))
}

// FindingsMarkdown은 findings를 한 편의 요약 보고서로 묶습니다. 요약과 심각도별
// 묶음이 있고, 각 항목에 분류/상태/소속 작업/증거/상세 보고서가 들어갑니다.
func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# 취약점 발견 요약 보고서\n\n")
	fmt.Fprintf(&b, "- **생성 시각**: %s\n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **발견 총수**: %d개\n\n", len(items))

	// 요약: 심각도별 개수.
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## 요약\n\n")
	b.WriteString("| 심각도 | 개수 |\n| --- | --- |\n")
	for _, s := range []struct{ key, label string }{
		{"critical", "심각"}, {"high", "높음"}, {"medium", "중간"}, {"low", "낮음"},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_일치하는 취약점이 없습니다._\n")
		return b.String()
	}

	b.WriteString("## 취약점 상세\n\n")
	for i, f := range items {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **분류**: %s\n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- **상태**: %s\n", nz(f.Status, "pending"))
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **소속 작업**: %s\n", desc)
		}
		fmt.Fprintf(&b, "- **발견 시각**: %s\n\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**증거:**\n\n```\n%s\n```\n\n", e)
		}
		if rep := strings.TrimSpace(f.Report); rep != "" {
			b.WriteString("**상세 보고서:**\n\n")
			b.WriteString(rep)
			b.WriteString("\n\n")
		}
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

// SingleFindingMarkdown은 취약점 하나를 독립 Markdown으로 만듭니다(「취약점 하나당 파일 하나」 묶음).
func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- **분류**: %s\n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- **심각도**: %s\n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- **상태**: %s\n", nz(f.Status, "pending"))
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **소속 작업**: %s\n", desc)
	}
	fmt.Fprintf(&b, "- **발견 시각**: %s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **생성 시각**: %s\n\n", generatedAt.Format("2006-01-02 15:04:05"))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## 개요\n\n%s\n\n", s)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## 증거\n\n```\n%s\n```\n\n", e)
	}
	if rep := strings.TrimSpace(f.Report); rep != "" {
		b.WriteString("## 상세 보고서\n\n")
		b.WriteString(rep)
		b.WriteString("\n")
	}
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

// FindingFilename은 「취약점 하나당 파일 하나」용으로 안전한 .md 이름을 만듭니다.
// 예: `critical_SQL주입_#123.md`. 경로 구분자와 제어 문자를 빼서 zip 안에
// 잘못된 경로가 들어가지 않게 합니다.
func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := findingTitle(f)
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	// 방어: 경로를 한 번 더 벗겨 zip slip을 막습니다.
	name = path.Base(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// FindingsCSV는 findings를 CSV로 만듭니다(UTF-8 BOM을 붙여 Excel이 한글을 읽게 합니다).
// 긴 report/evidence 전문은 넣지 않고 요약 필드만 둡니다. 전문은 Markdown/JSON으로 보냅니다.
func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "이름", "분류", "심각도", "상태", "소속 작업", "발견 시각", "개요", "트래픽 증거 수", "트래픽 증거 ID"})
	for _, f := range items {
		_ = w.Write([]string{
			fmt.Sprintf("%d", f.ID),
			findingTitle(f),
			f.VulnClass,
			nz(f.Severity, "info"),
			nz(f.Status, "pending"),
			f.TaskDescription,
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			strings.TrimSpace(f.Summary),
			fmt.Sprint(len(f.TrafficBindings)), findingTrafficIDs(f),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n## 연결된 트래픽 증거\n\n")
	fmt.Fprintf(&out, "증거 버전: %d. 연결 수: %d.\n\n", f.EvidenceVersion, len(f.TrafficBindings))
	if stale {
		out.WriteString("증거가 바뀌어 상세 보고서를 다시 써야 합니다.\n\n")
	}
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **증거 #%d · %s** — `%s %s`, 상태 코드 %d\n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [요청 원문](evidence/%d/%d/request.http) · [응답 원문](evidence/%d/%d/response.http)\n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
