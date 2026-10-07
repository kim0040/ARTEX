package db

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Finding asset tree — 전역 발견 목록의 「자산별」 뷰.
//
// 계층은 BuildCoverageGraph와 같은 출처다(company → root_domain/ip/app → subdomain →
// service → endpoint). 그쪽은 「한 작업 범위 안의 자산」을 그리는 힘 방향 그래프이고, 이쪽은
// 「저장소 전체에서 발견이 있는 자산」의 트리다. 발견이 있는 자산과 그 조상 체인만 담고, 노드에는 하위 트리 집계 수가 붙는다.
// 둘의 부모-자식 우선순위 규칙은 같아야 한다. 한쪽을 고치면 task_scope.go를 보고 다른 쪽도 맞춘다.
// ---------------------------------------------------------------------------

// FindingUnassignedAsset은 「미연결 자산」의 노드 key이며, 목록 API의 필터 표식이다.
// asset_ids가 비어 있거나, 가리키는 자산이 이미 삭제된 발견에 해당한다.
const FindingUnassignedAsset = "__none__"

// findingAssetTreeMaxNodes는 프론트엔드에 돌려주는 노드 상한이다. 넘치면 아래부터 한 층 전체를 버린다
// (endpoint를 먼저, 그다음 service). 그 수는 이미 부모 노드에 누적되어 있어, 노드는 버려도 숫자는 남긴다.
const findingAssetTreeMaxNodes = 3000

// FindingAssetNode는 자산 트리의 한 노드다. Key는 커버리지 그래프와 같은 꼴이다. 자산 행은 "a:<id>",
// 기업은 "c:<id>", 자산 행이 없는 루트 도메인은 합성된 "r:<domain>", 미연결 버킷은 "__none__"이다.
// 이 노드는 자산 그래프의 한 줄을 UI의 자산별 발견 트리에 보여주는 단위다.
type FindingAssetNode struct {
	Key       string `json:"key"`
	Parent    string `json:"parent,omitempty"`
	Kind      string `json:"kind"` // company|root_domain|subdomain|ip|service|app|endpoint|none
	Label     string `json:"label"`
	AssetID   int64  `json:"asset_id,omitempty"`
	CompanyID int64  `json:"company_id,omitempty"`
	// Self는 그 자산에 직접 달린 발견 수다. Total은 모든 자손을 포함하며 finding 기준으로 중복을 뺀다
	// (한 발견이 여러 자산에 달리면, 공통 조상에서 한 번만 센다).
	Self        int       `json:"self"`
	Total       int       `json:"total"`
	Critical    int       `json:"critical"`
	High        int       `json:"high"`
	Medium      int       `json:"medium"`
	Low         int       `json:"low"`
	LastFoundAt time.Time `json:"last_found_at"`
}

// FindingAssetTree는 트리 전체의 한 번에 찍은 스냅샷이다. Nodes는 이미 정렬되어 있다. 같은 부모 아래에서는 발견 수
// 내림차순, 라벨 오름차순이고, 「미연결 자산」은 항상 마지막이다.
// UI는 이 스냅샷 하나로 자산별 발견 트리를 그린다.
type FindingAssetTree struct {
	Nodes        []FindingAssetNode `json:"nodes"`
	FindingTotal int                `json:"finding_total"`
	// Truncated=true는 크기를 맞추려고 DroppedKinds에 적힌 계층을 버렸다는 뜻이다.
	Truncated    bool     `json:"truncated"`
	DroppedKinds []string `json:"dropped_kinds,omitempty"`
}

// assetRow는 트리를 만드는 데 필요한 자산 필드 부분집합이다.
// 자산 그래프의 전체 행 가운데 트리에 쓰는 칸만 골라 담는다.
type assetRow struct {
	id          int64
	kind        string
	companyID   int64
	domain      string
	rootDomain  string
	ip          string
	url         string
	port        int
	serviceType string
	appName     string
}

