package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"unicode/utf8"
)

const (
	MaxTaskAssetMutationCount = 100
	MaxTaskAssetSummaryRunes  = 500
	defaultTaskAssetSource    = "system"
	manualTaskScopeSummary    = "사용자가 테스트 자산 페이지에서 수동으로 추가"
)

var (
	ErrTaskAssetInvalid       = errors.New("invalid task asset association")
	ErrTaskAssetTaskNotFound  = errors.New("작업을 찾을 수 없습니다")
	ErrTaskAssetAssetNotFound = errors.New("자산을 찾을 수 없습니다")
)

// TaskAssetMutation은 붙이기 요청 하나의 요약이다. Attached는 새로 더한 연결 수이고,
// Existing은 요청한 자산 중 이미 작업에 있던 수다.
type TaskAssetMutation struct {
	Requested int `json:"requested"`
	Attached  int `json:"attached"`
	Existing  int `json:"existing"`
}

// TaskAssetScopeMutation은 자유 형식 범위 등록 하나의 요약이다.
// 도메인과 IP 항목은 전역 자산을 만들거나 다시 쓴다. 모든 항목은
// 같은 요청을 다시 해도 되는 task_scope 행이 된다.
type TaskAssetScopeMutation struct {
	Requested      int `json:"requested"`
	AssetsLinked   int `json:"assets_linked"`
	AssetsExisting int `json:"assets_existing"`
	ScopesAdded    int `json:"scopes_added"`
	ScopesExisting int `json:"scopes_existing"`
}

// IntentAsset은 워커 의도에 명시적으로 앵커된 자산이다. 탐색 그래프의 의도와 자산 그래프의 자산을 잇는다.
type IntentAsset struct {
	IntentID      int64  `json:"intent_id"`
	AssetID       int64  `json:"asset_id"`
	Type          string `json:"type"`
	Label         string `json:"label"`
	Source        string `json:"source"`
	SourceSummary string `json:"source_summary"`
	SourceNodeID  *int64 `json:"source_node_id,omitempty"`
	SourceTaskID  int64  `json:"source_task_id"`
	Inherited     bool   `json:"inherited"`
}

func normalizeTaskAssetIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: asset_ids is required", ErrTaskAssetInvalid)
	}
	seen := make(map[int64]struct{}, len(ids))
	normalized := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("%w: asset id must be positive", ErrTaskAssetInvalid)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
		if len(normalized) > MaxTaskAssetMutationCount {
			return nil, fmt.Errorf("%w: at most %d assets per request", ErrTaskAssetInvalid, MaxTaskAssetMutationCount)
		}
	}
	return normalized, nil
}

func normalizeTaskAssetSource(source, summary string) (string, string, error) {
	source = strings.TrimSpace(strings.ToLower(source))
	if source == "" {
		source = defaultTaskAssetSource
	}
	summary = strings.TrimSpace(summary)
	if utf8.RuneCountInString(summary) > MaxTaskAssetSummaryRunes {
		return "", "", fmt.Errorf("%w: source summary exceeds %d characters", ErrTaskAssetInvalid, MaxTaskAssetSummaryRunes)
	}
	return source, summary, nil
}

// SetTaskAssetSource는 이미 있는 작업 연결의, 트리거가 만든 일반 출처를 더 구체적으로 고친다.
// 자산을 만들거나 지우지는 않는다.
func (s *AssetStore) SetTaskAssetSource(taskID, assetID int64, source, summary string, sourceNodeID *int64) error {
	if taskID <= 0 || assetID <= 0 {
		return fmt.Errorf("%w: task and asset ids must be positive", ErrTaskAssetInvalid)
	}
	source, summary, err := normalizeTaskAssetSource(source, summary)
	if err != nil {
		return err
	}
	query := `
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary, source_node_id)
SELECT task.id, asset.id, $3, $4, $5
FROM tasks task
JOIN assets asset ON asset.id=$2 AND task.id=ANY(asset.task_ids)
WHERE task.id=$1 AND task.deleted_at IS NULL
ON CONFLICT (task_id, asset_id) DO UPDATE
SET source=EXCLUDED.source,
    source_summary=EXCLUDED.source_summary,
    source_node_id=COALESCE(EXCLUDED.source_node_id, task_asset_links.source_node_id)`
	var result sql.Result
	if s.tx != nil {
		result, err = s.tx.Exec(query, taskID, assetID, source, summary, sourceNodeID)
	} else {
		result, err = s.db.Exec(query, taskID, assetID, source, summary, sourceNodeID)
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: task or asset association does not exist", ErrTaskAssetInvalid)
	}
	return nil
}

