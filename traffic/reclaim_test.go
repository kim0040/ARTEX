package traffic

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bulkRecord 는 색인을 칸 안의 본문으로 채웁니다. index.sqlite 를 실제로
// 키우는 것들입니다. 이진 content type 은 전문 색인에서 빼서 테스트를
// 빠르게 합니다. FTS 쪽은 TestReclaimMergesFTSTombstones 가 덮습니다.
func bulkRecord(tr *Traffic, host string, n, size int) {
	body := []byte(strings.Repeat("A", size))
	for i := 0; i < n; i++ {
		tr.record(newFlow(host, "GET", fmt.Sprintf("/blob/%d", i), nil, body,
			withRespType("application/octet-stream")))
	}
}

func TestNewIndexEnablesIncrementalVacuum(t *testing.T) {
	tr, _ := openTraffic(t)
	if !tr.incrementalVacuum {
		t.Fatal("新建索引库未启用增量回收") // han-allow 업스트림 프롬프트·픽스처
	}
	var mode int
	if err := tr.DB().QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != autoVacuumIncremental {
		t.Fatalf("auto_vacuum=%d，应为 %d", mode, autoVacuumIncremental) // han-allow 업스트림 프롬프트·픽스처
	}
}

// TestDeleteReclaimsIndexSpace 는 회귀입니다. 트래픽을 지워도 index.sqlite 가
// 최고 수위에 영원히 남았습니다. SQLite 는 빈 페이지를 프리리스트에만
// 매달고, 파일 시스템으로 되돌리는 일이 없었기 때문입니다.
func TestDeleteReclaimsIndexSpace(t *testing.T) {
	tr, _ := openTraffic(t)
	const host = "bulk.example.com"
	// 30 × 200KB 는 maxInlineBody 아래입니다. 모든 본문이 blob 이 아니라
	// 데이터베이스 자체에 들어갑니다. 커짐이 안 보이던 곳이 거기입니다.
	bulkRecord(tr, host, 30, 200*1024)
	grown := tr.indexBytes()
	if grown < 5<<20 {
		t.Fatalf("索引只有 %d 字节，样本不足以验证回收", grown) // han-allow 업스트림 프롬프트·픽스처
	}

	if n, err := tr.DeleteHostsExact([]string{host}); err != nil || n != 30 {
		t.Fatalf("DeleteHostsExact=(%d,%v)，应为 (30,nil)", n, err) // han-allow 업스트림 프롬프트·픽스처
	}
	tr.reaping.Wait() // 회수는 백그라운드에서 덩어리로 진행됩니다

	after := tr.indexBytes()
	if after > grown/4 {
		t.Fatalf("删除后索引仍占 %d 字节（删除前 %d），空间没有还给文件系统", after, grown) // han-allow 업스트림 프롬프트·픽스처
	}
	// incremental_vacuum 이 파일 끝으로 못 옮긴 페이지 몇 장은 정상 잔여입니다.
	// 삭제가 비운 약 1500장은 사라져야 합니다.
	var free int
	if err := tr.DB().QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free > 64 {
		t.Fatalf("仍有 %d 个空闲页未回收", free) // han-allow 업스트림 프롬프트·픽스처
	}
}

// TestReclaimMergesFTSTombstones 는 누수의 나머지입니다. ex_fts 는
// contentless_delete 색인이라 DELETE 는 묘비 표시만 씁니다. 병합이 없으면
// 지울 때마다 색인이 커집니다. 트래픽을 지우면 오히려 커졌습니다.
func TestReclaimMergesFTSTombstones(t *testing.T) {
	tr, _ := openTraffic(t)
	if !tr.fts {
		t.Skip("驱动未启用 FTS5") // han-allow 업스트림 프롬프트·픽스처
	}
	// 묶음으로 지웁니다. 색인을 한 번에 비우지 않고 묘비 표시가
	// 여러 세그먼트에 퍼지게 하는 방식입니다.
	for round := 0; round < 4; round++ {
		host := fmt.Sprintf("fts%d.example.com", round)
		for i := 0; i < 20; i++ {
			tr.record(newFlow(host, "GET", fmt.Sprintf("/p/%d", i), nil,
				[]byte(strings.Repeat("secret token 中文正文 padding ", 200)))) // han-allow 업스트림 프롬프트·픽스처
		}
		if _, err := tr.DeleteHostsExact([]string{host}); err != nil {
			t.Fatal(err)
		}
		tr.reaping.Wait()
	}

	var exchanges, segments int
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&exchanges); err != nil {
		t.Fatal(err)
	}
	if err := tr.DB().QueryRow(`SELECT COUNT(*) FROM ex_fts_data`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if exchanges != 0 {
		t.Fatalf("还剩 %d 条流量", exchanges) // han-allow 업스트림 프롬프트·픽스처
	}
	// 완전히 병합된 빈 contentless 색인은 구조 행만 남깁니다.
	if segments > 8 {
		t.Fatalf("全文索引残留 %d 行段数据，tombstone 未被合并回收", segments) // han-allow 업스트림 프롬프트·픽스처
	}
}

