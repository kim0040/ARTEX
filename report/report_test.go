package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestDisplayLabelsUseKoreanAndKeepUnknownValues(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "severity", got: severityLabel("HIGH"), want: "높음"},
		{name: "status", got: findingStatusLabel("pending"), want: "처리 대기"},
		{name: "asset", got: assetTypeLabel("subdomain"), want: "서브도메인"},
		{name: "unknown severity", got: severityLabel("vendor_new_level"), want: "vendor_new_level"},
		{name: "unknown status", got: findingStatusLabel("vendor_new_status"), want: "vendor_new_status"},
		{name: "unknown asset", got: assetTypeLabel("vendor_new_asset"), want: "vendor_new_asset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("표시값 = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestMarkdownLocalizesSeverityAndAssetTypeWithoutTouchingPoC(t *testing.T) {
	poc := "curl -H 'X-Test: pending HIGH subdomain' https://example.test"
	payload, err := json.Marshal(map[string]any{
		"name":     "테스트 발견",
		"severity": "HIGH",
		"summary":  "요약",
		"evidence": map[string]string{"poc": poc},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := Markdown(Input{
		Title:       "작업",
		Goal:        "목표",
		GeneratedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		AssetCounts: map[string]int{"subdomain": 2, "vendor_new_asset": 1},
		Findings:    []*db.Node{{Payload: payload}},
	})
	for _, want := range []string{"서브도메인 2", "vendor_new_asset 1", "[높음] 테스트 발견", poc} {
		if !strings.Contains(got, want) {
			t.Fatalf("보고서에 %q가 없습니다:\n%s", want, got)
		}
	}
}

func TestFindingExportsLocalizeStatusAndSeverityAndKeepUnknowns(t *testing.T) {
	known := &db.DBFinding{
		ID:       1,
		Name:     "알려진 발견",
		Severity: "high",
		Status:   "pending",
		Summary:  "요약",
		Evidence: "원문 증거 pending HIGH subdomain",
	}
	unknown := &db.DBFinding{
		ID:       2,
		Name:     "새 값 발견",
		Severity: "vendor_new_level",
		Status:   "vendor_new_status",
	}

	markdown := FindingsMarkdown([]*db.DBFinding{known, unknown}, time.Now())
	for _, want := range []string{"[높음] 알려진 발견", "**상태**: 처리 대기", "vendor_new_level", "vendor_new_status", known.Evidence} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Markdown에 %q가 없습니다:\n%s", want, markdown)
		}
	}

	single := SingleFindingMarkdown(known, time.Now())
	for _, want := range []string{"# [높음] 알려진 발견", "**심각도**: 높음", "**상태**: 처리 대기"} {
		if !strings.Contains(single, want) {
			t.Fatalf("단일 Markdown에 %q가 없습니다:\n%s", want, single)
		}
	}

	csv := string(FindingsCSV([]*db.DBFinding{known, unknown}))
	for _, want := range []string{"높음", "처리 대기", "vendor_new_level", "vendor_new_status"} {
		if !strings.Contains(csv, want) {
			t.Fatalf("CSV에 %q가 없습니다:\n%s", want, csv)
		}
	}
	if !strings.HasPrefix(FindingFilename(known), "높음_") {
		t.Fatalf("파일 이름에도 심각도 표시 이름을 써야 합니다: %q", FindingFilename(known))
	}
}