// RegisterTaskAssetScopes는 기업 자산과 같은 구조화 범위 규칙을 받는다.
// 요청 전체가 한 번에 적용된다. 입력이 잘못되거나 저장이 실패하면
// 전역 자산과 작업 범위 모두 그대로다.
func (s *AssetStore) RegisterTaskAssetScopes(taskID int64, inputs []ScopeInput) (TaskAssetScopeMutation, error) {
	mutation := TaskAssetScopeMutation{Requested: len(inputs)}
	if taskID <= 0 {
		return mutation, fmt.Errorf("%w: task id must be positive", ErrTaskAssetInvalid)
	}
	if len(inputs) == 0 {
		return mutation, fmt.Errorf("%w: scope is required", ErrTaskAssetInvalid)
	}
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return mutation, fmt.Errorf("%w: %v", ErrTaskAssetInvalid, err)
	}
	parsed := make([]ParsedScope, 0, len(inputs))
	for index, input := range inputs {
		rule, err := ParseScopeInput(input)
		if err != nil {
			return mutation, fmt.Errorf("%w: %d번째 범위가 유효하지 않음: %v", ErrTaskAssetInvalid, index+1, err)
		}
		parsed = append(parsed, rule)
	}
	if err := validateParsedScopeBounds(parsed); err != nil {
		return mutation, fmt.Errorf("%w: %v", ErrTaskAssetInvalid, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return mutation, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return mutation, err
	}
	var taskExists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL)`, taskID).Scan(&taskExists); err != nil {
		return mutation, err
	}
	if !taskExists {
		return mutation, ErrTaskAssetTaskNotFound
	}

	scoped := &AssetStore{db: s.db, company: s.company, tx: tx}
	for _, rule := range parsed {
		taskScope := TaskScope{
			TaskID: taskID,
			Source: "manual",
			Reason: manualTaskScopeSummary,
		}
		var assetID int64
		switch rule.Kind {
		case "domain":
			taskScope.Kind = "root_domain"
			taskScope.Domain = rule.Domain
			var alreadyLinked bool
			err := tx.QueryRow(`SELECT id, $2=ANY(task_ids) FROM assets WHERE type='root_domain' AND domain=$1`, rule.Domain, taskID).
				Scan(&assetID, &alreadyLinked)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return mutation, err
			}
			assetID, err = scoped.UpsertRootDomain(UpsertRootDomainReq{Domain: rule.Domain, TaskID: taskID})
			if err != nil {
				return mutation, err
			}
			if alreadyLinked {
				mutation.AssetsExisting++
			} else {
				mutation.AssetsLinked++
			}
		case "ip":
			taskScope.Kind = "ip"
			taskScope.Net = rule.Net
			ip, _, parseErr := net.ParseCIDR(rule.Net)
			if parseErr != nil {
				return mutation, fmt.Errorf("%w: 유효하지 않은 IP: %s", ErrTaskAssetInvalid, rule.Raw)
			}
			ipValue := ip.String()
			var alreadyLinked bool
			err := tx.QueryRow(`SELECT id, $2=ANY(task_ids) FROM assets WHERE type='ip' AND ip=$1`, ipValue, taskID).
				Scan(&assetID, &alreadyLinked)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return mutation, err
			}
			assetID, err = scoped.UpsertIP(UpsertIPReq{IP: ipValue, TaskID: taskID})
			if err != nil {
				return mutation, err
			}
			if alreadyLinked {
				mutation.AssetsExisting++
			} else {
				mutation.AssetsLinked++
			}
		case "cidr":
			taskScope.Kind = "cidr"
			taskScope.Net = rule.Net
		case "icp", "keyword":
			taskScope.Kind = rule.Kind
			taskScope.Value = rule.Value
		default:
			return mutation, fmt.Errorf("%w: unsupported scope kind %q", ErrTaskAssetInvalid, rule.Kind)
		}

		if assetID > 0 {
			if err := scoped.SetTaskAssetSource(taskID, assetID, "manual", manualTaskScopeSummary, nil); err != nil {
				return mutation, err
			}
		}
		inserted, err := scoped.upsertTaskScopeResult(taskScope)
		if err != nil {
			return mutation, err
		}
		if inserted {
			mutation.ScopesAdded++
		} else {
			mutation.ScopesExisting++
		}
	}
	if err := tx.Commit(); err != nil {
		return mutation, err
	}
	return mutation, nil
}

// AttachAssetsToTask는 이미 있는 전역 자산을 살아있는 작업 하나에 연결하고,
// 운영자가 적은 출처 요약을 남긴다. 전역 자산 행은 그대로 둔다.
func (s *AssetStore) AttachAssetsToTask(taskID int64, assetIDs []int64, sourceSummary string) (TaskAssetMutation, error) {
	var mutation TaskAssetMutation
	assetIDs, err := normalizeTaskAssetIDs(assetIDs)
	if err != nil {
		return mutation, err
	}
	_, sourceSummary, err = normalizeTaskAssetSource("manual", sourceSummary)
	if err != nil {
		return mutation, err
	}
	if sourceSummary == "" {
		return mutation, fmt.Errorf("%w: source_summary is required", ErrTaskAssetInvalid)
	}
	mutation.Requested = len(assetIDs)
	tx, err := s.db.Begin()
	if err != nil {
		return mutation, err
	}
	defer tx.Rollback() //nolint:errcheck

	var taskExists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL)`, taskID).Scan(&taskExists); err != nil {
		return mutation, err
	}
	if !taskExists {
		return mutation, ErrTaskAssetTaskNotFound
	}
	var found, existing int
	if err := tx.QueryRow(`
SELECT count(*), count(*) FILTER (WHERE $1=ANY(task_ids))
FROM assets WHERE id=ANY($2::bigint[])`, taskID, assetIDs).Scan(&found, &existing); err != nil {
		return mutation, err
	}
	if found != len(assetIDs) {
		return mutation, ErrTaskAssetAssetNotFound
	}
	// 순서가 중요하다. 이 UPDATE가 trg_assets_task_links를 일으켜
	// source='system'인 일반 연결 행을 만든다. 아래 INSERT는 그 뒤에 있어야
	// 운영자가 적은 'manual' 출처가 이긴다. 두 문을 바꾸면 수동 연결이 조용히 'system'으로 내려간다.
	if _, err := tx.Exec(`
UPDATE assets
SET task_ids=CASE WHEN $1=ANY(task_ids) THEN task_ids ELSE array_append(task_ids,$1) END
WHERE id=ANY($2::bigint[])`, taskID, assetIDs); err != nil {
		return mutation, err
	}
	if _, err := tx.Exec(`
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT $1, id, 'manual', $3 FROM assets WHERE id=ANY($2::bigint[])
ON CONFLICT (task_id, asset_id) DO UPDATE
SET source='manual', source_summary=EXCLUDED.source_summary, source_node_id=NULL`,
		taskID, assetIDs, sourceSummary); err != nil {
		return mutation, err
	}
	mutation.Existing = existing
	mutation.Attached = len(assetIDs) - existing
	return mutation, tx.Commit()
}

