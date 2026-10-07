package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func useDedicatedCoreLifecycleDB(t *testing.T) {
	t.Helper()
	base, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Skipf("core lifecycle test requires local PostgreSQL DSN")
	}
	dbName := fmt.Sprintf("artex_core_lifecycle_%d", time.Now().UnixNano())
	u.Path = "/" + dbName
	t.Setenv("ARTEX_PG_DSN", u.String())
	t.Cleanup(func() {
		admin := *u
		admin.Path = "/postgres"
		sqlDB, err := sql.Open("pgx", admin.String())
		if err != nil {
			return
		}
		defer sqlDB.Close()
		_, _ = sqlDB.Exec(`DROP DATABASE IF EXISTS "` + dbName + `"`)
	})
}

// TestCoreTaskLifecyclePG는 PG로 옮긴 핵심(작업/탐색)을
// 실제 HTTP mux로 봅니다. 만들기 → 목표 노드 심기 → 목록 → 삭제 연쇄.
func TestCoreTaskLifecyclePG(t *testing.T) {
	useDedicatedCoreLifecycleDB(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	td := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, td, td, td)
	var createdTaskID string
	t.Cleanup(func() {
		cancel()
		if createdTaskID != "" {
			s.engine.StopTask(createdTaskID)
		}
		s.archiveWG.Wait()
		if s.side != nil {
			<-s.side.done
		}
		_ = m.Close()
	})
	h := s.Handler()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	do := func(method, path string, body any) (int, map[string]any) {
		var r *http.Request
		if body != nil {
			b, _ := json.Marshal(body)
			r = httptest.NewRequest(method, path, bytes.NewReader(b))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// doRetry는 5xx면 다시 시도합니다(테스트 패키지가 병렬로 돌 때 DB가 잠깐 부딪힘).
	doRetry := func(method, path string, body any) (int, map[string]any) {
		var code int
		var out map[string]any
		for i := 0; i < 5; i++ {
			code, out = do(method, path, body)
			if code < 500 {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		return code, out
	}

	// 작업을 만들면 201이고, PG 작업을 돌려줍니다(문자열 id와 exploration_id).
	code, out := doRetry("POST", "/api/tasks", map[string]string{"description": "smoke", "goal": "SQLi/XSS 테스트"})
	if code != 201 {
		t.Fatalf("create task: %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	createdTaskID = id
	expID := int64(out["exploration_id"].(float64))
	if id == "" || expID == 0 {
		t.Fatalf("bad task payload: %v", out)
	}

	// 그 탐색은 목표 노드를 가집니다. 목표 심기는 비동기라 잠깐 기다립니다.
	var goals []*db.Node
	for i := 0; i < 30; i++ {
		goals, err = m.pg.Exploration(expID).ListByKind("goal", 10)
		if err != nil || len(goals) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) < 1 {
		t.Fatalf("expected goal nodes seeded, got %d", len(goals))
	}

	// 작업 목록에 나타납니다.
	code, out = doRetry("GET", "/api/tasks", nil)
	if code != 200 {
		t.Fatalf("list tasks: %d", code)
	}

	// 작업 상세 머리는 최상위 엔진 모드를 쓰고, 어떤 클라이언트는
	// 활성 작업 스냅샷을 읽습니다. 두 표현을 같이 유지합니다.
	code, out = doRetry("GET", "/api/stats?task="+id, nil)
	if code != 200 {
		t.Fatalf("task stats: %d (%v)", code, out)
	}
	activeTask, ok := out["active_task"].(map[string]any)
	if !ok || activeTask["engine_mode"] != out["engine_mode"] {
		t.Fatalf("engine mode mismatch: top=%v active_task=%v", out["engine_mode"], activeTask)
	}

	// 삭제하면 연쇄로 탐색 부분 그래프와 고른 관련 데이터가 지워집니다.
	taskDir := filepath.Join(m.dir, "tasks", id)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "artifact.txt"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	transcriptBase := "exp" + strconv.FormatInt(expID, 10) + "-main"
	transcriptPath := filepath.Join(m.dir, "transcripts", transcriptBase+".jsonl")
	sidechainPath := filepath.Join(m.dir, "transcripts", transcriptBase)
	if err := os.MkdirAll(filepath.Join(sidechainPath, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidechainPath, "subagents", "child.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nid, _ := strconv.ParseInt(id, 10, 64)
	findingID, err := m.pg.AddFinding(nid, 0, "__core_task_delete__", "", db.SeverityHigh, "summary", "evidence", "tester", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.InsertLLMRecord(&db.LLMRecord{TaskID: id, SessionID: "delete-test", Status: "ok", RequestBody: "secret"}); err != nil {
		t.Fatal(err)
	}
	// 정규가 아닌 경로도, 장벽·작업 폴더·메모리 레지스트리·DB 삭제에서는
	// 그 작업의 정규 키 하나로 모입니다.
	code, out = doRetry("DELETE", "/api/tasks/000"+id, map[string]bool{
		"delete_files":       true,
		"delete_findings":    true,
		"delete_llm_records": true,
	})
	if code != 200 {
		t.Fatalf("delete task: %d", code)
	}
	if deleted, _ := out["files_deleted"].(bool); !deleted {
		t.Fatalf("expected files_deleted response, got %v", out)
	}
	if deleted, _ := out["findings_deleted"].(float64); deleted != 1 {
		t.Fatalf("expected one deleted finding, got %v", out)
	}
	if deleted, _ := out["llm_records_deleted"].(float64); deleted != 1 {
		t.Fatalf("expected one deleted LLM record, got %v", out)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task directory should be deleted, stat err=%v", err)
	}
	for _, path := range []string{transcriptPath, sidechainPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("task transcript should be deleted (%s), stat err=%v", path, err)
		}
	}
	if got, _ := m.pg.GetTask(nid); got != nil {
		t.Fatalf("task should be deleted from PG")
	}
	if finding, err := m.pg.GetFinding(findingID); err != nil || finding != nil {
		t.Fatalf("finding should be deleted, got finding=%+v err=%v", finding, err)
	}
	var llmRecords int
	if err := m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE task_id=$1`, id).Scan(&llmRecords); err != nil || llmRecords != 0 {
		t.Fatalf("task LLM records should be deleted, count=%d err=%v", llmRecords, err)
	}
	var n int
	m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1`, expID).Scan(&n)
	if n != 0 {
		t.Fatalf("exploration nodes should be cascade-deleted, got %d", n)
	}
}
