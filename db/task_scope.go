package db

import (
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// TaskScope는 작업 시험 범위의 한 줄이다. 커버리지의 분모이자
// 작업마다의 인가 경계다. 행은 insertAssets에서 오거나(source='auto',
// 보수적, 명시적으로 넣은 자산마다 하나) add_task_scope 도구에서 온다
// (source='agent', 기업 / 도메인 / 네트워크 / ICP / 키워드 범위).
type TaskScope struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"`
	Kind      string `json:"kind"` // company|root_domain|subdomain|ip|cidr|icp|keyword (범위 종류)
	CompanyID *int64 `json:"company_id,omitempty"`
	// CompanyName은 kind=company일 때 채워, 호출자가 범위를 두 번 조회하지 않고 이름 붙이게 한다.
	// 기업 참조가 아니면 비어 있다.
	CompanyName string `json:"company_name,omitempty"`
	Domain      string `json:"domain,omitempty"`
	Net         string `json:"net,omitempty"`
	Value       string `json:"value,omitempty"`
	Source      string `json:"source"`
	Reason      string `json:"reason,omitempty"`
}

// stripHostPort는 host:port / ip:port / [ipv6]:port 끝의 :포트를 떼고
// 호스트만 돌려준다. 호스트만 있거나, IP만 있거나(v4·v6는 콜론 때문에
// 헷갈린다), host:port 꼴이 아니면 그대로 돌려준다. 범위는
// 호스트/네트워크 기준이라 "10.0.188.136:3000"이나
// "api.example.com:8080"의 포트는 키에 넣지 않고 버린다.
func stripHostPort(v string) string {
	v = strings.TrimSpace(v)
	if host, _, err := net.SplitHostPort(v); err == nil {
		return host
	}
	return v
}

// ipToHostCIDR는 IP 하나를 호스트 하나짜리 CIDR(/32 또는 /128)로 바꾼다. 잘못되면 "".
func ipToHostCIDR(ip string) string {
	ip = strings.TrimSpace(ip)
	p := net.ParseIP(ip)
	if p == nil {
		return ""
	}
	if p.To4() != nil {
		return ip + "/32"
	}
	return ip + "/128"
}

// upsertTaskScope는 범위 한 줄을 중복 없이 넣는다(uq_task_scope). 중복은
// 조용히 무시한다. taskID가 0 이하거나 kind가 비면 아무 일도 하지 않는다.
func (s *AssetStore) upsertTaskScopeResult(ts TaskScope) (bool, error) {
	if ts.TaskID <= 0 || ts.Kind == "" {
		return false, nil
	}
	var domainVal, netVal, companyVal, valueVal any
	if ts.Domain != "" {
		domainVal = ts.Domain
	}
	if ts.Net != "" {
		netVal = ts.Net
	}
	if ts.CompanyID != nil && *ts.CompanyID > 0 {
		companyVal = *ts.CompanyID
	}
	if ts.Value != "" {
		valueVal = ts.Value
	}
	src := ts.Source
	if src == "" {
		src = "auto"
	}
	query := `
INSERT INTO task_scope(task_id, kind, company_id, domain, net, value, source, reason)
VALUES ($1,$2,$3,$4,$5::cidr,$6,$7,NULLIF($8,''))
ON CONFLICT DO NOTHING`
	var (
		result sql.Result
		err    error
	)
	if s.tx != nil {
		result, err = s.tx.Exec(query, ts.TaskID, ts.Kind, companyVal, domainVal, netVal, valueVal, src, ts.Reason)
	} else {
		result, err = s.db.Exec(query, ts.TaskID, ts.Kind, companyVal, domainVal, netVal, valueVal, src, ts.Reason)
	}
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *AssetStore) upsertTaskScope(ts TaskScope) error {
	_, err := s.upsertTaskScopeResult(ts)
	return err
}