// DetachAssetFromTask는 작업 연결만 지운다. 전역 자산과
// 탐색 앵커는 남겨, 과거 칠판 감사를 계속할 수 있다.
func (s *AssetStore) DetachAssetFromTask(taskID, assetID int64) (bool, error) {
	if taskID <= 0 || assetID <= 0 {
		return false, fmt.Errorf("%w: task and asset ids must be positive", ErrTaskAssetInvalid)
	}
	var detachedID int64
	err := s.db.QueryRow(`
UPDATE assets SET task_ids=array_remove(task_ids,$1)
WHERE id=$2 AND $1=ANY(task_ids)
RETURNING id`, taskID, assetID).Scan(&detachedID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return detachedID == assetID, nil
}

func (s *AssetStore) hydrateTaskAssetSources(taskID int64, assets []*Asset) error {
	if len(assets) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(assets))
	byID := make(map[int64]*Asset, len(assets))
	for _, asset := range assets {
		ids = append(ids, asset.ID)
		byID[asset.ID] = asset
	}
	rows, err := s.db.Query(`
SELECT asset_id, source, source_summary, source_node_id
FROM task_asset_links
WHERE task_id=$1 AND asset_id=ANY($2::bigint[])`, taskID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var assetID int64
		var source, summary string
		var sourceNodeID sql.NullInt64
		if err := rows.Scan(&assetID, &source, &summary, &sourceNodeID); err != nil {
			return err
		}
		if asset := byID[assetID]; asset != nil {
			asset.TaskSource = source
			asset.TaskSourceSummary = summary
			if sourceNodeID.Valid {
				id := sourceNodeID.Int64
				asset.TaskSourceNodeID = &id
			}
		}
	}
	return rows.Err()
}

