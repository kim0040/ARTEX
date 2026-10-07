package db

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// testSetup은 DB를 열고 두 저장소를 돌려준다. PG가 없으면 건너뛴다.
func testSetup(t *testing.T) (*DB, *AssetStore, *CompanyStore) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	return d, d.Assets(), d.Companies()
}

// deleteAsset은 v2 자산 하나를 id로 지운다.
func deleteAsset(d *DB, id int64) {
	d.Exec(`DELETE FROM assets WHERE id = $1`, id)
}

// =====================================================================
// 루트 도메인
// =====================================================================

func TestUpsertRootDomain(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id1, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "roottest.io", ICP: "A12345", TaskID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id1)
	if id1 == 0 {
		t.Fatal("expected non-zero id")
	}

	// 다시 upsert한다. 같은 id(중복 제거)이고 ICP는 유지된다
	id2, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "roottest.io", TaskID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("dedup failed: %d != %d", id2, id1)
	}

	// task_ids에는 이제 1과 2가 둘 다 있어야 한다
	var taskIDs []byte
	d.QueryRow(`SELECT task_ids FROM assets WHERE id = $1`, id1).Scan(&taskIDs)

	// ICP는 아직 있어야 한다(COALESCE가 기존 값을 유지)
	var icp *string
	d.QueryRow(`SELECT icp FROM assets WHERE id = $1`, id1).Scan(&icp)
	if icp == nil || *icp != "A12345" {
		t.Errorf("ICP not preserved: %v", icp)
	}
}

func TestUpsertRootDomainEmpty(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	_, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: ""})
	if err == nil {
		t.Error("expected error for empty domain")
	}
}

// =====================================================================
// IP 자산
// =====================================================================

