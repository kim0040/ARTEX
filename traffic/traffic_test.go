package traffic

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

func TestRequestHeaderLinesIncludesHost(t *testing.T) {
	req := &mproxy.Request{
		URL:    &url.URL{Host: "target.example:8443"},
		Header: http.Header{"Accept": []string{"application/json"}},
	}
	got := requestHeaderLines(req)
	if !strings.Contains(got, "Host: target.example:8443\n") {
		t.Fatalf("request headers missing Host: %q", got)
	}
	if !strings.Contains(got, "Accept: application/json\n") {
		t.Fatalf("request headers missing regular header: %q", got)
	}
}

// TestDeleteHost 는 삭제 계약을 확인합니다. 부분 문자열이 들어간 호스트의
// 행과 파일 트리를 함께 지우고, 안 맞는 호스트는 그대로 두며, 개수가 맞습니다.
func TestDeleteHost(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	// 호스트 둘의 색인 행과 트리를 직접 심습니다(record() 는 살아 있는 Flow 가 필요).
	for i, h := range []string{"a.example.com", "b.example.com"} {
		id := fmt.Sprintf("1-%04d", i+1)
		exDir := filepath.Join(dir, h, "GET", id)
		if err := os.MkdirAll(exDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(exDir, "meta.json"), []byte(fmt.Sprintf(`{"id":%q,"host":%q}`, id, h)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, i+1, h, "GET", "/", "http://"+h+"/", 200, "text/html", 0, 0, h+"/GET/"+id); err != nil {
			t.Fatal(err)
		}
	}

	// 부분 문자열: "a.example" 은 a.example.com 만 맞고, b.example.com 은 남깁니다.
	n, err := tr.DeleteHost("a.example")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted=%d, want 1", n)
	}
	// 대상의 트리는 지우고, 다른 호스트의 트리는 그대로입니다.
	if _, err := os.Stat(filepath.Join(dir, "a.example.com")); !os.IsNotExist(err) {
		t.Fatalf("a.example.com tree still exists (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.example.com")); err != nil {
		t.Fatalf("b.example.com tree removed: %v", err)
	}
	// 색인은 다른 호스트의 행 하나만 남습니다.
	var c int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 1 {
		t.Fatalf("rows=%d, want 1", c)
	}
	// 아무것도 안 맞는 부분 문자열은 오류가 아니라 아무 일도 없습니다.
	n, err = tr.DeleteHost("nope.example")
	if err != nil || n != 0 {
		t.Fatalf("DeleteHost(missing)=%d, err=%v; want 0, nil", n, err)
	}
	// 더 넓은 부분 문자열은 남은 호스트도 쓸어 갑니다.
	if n, err = tr.DeleteHost("example.com"); err != nil || n != 1 {
		t.Fatalf("DeleteHost(example.com)=%d, err=%v; want 1, nil", n, err)
	}
	c = 0
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&c); err == nil && c != 0 {
		t.Fatalf("rows=%d, want 0 after full sweep", c)
	}
}

// TestHosts 는 대상 고르기의 계약입니다. 서로 다른 호스트와 개수이고,
// 최근 활동이 앞입니다.
func TestHosts(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	for i, row := range []struct {
		host string
		ts   int64
	}{{"old.example.com", 1}, {"new.example.com", 3}, {"old.example.com", 2}} {
		id := fmt.Sprintf("1-%04d", i+1)
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, row.ts, row.host, "GET", "/", "http://"+row.host+"/", 200, "text/html", 0, 0, row.host+"/GET/"+id); err != nil {
			t.Fatal(err)
		}
	}
	hosts, err := tr.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts=%d, want 2", len(hosts))
	}
	// 가장 최근 활동(ts=3)이 앞입니다
	if hosts[0].Host != "new.example.com" || hosts[0].Count != 1 {
		t.Fatalf("hosts[0]=%+v, want new.example.com/1", hosts[0])
	}
	if hosts[1].Host != "old.example.com" || hosts[1].Count != 2 {
		t.Fatalf("hosts[1]=%+v, want old.example.com/2", hosts[1])
	}
}