// TestReclaimOnLegacyIndexIsHarmless 는 auto_vacuum=incremental 이 기본이
// 되기 전에 만든 설치입니다. 거기서는 incremental_vacuum 이 조용히 아무
// 일도 안 하므로, 회수는 그 사실을 알리고 끝나야 합니다. 돌거나 실패하면
// 안 됩니다. 그런 파일은 전체 압축만 바꿉니다.
func TestReclaimOnLegacyIndexIsHarmless(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "_index"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 테이블을 먼저 만듭니다. auto_vacuum 은 기본 0 입니다. Open 이
	// 예전에 남기던 모양 그대로입니다.
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	if tr.incrementalVacuum {
		t.Fatal("旧库不应报告已启用增量回收") // han-allow 업스트림 프롬프트·픽스처
	}

	const host = "legacy.example.com"
	bulkRecord(tr, host, 8, 200*1024)
	if n, err := tr.DeleteHostsExact([]string{host}); err != nil || n != 8 {
		t.Fatalf("DeleteHostsExact=(%d,%v)，应为 (8,nil)", n, err) // han-allow 업스트림 프롬프트·픽스처
	}
	tr.reaping.Wait() // 반드시 수렴해야 하며, 예산 안에 멈춰 있으면 안 됩니다

	// 프리리스트는 채워진 채입니다. 이미 있는 데이터베이스에 압축 진입점이
	// 필요한 이유가 이것입니다.
	var free int
	if err := tr.DB().QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Fatal("旧库居然回收了空闲页，说明测试没有真的构造出旧库") // han-allow 업스트림 프롬프트·픽스처
	}
}

// TestDeleteAllPurgesAndCompacts 는 화면의 전부 지우기입니다. 아무것도
// 남기면 안 됩니다. 색인이 더 이상 모르는 호스트 디렉터리도 포함합니다.
// 색인 공간도 되돌려야 합니다. 빈 색인이 전체 다시 쓰기가 싼 유일한 순간입니다.
func TestDeleteAllPurgesAndCompacts(t *testing.T) {
	tr, dir := openTraffic(t)
	bulkRecord(tr, "a.example.com", 10, 200*1024)
	bulkRecord(tr, "b.example.com", 10, 200*1024)
	// 글 본문은 전문 색인에 실제 내용이 있게 하고, 넘긴 본문은
	// 수거할 blob 이 있게 합니다.
	tr.record(newFlow("c.example.com", "GET", "/page", nil, []byte(strings.Repeat("secret-token ", 500))))
	tr.record(newFlow("c.example.com", "GET", "/big", nil,
		[]byte(strings.Repeat("B", maxInlineBody+1024)), withRespType("application/sql")))
	// 고아 레거시 디렉터리입니다. 가리키는 색인 행이 없어서,
	// 전부 지우기만 그것을 가져갑니다.
	orphan := filepath.Join(dir, "orphan.example.com")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	grown := tr.indexBytes()
	if grown < 5<<20 {
		t.Fatalf("索引只有 %d 字节，样本不足", grown) // han-allow 업스트림 프롬프트·픽스처
	}

	deleted, reclaimed, err := tr.DeleteAll()
	if err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if deleted != 22 {
		t.Fatalf("deleted=%d，应为 22", deleted) // han-allow 업스트림 프롬프트·픽스처
	}
	tr.reaping.Wait()

	if reclaimed < grown/2 {
		t.Fatalf("只回收了 %d 字节（删除前索引 %d）", reclaimed, grown) // han-allow 업스트림 프롬프트·픽스처
	}
	if after := tr.indexBytes(); after > grown/8 {
		t.Fatalf("清空后索引仍占 %d 字节（删除前 %d）", after, grown) // han-allow 업스트림 프롬프트·픽스처
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM exchanges`,
		`SELECT COUNT(*) FROM exchange_bodies`,
		`SELECT COUNT(*) FROM blob_refs`,
	} {
		var c int
		if err := tr.DB().QueryRow(q).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c != 0 {
			t.Fatalf("%s = %d，应为 0", q, c) // han-allow 업스트림 프롬프트·픽스처
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤立的历史 host 目录未被清理：%v", err) // han-allow 업스트림 프롬프트·픽스처
	}
	// 방금 다시 쓴 파일에도 기록이 계속 되어야 합니다.
	tr.record(newFlow("d.example.com", "GET", "/after", nil, []byte("清空后仍可录制"))) // han-allow 업스트림 프롬프트·픽스처
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("清空后 Count=(%d,%v)，应为 (1,nil)", n, err) // han-allow 업스트림 프롬프트·픽스처
	}
}

// TestDeleteAllConvertsLegacyIndex 는 비우기가 삭제만 하지 않고 압축하는
// 이유입니다. auto_vacuum 은 VACUUM 을 통하지 않고는 나중에 켤 수 없고,
// 빈 색인이 그 비용을 치르기 가장 쌉니다. 이후에는 보통 삭제가
// 스스로 공간을 되돌립니다.
func TestDeleteAllConvertsLegacyIndex(t *testing.T) {
	dir := t.TempDir()
	old := openLegacyIndex(t, dir)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	if tr.incrementalVacuum {
		t.Fatal("旧库不应报告已启用增量回收") // han-allow 업스트림 프롬프트·픽스처
	}

	bulkRecord(tr, "legacy.example.com", 10, 200*1024)
	if _, _, err := tr.DeleteAll(); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if !tr.incrementalVacuum {
		t.Fatal("清空后旧库未被转换为增量回收模式") // han-allow 업스트림 프롬프트·픽스처
	}

	// 바뀐 데이터베이스는 이제 보통 호스트 삭제에서 공간을 되돌립니다.
	bulkRecord(tr, "again.example.com", 10, 200*1024)
	grown := tr.indexBytes()
	if _, err := tr.DeleteHostsExact([]string{"again.example.com"}); err != nil {
		t.Fatal(err)
	}
	tr.reaping.Wait()
	if after := tr.indexBytes(); after > grown/4 {
		t.Fatalf("转换后普通删除仍未回收：%d 字节（删除前 %d）", after, grown) // han-allow 업스트림 프롬프트·픽스처
	}
}
