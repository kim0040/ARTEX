package traffic

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openLegacyIndex 는 회수 이전의 Open 이 만들던 색인을 그대로 만듭니다.
// 맨 경로 DSN, 풀을 통한 pragma, auto_vacuum 은 기본 0 입니다.
func openLegacyIndex(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "_index"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := old.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestUpgradeFromOldInstall 은 업그레이드 경로를 지킵니다. Open 은 이제
// file: URI 로 데이터베이스를 부릅니다. 연결마다의 pragma 를 DSN 에 태우기
// 위해서입니다. 드라이버가 그것을 URI 로 보지 않으면 "file:/…" 라는
// 이름의 파일을 조용히 엽니다. 빈 색인이 되고, 기록된 교환이 사라진 것처럼
// 보입니다. 아래 단언이 그것이 안 일어난다는 증거입니다.
func TestUpgradeFromOldInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_index", "index.sqlite")
	old := openLegacyIndex(t, dir)
	if _, err := old.Exec(ftsSchema); err != nil {
		t.Fatal(err)
	}
	// 과거 트래픽 세 건. 그중 한 행은 legacy path<>'' 입니다
	for i, row := range [][]any{
		{"1700000000-0001", "old.example.com", ""},
		{"1700000000-0002", "old.example.com", ""},
		{"1700000000-0003", "legacy.example.com", "legacy.example.com/GET/x"},
	} {
		if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,'GET','/x','http://x/x',200,'text/html',0,9,?)`, row[0], 1700000000+i, row[1], row[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec( /* han-allow 업스트림 프롬프트·픽스처 */ `INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,'GET /x','','HTTP 200','老数据正文')`, row[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, i+1, "老数据正文 secret-token"); err != nil { // han-allow 업스트림 프롬프트·픽스처
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 새 버전이 이어받음
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("新版本无法打开旧库: %v", err) // han-allow 업스트림 프롬프트·픽스처
	}
	defer tr.Close()

	// 1. 같은 파일이어야 합니다. 새 빈 저장소를 몰래 열면 안 됩니다
	if st2, err := os.Stat(path); err != nil || st2.Size() == 0 {
		t.Fatalf("原索引文件异常: size=%v err=%v", st2, err) // han-allow 업스트림 프롬프트·픽스처
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "_index")); len(entries) > 3 {
		for _, e := range entries {
			t.Logf("_index 下: %s", e.Name()) // han-allow 업스트림 프롬프트·픽스처
		}
		t.Fatal("_index 下出现了预期外的文件，DSN 可能指向了别的库") // han-allow 업스트림 프롬프트·픽스처
	}
	t.Logf("旧库 %d 字节，新版本接管后仍是同一文件", stat.Size()) // han-allow 업스트림 프롬프트·픽스처

	// 2. 과거 데이터가 모두 보여야 합니다
	n, err := tr.Count()
	if err != nil || n != 3 {
		t.Fatalf("Count=(%d,%v)，应为 (3,nil) —— 历史流量丢失", n, err) // han-allow 업스트림 프롬프트·픽스처
	}
	// 3. 과거 전문 인덱스로 여전히 검색할 수 있어야 합니다
	if tr.fts {
		rows, err := tr.query("old.example.com", "", "secret-token", 0, 10)
		if err != nil {
			t.Fatalf("历史全文搜索失败: %v", err) // han-allow 업스트림 프롬프트·픽스처
		}
		if len(rows) != 2 {
			t.Fatalf("历史全文搜索命中 %d 条，应为 2", len(rows)) // han-allow 업스트림 프롬프트·픽스처
		}
	}
	// 4. 과거 본문을 여전히 읽을 수 있어야 합니다
	if _, resp, err := tr.Get("1700000000-0001"); err != nil {
		t.Fatalf("读取历史正文失败: %v", err) // han-allow 업스트림 프롬프트·픽스처
	} else if resp == "" {
		t.Fatal("历史响应为空") // han-allow 업스트림 프롬프트·픽스처
	}
	// 5. 옛 저장소를 이미 증분 회수가 켜진 것으로 잘못 보면 안 됩니다
	if tr.incrementalVacuum {
		t.Fatal("旧库被误判为已启用增量回收") // han-allow 업스트림 프롬프트·픽스처
	}
	// 6. 삭제는 여전히 동작하고, 회수 절차가 옛 저장소에서 수렴해야 합니다
	deleted, err := tr.DeleteHostsExact([]string{"old.example.com"})
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteHostsExact=(%d,%v)，应为 (2,nil)", deleted, err) // han-allow 업스트림 프롬프트·픽스처
	}
	tr.reaping.Wait()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("删除后 Count=(%d,%v)，应为 (1,nil)", n, err) // han-allow 업스트림 프롬프트·픽스처
	}
	// 7. legacy path<>'' 행이 엮이면 안 됩니다
	var legacyPath string
	if err := tr.DB().QueryRow(`SELECT path FROM exchanges`).Scan(&legacyPath); err != nil {
		t.Fatal(err)
	}
	if legacyPath == "" {
		t.Fatal("legacy 行的 path 被清空了") // han-allow 업스트림 프롬프트·픽스처
	}
}

// TestDowngradeToOldBinary 는 되돌리기입니다. auto_vacuum=incremental 로
// 만든 데이터베이스는 그것을 모르는 빌드에서도 읽고 쓸 수 있어야 합니다.
// auto_vacuum 은 SQLite 가 빈 페이지를 어디서 추적하는지만 바꾸므로,
// 옛 바이너리는 그냥 그것들을 되돌리지 않는 상태로 돌아갑니다.
func TestDowngradeToOldBinary(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.incrementalVacuum {
		t.Fatal("新库应启用增量回收") // han-allow 업스트림 프롬프트·픽스처
	}
	bulkRecord(tr, "keep.example.com", 5, 100*1024)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	old := openLegacyIndex(t, dir) // 옛 버전 바이너리가 이어받음
	defer old.Close()
	var n int
	if err := old.QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("旧版本读到 (%d,%v)，应为 (5,nil)", n, err) // han-allow 업스트림 프롬프트·픽스처
	}
	if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('x',1,'new.example.com','GET','/x','http://x/x',200,'',0,0,'')`); err != nil {
		t.Fatalf("旧版本写入失败: %v", err) // han-allow 업스트림 프롬프트·픽스처
	}
	if _, err := old.Exec(`DELETE FROM exchanges WHERE host='keep.example.com'`); err != nil {
		t.Fatalf("旧版本删除失败: %v", err) // han-allow 업스트림 프롬프트·픽스처
	}
}
