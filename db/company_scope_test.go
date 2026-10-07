package db

import (
	"fmt"
	"testing"
	"time"
)

// TestParseScopeLine은 DB 없이 분류와 가드를 덮는다.
func TestParseScopeLine(t *testing.T) {
	cases := []struct {
		in      string
		kind    string
		wantErr bool
	}{
		{"example.com", "domain", false},
		{"https://sub.example.com/path", "domain", false},
		{"1.2.3.4", "ip", false},
		{"10.0.0.0/8", "", true}, // 너무 넓은 IPv4(/16보다 큼)
		{"198.51.100.0/24", "cidr", false},
		{"co.uk", "", true}, // 맨 공개 접미사
		{"not a host", "", true},
		{"1.2.3.1-1.2.3.9", "", true}, // 범위는 CIDR이어야 한다
	}
	for _, c := range cases {
		r, err := ParseScopeLine(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseScopeLine(%q) want error, got %+v", c.in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseScopeLine(%q) unexpected error: %v", c.in, err)
			continue
		}
		if r.Kind != c.kind {
			t.Errorf("ParseScopeLine(%q) kind=%q want %q", c.in, r.Kind, c.kind)
		}
	}
}

func TestParseAutoScopeLine(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		kind       string
		normalized string
		wantErr    bool
	}{
		{name: "domain", input: "example.com", kind: "domain", normalized: "example.com"},
		{name: "url", input: "https://sub.example.com/path", kind: "domain", normalized: "sub.example.com"},
		{name: "url query", input: "https://example.com/path?source=x", kind: "domain", normalized: "example.com"},
		{name: "url credentials", input: "https://user:pass@example.com/path", kind: "domain", normalized: "example.com"},
		{name: "url ip", input: "http://203.0.113.10/path", kind: "ip", normalized: "203.0.113.10/32"},
		{name: "ipv4", input: "203.0.113.10", kind: "ip", normalized: "203.0.113.10/32"},
		{name: "ipv6", input: "2001:db8::10", kind: "ip", normalized: "2001:db8::10/128"},
		{name: "cidr", input: "198.51.100.0/24", kind: "cidr", normalized: "198.51.100.0/24"},
		{name: "icp latin", input: "京 ICP备 123号", kind: "icp", normalized: "京icp备123号"}, // han-allow 프로토콜 원문
		{name: "icp chinese", input: "沪网备案 9988", kind: "icp", normalized: "沪网备案9988"},  // han-allow 프로토콜 원문
		{name: "icp domain", input: "icp.example.com", kind: "domain", normalized: "icp.example.com"},
		{name: "icp url query", input: "https://example.com/path?icp=1", kind: "domain", normalized: "example.com"},
		// 등록번호에는 점(.)이 없다. 도메인/버전 번호가 섞인 설명 문장은 키워드로 두고, 그렇지 않으면 한 줄로 저장되어
		// 영원히 맞지 않는 죽은 ICP 규칙이 된다.
		{name: "icp with domain text", input: "备案 www.example.com", kind: "keyword", normalized: "备案 www.example.com"}, // han-allow 프로토콜 원문
		{name: "icp with version text", input: "某公司 ICP v1.0", kind: "keyword", normalized: "某公司 icp v1.0"},            // han-allow 프로토콜 원문
		{name: "icp fullwidth dot", input: "备案 例．com", kind: "keyword", normalized: "备案 例．com"},                        // han-allow 프로토콜 원문
		{name: "keyword", input: "  ACME   Security  ", kind: "keyword", normalized: "acme security"},
		{name: "colon keyword", input: "ACME: Cloud: Security", kind: "keyword", normalized: "acme: cloud: security"},
		{name: "empty", input: "  ", wantErr: true},
		{name: "invalid cidr", input: "10.0.0.0/not-a-prefix", wantErr: true},
		{name: "invalid ipv4", input: "999.0.0.1", wantErr: true},
		{name: "invalid ipv6", input: "2001:db8::zz", wantErr: true},
		{name: "invalid ipv6 cidr", input: "2001:db8::zz/64", wantErr: true},
		{name: "overbroad cidr", input: "10.0.0.0/8", wantErr: true},
		{name: "bare suffix", input: "co.uk", wantErr: true},
		{name: "empty domain label", input: "foo..example.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := ParseAutoScopeLine(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAutoScopeLine(%q) = %+v, want error", tc.input, rule)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAutoScopeLine(%q): %v", tc.input, err)
			}
			if rule.Kind != tc.kind {
				t.Fatalf("kind=%q want %q", rule.Kind, tc.kind)
			}
			got := rule.Value
			if rule.Kind == "domain" {
				got = rule.Domain
			} else if rule.Kind == "ip" || rule.Kind == "cidr" {
				got = rule.Net
			}
			if got != tc.normalized {
				t.Fatalf("normalized=%q want %q", got, tc.normalized)
			}
		})
	}
}

