package db

import (
	"testing"
)

// TestSkillUsageLedger는 행 몇 개를 쓰고 스킬 화면이 쓰는 집계 셋을 확인한다.
// 스킬별 합계(해결된 호출만), 실패 목록,
// 최근 호출 목록이다. 유일한 스킬 이름으로 행을 갈라
// 공유 개발 DB를 쓸 수 있게 하고, 테스트가 끝나면 치운다.
func TestSkillUsageLedger(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	const (
		skillA  = "zz-test-skill-a"
		missing = "zz-test-skill-missing"
	)
	cleanup := func() {
		_, _ = d.Exec(`DELETE FROM skill_usage WHERE skill IN ($1,$2)`, skillA, missing)
	}
	cleanup()
	defer cleanup()

	rows := []*SkillUsage{
		{Skill: skillA, AgentKey: "worker", TaskID: 991, ExplorationID: 5, IntentID: 7, ArgsLen: 12, Found: true},
		{Skill: skillA, AgentKey: "worker", TaskID: 991, ExplorationID: 5, ArgsLen: 0, Found: true},
		{Skill: skillA, AgentKey: "planner", TaskID: 992, ExplorationID: 6, Found: true},
		{Skill: skillA, AgentKey: "chatbot", SessionID: "conv-1", Found: true},
		{Skill: missing, AgentKey: "worker", TaskID: 991, Found: false},
		{Skill: missing, AgentKey: "worker", TaskID: 991, Found: false},
	}
	for _, r := range rows {
		if err := d.InsertSkillUsage(r); err != nil {
			t.Fatalf("insert %s: %v", r.Skill, err)
		}
	}

	stats, err := d.SkillStats()
	if err != nil {
		t.Fatal(err)
	}
	var got *SkillStat
	for i := range stats {
		if stats[i].Skill == skillA {
			got = &stats[i]
		}
		if stats[i].Skill == missing {
			t.Fatalf("misses must not appear in SkillStats: %+v", stats[i])
		}
	}
	if got == nil {
		t.Fatalf("skill %s absent from SkillStats", skillA)
	}
	if got.Calls != 4 {
		t.Errorf("calls: want 4, got %d", got.Calls)
	}
	// 991 + 992. 채팅 행의 task_id는 NULL이고 COUNT(DISTINCT)는 NULL을 건너뛴다.
	if got.Tasks != 2 {
		t.Errorf("tasks: want 2, got %d", got.Tasks)
	}
	if len(got.Agents) != 3 {
		t.Errorf("agents: want 3 distinct, got %v", got.Agents)
	}
	if got.LastUsed == nil {
		t.Error("last_used must be set")
	}

	miss, err := d.MissingSkillStats(10)
	if err != nil {
		t.Fatal(err)
	}
	var missCalls int
	for _, m := range miss {
		if m.Skill == missing {
			missCalls = m.Calls
		}
	}
	if missCalls != 2 {
		t.Errorf("missing calls: want 2, got %d", missCalls)
	}

	calls, err := d.RecentSkillCalls(skillA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("recent calls: want 4, got %d", len(calls))
	}
	// 최신순
	for i := 1; i < len(calls); i++ {
		if calls[i].TS.After(calls[i-1].TS) {
			t.Errorf("recent calls not newest-first at %d", i)
		}
	}

	byTask, err := d.SkillCallsByTask(991)
	if err != nil {
		t.Fatal(err)
	}
	var taskCalls int
	for _, s := range byTask {
		if s.Skill == skillA {
			taskCalls = s.Calls
		}
		if s.Skill == missing {
			t.Errorf("misses must not appear in SkillCallsByTask: %+v", s)
		}
	}
	if taskCalls != 2 {
		t.Errorf("task 991 calls: want 2, got %d", taskCalls)
	}
}