// AddAutoScope는 명시적으로 넣은 자산 항목 하나(source=auto)가 뜻하는 보수적인 작업 범위를 기록한다.
// insertAssets의 최상위 루프에서만 호출해야 한다.
// db 계층 부수 효과(linkHostAssets)에서는 호출하지 않는다. 그래야 파생 자산이 범위를 무작정 넓히지 않는다.
// 규칙: 범위 세분성은 자산 자신의 타입을 따른다. taskID<=0이면 아무 일도 하지 않는다.
func (s *AssetStore) AddAutoScope(taskID int64, assetType, domain, rawURL, ip string) error {
	if taskID <= 0 {
		return nil
	}
	switch assetType {
	case "root_domain":
		if d := DomainKey(domain); d != "" {
			return s.upsertTaskScope(TaskScope{TaskID: taskID, Kind: "root_domain", Domain: d})
		}
	case "subdomain":
		if d := DomainKey(domain); d != "" {
			return s.upsertTaskScope(TaskScope{TaskID: taskID, Kind: "subdomain", Domain: d})
		}
	case "service", "endpoint":
		host := domain
		if host == "" && rawURL != "" {
			host, _, _ = parseURL(normalizeURL(rawURL))
		}
		host = DomainKey(host)
		if host != "" && net.ParseIP(host) == nil {
			return s.upsertTaskScope(TaskScope{TaskID: taskID, Kind: "subdomain", Domain: host})
		}
		// 호스트가 IP 그대로이거나 호스트가 없으면, IP가 있을 때 ip 범위로 내려간다.
		if c := ipToHostCIDR(ip); c != "" {
			return s.upsertTaskScope(TaskScope{TaskID: taskID, Kind: "ip", Net: c})
		}
	case "ip":
		if c := ipToHostCIDR(ip); c != "" {
			return s.upsertTaskScope(TaskScope{TaskID: taskID, Kind: "ip", Net: c})
		}
	}
	return nil
}

// AddAgentScope는 (kind, value) 쌍을 읽어 작업 범위로 기록한다.
// LLM 도구에서 부르면 source는 "agent", UI에서 부르면 "manual"이다.
// company: 값은 기업 이름 또는 id(이미 있어야 한다). root_domain/subdomain:
// 값은 도메인. ip/cidr: 값은 IP 또는 CIDR(IP 하나면 /32, /128).
func (s *AssetStore) AddAgentScope(taskID int64, kind, value, reason, source string) (TaskScope, error) {
	if source == "" {
		source = "agent"
	}
	ts := TaskScope{TaskID: taskID, Kind: kind, Source: source, Reason: reason}
	if taskID <= 0 {
		return ts, fmt.Errorf("task_id가 필요합니다")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ts, fmt.Errorf("value는 비울 수 없습니다")
	}
	switch kind {
	case "company":
		if s.company == nil {
			return ts, fmt.Errorf("company store가 활성화되지 않았습니다")
		}
		var comp *Company
		var err error
		if id, e := strconv.ParseInt(value, 10, 64); e == nil {
			comp, err = s.company.GetCompany(id)
		} else {
			comp, err = s.company.GetCompanyByName(value)
		}
		if err != nil {
			return ts, err
		}
		if comp == nil {
			return ts, fmt.Errorf("company가 없습니다: %s(먼저 list_companies로 확인하거나 기업을 만드세요)", value)
		}
		ts.CompanyID = &comp.ID
	case "root_domain":
		d := DomainKey(stripHostPort(value))
		root, _ := RootDomain(d)
		if root == "" {
			root = d
		}
		if root == "" {
			return ts, fmt.Errorf("유효하지 않은 루트 도메인: %s", value)
		}
		ts.Domain = root
	case "subdomain":
		d := DomainKey(stripHostPort(value))
		if d == "" {
			return ts, fmt.Errorf("유효하지 않은 서브도메인: %s", value)
		}
		ts.Domain = d
	case "ip", "cidr":
		v := value
		if !strings.Contains(v, "/") {
			v = ipToHostCIDR(stripHostPort(v))
			ts.Kind = "ip"
		} else {
			ts.Kind = "cidr"
		}
		if v == "" {
			return ts, fmt.Errorf("유효하지 않은 ip/cidr: %s", value)
		}
		if _, _, err := net.ParseCIDR(v); err != nil {
			return ts, fmt.Errorf("유효하지 않은 ip/cidr: %s", value)
		}
		ts.Net = v
	case "icp", "keyword":
		parsed, err := ParseScopeInput(ScopeInput{Kind: kind, Value: value})
		if err != nil {
			return ts, err
		}
		ts.Value = parsed.Value
	default:
		return ts, fmt.Errorf("지원하지 않는 kind: %s(company/root_domain/subdomain/ip/cidr/icp/keyword)", kind)
	}
	if err := s.upsertTaskScope(ts); err != nil {
		return ts, err
	}
	return ts, nil
}