func TestUpsertIP(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id1, err := av2.UpsertIP(UpsertIPReq{
		IP:        "192.168.10.5",
		OpenPorts: []PortService{{Port: 22, Service: "ssh"}, {Port: 80, Service: "http"}},
		TaskID:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id1)

	// c_segment는 192.168.10.0/24여야 한다
	var cseg *string
	d.QueryRow(`SELECT c_segment::text FROM assets WHERE id = $1`, id1).Scan(&cseg)
	if cseg == nil || *cseg != "192.168.10.0/24" {
		t.Errorf("c_segment: want 192.168.10.0/24, got %v", cseg)
	}

	// UpsertIP로 포트를 하나 더 붙인다(합침)
	id2, err := av2.UpsertIP(UpsertIPReq{
		IP:        "192.168.10.5",
		OpenPorts: []PortService{{Port: 443, Service: "https"}},
		TaskID:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("dedup failed: %d != %d", id2, id1)
	}

	// 포트 3개가 모두 있는지 확인한다
	var cnt int
	d.QueryRow(`SELECT cardinality(open_ports) FROM assets WHERE id = $1`, id1).Scan(&cnt)
	if cnt != 3 {
		t.Errorf("open_ports merge: want 3 ports, got %d", cnt)
	}
}

func TestUpsertIPBoundDomains(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertIP(UpsertIPReq{
		IP:           "10.1.2.3",
		BoundDomains: []string{"a.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	// 도메인을 하나 더 붙인다
	id2, err := av2.UpsertIP(UpsertIPReq{
		IP:           "10.1.2.3",
		BoundDomains: []string{"b.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Errorf("dedup failed: %d != %d", id2, id)
	}

	var cnt int
	d.QueryRow(`SELECT array_length(bound_domains, 1) FROM assets WHERE id = $1`, id).Scan(&cnt)
	if cnt != 2 {
		t.Errorf("bound_domains merge: want 2, got %d", cnt)
	}
}

func TestAppendIPPort(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertIP(UpsertIPReq{IP: "10.9.8.7"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	if err := av2.AppendIPPort("10.9.8.7", 3306, "mysql"); err != nil {
		t.Fatal(err)
	}
	if err := av2.AppendIPPort("10.9.8.7", 22, "ssh"); err != nil {
		t.Fatal(err)
	}

	var cnt int
	d.QueryRow(`SELECT cardinality(open_ports) FROM assets WHERE id = $1`, id).Scan(&cnt)
	if cnt != 2 {
		t.Errorf("AppendIPPort: want 2 ports, got %d", cnt)
	}
}

// =====================================================================
// 서브도메인
// =====================================================================

func TestUpsertSubdomain(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertSubdomain(UpsertSubdomainReq{
		Domain:      "sub.subdtest.com",
		RecordType:  "A",
		RecordValue: []string{"1.2.3.4"},
		ICP:         "B99",
		TaskID:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE domain IN ('sub.subdtest.com', 'subdtest.com') OR ip = '1.2.3.4'`)

	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	// root_domain이 자동으로 채워졌는지 확인한다
	var rootDomain string
	d.QueryRow(`SELECT COALESCE(root_domain,'') FROM assets WHERE id = $1`, id).Scan(&rootDomain)
	if rootDomain != "subdtest.com" {
		t.Errorf("root_domain: want subdtest.com, got %q", rootDomain)
	}

	// 부수 효과: 루트 도메인 자산이 있어야 한다
	var rootCnt int
	d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type = 'root_domain' AND domain = 'subdtest.com'`).Scan(&rootCnt)
	if rootCnt != 1 {
		t.Error("side-effect root domain not created")
	}

	// 부수 효과: bound_domain이 있는 IP 자산
	var ipCnt int
	d.QueryRow(`SELECT COUNT(*) FROM assets WHERE type = 'ip' AND ip = '1.2.3.4'`).Scan(&ipCnt)
	if ipCnt != 1 {
		t.Error("side-effect IP asset not created")
	}
}

func TestUpsertSubdomainDedup(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id1, err := av2.UpsertSubdomain(UpsertSubdomainReq{Domain: "dedup.subdtest2.net", RecordType: "A", RecordValue: []string{"5.5.5.5"}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE domain IN ('dedup.subdtest2.net', 'subdtest2.net') OR ip = '5.5.5.5'`)

	id2, err := av2.UpsertSubdomain(UpsertSubdomainReq{Domain: "dedup.subdtest2.net", RecordType: "A", RecordValue: []string{"5.5.5.5"}})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("subdomain dedup failed: %d vs %d", id2, id1)
	}
}

// =====================================================================
// 앱
// =====================================================================

func TestUpsertAppByBundle(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id1, err := av2.UpsertApp(UpsertAppReq{Name: "MyApp", BundleID: "com.example.myapp", Category: "tools"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id1)

	// 번들로 중복을 제거한다
	id2, err := av2.UpsertApp(UpsertAppReq{Name: "MyApp Updated", BundleID: "com.example.myapp"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("app bundle dedup failed: %d vs %d", id2, id1)
	}
}

func TestUpsertAppByName(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id1, err := av2.UpsertApp(UpsertAppReq{Name: "UniqueAppNoBundle", Category: "utility"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id1)

	id2, err := av2.UpsertApp(UpsertAppReq{Name: "UniqueAppNoBundle"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("app name dedup failed: %d vs %d", id2, id1)
	}
}

// =====================================================================
// HTTP 서비스
// =====================================================================

func TestUpsertHTTPService(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	sc := 200
	cl := int64(1024)
	id1, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:           "https://www.httptest.example.com/",
		Technologies:  []string{"nginx", "vue"},
		StatusCode:    &sc,
		ContentLength: &cl,
		PageTitle:     "Test Site",
		TaskID:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE url = 'https://www.httptest.example.com' OR domain = 'www.httptest.example.com' OR domain = 'httptest.example.com'`)

	if id1 == 0 {
		t.Fatal("expected non-zero id")
	}

	// 기술이 저장됐는지 확인한다
	var techCnt int
	d.QueryRow(`SELECT array_length(technologies, 1) FROM assets WHERE id = $1`, id1).Scan(&techCnt)
	if techCnt != 2 {
		t.Errorf("technologies: want 2, got %d", techCnt)
	}

	// 기술을 더해 다시 upsert하면 합쳐져야 한다(뒤에 붙음)
	id2, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:          "https://www.httptest.example.com/",
		Technologies: []string{"react", "webpack"},
		TaskID:       2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("http service dedup failed: %d vs %d", id2, id1)
	}

	d.QueryRow(`SELECT array_length(technologies, 1) FROM assets WHERE id = $1`, id1).Scan(&techCnt)
	if techCnt != 4 {
		t.Errorf("technologies merge: want 4, got %d", techCnt)
	}
}

func TestUpsertHTTPServiceAuthAppend(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	auth1 := []map[string]any{{"type": "basic", "username": "admin", "password": "pass"}}
	id1, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:    "http://authtest.example.org/",
		Auth:   auth1,
		TaskID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE url = 'http://authtest.example.org' OR domain IN ('authtest.example.org', 'example.org')`)

	auth2 := []map[string]any{{"type": "bearer", "token": "tok123"}}
	id2, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{
		URL:    "http://authtest.example.org/",
		Auth:   auth2,
		TaskID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("dedup failed: %d vs %d", id2, id1)
	}

	// auth에는 이제 항목이 2개여야 한다
	var authCnt int
	d.QueryRow(`SELECT cardinality(auth) FROM assets WHERE id = $1`, id1).Scan(&authCnt)
	if authCnt != 2 {
		t.Errorf("auth append: want 2, got %d", authCnt)
	}
}

// =====================================================================
// 기타 서비스
// =====================================================================

func TestUpsertOtherService(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id1, err := av2.UpsertOtherService(UpsertOtherServiceReq{
		IP:          "172.16.0.1",
		Port:        22,
		ServiceName: "ssh",
		TaskID:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE (type = 'service' AND service_name = 'ssh' AND ip = '172.16.0.1') OR (type = 'ip' AND ip = '172.16.0.1')`)

	if id1 == 0 {
		t.Fatal("expected non-zero id")
	}

	// 중복 제거
	id2, err := av2.UpsertOtherService(UpsertOtherServiceReq{
		IP:          "172.16.0.1",
		Port:        22,
		ServiceName: "ssh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("other service dedup failed: %d vs %d", id2, id1)
	}

	// 부수 효과: IP 자산의 open_ports에 포트 22가 있어야 한다
	var cnt int
	d.QueryRow(`SELECT cardinality(open_ports) FROM assets WHERE type='ip' AND ip='172.16.0.1'`).Scan(&cnt)
	if cnt == 0 {
		t.Error("side-effect: IP open_ports not populated")
	}
}

func TestUpsertOtherServiceAuthAppend(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	auth1 := []map[string]any{{"username": "admin", "password": "secret"}}
	id1, err := av2.UpsertOtherService(UpsertOtherServiceReq{
		IP:          "10.5.5.5",
		Port:        3306,
		ServiceName: "mysql",
		Auth:        auth1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE (type = 'service' AND ip = '10.5.5.5') OR (type = 'ip' AND ip = '10.5.5.5')`)

	auth2 := []map[string]any{{"username": "root", "password": "root"}}
	id2, err := av2.UpsertOtherService(UpsertOtherServiceReq{
		IP:          "10.5.5.5",
		Port:        3306,
		ServiceName: "mysql",
		Auth:        auth2,
	})
	if err != nil || id2 != id1 {
		t.Fatalf("dedup or error: %v, ids %d vs %d", err, id2, id1)
	}

	var authCnt int
	d.QueryRow(`SELECT cardinality(auth) FROM assets WHERE id = $1`, id1).Scan(&authCnt)
	if authCnt != 2 {
		t.Errorf("auth append: want 2, got %d", authCnt)
	}
}

// =====================================================================
// 엔드포인트
// =====================================================================

func TestUpsertEndpoint(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	params1 := []map[string]any{{"location": "query", "name": "id", "value": "1"}}
	id1, err := av2.UpsertEndpoint(UpsertEndpointReq{
		URL:    "https://api.eptest.com/users?id=1",
		Method: "GET",
		Params: params1,
		TaskID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE url LIKE '%eptest.com%' OR domain IN ('api.eptest.com', 'eptest.com')`)

	if id1 == 0 {
		t.Fatal("expected non-zero id")
	}

	// 중복 제거(같은 URL + method = 같은 엔드포인트)
	id2, err := av2.UpsertEndpoint(UpsertEndpointReq{
		URL:    "https://api.eptest.com/users?id=1",
		Method: "GET",
		Params: []map[string]any{{"location": "query", "name": "filter", "value": "active"}},
		TaskID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("endpoint dedup failed: %d vs %d", id2, id1)
	}

	// params는 합쳐져야 한다(서로 다른 파라미터 2개)
	var paramCnt int
	d.QueryRow(`SELECT cardinality(params) FROM assets WHERE id = $1`, id1).Scan(&paramCnt)
	if paramCnt != 2 {
		t.Errorf("params merge: want 2, got %d", paramCnt)
	}
}

func TestUpsertEndpointRequiredFields(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	_, err := av2.UpsertEndpoint(UpsertEndpointReq{URL: "", Method: "GET"})
	if err == nil {
		t.Error("expected error for empty URL")
	}

	_, err = av2.UpsertEndpoint(UpsertEndpointReq{URL: "https://example.com/", Method: ""})
	if err == nil {
		t.Error("expected error for empty method")
	}
}

// =====================================================================
// 조회 도우미
// =====================================================================

func TestQueryByType(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "querytest.example.net"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	assets, err := av2.QueryByType("root_domain", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range assets {
		if a.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("QueryByType: inserted asset not found")
	}
}

// TestDeleteByTaskID: 고유 자산은 삭제되고, 다른 작업과 공유된 자산은 연결만 해제되며(유지), host 역조회가 정확하다.
func TestDeleteByTaskID(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	const taskA = int64(90001)
	const taskB = int64(90002)
	// solo: taskA에만 속함
	solo, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "solo-del.test", TaskID: taskA})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, solo)
	// shared: 먼저 taskA 다음 taskB → task_ids={A,B}
	shared, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "shared-del.test", TaskID: taskA})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, shared)
	if _, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "shared-del.test", TaskID: taskB}); err != nil {
		t.Fatal(err)
	}

	// host 역조회(자산 삭제 전): 도메인 두 개를 포함해야 함
	hosts, err := av2.HostsByTask(taskA)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hosts, "solo-del.test") || !slices.Contains(hosts, "shared-del.test") {
		t.Fatalf("HostsByTask에 host가 없음: %v", hosts)
	}

	n, err := av2.DeleteByTaskID(taskA)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteByTaskID: 고유 자산 1개를 삭제해야 하는데, 실제 삭제 %d", n)
	}
	// solo는 이미 삭제됨
	if a, _ := av2.GetByIDs([]int64{solo}); len(a) != 0 {
		t.Fatalf("solo 자산은 삭제되어야 합니다")
	}
	// shared는 유지되고, task_ids에는 taskB만 남음
	sa, _ := av2.GetByIDs([]int64{shared})
	if len(sa) != 1 {
		t.Fatalf("shared 자산은 유지되어야 합니다")
	}
	if slices.Contains(sa[0].TaskIDs, taskA) || !slices.Contains(sa[0].TaskIDs, taskB) {
		t.Fatalf("shared task_ids는 A 연결을 해제하고 B를 유지해야 하는데, 결과 %v", sa[0].TaskIDs)
	}
}

func TestQueryByTask(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	const taskID = int64(99999)
	id, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "taskquery.net", TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	assets, err := av2.QueryByTask(taskID, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range assets {
		if a.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("QueryByTask: inserted asset not found")
	}

	n, err := av2.CountByTask(taskID, "")
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("CountByTask: got %d, want >= 1", n)
	}
	if counts, err := av2.CountsByTypeForTask(taskID); err != nil {
		t.Fatal(err)
	} else if counts["root_domain"] < 1 {
		t.Errorf("CountsByTypeForTask: root_domain = %d, want >= 1", counts["root_domain"])
	}

	// 끝을 넘는 offset은 1페이지로 돌아가지 않고 빈 결과를 돌려줘야 한다
	rest, err := av2.QueryByTask(taskID, "", 10, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Errorf("QueryByTask offset=%d: got %d rows, want 0", n, len(rest))
	}
}

// 작업 자산 목록은 페이지 단위로 가져오며, 더 이상 고정 건수로 잘리지 않는다: 자산 60건을 25건/페이지로 끝까지 넘길 수 있어야 한다.
func TestQueryByTaskPaging(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	const taskID = int64(99998)
	const n = 60
	for i := 0; i < n; i++ {
		id, err := av2.UpsertRootDomain(UpsertRootDomainReq{
			Domain: fmt.Sprintf("paging-%02d.querybytask.test", i),
			TaskID: taskID,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer deleteAsset(d, id)
	}

	total, err := av2.CountByTask(taskID, "root_domain")
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Fatalf("CountByTask: got %d, want %d", total, n)
	}

	const size = 25
	seen := map[int64]bool{}
	for offset := 0; offset < total; offset += size {
		page, err := av2.QueryByTask(taskID, "root_domain", size, offset)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			if seen[a.ID] {
				t.Errorf("offset=%d: asset %d returned twice", offset, a.ID)
			}
			seen[a.ID] = true
		}
	}
	if len(seen) != n {
		t.Errorf("paged through %d assets, want %d", len(seen), n)
	}
}

func TestQueryByCompany(t *testing.T) {
	d, av2, cs := testSetup(t)
	defer d.Close()

	companyID, _, err := cs.UpsertCompany("QueryByCompanyCorp", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCompany(d, companyID)

	cs.AddScope(companyID, []string{"qbc-test.io"}, "test")

	id, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "qbc-test.io"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatal(err)
	}

	assets, err := av2.QueryByCompany(companyID, "root_domain", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range assets {
		if a.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("QueryByCompany: attributed asset not found")
	}

	n, err := av2.CountByCompany(companyID, "root_domain")
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("CountByCompany: got %d, want >= 1", n)
	}

	// 끝을 넘는 offset은 1페이지로 돌아가지 않고 빈 결과를 돌려줘야 한다
	rest, err := av2.QueryByCompany(companyID, "root_domain", 10, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Errorf("QueryByCompany offset=%d: got %d rows, want 0", n, len(rest))
	}
}

// 기업 자산 목록도 마찬가지로 페이지 단위로 가져오며, 고정 건수로 잘리지 않는다.
func TestQueryByCompanyPaging(t *testing.T) {
	d, av2, cs := testSetup(t)
	defer d.Close()

	companyID, _, err := cs.UpsertCompany("QueryByCompanyPagingCorp", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCompany(d, companyID)

	if added, _, _, errs := cs.AddScope(companyID, []string{"qbc-paging.io"}, "test"); added != 1 {
		t.Fatalf("AddScope: added=%d, errors=%v", added, errs)
	}

	// UpsertSubdomain은 루트 도메인 자산도 함께 만들므로, 같이 정리한다
	defer d.Exec(`DELETE FROM assets WHERE root_domain = 'qbc-paging.io'`)

	const n = 60
	for i := 0; i < n; i++ {
		if _, err := av2.UpsertSubdomain(UpsertSubdomainReq{
			Domain: fmt.Sprintf("paging-%02d.qbc-paging.io", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	total, err := av2.CountByCompany(companyID, "subdomain")
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Fatalf("CountByCompany: got %d, want %d", total, n)
	}

	const size = 25
	seen := map[int64]bool{}
	for offset := 0; offset < total; offset += size {
		page, err := av2.QueryByCompany(companyID, "subdomain", size, offset)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			if seen[a.ID] {
				t.Errorf("offset=%d: asset %d returned twice", offset, a.ID)
			}
			seen[a.ID] = true
		}
	}
	if len(seen) != n {
		t.Errorf("paged through %d assets, want %d", len(seen), n)
	}
}

func TestCountsByType(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	id, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "counttest.example.biz"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, id)

	counts, err := av2.CountsByType()
	if err != nil {
		t.Fatal(err)
	}
	if counts["root_domain"] == 0 {
		t.Error("CountsByType: root_domain count should be > 0")
	}
}

// =====================================================================
// 넣을 때의 회사 귀속
// =====================================================================

func TestCompanyAttributionAtInsertTime(t *testing.T) {
	d, av2, cs := testSetup(t)
	defer d.Close()

	companyID, _, err := cs.UpsertCompany("InsertAttrCorp", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCompany(d, companyID)

	cs.AddScope(companyID, []string{"insertattr.com"}, "test")

	// 이제 루트 도메인을 넣으면 자동으로 귀속돼야 한다
	assetID, err := av2.UpsertRootDomain(UpsertRootDomainReq{Domain: "insertattr.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, assetID)

	var cid *int64
	d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, assetID).Scan(&cid)
	if cid == nil || *cid != companyID {
		t.Errorf("attribution at insert time: want company %d, got %v", companyID, cid)
	}
}

// =====================================================================
// C 세그먼트 계산
// =====================================================================

func TestCalcCSegment(t *testing.T) {
	tests := []struct {
		ip   string
		want string
	}{
		{"192.168.1.5", "192.168.1.0/24"},
		{"10.0.0.255", "10.0.0.0/24"},
		{"", ""},
		{"notanip", ""},
	}
	for _, tc := range tests {
		got := calcCSegment(tc.ip)
		if got != tc.want {
			t.Errorf("calcCSegment(%q): want %q, got %q", tc.ip, tc.want, got)
		}
	}
}

// =====================================================================
// 문자열 배열 직렬화
// =====================================================================

func TestMarshalStringArray(t *testing.T) {
	got := marshalStringArray([]string{"a", "b", "c"})
	if got != `{"a","b","c"}` {
		t.Errorf("unexpected: %q", got)
	}
	got2 := marshalStringArray(nil)
	if got2 != "{}" {
		t.Errorf("empty: %q", got2)
	}
}

// =====================================================================
// URL 정규화
// =====================================================================

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"HTTPS://Example.COM/path", "https://example.com/path"},
		{"http://example.com/", "http://example.com"},
		{"http://example.com/page", "http://example.com/page"},
	}
	for _, tc := range tests {
		got := normalizeURL(tc.raw)
		if got != tc.want {
			t.Errorf("normalizeURL(%q): want %q, got %q", tc.raw, tc.want, got)
		}
	}
}

func TestAssetPaginationUsesStableIDTieBreaker(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	stamp := time.Now().UnixNano()
	taskID := stamp
	marker := fmt.Sprintf("stable-page-%d", stamp)
	sharedSeen := time.Date(2026, time.August, 21, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		var id int64
		domain := fmt.Sprintf("%s-%d.invalid", marker, i)
		if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
			VALUES ('root_domain',$1,$1,ARRAY[$2]::bigint[],$3) RETURNING id`,
			domain, taskID, sharedSeen).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	defer func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, ids) }()

	want := slices.Clone(ids)
	slices.Reverse(want)
	collect := func(query func(limit, offset int) ([]*Asset, error)) []int64 {
		t.Helper()
		var got []int64
		for offset := 0; offset < len(ids); offset += 2 {
			page, err := query(2, offset)
			if err != nil {
				t.Fatal(err)
			}
			for _, asset := range page {
				got = append(got, asset.ID)
			}
		}
		return got
	}
	if got := collect(func(limit, offset int) ([]*Asset, error) {
		return assets.QueryByTask(taskID, "root_domain", limit, offset)
	}); !slices.Equal(got, want) {
		t.Fatalf("task pagination order=%v want=%v", got, want)
	}
	if got := collect(func(limit, offset int) ([]*Asset, error) {
		return assets.QueryDSL(marker, "root_domain", 0, limit, offset)
	}); !slices.Equal(got, want) {
		t.Fatalf("DSL pagination order=%v want=%v", got, want)
	}
}

// TestAssetIPRejectsHostname은 호스트 이름이
// assets.ip에 들어가 회사 귀속이 깨진 뒤 넣은 쓰기 가드를 고정한다. ip를 저장하는 자산 종류는
// 주소가 아닌 값을 거부하고, 메시지는 고치는 법을 알려야 한다.
func TestAssetIPRejectsHostname(t *testing.T) {
	d, av2, _ := testSetup(t)
	defer d.Close()

	hostname := "d63cd476.cdn.ucloud.com.cn"
	cases := []struct {
		name string
		call func() (int64, error)
	}{
		{"ip", func() (int64, error) {
			return av2.UpsertIP(UpsertIPReq{IP: hostname})
		}},
		{"http service", func() (int64, error) {
			return av2.UpsertHTTPService(UpsertHTTPServiceReq{URL: "https://example.com/", IP: hostname})
		}},
		{"other service", func() (int64, error) {
			return av2.UpsertOtherService(UpsertOtherServiceReq{IP: hostname, Port: 22, ServiceName: "ssh"})
		}},
		{"endpoint", func() (int64, error) {
			return av2.UpsertEndpoint(UpsertEndpointReq{URL: "https://example.com/a", Method: "GET", IP: hostname})
		}},
	}
	for _, tc := range cases {
		id, err := tc.call()
		if !errors.Is(err, ErrAssetIPInvalid) {
			deleteAsset(d, id)
			t.Fatalf("%s: err=%v, want %v", tc.name, err, ErrAssetIPInvalid)
		}
		// 호출자는 무엇이 잘못됐는지만이 아니라 고치는 법을 알아야 한다.
		if !strings.Contains(err.Error(), hostname) || !strings.Contains(err.Error(), "type=subdomain") {
			t.Fatalf("%s: message is not actionable: %v", tc.name, err)
		}
	}

	// ip가 선택인 종류에서는 빈 ip가 여전히 허용되고, 진짜
	// 주소도 그대로 받는다.
	id, err := av2.UpsertHTTPService(UpsertHTTPServiceReq{URL: "https://ip-guard.example.com/"})
	if err != nil {
		t.Fatalf("empty ip rejected: %v", err)
	}
	deleteAsset(d, id)
	id, err = av2.UpsertIP(UpsertIPReq{IP: "203.0.113.7"})
	if err != nil {
		t.Fatalf("valid ip rejected: %v", err)
	}
	deleteAsset(d, id)
}

// TestQueryDSLInScopeMembership은 에이전트용 list_assets 동작을 고정한다.
// 작업이 선언한 범위에 속한 자산을 돌려준다(글자 그대로가 아니라 소속).
// root_domain 범위는 그 서브도메인/서비스/엔드포인트를 보여 준다. 그 행을
// 다른 작업이 만들었더라도 그렇다. IP 리터럴 호스트의 ip/cidr 범위도 지킨다
// (try_inet(domain) 갈래). 범위 밖 자산은 id로 물어도
// 빠진다. 직접 원본 작업의 범위는 포함한다.
func TestQueryDSLInScopeMembership(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	task, err := d.CreateTask("scope membership", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	src, err := d.CreateTask("scope source", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID); _ = d.DeleteTask(src.ID) })
	// task ← src는 직접 원본 관계다.
	if _, err := d.Exec(`INSERT INTO task_relations(task_id, source_task_id) VALUES ($1,$2)`, task.ID, src.ID); err != nil {
		t.Fatal(err)
	}

	stamp := time.Now().UnixNano()
	root := fmt.Sprintf("sc%d.invalid", stamp)     // 범위 안 루트 도메인(이 작업)
	srcRoot := fmt.Sprintf("src%d.invalid", stamp) // 원본 작업을 통해 범위 안
	out := fmt.Sprintf("out%d.invalid", stamp)     // 어느 범위에도 없음
	marker := fmt.Sprintf("mk%d", stamp)           // 모든 행에 있는 맨 글자 토큰
	foreignTask := stamp + 777                     // 관계없는 작업이 만든 자산

	// 범위: 이 작업이 root를, 원본 작업이 srcRoot를, 이 작업이 IP /24를 가진다.
	for _, sc := range []struct {
		tid  int64
		kind string
		dom  string
		net  string
	}{
		{task.ID, "root_domain", root, ""},
		{src.ID, "root_domain", srcRoot, ""},
		{task.ID, "ip", "", "198.51.100.0/24"},
	} {
		if err := assets.upsertTaskScope(TaskScope{TaskID: sc.tid, Kind: sc.kind, Domain: sc.dom, Net: sc.net}); err != nil {
			t.Fatalf("seed scope: %v", err)
		}
	}

	// root_domain/task_ids를 정확히 통제하려 행을 직접 넣는다. 범위 안
	// 행의 task_ids에는 foreignTask만 있다(이번 작업은 없다). 소속은
	// 만든 작업이 아니라 범위로 정한다는 것을 보이기 위해서다.
	type row struct {
		typ, domain, rootDom, url, method, ip, title string
	}
	rows := []row{
		{"subdomain", "api." + root, root, "", "", "", marker},                        // 이 작업의 루트 아래
		{"service", "www." + root, root, "https://www." + root + "/", "", "", marker}, // 이 작업의 루트 아래 서비스
		{"endpoint", "www." + root, root, "https://www." + root + "/a?" + marker + "=1", "GET", "", ""},
		{"subdomain", "dev." + srcRoot, srcRoot, "", "", "", marker},                                      // 원본 작업의 루트 아래
		{"endpoint", "198.51.100.9", "198.51.100.9", "http://198.51.100.9:8080/" + marker, "GET", "", ""}, // IP 리터럴 호스트, ip 열은 비어 있음
		{"subdomain", "x." + out, out, "", "", "", marker},                                                // 범위 밖
	}
	var inScopeIDs, outIDs []int64
	for _, r := range rows {
		var id int64
		if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,url,method,ip,page_title,task_ids,last_seen)
			VALUES ($1,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),ARRAY[$8]::bigint[],now())
			RETURNING id`,
			r.typ, r.domain, r.rootDom, r.url, r.method, r.ip, r.title, foreignTask).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", r.domain, err)
		}
		if r.rootDom == out {
			outIDs = append(outIDs, id)
		} else {
			inScopeIDs = append(inScopeIDs, id)
		}
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[]) OR id=ANY($2::bigint[])`, inScopeIDs, outIDs)
	})

	got, err := assets.QueryDSLInScope(marker, "", task.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := map[int64]bool{}
	for _, a := range got {
		gotIDs[a.ID] = true
	}
	for _, id := range inScopeIDs {
		if !gotIDs[id] {
			t.Fatalf("in-scope asset %d missing from QueryDSLInScope result %v", id, gotIDs)
		}
	}
	for _, id := range outIDs {
		if gotIDs[id] {
			t.Fatalf("out-of-scope asset %d leaked into scoped query", id)
		}
	}

	// GetByIDsInScope: 범위 안 id는 돌아오고, 범위 밖 id는 빠진다.
	mixed := append(append([]int64{}, inScopeIDs[0]), outIDs[0])
	byID, err := assets.GetByIDsInScope(task.ID, mixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(byID) != 1 || byID[0].ID != inScopeIDs[0] {
		t.Fatalf("GetByIDsInScope mixed ids → %+v, want only %d", byID, inScopeIDs[0])
	}

	// taskID<=0(작업 밖 맥락)이면 전역으로 돌아간다. 범위 밖 행도 닿는다.
	global, err := assets.QueryDSLInScope(marker, "", 0, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawOut bool
	for _, a := range global {
		if a.ID == outIDs[0] {
			sawOut = true
		}
	}
	if !sawOut {
		t.Fatalf("taskID<=0 should fall back to global and include out-of-scope asset %d", outIDs[0])
	}
}