// IntentAssets는 로컬 워커 대상과, 작업의 직접 원본에서 온 고칠 수 없는 대상을 돌려준다.
// 끝나지 않은 물려받은 의도는 숨긴다. 원본을 아는 기존 세션 계약과 같다.
func (s *AssetStore) IntentAssets(taskID int64) ([]IntentAsset, error) {
	rows, err := s.db.Query(`
WITH context AS (
    SELECT task.id AS task_id, task.exploration_id, false AS inherited
    FROM tasks task
    WHERE task.id=$1 AND task.deleted_at IS NULL
    UNION ALL
    SELECT source.id, source.exploration_id, true
    FROM task_relations relation
    JOIN tasks source ON source.id=relation.source_task_id AND source.deleted_at IS NULL
    WHERE relation.task_id=$1
)
SELECT intent.id, asset.id, asset.type,
       CASE asset.type
         WHEN 'root_domain' THEN COALESCE(asset.domain,'')
         WHEN 'subdomain' THEN COALESCE(asset.domain,'')
         WHEN 'ip' THEN COALESCE(asset.ip,'')
         WHEN 'app' THEN COALESCE(asset.app_name,'')
		 WHEN 'service' THEN COALESCE(NULLIF(asset.url,''), NULLIF(concat_ws(':', COALESCE(NULLIF(asset.domain,''), NULLIF(asset.ip,'')), asset.port::text),''), NULLIF(asset.service_name,''), '#' || asset.id::text)
         WHEN 'endpoint' THEN COALESCE(NULLIF(asset.url,''), '#' || asset.id::text)
         ELSE '#' || asset.id::text
       END,
       COALESCE(link.source,'anchor'),
       COALESCE(NULLIF(link.source_summary,''), '의도가 탐색 그래프에서 이 자산을 앵커로 연결했습니다'),
       link.source_node_id, context.task_id, context.inherited
FROM context
JOIN exploration_nodes intent ON intent.exploration_id=context.exploration_id AND intent.kind='intent'
JOIN exploration_anchors anchor ON anchor.node_id=intent.id
JOIN assets asset ON asset.id=anchor.asset_id
LEFT JOIN task_asset_links link ON link.task_id=context.task_id AND link.asset_id=asset.id
WHERE NOT context.inherited OR intent.state IN ('done','blocked','exhausted','stopped')
ORDER BY context.inherited, intent.id DESC, asset.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntentAsset{}
	for rows.Next() {
		var asset IntentAsset
		var sourceNodeID sql.NullInt64
		if err := rows.Scan(&asset.IntentID, &asset.AssetID, &asset.Type, &asset.Label,
			&asset.Source, &asset.SourceSummary, &sourceNodeID, &asset.SourceTaskID, &asset.Inherited); err != nil {
			return nil, err
		}
		if sourceNodeID.Valid {
			id := sourceNodeID.Int64
			asset.SourceNodeID = &id
		}
		out = append(out, asset)
	}
	return out, rows.Err()
}
