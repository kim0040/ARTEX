package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 깨진 JSON, 빈 입력, 타입이 틀린 필드 — 전부 영 값 Filter, 즉 「필터 없음」으로
	// 물러나야 합니다. 이 불변은 「빠뜨리기보다 더 보내기」의 착지점입니다.
	// 여기서 오류나 반쪽 해석으로 바꾸면, 사용자가 한 글자를 잘못 넣었을 때 심각한 알림이 조용히 사라집니다.
	cases := []struct {
		name string
		raw  string
	}{
		{"빈 입력", ""},
		{"잘못된 JSON", `{not json`},
		{"잘린 JSON", `{"min_severity":`},
		{"타입 불일치", `{"min_severity": 123, "task_ids": "abc"}`},
		{"최상위가 배열", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("깨진 설정은 영 값 Filter로 물러나야 합니다. 결과 %+v", f)
			}
			// 영 값 Filter는 어떤 이벤트든 맞아야 합니다.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("영 값 Filter는 모든 이벤트에 맞아야 합니다")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 모르는 심각도의 순서는 0이라, 비어 있지 않은 문턱은 모두 막습니다(애매하면 보내지 않음).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 기대 %v 결과 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL 주입",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"빈 범위=제한 없음", Filter{}, true},
		{"작업 일치", Filter{TaskIDs: []int64{7}}, true},
		{"작업 불일치", Filter{TaskIDs: []int64{8}}, false},
		{"작업 복수 선택에 일치 포함", Filter{TaskIDs: []int64{8, 7}}, true},
		{"자산 교집합", Filter{AssetIDs: []int64{20, 99}}, true},
		{"자산 교집합 없음", Filter{AssetIDs: []int64{99}}, false},
		{"작업과 자산이 동시에 일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"작업은 일치하고 자산은 불일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("기대 %v 결과 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include가 비면 전부", Filter{}, "임의 유형", true},
		{"include 일치", Filter{VulnClassInclude: []string{"SQL"}}, "SQL 주입", true},
		{"include 불일치", Filter{VulnClassInclude: []string{"명령 실행"}}, "SQL 주입", false},
		{"include 여러 단어 중 하나 일치", Filter{VulnClassInclude: []string{"명령 실행", "SQL"}}, "SQL 주입", true},
		{"대소문자 무시", Filter{VulnClassInclude: []string{"sql"}}, "SQL 주입", true},
		{"exclude 일치면 제외", Filter{VulnClassExclude: []string{"정보 유출"}}, "정보 유출", false},
		{"exclude 불일치면 통과", Filter{VulnClassExclude: []string{"정보 유출"}}, "SQL 주입", true},
		// 제외가 포함보다 우선입니다. 둘 다 맞으면 탈락해야 합니다.
		{"제외가 포함보다 우선", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"주입"},
		}, "SQL 주입", false},
		// 공백만 있는 키워드는 무시합니다. 그렇지 않으면 「공백이 있는 모든 문자열」과 맞게 퇴화합니다.
		{"공백 키워드는 무시", Filter{VulnClassInclude: []string{"", "  "}}, "SQL 주입", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("기대 %v 결과 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본은 꺼짐. 대부분 「발견 알림」은 새 발견이지 상태 장부가 아닙니다.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("상태 변경 이벤트는 켜지 않으면 건너뛰어야 합니다")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change를 켜면 상태 변경 이벤트가 맞아야 합니다")
	}
	// 생성 이벤트는 on_status_change에 영향을 받지 않습니다.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("생성 이벤트는 on_status_change에 의존하면 안 됩니다")
	}
}
