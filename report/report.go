// Package report 는 탐색 그래프의 확인된 발견으로 마크다운 보고서를 만듭니다.
//
// 초보: 사람이 읽는 보고서입니다. 본문은 작업마다 있는 탐색 그래프의 발견에서 오고,
// 자산 수는 작업들이 공유하는 PostgreSQL 자산 그래프의 집계입니다.
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Input 은 보고서를 만드는 데 필요한 묶음입니다. 발견은 탐색 그래프에서, 자산 수는 자산 그래프에서 옵니다.
type Input struct {
	Title       string
	Goal        string
	GeneratedAt time.Time
	AssetCounts map[string]int
	Findings    []*db.Node
}

type findingView struct {
	VulnClass string
	Name      string
	Severity  string
	Summary   string
	PoC       string
}

func parseFinding(n *db.Node) findingView {
	var p struct {
		VulnClass string `json:"vulnclass"`
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		Evidence  struct {
			PoC string `json:"poc"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return findingView{p.VulnClass, p.Name, p.Severity, p.Summary, p.Evidence.PoC}
}

var sevRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "": 4}

// 보고서가 사람이 읽는 자리에서만 쓰는 표시 이름이다. 저장소와 API는
// 아래의 영문 코드를 그대로 유지해야 하므로, 이 표시는 렌더링 경계에
// 둔다. 알려지지 않은 값은 labelOrRaw가 원문을 돌려준다.
var displayLabels = map[string]map[string]string{
	"severity": {
		"critical": "심각",
		"high":     "높음",
		"medium":   "중간",
		"low":      "낮음",
	},
	"finding_status": {
		"pending":        "처리 대기",
		"in_progress":    "처리 중",
		"confirmed":      "확인됨",
		"resolved":       "처리됨",
		"fixed":          "수정됨",
		"false_positive": "오탐",
		"ignored":        "무시",
		"duplicate":      "중복",
		"risk_accepted":  "위험 수용",
	},
	"asset_type": {
		"company":     "기업",
		"root_domain": "루트 도메인",
		"ip":          "IP",
		"subdomain":   "서브도메인",
		"app":         "앱",
		"service":     "서비스",
		"endpoint":    "엔드포인트",
		"none":        "연결되지 않음",
	},
}

// labelOrRaw는 표시할 때만 알려진 코드를 한국어로 바꾼다. 공백/대소문자
// 차이만 있는 알려진 코드는 같은 코드로 취급하지만, 모르는 값은 입력
// 문자열을 그대로 돌려줘서 새 코드나 운영 데이터가 사라지지 않게 한다.
func labelOrRaw(domain, raw, emptyLabel string) string {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		return emptyLabel
	}
	if label, ok := displayLabels[domain][key]; ok {
		return label
	}
	return raw
}

func severityLabel(raw string) string { return labelOrRaw("severity", raw, "정보") }

func findingStatusLabel(raw string) string {
	return labelOrRaw("finding_status", raw, "처리 대기")
}

func assetTypeLabel(raw string) string { return labelOrRaw("asset_type", raw, "미분류 자산") }

func severityRank(raw string) int {
	key := strings.ToLower(strings.TrimSpace(raw))
	if rank, ok := sevRank[key]; ok {
		return rank
	}
	return sevRank[""]
}

// Markdown 은 사람이 읽는 보고서 본문을 만듭니다.
func Markdown(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 침투 테스트 보고서 — %s\n\n", nz(in.Title, "이름 없는 작업"))
	fmt.Fprintf(&b, "- **작업 목표**: %s\n", nz(in.Goal, "(지정되지 않음)"))
	fmt.Fprintf(&b, "- **생성 시각**: %s\n\n", in.GeneratedAt.Format("2006-01-02 15:04:05"))

	// 요약
	fmt.Fprintf(&b, "## 요약\n\n")
	fmt.Fprintf(&b, "- 확인된 발견: **%d**개\n", len(in.Findings))
	fmt.Fprintf(&b, "- 자산: ")
	var types []string
	for t := range in.AssetCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for i, t := range types {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d", assetTypeLabel(t), in.AssetCounts[t])
	}
	b.WriteString("\n\n")

	// 발견
	fmt.Fprintf(&b, "## 발견\n\n")
	if len(in.Findings) == 0 {
		b.WriteString("_이번에는 확인된 취약점이 없습니다._\n\n")
	} else {
		fs := make([]findingView, 0, len(in.Findings))
		for _, n := range in.Findings {
			fs = append(fs, parseFinding(n))
		}
		sort.SliceStable(fs, func(i, j int) bool { return severityRank(fs[i].Severity) < severityRank(fs[j].Severity) })
		for i, f := range fs {
			fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, severityLabel(f.Severity), nz(f.Name, nz(f.VulnClass, "미분류")))
			fmt.Fprintf(&b, "%s\n\n", nz(f.Summary, ""))
			if f.PoC != "" {
				fmt.Fprintf(&b, "**PoC / 증거:**\n\n```\n%s\n```\n\n", f.PoC)
			}
		}
	}

	return b.String()
}

func nz(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
