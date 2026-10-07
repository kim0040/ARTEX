package server

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// 저장된 카탈로그 항목이 전역 런타임 변수와 이름이 겹치면(예: 예전에 심긴
// goals의 "Now") 중복이 나면 안 됩니다. 전역이 이기고 이름은
// 정확히 한 번만 나와, 화면이 React 키 중복을 보지 않습니다.
func TestWithGlobalVarsDedupesCollidingNames(t *testing.T) {
	t.Parallel()
	stored := []db.PromptVar{
		{Name: "EngagementDescription", Description: "task", Source: "exploration"},
		{Name: "Now", Description: "stale per-agent copy", Source: "runtime"},
	}
	out := withGlobalVars(stored)

	counts := map[string]int{}
	var now db.PromptVar
	for _, v := range out {
		counts[v.Name]++
		if v.Name == "Now" {
			now = v
		}
	}
	if counts["Now"] != 1 {
		t.Fatalf("Now appeared %d times, want 1: %+v", counts["Now"], out)
	}
	if counts["EngagementDescription"] != 1 {
		t.Fatalf("non-colliding stored var was dropped or duplicated: %+v", out)
	}
	// 살아남은 Now는 낡은 저장본이 아니라, 기준이 되는 전역 정의여야 합니다.
	// (저장된 낡은 정의가 아님).
	if now.Description == "stale per-agent copy" {
		t.Fatalf("stored var shadowed the global instead of the other way around: %+v", now)
	}
	// 전역은 모두 정확히 한 번씩 있습니다.
	for _, g := range globalPromptVars {
		if counts[g.Name] != 1 {
			t.Fatalf("global %q present %d times, want 1", g.Name, counts[g.Name])
		}
	}
}

// 흔한 경우(겹침 없음)는 저장된 변수를 모두 두고 전역을 전부 뒤에 붙입니다.
func TestWithGlobalVarsKeepsDistinctVars(t *testing.T) {
	t.Parallel()
	stored := []db.PromptVar{{Name: "Goal", Source: "exploration"}}
	out := withGlobalVars(stored)
	if len(out) != len(stored)+len(globalPromptVars) {
		t.Fatalf("len=%d, want %d: %+v", len(out), len(stored)+len(globalPromptVars), out)
	}
	if out[0].Name != "Goal" {
		t.Fatalf("stored var order not preserved: %+v", out)
	}
}