func TestExplicitCompanyAttributionSurvivesScopeRebuild(t *testing.T) {
	d, as, cs := testSetup(t)
	defer d.Close()

	stamp := time.Now().UnixNano()
	explicitCompany, _, err := cs.UpsertCompany(fmt.Sprintf("Explicit Attribution %d", stamp), "")
	if err != nil {
		t.Fatal(err)
	}
	autoCompany, _, err := cs.UpsertCompany(fmt.Sprintf("Automatic Attribution %d", stamp), "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCompany(d, explicitCompany)
	defer cleanupCompany(d, autoCompany)

	domain := fmt.Sprintf("explicit-%d.invalid", stamp)
	icp := fmt.Sprintf("ICP-%d", stamp)
	network := fmt.Sprintf("2001:db8:%x::/64", uint64(stamp)&0xffff)
	ip := fmt.Sprintf("2001:db8:%x::10", uint64(stamp)&0xffff)

	// company_id만 있고 출처 값이 없는 기존 행은 예전
	// 설치본이다. 스키마 기본값은 조심스럽게 명시 귀속으로 본다.
	var assetID int64
	if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,company_id)
		VALUES ('root_domain',$1,$1,$2) RETURNING id`, domain, explicitCompany).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, assetID)
	appID, err := as.UpsertApp(UpsertAppReq{
		Name: fmt.Sprintf("explicit-app-%d", stamp), ICP: icp, CompanyID: &explicitCompany,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, appID)
	autoAssetID, err := as.UpsertIP(UpsertIPReq{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, autoAssetID)

	rules := []ScopeInput{
		{Kind: "domain", Value: domain},
		{Kind: "icp", Value: icp},
		{Kind: "cidr", Value: network},
	}
	if added, _, invalid, errs := cs.AddScopeInputs(autoCompany, rules, "test"); added != len(rules) || invalid != 0 {
		t.Fatalf("AddScopeInputs: added=%d invalid=%d errors=%v", added, invalid, errs)
	}

	assertCompany := func(id, want int64, source string) {
		t.Helper()
		var got *int64
		var gotSource string
		if err := d.QueryRow(`SELECT company_id,company_source FROM assets WHERE id=$1`, id).Scan(&got, &gotSource); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != want || gotSource != source {
			t.Fatalf("asset %d company=%v source=%q, want %d/%q", id, got, gotSource, want, source)
		}
	}
	assertCompany(assetID, explicitCompany, "explicit")
	assertCompany(appID, explicitCompany, "explicit")
	assertCompany(autoAssetID, autoCompany, "scope")

	// 맞는 규칙을 모두 바꾸면 자동으로 소유한 행만 떨어진다.
	if _, invalid, errs := cs.UpdateScopeInputs(autoCompany, []ScopeInput{
		{Kind: "domain", Value: fmt.Sprintf("replacement-%d.invalid", stamp)},
	}, "test"); invalid != 0 || len(errs) != 0 {
		t.Fatalf("UpdateScopeInputs: invalid=%d errors=%v", invalid, errs)
	}
	assertCompany(assetID, explicitCompany, "explicit")
	assertCompany(appID, explicitCompany, "explicit")
	var autoCompanyID *int64
	var autoSource string
	if err := d.QueryRow(`SELECT company_id,company_source FROM assets WHERE id=$1`, autoAssetID).Scan(&autoCompanyID, &autoSource); err != nil {
		t.Fatal(err)
	}
	if autoCompanyID != nil || autoSource != "scope" {
		t.Fatalf("automatic asset was not detached: company=%v source=%q", autoCompanyID, autoSource)
	}

	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatal(err)
	}
	assertCompany(assetID, explicitCompany, "explicit")
	assertCompany(appID, explicitCompany, "explicit")

	// 명시로 고른 회사를 지우면 외래 키로 떨어지고, 그다음
	// 트랜잭션 재구성이 아직 유효한 범위로 자산을 받아들일 수 있다.
	if _, invalid, errs := cs.UpdateScopeInputs(autoCompany, []ScopeInput{
		{Kind: "domain", Value: domain},
		{Kind: "icp", Value: icp},
	}, "test"); invalid != 0 || len(errs) != 0 {
		t.Fatalf("restore fallback scope: invalid=%d errors=%v", invalid, errs)
	}
	if err := cs.DeleteCompany(explicitCompany); err != nil {
		t.Fatal(err)
	}
	assertCompany(assetID, autoCompany, "scope")
	assertCompany(appID, autoCompany, "scope")
}

func TestParseStructuredCompanyScope(t *testing.T) {
	icp, err := ParseScopeInput(ScopeInput{Kind: "ICP", Value: " 京ICP 备 123号-1\t"}) // han-allow 프로토콜 원문
	if err != nil {
		t.Fatalf("parse ICP: %v", err)
	}
	if icp.Kind != "icp" || icp.Value != "京icp备123号-1" { // han-allow 프로토콜 원문
		t.Fatalf("unexpected normalized ICP: %+v", icp)
	}
	keyword, err := ParseScopeInput(ScopeInput{Kind: "keyword", Value: "  ACME   Security  "})
	if err != nil {
		t.Fatalf("parse keyword: %v", err)
	}
	if keyword.Value != "acme security" {
		t.Fatalf("unexpected normalized keyword: %+v", keyword)
	}
	if _, err := ParseScopeInput(ScopeInput{Kind: "ip", Value: "example.com"}); err == nil {
		t.Fatal("typed IP accepted a domain")
	}
}

func TestCompanyICPAttribution(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	// 연결을 닫을 때는 반드시 t.Cleanup을 쓰고 **정리보다 먼저 등록**해야 한다. t.Cleanup은 후입선출이라,
	// 닫기를 먼저 등록하면 닫기가 마지막에 실행되어, 아래 데이터 정리가 아직 DB에 연결된다.
	// 원래는 `defer d.Close()`였다. defer는 함수가 반환될 때 먼저 실행되고, t.Cleanup은 그 다음에 실행된다
	// 그때서야 실행되어, 정리 문이 전부 **이미 닫힌 연결**에 떨어지고 오류는 `_, _ =`로 버려지며,
	// 자산과 회사가 데이터베이스에 영원히 남습니다. 잔여 자체는 바로 오류를 내지 않지만, 이 테스트 케이스는
	// `MAX(companies.id)+1`을 가짜 TaskID로 삼아 자산에 표시하고(아래 suffix 참고),
	// 이 숫자가 다른 케이스의 작업 id와 겹치면, 그 케이스가 「자산이 정확히 N개」라는 단언에 따라
	// 이유 없이 실패하고, 원인을 찾는 비용이 매우 큽니다.
	t.Cleanup(func() { d.Close() })

	var suffix int64
	if err := d.QueryRow(`SELECT COALESCE(MAX(id),0)+1 FROM companies`).Scan(&suffix); err != nil {
		t.Fatal(err)
	}
	cs := d.Companies()
	as := d.Assets()
	companyID, _, err := cs.UpsertCompany(fmt.Sprintf("ICP Scope Co %d", suffix), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 오류를 삼키지 않습니다. 정리 실패는 이후 케이스를 오염시키므로, 이번 실행에서 드러나야 합니다.
		if _, err := d.Exec(`DELETE FROM assets WHERE task_ids @> ARRAY[$1]::bigint[]`, suffix); err != nil {
			t.Errorf("테스트 자산 정리 실패: %v", err)
		}
		if _, err := d.Exec(`DELETE FROM companies WHERE id=$1`, companyID); err != nil {
			t.Errorf("테스트 회사 정리 실패: %v", err)
		}
	})

	// 키워드는 에이전트를 안내할 수 있지만, 이름으로 자산을 차지하면 안 된다.
	added, _, invalid, errs := cs.AddScopeInputs(companyID, []ScopeInput{
		{Kind: "icp", Value: "京 ICP备 998877号"}, // han-allow 프로토콜 원문
		{Kind: "keyword", Value: "ICP Scope"},
	}, "unit test")
	if added != 2 || invalid != 0 || len(errs) != 0 {
		t.Fatalf("add structured scope: added=%d invalid=%d errors=%v", added, invalid, errs)
	}

	rootID, err := as.UpsertRootDomain(UpsertRootDomainReq{
		Domain: fmt.Sprintf("icp-scope-%d.example", suffix), ICP: "京icp备998877号", TaskID: suffix, // han-allow 프로토콜 원문
	})
	if err != nil {
		t.Fatal(err)
	}
	appID, err := as.UpsertApp(UpsertAppReq{
		Name: fmt.Sprintf("ICP Scope Keyword Only %d", suffix), TaskID: suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
	icpAppID, err := as.UpsertApp(UpsertAppReq{
		Name: fmt.Sprintf("ICP Matched App %d", suffix), ICP: " 京 ICP备 998877号 ", TaskID: suffix, // han-allow 프로토콜 원문
	})
	if err != nil {
		t.Fatal(err)
	}

	assertCompany := func(assetID int64, want *int64) {
		t.Helper()
		var got *int64
		if err := d.QueryRow(`SELECT company_id FROM assets WHERE id=$1`, assetID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want == nil && got != nil {
			t.Fatalf("asset %d attributed by keyword: %d", assetID, *got)
		}
		if want != nil && (got == nil || *got != *want) {
			t.Fatalf("asset %d company=%v want %d", assetID, got, *want)
		}
	}
	assertCompany(rootID, &companyID)
	assertCompany(appID, nil)
	assertCompany(icpAppID, &companyID)
}

// TestCompanyScopeAttribution은 개발 PG에서 전체 고리를 돈다. 회사를
// 만들고(유일한 이름), 범위를 넣고, 넣을 때 자동 귀속과
// 기존 자산의 소급, CIDR과 도메인 접미사 일치를 확인한다. 그리고
// 범위 밖 자산은 귀속되지 않은 채 남는다.
func TestCompanyScopeAttribution(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	as := d.Assets()
	cs := d.Companies()

	var startMax int64
	if err := d.QueryRow(`SELECT COALESCE(MAX(id),0) FROM assets`).Scan(&startMax); err != nil {
		d.Close()
		t.Fatalf("startMax: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM assets WHERE id > $1`, startMax)
		d.Close()
	})

	uniq := startMax + 1
	root := fmt.Sprintf("scopetest%d.com", uniq)
	sub := "api." + root
	ipIn := "198.51.100.9"
	ipOut := "203.0.113.9"
	outDomain := fmt.Sprintf("other%d.net", uniq)

	cid, _, err := cs.UpsertCompany(fmt.Sprintf("ScopeCo %d", uniq), "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}

	// 범위보다 먼저 넣은 기존 자산은 소급돼야 한다.
	preID, err := as.UpsertSubdomain(UpsertSubdomainReq{Domain: sub})
	if err != nil {
		t.Fatalf("pre upsert: %v", err)
	}
	var preCompanyID *int64
	d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, preID).Scan(&preCompanyID)
	if preCompanyID != nil {
		t.Fatalf("pre-scope asset should be unattributed, got %v", *preCompanyID)
	}

	cs.AddScope(cid, []string{root, "198.51.100.0/24"}, "unit test")

	mustCid := func(id int64, want int64, label string) {
		var cID *int64
		d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, id).Scan(&cID)
		if cID == nil {
			t.Fatalf("%s company_id = nil, want %d", label, want)
		}
		if *cID != want {
			t.Fatalf("%s company_id = %d, want %d", label, *cID, want)
		}
	}
	mustNil := func(id int64, label string) {
		var cID *int64
		d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, id).Scan(&cID)
		if cID != nil {
			t.Fatalf("%s should be unattributed, got %d", label, *cID)
		}
	}

	// 소급이 기존 서브도메인을 귀속했다(도메인 접미사 일치).
	mustCid(preID, cid, "pre-existing subdomain (backfill)")

	// 넣을 때 귀속: CIDR 안의 ip, 또 다른 서브도메인.
	ipInID, err := as.UpsertIP(UpsertIPReq{IP: ipIn})
	if err != nil {
		t.Fatalf("UpsertIP in: %v", err)
	}
	mustCid(ipInID, cid, "in-CIDR ip (insert-time)")

	sub2ID, err := as.UpsertSubdomain(UpsertSubdomainReq{Domain: "www." + root})
	if err != nil {
		t.Fatalf("UpsertSubdomain: %v", err)
	}
	mustCid(sub2ID, cid, "new subdomain (insert-time)")

	// 범위 밖은 귀속되지 않은 채 남는다.
	outID, err := as.UpsertRootDomain(UpsertRootDomainReq{Domain: outDomain})
	if err != nil {
		t.Fatalf("UpsertRootDomain out: %v", err)
	}
	mustNil(outID, "out-of-scope domain")

	ipOutID, err := as.UpsertIP(UpsertIPReq{IP: ipOut})
	if err != nil {
		t.Fatalf("UpsertIP out: %v", err)
	}
	mustNil(ipOutID, "out-of-CIDR ip")
}
