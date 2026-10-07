package db

import "testing"

// TestActivityPageSessions는 세션별 이력을 최신순으로 페이지하는 동작을 덮는다.
// SSE 보정용이다. 메인/플랜/워커 필터, before 커서 페이징에
// 빈틈과 겹침이 없는지, hasMore, 작업 단위 스냅샷 커서를 본다. 문서 §11.1과 같다.
func TestActivityPageSessions(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "페이지 단위 이력")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	intentA, err := es.AddIntent(map[string]any{"summary": "intent A"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	intentB, err := es.AddIntent(map[string]any{"summary": "intent B"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}

	// 에이전트를 섞어 넣어 세션 필터가 정말로 골라내야 하게 한다.
	// 메인 25, 플래너 25(Goal과 플래너는 worker=planner를 공유), workerA 30, workerB 5.
	appendN := func(n int, a Activity) {
		for range n {
			if _, err := es.AppendActivity(a); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 넣는 순서가 중요하다. 돌아가며 넣어 한 세션의 id가
	// 흩어지게 한다. 연속 구간이 아니라 WHERE 필터가 고른다는 것을 보인다.
	for range 25 {
		appendN(1, Activity{Worker: "mainagent", Kind: "text", Summary: "m"})
		appendN(1, Activity{Worker: "planner", Kind: "text", Summary: "p"})
		appendN(1, Activity{NodeID: &intentA, Worker: "work#1", Kind: "text", Summary: "a"})
	}
	appendN(5, Activity{NodeID: &intentA, Worker: "work#1", Kind: "text", Summary: "a2"}) // workerA → 합계 30
	appendN(5, Activity{NodeID: &intentB, Worker: "work#2", Kind: "text", Summary: "b"})

	// 스냅샷 커서 = 작업 전체의 최대 id.
	snap, err := es.ActivityMaxID()
	if err != nil {
		t.Fatal(err)
	}

	// 도우미: 세션 전체를 뒤로 페이지하며 빠짐없이 덮는지 확인한다.
	collect := func(f ActivitySessionFilter, pageSize int) []Activity {
		var all []Activity
		before := int64(0)
		seen := map[int64]bool{}
		for {
			items, hasMore, err := es.ActivityPage(f, before, pageSize)
			if err != nil {
				t.Fatal(err)
			}
			// 페이지 안은 id 오름차순
			for i := 1; i < len(items); i++ {
				if items[i-1].ID >= items[i].ID {
					t.Fatalf("page not ascending: %d >= %d", items[i-1].ID, items[i].ID)
				}
			}
			// 페이지끼리 겹치지 않는다
			for _, a := range items {
				if seen[a.ID] {
					t.Fatalf("duplicate id %d across pages", a.ID)
				}
				seen[a.ID] = true
			}
			all = append([]Activity{}, append(items, all...)...) // 더 오래된 페이지를 앞에 붙인다
			if !hasMore || len(items) == 0 {
				break
			}
			before = items[0].ID
		}
		return all
	}

	main := collect(ActivitySessionFilter{Worker: "mainagent"}, 10)
	if len(main) != 25 {
		t.Fatalf("main count = %d, want 25", len(main))
	}
	plan := collect(ActivitySessionFilter{Worker: "planner"}, 7)
	if len(plan) != 25 {
		t.Fatalf("plan count = %d, want 25", len(plan))
	}
	wa := collect(ActivitySessionFilter{NodeID: &intentA}, 8)
	if len(wa) != 30 {
		t.Fatalf("workerA count = %d, want 30", len(wa))
	}
	wb := collect(ActivitySessionFilter{NodeID: &intentB}, 8)
	if len(wb) != 5 {
		t.Fatalf("workerB count = %d, want 5", len(wb))
	}
	// 다시 모은 세션 전체는 id 오름차순
	for i := 1; i < len(wa); i++ {
		if wa[i-1].ID >= wa[i].ID {
			t.Fatalf("reconstructed session not ascending at %d", i)
		}
	}
	// 최신 페이지(before=0)는 그 세션의 가장 새 기록을 포함해야 한다.
	latest, hasMore, err := es.ActivityPage(ActivitySessionFilter{NodeID: &intentA}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore {
		t.Fatalf("workerA should have more than one page")
	}
	if latest[len(latest)-1].ID != wa[len(wa)-1].ID {
		t.Fatalf("latest page missing newest record")
	}
	// 스냅샷 커서는 작업 전체 최댓값이고, 어느 세션 최댓값보다도 크거나 같다.
	if snap < wa[len(wa)-1].ID {
		t.Fatalf("snapshot %d < workerA max %d", snap, wa[len(wa)-1].ID)
	}
}

// TestListByKindPage는 세션 목록이 예전 고정 300개를 넘게 읽게 하는
// 워커(의도) 페이지 목록을 덮는다(문서 §8 / §11.1 항목 10).
func TestListByKindPage(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	expID, err := d.CreateExploration("test", "의도 페이지")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	const total = 25
	for range total {
		if _, err := es.AddIntent(map[string]any{"summary": "i"}, 1, nil, "planner"); err != nil {
			t.Fatal(err)
		}
	}
	// 10개씩 뒤로 페이지한다. 10, 10, 5가 나오고 마지막 hasMore는 false.
	seen := map[int64]bool{}
	before := int64(0)
	pages := 0
	for {
		items, hasMore, err := es.ListByKindPage(KindIntent, before, 10)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, n := range items {
			if seen[n.ID] {
				t.Fatalf("dup intent %d across pages", n.ID)
			}
			seen[n.ID] = true
		}
		if len(items) == 0 || !hasMore {
			break
		}
		// 페이지 안은 최신순이라 가장 오래된(가장 작은 id) 것이 끝이다. 그 앞에서
		// 더 오래된 이력을 다음에 읽는다.
		before = items[len(items)-1].ID
	}
	if len(seen) != total {
		t.Fatalf("paged intents = %d, want %d", len(seen), total)
	}
	if pages < 3 {
		t.Fatalf("expected ≥3 pages for %d items at size 10, got %d", total, pages)
	}
}