// DeleteTaskScope는 지정한 작업 안에서 id로 범위 한 줄을 지운다.
// 실제로 지웠는지를 돌려준다.
func (s *AssetStore) DeleteTaskScope(taskID, scopeID int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM task_scope WHERE id=$1 AND task_id=$2`, scopeID, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListTaskScope는 작업의 범위 행을 모두 돌려준다.
func (s *AssetStore) ListTaskScope(taskID int64) ([]TaskScope, error) {
	rows, err := s.db.Query(`
SELECT ts.id, ts.kind, COALESCE(ts.company_id,0), COALESCE(c.name,''), COALESCE(ts.domain,''),
       COALESCE(ts.net::text,''), COALESCE(ts.value,''), ts.source, COALESCE(ts.reason,'')
FROM task_scope ts
LEFT JOIN companies c ON c.id=ts.company_id
WHERE ts.task_id=$1 ORDER BY ts.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskScope{}
	for rows.Next() {
		var t TaskScope
		var cid int64
		if err := rows.Scan(&t.ID, &t.Kind, &cid, &t.CompanyName, &t.Domain, &t.Net, &t.Value, &t.Source, &t.Reason); err != nil {
			return nil, err
		}
		t.TaskID = taskID
		if cid > 0 {
			t.CompanyID = &cid
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CoverageAsset은 범위 안 자산 하나다. 아직 시험하지 않은 목록 표본에 쓴다.
type CoverageAsset struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

// CoverageByType은 자산 유형별 커버리지다. 범위 안 총수와 시험한 수.
type CoverageByType struct {
	Type   string `json:"type"`
	Total  int    `json:"total"`
	Tested int    `json:"tested"`
}

// Coverage는 작업의 대략적인 자산 시험 커버리지다. 에이전트용 참고 숫자이지
// 정밀 지표가 아니다. 분모는 활성 task_scope 행과 맞는 자산이고,
// Tested는 탐색 그래프의 사실 노드에 하나 이상 앵커된 자산이다.
type Coverage struct {
	Enabled     bool             `json:"enabled"`     // 자산 커버리지 기능이 켜져 있는지. false이면 나머지 필드는 제로값
	ScopeRows   int              `json:"scope_rows"`  // 0이면 범위가 앵커되지 않음
	Denominator int              `json:"denominator"` // 범위 안 자산 수
	Tested      int              `json:"tested"`      // 테스트됨(대략)
	Pct         *float64         `json:"pct"`         // 커버리지. 분모가 0이면 null
	ByType      []CoverageByType `json:"by_type"`     // 자산 유형별 총수/테스트됨
}

// CoverageEnabled는 작업의 자산 커버리지 기능이 켜져 있는지 알려 준다
// (tasks.coverage_enabled). 행이 없거나 오류면 true다(기본으로 열어 둔다).
// 그래서 모르거나 옛 작업은 예전 동작을 유지한다. taskID가 0 이하면 true다.
func (s *AssetStore) CoverageEnabled(taskID int64) bool {
	if taskID <= 0 {
		return true
	}
	var enabled bool
	if err := s.db.QueryRow(`SELECT COALESCE(coverage_enabled,true) FROM tasks WHERE id=$1`, taskID).Scan(&enabled); err != nil {
		return true
	}
	return enabled
}

// task_scope가 assets와 맞는 조건. 개수 / 유형별 / 미시험 조회가 같이 쓴다. $1=taskID.
const covTargetCTE = `
target AS (
  SELECT DISTINCT a.id, a.type,
         COALESCE(a.url, a.domain, a.ip, a.app_name, a.root_domain, '') AS label
  FROM assets a
  JOIN task_scope ts ON ts.task_id = $1 AND (
       (ts.kind='company'     AND a.company_id = ts.company_id)
    OR (ts.kind='root_domain' AND a.root_domain = ts.domain)
    OR (ts.kind='subdomain'   AND a.domain = ts.domain)
    OR (ts.kind IN ('ip','cidr') AND ts.net >>= try_inet(a.ip))
    OR (ts.kind='icp' AND (
         lower(regexp_replace(COALESCE(a.icp,''), '[[:space:]]+', '', 'g')) = ts.value
         OR lower(regexp_replace(COALESCE(a.app_icp,''), '[[:space:]]+', '', 'g')) = ts.value
       ))
  )
),
tested AS (
  SELECT DISTINCT ea.asset_id
  FROM exploration_anchors ea
  JOIN exploration_nodes en ON en.id = ea.node_id
  WHERE en.exploration_id = $2 AND en.kind = 'fact'
)`

// TaskCoverage는 작업의 유형별 대략 커버리지를 계산한다. taskID는
// task_scope와 assets를, expID는 사실 앵커를 가리킨다. 참고 숫자일 뿐이다.
func (s *AssetStore) TaskCoverage(taskID, expID int64) (*Coverage, error) {
	cov := &Coverage{ByType: []CoverageByType{}}
	_ = s.db.QueryRow(`SELECT count(*) FROM task_scope WHERE task_id=$1`, taskID).Scan(&cov.ScopeRows)
	rows, err := s.db.Query(`WITH `+covTargetCTE+`
SELECT t.type, count(*) AS total,
       count(*) FILTER (WHERE t.id IN (SELECT asset_id FROM tested)) AS tested
FROM target t GROUP BY t.type ORDER BY t.type`, taskID, expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bt CoverageByType
		if err := rows.Scan(&bt.Type, &bt.Total, &bt.Tested); err != nil {
			return nil, err
		}
		cov.ByType = append(cov.ByType, bt)
		cov.Denominator += bt.Total
		cov.Tested += bt.Tested
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cov.Denominator > 0 {
		p := float64(cov.Tested) / float64(cov.Denominator)
		cov.Pct = &p
	}
	return cov, nil
}

// ---------------------------------------------------------------------------
// 커버리지 그래프 — 작업 범위 안 자산을 힘 방향으로 펼친 보기.
// ---------------------------------------------------------------------------

// CoverageGraphNode는 자산 커버리지 그래프의 노드 하나다. 노드 식별은
// 문자열 키다. 자산 행은 "a:<id>", 기업은 "c:<id>", 자기 자산 행이 없는
// 루트 도메인은 합성 "r:<domain>"이다. 범위 밖 연결 노드
// (서브도메인을 잇려고만 끌어온 루트와, 그 위의 기업)는 InScope=false이고 회색으로 그린다.
type CoverageGraphNode struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"` // company|root_domain|subdomain|ip|service|app|endpoint (자산 종류)
	Label       string `json:"label"`
	Tested      bool   `json:"tested"`
	InScope     bool   `json:"in_scope"`
	AssetID     int64  `json:"asset_id,omitempty"` // 기업이거나 합성 루트이면 0
	CompanyID   int64  `json:"company_id,omitempty"`
	Domain      string `json:"domain,omitempty"`
	RootDomain  string `json:"root_domain,omitempty"`
	IP          string `json:"ip,omitempty"`
	URL         string `json:"url,omitempty"`
	Port        int    `json:"port,omitempty"`
	ServiceType string `json:"service_type,omitempty"`
	AppName     string `json:"app_name,omitempty"`
	PageTitle   string `json:"page_title,omitempty"`
	StatusCode  int    `json:"status_code,omitempty"`
}

// CoverageGraphEdge는 자식→부모 포함 간선이다(endpoint→service→
// subdomain/ip→root_domain→회사, app→회사).
type CoverageGraphEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// CoverageGraphData는 작업 하나의 그래프 전체다. UI가 이 값으로 범위 안 자산을 그린다.
type CoverageGraphData struct {
	Nodes []CoverageGraphNode `json:"nodes"`
	Edges []CoverageGraphEdge `json:"edges"`
}

func assetKey(id int64) string   { return "a:" + strconv.FormatInt(id, 10) }
func companyKey(id int64) string { return "c:" + strconv.FormatInt(id, 10) }

// hostPortOf는 service / endpoint 노드가 매달린 (호스트, 포트)를 돌려준다.
// 도메인이 있으면 그것을, 없으면 URL 호스트, 없으면 IP를 쓴다.
func hostPortOf(n *CoverageGraphNode) (string, int) {
	host, port := n.Domain, n.Port
	if host == "" && n.URL != "" {
		h, p, _ := parseURL(normalizeURL(n.URL))
		host = h
		if port == 0 {
			port = p
		}
	}
	if host == "" {
		host = n.IP
	}
	return host, port
}

// BuildCoverageGraph는 작업과 직접 읽기 전용 원본의 커버리지 그래프 전체를 조립한다.
// 범위 안 자산 전부와, 잇기용 루트 도메인·기업을 담고,
// 현재 또는 원본의 사실 앵커를 Tested에 반영한다. 옛 expID
// 인자는 API 호환으로 남긴다. 기준은 작업 등록부다.
func (s *AssetStore) BuildCoverageGraph(taskID, _ int64) (*CoverageGraphData, error) {
	g := &CoverageGraphData{Nodes: []CoverageGraphNode{}, Edges: []CoverageGraphEdge{}}
	if taskID <= 0 {
		return g, nil
	}
	rows, err := s.db.Query(`WITH `+contextCoverageCTE+`
SELECT a.id, a.type, COALESCE(a.company_id,0),
       COALESCE(a.domain,''), COALESCE(a.root_domain,''), COALESCE(a.ip,''),
       COALESCE(a.url,''), COALESCE(a.port,0), COALESCE(a.service_type,''),
       COALESCE(a.app_name,''), COALESCE(a.page_title,''), COALESCE(a.status_code,0),
       (a.id IN (SELECT asset_id FROM tested)) AS tested
FROM assets a JOIN target t ON t.id = a.id
ORDER BY a.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byKey := map[string]*CoverageGraphNode{}
	rootByDomain := map[string]string{} // 루트 도메인 → 노드 키
	subByDomain := map[string]string{}  // subdomain    → 노드 키
	ipByAddr := map[string]string{}     // IP 그대로   → 노드 키
	svcByHostPort := map[string]string{}
	svcByHost := map[string]string{}
	companyIDs := map[int64]bool{} // 참조된 기업 id (노드가 필요함)

	add := func(n CoverageGraphNode) *CoverageGraphNode {
		if _, ok := byKey[n.Key]; ok {
			return byKey[n.Key]
		}
		g.Nodes = append(g.Nodes, n)
		p := &g.Nodes[len(g.Nodes)-1]
		byKey[n.Key] = p
		return p
	}

	for rows.Next() {
		var n CoverageGraphNode
		var companyID int64
		if err := rows.Scan(&n.AssetID, &n.Kind, &companyID,
			&n.Domain, &n.RootDomain, &n.IP, &n.URL, &n.Port, &n.ServiceType,
			&n.AppName, &n.PageTitle, &n.StatusCode, &n.Tested); err != nil {
			return nil, err
		}
		n.Key = assetKey(n.AssetID)
		n.CompanyID = companyID
		n.InScope = true
		n.Label = coverageNodeLabel(&n)
		p := add(n)
		switch n.Kind {
		case "root_domain":
			if n.Domain != "" {
				rootByDomain[n.Domain] = p.Key
			}
		case "subdomain":
			if n.Domain != "" {
				subByDomain[n.Domain] = p.Key
			}
		case "ip":
			if n.IP != "" {
				ipByAddr[n.IP] = p.Key
			}
		case "service":
			host, port := hostPortOf(p)
			if host != "" {
				svcByHost[host] = p.Key
				svcByHostPort[host+"|"+strconv.Itoa(port)] = p.Key
			}
		}
		if companyID > 0 {
			companyIDs[companyID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 연결용 루트 도메인: 루트 도메인 자신이 범위 안 노드가 아닌 서브도메인.
	// 실제 자산 행이 있으면 그것을 가져온다(서랍이 진짜 자세한 내용을 보이게).
	// 없으면 맨 "r:<domain>" 자리표시를 만든다. 둘 다 회색이다.
	missingRoots := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Kind == "subdomain" && n.RootDomain != "" {
			if _, ok := rootByDomain[n.RootDomain]; !ok {
				missingRoots[n.RootDomain] = true
			}
		}
	}
	for root := range missingRoots {
		var id, companyID int64
		err := s.db.QueryRow(`SELECT id, COALESCE(company_id,0) FROM assets
WHERE type='root_domain' AND domain=$1 LIMIT 1`, root).Scan(&id, &companyID)
		var node CoverageGraphNode
		if err == nil && id > 0 {
			node = CoverageGraphNode{Key: assetKey(id), Kind: "root_domain", AssetID: id,
				CompanyID: companyID, Domain: root, Label: root}
			if companyID > 0 {
				companyIDs[companyID] = true
			}
		} else {
			node = CoverageGraphNode{Key: "r:" + root, Kind: "root_domain", Domain: root, Label: root}
		}
		add(node)
		rootByDomain[root] = node.Key
	}

	// 참조된 기업 id마다 기업 노드. 항상 회색 맥락이다.
	for id := range companyIDs {
		key := companyKey(id)
		if _, ok := byKey[key]; ok {
			continue
		}
		var name string
		if err := s.db.QueryRow(`SELECT name FROM companies WHERE id=$1`, id).Scan(&name); err != nil {
			continue
		}
		add(CoverageGraphNode{Key: key, Kind: "company", CompanyID: id,
			Label: name, AssetID: 0})
	}

	// 계산한 포함 간선(부모 노드가 있을 때만).
	link := func(childKey, parentKey string) {
		if parentKey == "" || parentKey == childKey {
			return
		}
		if _, ok := byKey[parentKey]; !ok {
			return
		}
		g.Edges = append(g.Edges, CoverageGraphEdge{Src: childKey, Dst: parentKey})
	}
	firstOf := func(keys ...string) string {
		for _, k := range keys {
			if k != "" {
				if _, ok := byKey[k]; ok {
					return k
				}
			}
		}
		return ""
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch n.Kind {
		case "subdomain":
			link(n.Key, rootByDomain[n.RootDomain])
		case "root_domain", "app", "ip":
			if n.CompanyID > 0 {
				link(n.Key, companyKey(n.CompanyID))
			}
		case "service":
			link(n.Key, firstOf(subByDomain[n.Domain], ipByAddr[n.IP], rootByDomain[n.RootDomain]))
		case "endpoint":
			host, port := hostPortOf(n)
			parent := firstOf(
				svcByHostPort[host+"|"+strconv.Itoa(port)], svcByHost[host],
				subByDomain[host], subByDomain[n.Domain], ipByAddr[host], ipByAddr[n.IP],
				rootByDomain[n.RootDomain])
			link(n.Key, parent)
		}
	}
	return g, nil
}

// coverageNodeLabel은 커버리지 그래프 노드에 사람이 읽을 이름을 고른다.
func coverageNodeLabel(n *CoverageGraphNode) string {
	switch n.Kind {
	case "endpoint", "service":
		if n.URL != "" {
			return n.URL
		}
	case "app":
		if n.AppName != "" {
			return n.AppName
		}
	}
	for _, v := range []string{n.URL, n.Domain, n.IP, n.AppName, n.RootDomain} {
		if v != "" {
			return v
		}
	}
	return n.Key
}

// ListUntestedAssets는 작업 범위 안에서 아직 시험하지 않은 자산을
// 자산 유형으로 거르고 페이지로 나눠 돌려준다. 페이지와 전체 개수를 돌려준다. limit이 0 이하면 10이다.
func (s *AssetStore) ListUntestedAssets(taskID, expID int64, typ string, limit, offset int) ([]CoverageAsset, int, error) {
	if limit <= 0 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}
	typeFilter := ""
	args := []any{taskID, expID}
	if typ != "" {
		typeFilter = " AND t.type = $3"
		args = append(args, typ)
	}
	var total int
	_ = s.db.QueryRow(`WITH `+covTargetCTE+`
SELECT count(*) FROM target t WHERE t.id NOT IN (SELECT asset_id FROM tested)`+typeFilter, args...).Scan(&total)
	pageArgs := append(append([]any{}, args...), limit, offset)
	limPos := strconv.Itoa(len(args) + 1)
	offPos := strconv.Itoa(len(args) + 2)
	rows, err := s.db.Query(`WITH `+covTargetCTE+`
SELECT t.id, t.type, t.label FROM target t
WHERE t.id NOT IN (SELECT asset_id FROM tested)`+typeFilter+`
ORDER BY t.id LIMIT $`+limPos+` OFFSET $`+offPos, pageArgs...)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()
	out := []CoverageAsset{}
	for rows.Next() {
		var a CoverageAsset
		if err := rows.Scan(&a.ID, &a.Type, &a.Label); err != nil {
			return nil, total, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}