// TestDeleteHostsExact 는 묶음 삭제입니다. 호스트가 정확히 같을 때만 지웁니다.
// 이름에 다른 호스트가 부분 문자열로 들어 있어도 그 호스트는 그대로입니다.
// 묶음의 중복은 해가 없고, 호스트별 트리는 지웁니다.
func TestDeleteHostsExact(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	seed := func(id, h string) {
		exDir := filepath.Join(dir, h, "GET", id)
		if err := os.MkdirAll(exDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, 1, h, "GET", "/", "http://"+h+"/", 200, "text/html", 0, 0, h+"/GET/"+id); err != nil {
			t.Fatal(err)
		}
	}
	// "api.example.com" 은 "api.example.com.cn" 의 부분 문자열입니다.
	seed("1-0001", "api.example.com")
	seed("1-0002", "api.example.com.cn")
	seed("1-0003", "shop.example.com")

	// 묶음에 같은 항목이 두 번 있어도 두 번 지우거나 오류가 나면 안 됩니다.
	n, err := tr.DeleteHostsExact([]string{"api.example.com", "api.example.com", "shop.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted=%d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "api.example.com")); !os.IsNotExist(err) {
		t.Fatalf("api.example.com tree still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.example.com")); !os.IsNotExist(err) {
		t.Fatalf("shop.example.com tree still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "api.example.com.cn")); err != nil {
		t.Fatalf("api.example.com.cn removed by an exact delete that shouldn't match: %v", err)
	}
	var c int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 1 {
		t.Fatalf("rows=%d, want 1 (api.example.com.cn only)", c)
	}
}

func TestDeleteHostsExactReportsTreeRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	const host = "api.example.com"
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"1-0001", 1, host, "GET", "/", "http://"+host+"/", 200, "text/html", 0, 0, host+"/GET/1-0001"); err != nil {
		t.Fatal(err)
	}

	notDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.dir = notDir

	n, err := tr.DeleteHostsExact([]string{host})
	if err == nil {
		t.Fatal("DeleteHostsExact returned nil after traffic tree removal failed")
	}
	if n != 0 {
		t.Fatalf("deleted=%d, want 0 after atomic rollback", n)
	}
	var count int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges WHERE host=?`, host).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rolled-back index count=%d err=%v, want 1", count, err)
	}
}

func TestDeleteHostsExactRollsBackWholeIndexBatch(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	for i, host := range []string{"a.example.com", "b.example.com"} {
		id := fmt.Sprintf("1-%04d", i+1)
		if err := os.MkdirAll(filepath.Join(dir, host, "GET", id), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, 1, host, "GET", "/", "http://"+host+"/", 200, "text/html", 0, 0, host+"/GET/"+id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tr.DB().Exec(`CREATE TRIGGER fail_second_host BEFORE DELETE ON exchanges
WHEN OLD.host='b.example.com' BEGIN SELECT RAISE(ABORT, 'forced delete failure'); END`); err != nil {
		t.Fatal(err)
	}

	if n, err := tr.DeleteHostsExact([]string{"a.example.com", "b.example.com"}); err == nil || n != 0 {
		t.Fatalf("DeleteHostsExact failure=(%d,%v), want (0,error)", n, err)
	}
	var count int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rolled-back index count=%d err=%v, want 2", count, err)
	}
	for _, host := range []string{"a.example.com", "b.example.com"} {
		if _, err := os.Stat(filepath.Join(dir, host)); err != nil {
			t.Fatalf("tree %s changed despite index rollback: %v", host, err)
		}
	}
}

func TestStageDeleteHostsExactRollbackRestoresIndexAndTree(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	const host = "rollback.example.com"
	const id = "1-0001"
	tree := filepath.Join(dir, host, "GET", id)
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tree, "request.http")
	if err := os.WriteFile(marker, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, 1, host, "GET", "/", "http://"+host+"/", 200, "text/html", 0, 0, host+"/GET/"+id); err != nil {
		t.Fatal(err)
	}

	stage, err := tr.StageDeleteHostsExact([]string{host})
	if err != nil {
		t.Fatal(err)
	}
	if stage.Deleted() != 1 {
		t.Fatalf("staged deleted=%d, want 1", stage.Deleted())
	}
	if _, err := os.Stat(filepath.Join(dir, host)); !os.IsNotExist(err) {
		t.Fatalf("host tree was not staged: %v", err)
	}
	if err := stage.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "original" {
		t.Fatalf("restored tree content=%q err=%v", got, err)
	}
	var count int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges WHERE host=?`, host).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restored index count=%d err=%v, want 1", count, err)
	}
}

