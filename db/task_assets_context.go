package db

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// directTaskContextCTE는 현재 작업과, 명시적으로 연결한 원본 작업만 담는다.
// 일부러 재귀하지 않는다. 물려받은 탐색 맥락이 원본을 한 단계만 보게 한다.
const directTaskContextCTE = `
context_tasks AS (
  SELECT t.id AS task_id, t.exploration_id
  FROM tasks t
  WHERE t.id=$1 AND t.deleted_at IS NULL
  UNION ALL
  SELECT source.id, source.exploration_id
  FROM task_relations relation
  JOIN tasks source ON source.id=relation.source_task_id AND source.deleted_at IS NULL
  WHERE relation.task_id=$1
)`

const contextCoverageCTE = directTaskContextCTE + `,
target AS (
  SELECT DISTINCT a.id, a.type,
         COALESCE(a.url, a.domain, a.ip, a.app_name, a.root_domain, '') AS label
  FROM assets a
  JOIN task_scope ts ON (
       (ts.kind='company'     AND a.company_id = ts.company_id)
    OR (ts.kind='root_domain' AND a.root_domain = ts.domain)
    OR (ts.kind='subdomain'   AND a.domain = ts.domain)
    OR (ts.kind IN ('ip','cidr') AND ts.net >>= try_inet(a.ip))
    OR (ts.kind='icp' AND (
         lower(regexp_replace(COALESCE(a.icp,''), '[[:space:]]+', '', 'g')) = ts.value
         OR lower(regexp_replace(COALESCE(a.app_icp,''), '[[:space:]]+', '', 'g')) = ts.value
       ))
  )
  JOIN context_tasks ctx ON ctx.task_id=ts.task_id
	UNION
	SELECT a.id, a.type,
	       COALESCE(a.url, a.domain, a.ip, a.app_name, a.root_domain, '') AS label
	FROM assets a
	JOIN exploration_anchors ea ON ea.asset_id=a.id
	JOIN exploration_nodes en ON en.id=ea.node_id
	JOIN context_tasks ctx ON ctx.exploration_id=en.exploration_id
),
tested AS (
  SELECT DISTINCT ea.asset_id
  FROM exploration_anchors ea
  JOIN exploration_nodes en ON en.id=ea.node_id AND en.kind='fact'
  JOIN context_tasks ctx ON ctx.exploration_id=en.exploration_id
)`

// scopeTargetCTE는 현재 작업과 그 직접 원본 작업이 선언한 범위에 속하는(BELONGS) 모든 자산을 고른다.
// 소속이지 리터럴 값이 아니다. root_domain 범위는 자신의
// root_domain 열이 그 값과 같은 subdomain / service / endpoint를 모두 끌어오고,
// ip/cidr 범위는 ip 또는 IP 리터럴 host가 그 네트워크 안에 있는 자산을 끌어온다. $1은 작업 id이다. contextCoverageCTE와 달리
// 사실-앵커 합집합도 테스트된 집합도 담지 않으며, 이미 다룬 것과 무관한 순수한 「선언된 범위 안」이다. 에이전트의
// list_assets가 써서, 쿼리가 공유 자산 그래프 전체가 아니라 해당 작업의 관련 자산만 돌려준다.
// 자산 그래프에서 이번 작업 범위의 행만 잘라, 탐색 그래프의 사실과 발견이 범위 밖 자산을 가리키지 않게 한다.
const scopeTargetCTE = `
context_tasks AS (
  SELECT t.id AS task_id
  FROM tasks t
  WHERE t.id=$1 AND t.deleted_at IS NULL
  UNION ALL
  SELECT source.id
  FROM task_relations relation
  JOIN tasks source ON source.id=relation.source_task_id AND source.deleted_at IS NULL
  WHERE relation.task_id=$1
),
target AS (
  SELECT DISTINCT a.id
  FROM assets a
  JOIN task_scope ts ON (
       (ts.kind='company'     AND a.company_id = ts.company_id)
    OR (ts.kind='root_domain' AND a.root_domain = ts.domain)
    OR (ts.kind='subdomain'   AND a.domain = ts.domain)
    OR (ts.kind IN ('ip','cidr') AND (ts.net >>= try_inet(a.ip) OR ts.net >>= try_inet(a.domain)))
    OR (ts.kind='icp' AND (
         lower(regexp_replace(COALESCE(a.icp,''), '[[:space:]]+', '', 'g')) = ts.value
         OR lower(regexp_replace(COALESCE(a.app_icp,''), '[[:space:]]+', '', 'g')) = ts.value
       ))
  )
  JOIN context_tasks ctx ON ctx.task_id=ts.task_id
)`