func (a *assetRow) coverageNode() CoverageGraphNode {
	return CoverageGraphNode{
		Kind: a.kind, Domain: a.domain, RootDomain: a.rootDomain, IP: a.ip,
		URL: a.url, Port: a.port, ServiceType: a.serviceType, AppName: a.appName,
	}
}

// label은 커버리지 그래프의 라벨 규칙을 그대로 쓴다(URL > domain > ip > app_name > root_domain). URL이 없는
// 서비스(SMB, HTTP가 아닌 포트 등)에는 포트를 붙인다. 그렇지 않으면 라벨이 호스트 IP/도메인 행과
// 똑같아져, 트리에서 부모와 자식 두 줄이 완전히 같아 보인다.
func (a *assetRow) label() string {
	if a.kind == "service" && a.url == "" {
		if host, port := a.hostPort(); host != "" && port > 0 {
			return host + ":" + strconv.Itoa(port)
		}
	}
	n := a.coverageNode()
	n.Key = assetKey(a.id)
	return coverageNodeLabel(&n)
}

// hostPort는 커버리지 그래프와 같다. domain을 먼저, 그다음 URL 안의 host, 마지막으로 ip를 쓴다.
func (a *assetRow) hostPort() (string, int) {
	n := a.coverageNode()
	return hostPortOf(&n)
}

const findingAssetSelectCols = `a.id, a.type, COALESCE(a.company_id,0),
       COALESCE(a.domain,''), COALESCE(a.root_domain,''), COALESCE(a.ip,''),
       COALESCE(a.url,''), COALESCE(a.port,0), COALESCE(a.service_type,''),
       COALESCE(a.app_name,'')`

func scanAssetRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]*assetRow, error) {
	defer rows.Close()
	var out []*assetRow
	for rows.Next() {
		a := &assetRow{}
		if err := rows.Scan(&a.id, &a.kind, &a.companyID, &a.domain, &a.rootDomain,
			&a.ip, &a.url, &a.port, &a.serviceType, &a.appName); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// findingAssetHit는 발견 하나가 트리를 만드는 단계에서 필요한 최소 정보다.
// 탐색 그래프의 발견을 자산 트리에 걸 때 이 묶음만 읽는다.
type findingAssetHit struct {
	severity string
	ts       time.Time
	assetIDs []int64
}

// BuildFindingAssetTree는 현재 필터로 자산 트리를 만든다. AssetScope 자체는 넣지 않는다(넣으면 트리가
// 선택된 노드를 따라 한 줄로 접힌다).
func (d *DB) BuildFindingAssetTree(f FindingFilter) (*FindingAssetTree, error) {
	return d.buildFindingAssetTree(f, findingAssetTreeMaxNodes)
}

// buildFindingAssetTree는 노드 상한이 있는 내부 구현이다. maxNodes<=0은 자르지 않는다는 뜻이다. AssetScope를
// 풀 때는 이 모드를 써야 한다. 그렇지 않으면 버려진 endpoint 때문에 하위 트리 id 집합이 불완전해진다.
func (d *DB) buildFindingAssetTree(f FindingFilter, maxNodes int) (*FindingAssetTree, error) {
	f.AssetScope = ""
	f.assetIDs, f.assetNone, f.assetMiss = nil, false, false
	where, args := f.where()

	rows, err := d.Query(`SELECT COALESCE(f.severity,''), f.created_at,
       COALESCE(f.asset_ids::text,'[]')
FROM findings f LEFT JOIN tasks t ON f.task_id = t.id`+where, args...)
	if err != nil {
		return nil, err
	}
	hits := []findingAssetHit{}
	assetIDs := map[int64]bool{}
	for rows.Next() {
		var h findingAssetHit
		var aidsJSON string
		if err := rows.Scan(&h.severity, &h.ts, &aidsJSON); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(aidsJSON), &h.assetIDs)
		for _, id := range h.assetIDs {
			if id > 0 {
				assetIDs[id] = true
			}
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	tree := &FindingAssetTree{Nodes: []FindingAssetNode{}, FindingTotal: len(hits)}
	byID, err := d.loadFindingAssetRows(assetIDs)
	if err != nil {
		return nil, err
	}

	nodes, parentOf := d.assembleFindingAssetNodes(byID)
	if err := d.attachCompanyNodes(nodes, parentOf); err != nil {
		return nil, err
	}

	// 집계: 발견 하나가 자기 자산마다의 조상 체인을 따라 올라가며, 중복을 뺀 key 집합을 모은 뒤 각각 +1한다.
	// 그래서 부모 노드는 발견 하나가 여러 자식 자산에 달렸다고 해서 중복으로 세지 않는다.
	unassigned := &FindingAssetNode{Key: FindingUnassignedAsset, Kind: "none", Label: "미연결 자산"}
	touched := map[string]bool{}
	for _, h := range hits {
		clear(touched)
		var direct []*FindingAssetNode
		for _, id := range h.assetIDs {
			node := nodes[assetKey(id)]
			if node == nil {
				continue
			}
			direct = append(direct, node)
			for key := node.Key; key != ""; key = parentOf[key] {
				touched[key] = true
			}
		}
		if len(direct) == 0 {
			countFinding(unassigned, h)
			unassigned.Self++
			continue
		}
		for _, node := range direct {
			node.Self++
		}
		for key := range touched {
			countFinding(nodes[key], h)
		}
	}

	for _, node := range nodes {
		if node.Total > 0 {
			tree.Nodes = append(tree.Nodes, *node)
		}
	}
	if unassigned.Total > 0 {
		tree.Nodes = append(tree.Nodes, *unassigned)
	}
	sortFindingAssetNodes(tree.Nodes)
	truncateFindingAssetTree(tree, maxNodes)
	return tree, nil
}

// countFinding은 발견 하나를 노드에 누적한다(총수 / 심각도 버킷 / 최근 발견 시각).
func countFinding(n *FindingAssetNode, h findingAssetHit) {
	if n == nil {
		return
	}
	n.Total++
	switch h.severity {
	case "critical":
		n.Critical++
	case "high":
		n.High++
	case "medium":
		n.Medium++
	case "low":
		n.Low++
	}
	if h.ts.After(n.LastFoundAt) {
		n.LastFoundAt = h.ts
	}
}

// loadFindingAssetRows는 맞은 자산 행을 읽고, 라운드마다 조상을 채운다(service의 호스트 도메인/IP,
// 서브도메인의 루트 도메인). 조상 자체에는 발견이 없을 수 있지만, 트리가 모양을 갖추려면 필요하다.
func (d *DB) loadFindingAssetRows(ids map[int64]bool) (map[int64]*assetRow, error) {
	byID := map[int64]*assetRow{}
	if len(ids) == 0 {
		return byID, nil
	}
	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	rows, err := d.Query(`SELECT `+findingAssetSelectCols+` FROM assets a WHERE a.id = ANY($1::bigint[])`, idList)
	if err != nil {
		return nil, err
	}
	found, err := scanAssetRows(rows)
	if err != nil {
		return nil, err
	}
	for _, a := range found {
		byID[a.id] = a
	}

	// 매 라운드마다 아직 부모 노드가 없는 호스트 식별자를 찾아 한 층을 한꺼번에 채운다. 층 수는 고정이다(endpoint→service→
	// subdomain/ip→root_domain). 4라운드면 수렴하기에 충분하다.
	for range 4 {
		want := missingParents(byID)
		if want.empty() {
			break
		}
		added, err := d.loadAssetsByHost(want, byID)
		if err != nil {
			return nil, err
		}
		if added == 0 {
			break
		}
	}
	return byID, nil
}

// missingHosts는 한 라운드의 보완에서 저장소에 찾으러 갈 호스트 식별자이며, 대상 자산 유형별로 나뉜다.
// 이 식별자로 자산 그래프에서 아직 읽지 않은 부모 자산을 가져온다.
type missingHosts struct {
	services []string // endpoint의 호스트(service 행을 찾음)
	domains  []string // service/endpoint의 호스트 도메인(subdomain 행을 찾음)
	ips      []string // service/endpoint의 호스트 IP(ip 행을 찾음)
	roots    []string // 서브도메인의 루트 도메인(root_domain 행을 찾음)
}

func (m missingHosts) empty() bool {
	return len(m.services) == 0 && len(m.domains) == 0 && len(m.ips) == 0 && len(m.roots) == 0
}

// missingParents는 아직 로드되지 않은 호스트를 모은다. service(endpoint가 붙음), 서브도메인/IP(
// service와 endpoint가 붙음), 루트 도메인(서브도메인이 붙음).
func missingParents(byID map[int64]*assetRow) missingHosts {
	haveService := map[string]bool{}
	haveDomain := map[string]bool{}
	haveIP := map[string]bool{}
	haveRoot := map[string]bool{}
	for _, a := range byID {
		switch a.kind {
		case "service":
			if host, _ := a.hostPort(); host != "" {
				haveService[host] = true
			}
		case "subdomain":
			haveDomain[a.domain] = true
		case "ip":
			haveIP[a.ip] = true
		case "root_domain":
			haveRoot[a.domain] = true
		}
	}
	wantService := map[string]bool{}
	wantDomain := map[string]bool{}
	wantIP := map[string]bool{}
	wantRoot := map[string]bool{}
	for _, a := range byID {
		switch a.kind {
		case "service", "endpoint":
			host, _ := a.hostPort()
			// endpoint는 먼저 같은 호스트의 service를 찾는다. 포트가 맞지 않는 service는 Total=0으로
			// 마지막에 걸러지며, 트리를 오염시키지 않는다.
			if a.kind == "endpoint" && host != "" && !haveService[host] {
				wantService[host] = true
			}
			if host != "" && !haveDomain[host] && !haveIP[host] && !haveRoot[host] {
				if isIPLiteral(host) {
					wantIP[host] = true
				} else {
					wantDomain[host] = true
				}
			}
			if a.ip != "" && !haveIP[a.ip] {
				wantIP[a.ip] = true
			}
		case "subdomain":
			if a.rootDomain != "" && !haveRoot[a.rootDomain] {
				wantRoot[a.rootDomain] = true
			}
		}
	}
	return missingHosts{
		services: keysOf(wantService),
		domains:  keysOf(wantDomain),
		ips:      keysOf(wantIP),
		roots:    keysOf(wantRoot),
	}
}

func keysOf(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// isIPLiteral은 host가 IP 리터럴인지 대략 판별한다(호스트를 ip 표에서 찾을지 subdomain 표에서 찾을지 정하는 데 쓴다).
func isIPLiteral(host string) bool {
	if strings.Contains(host, ":") {
		return true // IPv6
	}
	if host == "" {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return strings.Count(host, ".") == 3
}

// loadAssetsByHost는 호스트 식별자로 자산 행을 일괄 보완하고, 이번 라운드에 새로 추가된 행 수를 반환한다.
func (d *DB) loadAssetsByHost(want missingHosts, byID map[int64]*assetRow) (int, error) {
	added := 0
	load := func(q string, arg []string) error {
		if len(arg) == 0 {
			return nil
		}
		rows, err := d.Query(q, arg)
		if err != nil {
			return err
		}
		found, err := scanAssetRows(rows)
		if err != nil {
			return err
		}
		for _, a := range found {
			if _, ok := byID[a.id]; ok {
				continue
			}
			byID[a.id] = a
			added++
		}
		return nil
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='service' AND (a.domain = ANY($1::text[]) OR a.ip = ANY($1::text[]))`, want.services); err != nil {
		return 0, err
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='subdomain' AND a.domain = ANY($1::text[])`, want.domains); err != nil {
		return 0, err
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='ip' AND a.ip = ANY($1::text[])`, want.ips); err != nil {
		return 0, err
	}
	if err := load(`SELECT `+findingAssetSelectCols+` FROM assets a
WHERE a.type='root_domain' AND a.domain = ANY($1::text[])`, want.roots); err != nil {
		return 0, err
	}
	return added, nil
}

// assembleFindingAssetNodes는 자산 행을 노드로 만들고 부모-자식 관계를 연결한다. 부모 노드가 없을 때(저장소에
// 해당 루트 도메인 자산이 전혀 없으면) "r:<domain>" 자리 표시 노드를 합성하며, 커버리지 그래프 처리와 같다.
func (d *DB) assembleFindingAssetNodes(byID map[int64]*assetRow) (map[string]*FindingAssetNode, map[string]string) {
	nodes := map[string]*FindingAssetNode{}
	parentOf := map[string]string{}
	rootByDomain := map[string]string{}
	subByDomain := map[string]string{}
	ipByAddr := map[string]string{}
	svcByHost := map[string]string{}
	svcByHostPort := map[string]string{}

	for _, a := range byID {
		key := assetKey(a.id)
		nodes[key] = &FindingAssetNode{
			Key: key, Kind: a.kind, Label: a.label(),
			AssetID: a.id, CompanyID: a.companyID,
		}
		switch a.kind {
		case "root_domain":
			if a.domain != "" {
				rootByDomain[a.domain] = key
			}
		case "subdomain":
			if a.domain != "" {
				subByDomain[a.domain] = key
			}
		case "ip":
			if a.ip != "" {
				ipByAddr[a.ip] = key
			}
		case "service":
			if host, port := a.hostPort(); host != "" {
				svcByHost[host] = key
				svcByHostPort[host+"|"+strconv.Itoa(port)] = key
			}
		}
	}

	// 서브도메인의 루트 도메인이 저장소에 자산 행으로 없으면 자리 표시 루트를 합성하여, 서브도메인이 최상위로 흩어지지 않게 한다.
	for _, a := range byID {
		if a.kind != "subdomain" || a.rootDomain == "" {
			continue
		}
		if _, ok := rootByDomain[a.rootDomain]; ok {
			continue
		}
		key := "r:" + a.rootDomain
		nodes[key] = &FindingAssetNode{Key: key, Kind: "root_domain", Label: a.rootDomain}
		rootByDomain[a.rootDomain] = key
	}

	firstOf := func(keys ...string) string {
		for _, k := range keys {
			if k != "" {
				if _, ok := nodes[k]; ok {
					return k
				}
			}
		}
		return ""
	}
	for _, a := range byID {
		key := assetKey(a.id)
		var parent string
		switch a.kind {
		case "subdomain":
			parent = firstOf(rootByDomain[a.rootDomain])
		case "service":
			host, _ := a.hostPort()
			parent = firstOf(subByDomain[a.domain], subByDomain[host],
				ipByAddr[a.ip], ipByAddr[host], rootByDomain[a.rootDomain], rootByDomain[host])
		case "endpoint":
			host, port := a.hostPort()
			parent = firstOf(svcByHostPort[host+"|"+strconv.Itoa(port)], svcByHost[host],
				subByDomain[host], subByDomain[a.domain], ipByAddr[host], ipByAddr[a.ip],
				rootByDomain[a.rootDomain], rootByDomain[host])
		}
		if parent != "" && parent != key {
			parentOf[key] = parent
			nodes[key].Parent = parent
		}
	}
	return nodes, parentOf
}

// attachCompanyNodes는 최상위 자산(루트 도메인 / IP / 애플리케이션)에 기업 부모 노드를 붙인다. 자산이 실제로
// 기업에 속할 때만 기업 층이 나타나고, 소속이 없는 자산은 그대로 자신이 최상위이다.
func (d *DB) attachCompanyNodes(nodes map[string]*FindingAssetNode, parentOf map[string]string) error {
	want := map[int64]bool{}
	for _, n := range nodes {
		if n.Parent != "" || n.CompanyID <= 0 {
			continue
		}
		switch n.Kind {
		case "root_domain", "ip", "app":
			want[n.CompanyID] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	rows, err := d.Query(`SELECT id, COALESCE(name,'') FROM companies WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	names := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, name := range names {
		key := companyKey(id)
		if _, ok := nodes[key]; ok {
			continue
		}
		if name == "" {
			name = "기업 #" + strconv.FormatInt(id, 10)
		}
		nodes[key] = &FindingAssetNode{Key: key, Kind: "company", Label: name, CompanyID: id}
	}
	for _, n := range nodes {
		if n.Parent != "" || n.CompanyID <= 0 || n.Kind == "company" {
			continue
		}
		switch n.Kind {
		case "root_domain", "ip", "app":
			key := companyKey(n.CompanyID)
			if _, ok := nodes[key]; !ok {
				continue
			}
			n.Parent = key
			parentOf[n.Key] = key
		}
	}
	return nil
}

// sortFindingAssetNodes 정렬: 발견이 많은 것이 앞이고, 같은 수이면 라벨 순이다. 「미연결 자산」은 항상 마지막이다.
// 프론트엔드는 배열 순서대로 자식 노드를 붙이므로, 같은 부모 노드 아래의 상대 순서만 맞으면 된다.
func sortFindingAssetNodes(nodes []FindingAssetNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if (a.Kind == "none") != (b.Kind == "none") {
			return b.Kind == "none"
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Label < b.Label
	})
}

// truncateFindingAssetTree는 노드가 너무 많을 때 계층 전체를 버린다(endpoint를 먼저, 그다음 service). 개수는 이미
// 부모 노드에 누적되어 있으므로, 버리는 것은 펼칠 수 있는 세부 계층뿐이다.
func truncateFindingAssetTree(tree *FindingAssetTree, maxNodes int) {
	if maxNodes <= 0 || len(tree.Nodes) <= maxNodes {
		return
	}
	for _, kind := range []string{"endpoint", "service"} {
		kept := tree.Nodes[:0]
		for _, n := range tree.Nodes {
			if n.Kind == kind {
				continue
			}
			kept = append(kept, n)
		}
		tree.Nodes = kept
		tree.Truncated = true
		tree.DroppedKinds = append(tree.DroppedKinds, kind)
		if len(tree.Nodes) <= maxNodes {
			return
		}
	}
}

// applyAssetScope는 AssetScope(노드 key)를 SQL에 쓸 수 있는 자산 id 집합으로 해석한다. 노드 하나를 선택하면
// 그 노드의 하위 트리 전체를 선택한 것과 같으므로, 먼저 트리를 만든 다음 자손을 모아야 한다.
func (d *DB) applyAssetScope(f FindingFilter) (FindingFilter, error) {
	scope := strings.TrimSpace(f.AssetScope)
	f.assetIDs, f.assetNone, f.assetMiss = nil, false, false
	if scope == "" {
		return f, nil
	}
	if scope == FindingUnassignedAsset {
		f.assetNone = true
		return f, nil
	}
	// 자르지 않는다: 버려진 endpoint도 id 수집에 참여해야 하며, 그렇지 않으면 목록의 데이터가 빠진다.
	tree, err := d.buildFindingAssetTree(f, 0)
	if err != nil {
		return f, err
	}
	children := map[string][]FindingAssetNode{}
	byKey := map[string]FindingAssetNode{}
	for _, n := range tree.Nodes {
		byKey[n.Key] = n
		children[n.Parent] = append(children[n.Parent], n)
	}
	if _, ok := byKey[scope]; !ok {
		// 선택한 노드가 현재 필터에서는 이미 없으면, 결과는 비어야 하며 필터 없음으로 물러나서는 안 된다.
		f.assetMiss = true
		return f, nil
	}
	seen := map[string]bool{scope: true}
	queue := []string{scope}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if id := byKey[key].AssetID; id > 0 {
			f.assetIDs = append(f.assetIDs, id)
		}
		for _, child := range children[key] {
			if seen[child.Key] {
				continue
			}
			seen[child.Key] = true
			queue = append(queue, child.Key)
		}
	}
	if len(f.assetIDs) == 0 {
		f.assetMiss = true
	}
	return f, nil
}

// assetIDContainments는 자산 id를 jsonb 포함 판정의 오른쪽 피연산자 집합으로 바꾸고,
// idx_findings_asset_ids(GIN jsonb_path_ops)와 함께 쓴다.
func assetIDContainments(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "["+strconv.FormatInt(id, 10)+"]")
	}
	return out
}