func TestStageDeleteHostsExactRollbackReportsRestoreFailure(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	const host = "conflict.example.com"
	hostDir := filepath.Join(dir, host)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stage, err := tr.StageDeleteHostsExact([]string{host})
	if err != nil {
		t.Fatal(err)
	}
	// 밖에서 목적지가 겹친 상황을 흉내 냅니다. Rollback 은 바깥 데이터가
	// 복구됐다고 하지 않고, 실패한 rename 을 드러내야 합니다.
	if err := os.WriteFile(hostDir, []byte("conflict"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stage.Rollback(); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("rollback err=%v, want restore failure", err)
	}
	if _, err := os.Stat(stage.stageDir); err != nil {
		t.Fatalf("staging was removed after failed restore: %v", err)
	}
}

// TestDeleteHostGCBlobs 는 blob 수거입니다. 호스트 트리를 지운 뒤,
// 남은 교환이 가리키지 않는 blob 은 지우고, 아직 가리키는 blob
// (공유된 것 포함)은 남습니다.
func TestDeleteHostGCBlobs(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	// 서로 다른 blob 둘과, 호스트 둘이 가리키는 공유 blob 하나입니다.
	blobA := filepath.Join(dir, "_blobs", "sha256", "aa", "aa", strings.Repeat("a", 64)+".bin")
	blobB := filepath.Join(dir, "_blobs", "sha256", "bb", "bb", strings.Repeat("b", 64)+".bin")
	blobC := filepath.Join(dir, "_blobs", "sha256", "cc", "cc", strings.Repeat("c", 64)+".bin")
	for _, b := range []string{blobA, blobB, blobC} {
		if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(id, h, ref string) {
		exDir := filepath.Join(dir, h, "GET", id)
		if err := os.MkdirAll(exDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "no blob"
		if ref != "" {
			body = "@blob sha256:" + ref + " (len=1)"
		}
		if err := os.WriteFile(filepath.Join(exDir, "request.http"), []byte("GET / HTTP/1.1\n\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(exDir, "response.http"), []byte("HTTP 200 OK\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, 1, h, "GET", "/", "http://"+h+"/", 200, "text/html", 0, 0, h+"/GET/"+id); err != nil {
			t.Fatal(err)
		}
	}
	ha := strings.Repeat("a", 64)
	hb := strings.Repeat("b", 64)
	hc := strings.Repeat("c", 64)
	seed("1-0001", "a.example.com", ha) // blobA 를 혼자 가리킴
	seed("1-0002", "b.example.com", hb) // blobB 를 혼자 가리킴
	seed("1-0003", "c.example.com", hc) // blobC 를 d 와 공유
	seed("1-0004", "d.example.com", hc)

	// a 를 지우면 blobA 는 고아가 되어 지워집니다. blobB/blobC 는 아직 참조되어 남습니다.
	if n, err := tr.DeleteHost("a.example"); err != nil || n != 1 {
		t.Fatalf("DeleteHost(a.example)=%d, err=%v; want 1, nil", n, err)
	}
	if _, err := os.Stat(blobA); !os.IsNotExist(err) {
		t.Fatalf("orphaned blobA still exists: %v", err)
	}
	if _, err := os.Stat(blobB); err != nil {
		t.Fatalf("referenced blobB removed: %v", err)
	}
	if _, err := os.Stat(blobC); err != nil {
		t.Fatalf("shared blobC removed while d still references it: %v", err)
	}

	// c 를 지웁니다(blobC 를 d 와 공유). blobC 는 남아야 합니다.
	if n, err := tr.DeleteHost("c.example"); err != nil || n != 1 {
		t.Fatalf("DeleteHost(c.example)=%d, err=%v; want 1, nil", n, err)
	}
	if _, err := os.Stat(blobC); err != nil {
		t.Fatalf("shared blobC removed after deleting one sharer: %v", err)
	}

	// d 를 지우면 마지막 참조가 사라져 blobC 를 수거합니다.
	if n, err := tr.DeleteHost("d.example"); err != nil || n != 1 {
		t.Fatalf("DeleteHost(d.example)=%d, err=%v; want 1, nil", n, err)
	}
	if _, err := os.Stat(blobC); !os.IsNotExist(err) {
		t.Fatalf("blobC still exists after last reference removed: %v", err)
	}
}