// ListTaskScopeWithSources는 현재 작업의 범위를 먼저, 그다음 직접 원본 작업의 범위를 돌려준다.
// TaskScope.TaskID로 어느 작업에서 왔는지 남긴다.
func (s *AssetStore) ListTaskScopeWithSources(taskID int64) ([]TaskScope, error) {
	rows, err := s.db.Query(`WITH `+directTaskContextCTE+`
SELECT ts.id, ts.task_id, ts.kind, COALESCE(ts.company_id,0), COALESCE(c.name,''), COALESCE(ts.domain,''),
       COALESCE(ts.net::text,''), COALESCE(ts.value,''), ts.source, COALESCE(ts.reason,'')
FROM task_scope ts
JOIN context_tasks ctx ON ctx.task_id=ts.task_id
LEFT JOIN companies c ON c.id=ts.company_id
ORDER BY CASE WHEN ts.task_id=$1 THEN 0 ELSE 1 END, ts.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskScope{}
	for rows.Next() {
		var scope TaskScope
		var companyID int64
		if err := rows.Scan(&scope.ID, &scope.TaskID, &scope.Kind, &companyID, &scope.CompanyName, &scope.Domain, &scope.Net, &scope.Value, &scope.Source, &scope.Reason); err != nil {
			return nil, err
		}
		if companyID > 0 {
			scope.CompanyID = &companyID
		}
		out = append(out, scope)
	}
	return out, rows.Err()
}

// TaskCoverageWithSources는 현재 작업과 직접 원본의 합집합으로 커버리지 하나를 계산한다.
// 원본 범위와 앵커된 자산이 분모를 넓히고, 사실 앵커는 테스트된 것으로 센다.
// 행을 복사하지는 않는다. 자산 그래프의 범위와 탐색 그래프의 사실 앵커를 한 화면에 겹친다.
func (s *AssetStore) TaskCoverageWithSources(taskID int64) (*Coverage, error) {
	cov := &Coverage{ByType: []CoverageByType{}}
	_ = s.db.QueryRow(`WITH `+directTaskContextCTE+`
SELECT count(*) FROM task_scope ts JOIN context_tasks ctx ON ctx.task_id=ts.task_id`, taskID).Scan(&cov.ScopeRows)
	rows, err := s.db.Query(`WITH `+contextCoverageCTE+`
SELECT target.type, count(*) AS total,
       count(*) FILTER (WHERE target.id IN (SELECT asset_id FROM tested)) AS tested
FROM target GROUP BY target.type ORDER BY target.type`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var byType CoverageByType
		if err := rows.Scan(&byType.Type, &byType.Total, &byType.Tested); err != nil {
			return nil, err
		}
		cov.ByType = append(cov.ByType, byType)
		cov.Denominator += byType.Total
		cov.Tested += byType.Tested
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if cov.Denominator > 0 {
		pct := float64(cov.Tested) / float64(cov.Denominator)
		cov.Pct = &pct
	}
	return cov, nil
}

// ListUntestedAssetsWithSources는 물려받은 작업이 쓰는, 직접 원본을 아는 미시험 목록이다.
// 원본의 사실 앵커가 있는 자산은 목록에서 뺀다.
func (s *AssetStore) ListUntestedAssetsWithSources(taskID int64, typ string, limit, offset int) ([]CoverageAsset, int, error) {
	if limit <= 0 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}
	typeFilter := ""
	args := []any{taskID}
	if typ != "" {
		typeFilter = " AND target.type = $2"
		args = append(args, typ)
	}
	var total int
	if err := s.db.QueryRow(`WITH `+contextCoverageCTE+`
SELECT count(*) FROM target WHERE target.id NOT IN (SELECT asset_id FROM tested)`+typeFilter, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	limitPosition := strconv.Itoa(len(args) + 1)
	offsetPosition := strconv.Itoa(len(args) + 2)
	rows, err := s.db.Query(`WITH `+contextCoverageCTE+`
SELECT target.id, target.type, target.label FROM target
WHERE target.id NOT IN (SELECT asset_id FROM tested)`+typeFilter+`
ORDER BY target.id LIMIT $`+limitPosition+` OFFSET $`+offsetPosition, pageArgs...)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()
	out := []CoverageAsset{}
	for rows.Next() {
		var asset CoverageAsset
		if err := rows.Scan(&asset.ID, &asset.Type, &asset.Label); err != nil {
			return nil, total, err
		}
		out = append(out, asset)
	}
	return out, total, rows.Err()
}

// HostsByTaskWithSources는 현재 작업이나 직접 원본에 붙었거나, 앵커됐거나, 범위 안인
// 자산에서 정확한 HTTP 호스트 후보를 고른다. 트래픽은 전역이며 복사하지 않는다.
// 이 읽기 도우미를 작업을 지우는 정리에 쓰면 안 된다. HostsByTask가 그 좁은, 작업 소유 동작을 일부러 유지한다.
func (s *AssetStore) HostsByTaskWithSources(taskID int64) ([]string, error) {
	rows, err := s.db.Query(`WITH `+directTaskContextCTE+`,
context_assets AS (
  SELECT DISTINCT a.id
  FROM assets a
  WHERE EXISTS (SELECT 1 FROM context_tasks ctx WHERE ctx.task_id=ANY(a.task_ids))
  UNION
  SELECT ea.asset_id
  FROM exploration_anchors ea
  JOIN exploration_nodes en ON en.id=ea.node_id
  JOIN context_tasks ctx ON ctx.exploration_id=en.exploration_id
  UNION
  SELECT DISTINCT a.id
  FROM assets a
  JOIN task_scope ts ON (
       (ts.kind='company'     AND a.company_id=ts.company_id)
    OR (ts.kind='root_domain' AND a.root_domain=ts.domain)
    OR (ts.kind='subdomain'   AND a.domain=ts.domain)
    OR (ts.kind IN ('ip','cidr') AND ts.net >>= try_inet(a.ip))
    OR (ts.kind='icp' AND (
         lower(regexp_replace(COALESCE(a.icp,''), '[[:space:]]+', '', 'g')) = ts.value
         OR lower(regexp_replace(COALESCE(a.app_icp,''), '[[:space:]]+', '', 'g')) = ts.value
       ))
  )
  JOIN context_tasks ctx ON ctx.task_id=ts.task_id
)
SELECT COALESCE(a.domain,''), COALESCE(a.ip,''), COALESCE(a.url,'')
FROM assets a JOIN context_assets ctx ON ctx.id=a.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := map[string]struct{}{}
	add := func(host string) {
		host = strings.TrimSpace(strings.ToLower(host))
		if host != "" {
			hosts[host] = struct{}{}
		}
	}
	for rows.Next() {
		var domain, ip, rawURL string
		if err := rows.Scan(&domain, &ip, &rawURL); err != nil {
			return nil, err
		}
		add(domain)
		add(ip)
		if parsed, err := url.Parse(rawURL); err == nil {
			add(parsed.Hostname())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out, nil
}
