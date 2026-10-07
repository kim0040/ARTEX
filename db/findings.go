package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DBFinding은 독립 findings 테이블의 한 줄이다. 작업을 지워도 남는다.
// 호출자가 관련 발견 정리를 명시적으로 요청할 때만 함께 지운다. 발견은 탐색 그래프 노드와 별도로 UI 목록에 남는다.
type DBFinding struct {
	TrafficCount          int
	EvidenceVersion       int64
	ReportEvidenceVersion int64
	TrafficBindings       []FindingTrafficBinding // 내보낼 때만 채운다

	ID              int64
	TaskID          *int64
	NodeID          *int64
	VulnClass       string
	Name            string // 발견 이름(읽기 쉬운 제목); 비어 있으면 프론트엔드는 VulnClass를 대신 보여 줍니다.
	Severity        string
	Summary         string
	Evidence        string
	Worker          string
	AssetIDs        []int64
	Status          string
	Report          string // 상세 보고서(Markdown); GetFinding에서만 채우고, 목록 조회에는 포함하지 않습니다.
	CreatedAt       time.Time
	TaskDescription string // tasks를 LEFT JOIN해서 채운다
}

// 발견 분류 상태(findings.status).
const (
	FindingPending       = "pending"        // 처리 대기
	FindingInProgress    = "in_progress"    // 처리 중
	FindingConfirmed     = "confirmed"      // 확인됨(실제 발견, 아직 수정되지 않음)
	FindingResolved      = "resolved"       // 처리됨
	FindingFixed         = "fixed"          // 수정됨
	FindingFalsePositive = "false_positive" // 오탐
	FindingIgnored       = "ignored"        // 무시
	FindingDuplicate     = "duplicate"      // 중복
	FindingRiskAccepted  = "risk_accepted"  // 위험 수용
)

// ValidFindingStatus는 s가 알려진 분류 상태인지 알려 준다.
func ValidFindingStatus(s string) bool {
	switch s {
	case FindingPending, FindingInProgress, FindingConfirmed, FindingResolved, FindingFixed,
		FindingFalsePositive, FindingIgnored, FindingDuplicate, FindingRiskAccepted:
		return true
	}
	return false
}

// 발견 심각도(findings.severity).
const (
	SeverityCritical = "critical" // 심각
	SeverityHigh     = "high"     // 높음
	SeverityMedium   = "medium"   // 중간
	SeverityLow      = "low"      // 낮음
)

// ValidSeverity는 s가 알려진 심각도인지 알려 준다.
func ValidSeverity(s string) bool {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow:
		return true
	}
	return false
}

// AddFinding은 독립 findings 테이블에 발견을 넣는다. taskID와
// nodeID는 0일 수 있다(NULL로 저장). name은 ""일 수 있다(프론트는
// vulnclass로 내려간다). 새 발견 id를 돌려준다.
func (d *DB) AddFinding(taskID, nodeID int64, vulnclass, name, severity, summary, evidence, worker string, assetIDs []int64) (int64, error) {
	aidsJSON, _ := json.Marshal(assetIDs)
	if assetIDs == nil {
		aidsJSON = []byte("[]")
	}
	var tid, nid *int64
	if taskID > 0 {
		tid = &taskID
	}
	if nodeID > 0 {
		nid = &nodeID
	}
	var id int64
	err := d.QueryRow(
		`INSERT INTO findings (task_id, node_id, vulnclass, name, severity, summary, evidence, worker, asset_ids)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		tid, nid, vulnclass, name, severity, summary, evidence, worker, string(aidsJSON),
	).Scan(&id)
	return id, err
}

// findingSelectCols는 모든 발견 목록 조회가 고르는 열 목록이다(task_description 조인 포함).
// 그래서 scanFinding이 호출자마다 어긋나지 않는다.
const findingSelectCols = `f.id, f.task_id, f.node_id, f.vulnclass, COALESCE(f.name, ''), f.severity, f.summary,
	       f.evidence, f.worker, f.asset_ids, COALESCE(f.status, 'pending'), f.created_at,
	       COALESCE(t.description, '') AS task_description, f.evidence_version, f.report_evidence_version,
 (SELECT count(*) FROM finding_traffic_bindings b WHERE b.finding_id=f.id)`

// scanFindings는 findingSelectCols로 고른 행을 구조체로 만든다.
func scanFindings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*DBFinding, error) {
	var out []*DBFinding
	for rows.Next() {
		f := &DBFinding{}
		var aidsJSON string
		if err := rows.Scan(&f.ID, &f.TaskID, &f.NodeID, &f.VulnClass, &f.Name, &f.Severity,
			&f.Summary, &f.Evidence, &f.Worker, &aidsJSON, &f.Status, &f.CreatedAt, &f.TaskDescription, &f.EvidenceVersion, &f.ReportEvidenceVersion, &f.TrafficCount); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(aidsJSON), &f.AssetIDs)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFindings는 모든 발견을 반환합니다(최신순). 작업 설명과 조인합니다.
// 대시보드 요약용으로 유지하며, 페이지가 나뉜 발견 화면은 ListFindingsPage를 사용합니다.
func (d *DB) ListFindings(limit int) ([]*DBFinding, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.Query(`
		SELECT `+findingSelectCols+`
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id
		ORDER BY f.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}

// FindingFilter는 페이지로 나눈 발견 조회를 좁힌다. 빈 문자열 필드는
// 「그 열은 거르지 않음」이다. Sort가 "severity"면 심각도 내림차순 다음 최신이고,
// 그 외에는 최신 먼저다.
type FindingFilter struct {
	Severity  string // high(높음) | medium(중간) | low(낮음)
	Status    string // pending(대기) | false_positive(오탐) | ignored(무시) | resolved(해결)
	VulnClass string
	TaskID    string // 작업 id(문자열 형태; 비었거나 잘못됨 = 작업으로 거르지 않음)
	Query     string // 이름/유형/요약/증거/보고서 본문의 부분 검색 키워드
	Sort      string // "severity"(심각도) | "time"(시각)
	// AssetScope는 자산 트리의 노드 key입니다(a:<id> / c:<id> / r:<domain> / __none__). 이 key는 자산 그래프의 노드를 발견 필터에 연결합니다.
	// 노드 하나를 고르면 그 서브트리 전체가 선택된 것입니다. 비어 있음 = 자산으로 거르지 않음.
	AssetScope string

	// 아래 세 값은 applyAssetScope가 AssetScope에서 풀어 낸 결과이며, 호출하는 쪽은 설정하지 않습니다.
	assetIDs  []int64 // 서브트리 안의 모든 자산 id
	assetNone bool    // 자산에 연결되지 않은 발견만
	assetMiss bool    // 선택한 노드가 현재 필터에는 없음 → 결과는 항상 비어 있음
}

// FindingUnassignedTask는 작업이 없는 발견을 거르는 표식이다.
// 작업 없이 만든 행과, 원래 작업을 지운 뒤 남은 행을 포함한다
// (findings 외래 키는 ON DELETE SET NULL이다).
const FindingUnassignedTask = "__unassigned__"

// where는 WHERE 절(페이지 조회와 개수 조회가 같이 씀)과 위치 인자를 만든다.
// 값은 모두 매개변수다. Query는 ILIKE 와일드카드도 이스케이프해서
// 사용자 입력을 항상 글자 그대로 맞춘다.
func (f FindingFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(col, val string) {
		if val == "" {
			return
		}
		args = append(args, val)
		conds = append(conds, fmt.Sprintf("f.%s = $%d", col, len(args)))
	}
	add("severity", f.Severity)
	add("status", f.Status)
	add("vulnclass", f.VulnClass)
	// task_id 는 bigint 열이라 정수로 비교한다(위의 텍스트 add 경로를 타면 안 된다). 빈 값이나 잘못된 값은 무시한다.
	if f.TaskID == FindingUnassignedTask {
		conds = append(conds, "(f.task_id IS NULL OR t.id IS NULL)")
	} else if tid, err := strconv.ParseInt(f.TaskID, 10, 64); err == nil && tid > 0 {
		args = append(args, tid)
		conds = append(conds, fmt.Sprintf("f.task_id = $%d", len(args)))
	}
	// 자산 필터: asset_ids 는 jsonb 배열이며, @> ANY(...) 는 idx_findings_asset_ids 를 탈 수 있다.
	switch {
	case f.assetMiss:
		conds = append(conds, "FALSE")
	case f.assetNone:
		// 「연결되지 않은 자산」= asset_ids 가 비어 있거나, 안의 id 가 하나도 assets 테이블에 없는 경우
		// (자산이 이미 삭제됨). 두 종류 모두 자산 트리의 미연결 버킷으로 들어가므로, 여기서도 똑같이 받아야 한다. 그렇지 않으면 버킷
		// 의 개수가 펼친 뒤에 조회되는 건수보다 커진다.
		conds = append(conds, `(
			jsonb_array_length(COALESCE(f.asset_ids, '[]'::jsonb)) = 0
			OR NOT EXISTS (
				SELECT 1 FROM jsonb_array_elements_text(f.asset_ids) e(v)
				JOIN assets a ON a.id = e.v::bigint
			)
		)`)
	case len(f.assetIDs) > 0:
		args = append(args, assetIDContainments(f.assetIDs))
		conds = append(conds, fmt.Sprintf("f.asset_ids @> ANY($%d::jsonb[])", len(args)))
	}
	if query := strings.TrimSpace(f.Query); query != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
		args = append(args, "%"+escaped+"%")
		placeholder := fmt.Sprintf("$%d", len(args))
		conds = append(conds, fmt.Sprintf(`(
			COALESCE(f.name, '') ILIKE %s ESCAPE '\' OR
			f.vulnclass ILIKE %s ESCAPE '\' OR
			f.summary ILIKE %s ESCAPE '\' OR
			f.evidence ILIKE %s ESCAPE '\' OR
			COALESCE(f.report, '') ILIKE %s ESCAPE '\'
		)`, placeholder, placeholder, placeholder, placeholder, placeholder))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListFindingsPage는 필터에 맞는 발견 한 페이지와
// 맞는 행의 전체 개수를 돌려준다(프론트 페이지 번호용). page는 1부터 센다.
func (d *DB) ListFindingsPage(f FindingFilter, page, pageSize int) ([]*DBFinding, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	f, err := d.applyAssetScope(f)
	if err != nil {
		return nil, 0, err
	}
	where, args := f.where()

	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM findings f LEFT JOIN tasks t ON f.task_id=t.id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	// 페이지 번호가 아주 커도 (page-1)*pageSize가 넘치지 않게 한다.
	// 요청한 페이지가 정확한 개수를 넘으면 데이터 조회는 필요 없다.
	if total == 0 || page > (total-1)/pageSize+1 {
		return []*DBFinding{}, total, nil
	}

	order := "f.created_at DESC, f.id DESC"
	if f.Sort == "severity" {
		// critical > high > medium > low > 기타, 그다음 최신순.
		order = `CASE f.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC, f.created_at DESC, f.id DESC`
	}
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	q := fmt.Sprintf(`
		SELECT %s
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id%s
		ORDER BY %s
		LIMIT $%d OFFSET $%d`, findingSelectCols, where, order, len(args)+1, len(args)+2)
	rows, err := d.Query(q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanFindings(rows)
	return out, total, err
}

// FindingGroup은 전역 발견 보기에서 작업 단위 통 하나다. TaskID는
// 작업이 한 번도 없던 발견과, 작업을 지운 뒤 남은 발견 모두 nil이다.
// 그 기록은 일부러 「미지정/삭제됨」 통 하나를 같이 쓴다.
type FindingGroup struct {
	TaskID          *int64    `json:"task_id"`
	TaskName        string    `json:"task_name"` // 선택적 작업 이름; 비어 있음=이름 없음
	TaskDescription string    `json:"task_description"`
	TaskStatus      string    `json:"task_status"`
	Count           int       `json:"count"`
	Critical        int       `json:"critical"`
	High            int       `json:"high"`
	Medium          int       `json:"medium"`
	Low             int       `json:"low"`
	LastFoundAt     time.Time `json:"last_found_at"`
}

// ListFindingGroups는 ListFindingsPage와 같은 필터의 작업 묶음 한 페이지를 돌려준다.
// 묶음 수와 발견 수는 따로인 합계라, 클라이언트가 묶음을 넘겨도
// 내보내기·선택의 정확한 개수를 잃지 않는다.
func (d *DB) ListFindingGroups(f FindingFilter, page, pageSize int) ([]FindingGroup, int, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	f, err := d.applyAssetScope(f)
	if err != nil {
		return nil, 0, 0, err
	}
	where, args := f.where()
	grouped := ` FROM findings f LEFT JOIN tasks t ON f.task_id=t.id` + where +
		` GROUP BY t.id, t.name, t.description, t.status, t.paused, t.queued`

	var groupTotal, findingTotal int
	countQuery := `SELECT COUNT(*), COALESCE(SUM(finding_count),0) FROM (` +
		`SELECT COUNT(*) AS finding_count` + grouped + `) grouped_findings`
	if err := d.QueryRow(countQuery, args...).Scan(&groupTotal, &findingTotal); err != nil {
		return nil, 0, 0, err
	}
	if groupTotal == 0 || page > (groupTotal-1)/pageSize+1 {
		return []FindingGroup{}, groupTotal, findingTotal, nil
	}

	order := "MAX(f.created_at) DESC, t.id DESC NULLS LAST"
	if f.Sort == "severity" {
		order = `MAX(CASE f.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END) DESC, MAX(f.created_at) DESC, t.id DESC NULLS LAST`
	}
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	query := fmt.Sprintf(`SELECT t.id, COALESCE(t.name,''), COALESCE(t.description,''), COALESCE(
		CASE
			WHEN t.status IN ('done','failed','timeout') THEN t.status
			WHEN t.queued THEN 'queued'
			WHEN t.paused THEN 'paused'
			ELSE t.status
		END, ''),
		COUNT(*),
		COUNT(*) FILTER (WHERE f.severity='critical'),
		COUNT(*) FILTER (WHERE f.severity='high'),
		COUNT(*) FILTER (WHERE f.severity='medium'),
		COUNT(*) FILTER (WHERE f.severity='low'),
		MAX(f.created_at)%s
		ORDER BY %s LIMIT $%d OFFSET $%d`, grouped, order, len(args)+1, len(args)+2)
	rows, err := d.Query(query, pageArgs...)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	groups := []FindingGroup{}
	for rows.Next() {
		var group FindingGroup
		var taskID sql.NullInt64
		if err := rows.Scan(&taskID, &group.TaskName, &group.TaskDescription, &group.TaskStatus, &group.Count,
			&group.Critical, &group.High, &group.Medium, &group.Low, &group.LastFoundAt); err != nil {
			return nil, 0, 0, err
		}
		if taskID.Valid {
			id := taskID.Int64
			group.TaskID = &id
		}
		groups = append(groups, group)
	}
	return groups, groupTotal, findingTotal, rows.Err()
}

// ErrFindingOriginUnavailable은 남아 있는 발견에, 후속 의도를 만들
// 살아있는 소유 작업과 발견 노드가 더 이상 없다는 뜻이다.
var ErrFindingOriginUnavailable = errors.New("발견의 원래 대상을 더 이상 쓸 수 없습니다")

// AddFindingFollowUpIntent는 살아있는 발견 노드에서 우선순위 10인 사람 의도를 한 번에 만든다.
// 그 발견의 자산 앵커를 복사하고, finding --derived_from--> intent 계보 간선을 남기고,
// 감사 활동을 저장한다. 돌려주는 활동은 커밋된 행이라
// AppendActivity를 다시 부르지 않고 그대로 방송할 수 있다.
func (s *ExplorationStore) AddFindingFollowUpIntent(findingID, findingNodeID int64, description string, audit Activity) (int64, Activity, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, Activity{}, err
	}
	defer tx.Rollback()

	var liveNodeID int64
	err = tx.QueryRow(`SELECT n.id
		FROM findings f
		JOIN tasks t ON t.id=f.task_id
		JOIN exploration_nodes n ON n.id=f.node_id AND n.exploration_id=t.exploration_id
		WHERE f.id=$1 AND f.node_id=$2 AND t.exploration_id=$3 AND n.kind='finding'
		FOR SHARE OF f, t, n`, findingID, findingNodeID, s.expID).Scan(&liveNodeID)
	if err == sql.ErrNoRows {
		return 0, Activity{}, ErrFindingOriginUnavailable
	}
	if err != nil {
		return 0, Activity{}, err
	}

	anchors := []int64{}
	anchorRows, err := tx.Query(`SELECT asset_id FROM exploration_anchors WHERE node_id=$1 ORDER BY asset_id`, liveNodeID)
	if err != nil {
		return 0, Activity{}, err
	}
	for anchorRows.Next() {
		var assetID int64
		if err := anchorRows.Scan(&assetID); err != nil {
			anchorRows.Close()
			return 0, Activity{}, err
		}
		anchors = append(anchors, assetID)
	}
	if err := anchorRows.Err(); err != nil {
		anchorRows.Close()
		return 0, Activity{}, err
	}
	if err := anchorRows.Close(); err != nil {
		return 0, Activity{}, err
	}

	payload := map[string]any{
		"summary":                description,
		"source_finding_id":      findingID,
		"source_finding_node_id": liveNodeID,
	}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, Activity{}, err
	}
	var intentID int64
	if err := tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
		VALUES ($1,'intent',$2,10,'open','human') RETURNING id`, s.expID, raw).Scan(&intentID); err != nil {
		return 0, Activity{}, err
	}
	if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id,asset_id)
		SELECT $1, asset_id FROM exploration_anchors WHERE node_id=$2
		ON CONFLICT DO NOTHING`, intentID, liveNodeID); err != nil {
		return 0, Activity{}, err
	}
	if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
		VALUES ($1,$2,$3,$4)`, s.expID, liveNodeID, RelDerivedFrom, intentID); err != nil {
		return 0, Activity{}, err
	}

	audit.NodeID = &intentID
	if summary := strings.TrimSpace(audit.Summary); summary != "" {
		audit.Summary = fmt.Sprintf("%s #%d", summary, intentID)
	} else {
		audit.Summary = ""
	}
	metadata := audit.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if err := tx.QueryRow(`
INSERT INTO activity(exploration_id, node_id, worker, kind, tool, tool_use_id, is_error, summary, detail, metadata, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14)
RETURNING id, created_at`, s.expID, audit.NodeID, utf8Clean(audit.Worker), utf8Clean(audit.Kind), utf8Clean(audit.Tool), utf8Clean(audit.ToolUseID), audit.IsError,
		utf8Clean(audit.Summary), utf8Clean(audit.Detail), metadata, audit.InputTokens, audit.OutputTokens, audit.CacheReadTokens, audit.CacheWriteTokens).
		Scan(&audit.ID, &audit.CreatedAt); err != nil {
		return 0, Activity{}, err
	}
	audit.Metadata = metadata
	if err := tx.Commit(); err != nil {
		return 0, Activity{}, err
	}
	return intentID, audit, nil
}

// ListFindingsForExport 는 발견 페이지 내보내기에 쓸 발견 목록을 반환한다. 전체
// report 필드를 담고 페이지를 나누지 않는다. ids 가 비어 있지 않으면 그 finding id 묶음을 정확히 내보낸다(선택 내보내기). 이때
// filter 는 무시한다. ids 가 비어 있으면 filter 기준으로 내보낸다(현재 필터 또는 전체 내보내기). 결과는 심각도 내림차순,
// 이어서 시간 역순이며, 「요약 보고서 내보내기」의 그룹 순서와 같다.
func (d *DB) ListFindingsForExport(f FindingFilter, ids []int64) ([]*DBFinding, error) {
	const order = `ORDER BY CASE f.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC, f.created_at DESC`
	cols := findingSelectCols + `, COALESCE(f.report, '')`

	var q string
	var args []any
	if len(ids) > 0 {
		ph := make([]string, len(ids))
		for i, id := range ids {
			ph[i] = fmt.Sprintf("$%d", i+1)
			args = append(args, id)
		}
		q = `SELECT ` + cols + `
			FROM findings f
			LEFT JOIN tasks t ON f.task_id = t.id
			WHERE f.id IN (` + strings.Join(ph, ",") + `)
			` + order
	} else {
		scoped, err := d.applyAssetScope(f)
		if err != nil {
			return nil, err
		}
		where, wargs := scoped.where()
		q = `SELECT ` + cols + `
			FROM findings f
			LEFT JOIN tasks t ON f.task_id = t.id` + where + `
			` + order
		args = wargs
	}

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DBFinding
	for rows.Next() {
		f := &DBFinding{}
		var aidsJSON string
		if err := rows.Scan(&f.ID, &f.TaskID, &f.NodeID, &f.VulnClass, &f.Name, &f.Severity,
			&f.Summary, &f.Evidence, &f.Worker, &aidsJSON, &f.Status, &f.CreatedAt,
			&f.TaskDescription, &f.EvidenceVersion, &f.ReportEvidenceVersion, &f.TrafficCount, &f.Report); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(aidsJSON), &f.AssetIDs)
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindingStats 는 발견 페이지의 통계 카드와 취약점 분류 필터를 채우는 전체 테이블 집계다.
// 서버에서 계산하므로 페이지 나누기와 관계없이 수치가 정확하다.
// 이 집계는 탐색 그래프의 노드가 아니라, 발견 페이지 UI 의 카드와 필터가 읽는 숫자다.
type FindingStats struct {
	Total       int                 `json:"total"`
	Pending     int                 `json:"pending"`
	Critical    int                 `json:"critical"`
	High        int                 `json:"high"`
	Medium      int                 `json:"medium"`
	Low         int                 `json:"low"`
	VulnClasses []string            `json:"vulnclasses"`
	Tasks       []FindingTaskOption `json:"tasks"` // 발견이 있는 작업(「작업별」 드롭다운용)
}

// FindingTaskOption 은 발견 페이지의 작업 필터 항목 하나다. 발견이 하나 이상 있는
// 작업이며, 설명과 발견 개수를 담는다. 작업이 이후 삭제되면 설명은 비어 있다(발견 행은 남음).
// 그래서 프론트엔드는 id 로 되돌아간다.
// 이 항목은 발견 페이지 UI 의 작업 필터를 채우며, 작업과 그 발견 개수를 잇는다.
type FindingTaskOption struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"` // 선택적 작업 이름; 비어 있음=이름 없음
	Description string `json:"description"`
	Count       int    `json:"count"`
}

// FindingStats는 테이블 전체 개수(심각도별 + 대기)와
// 서로 다른 취약 분류를 정렬해 돌려준다.
func (d *DB) FindingStats() (*FindingStats, error) {
	st := &FindingStats{VulnClasses: []string{}, Tasks: []FindingTaskOption{}}
	err := d.QueryRow(`SELECT
		COUNT(*),
		COUNT(*) FILTER (WHERE status = 'pending'),
		COUNT(*) FILTER (WHERE severity = 'critical'),
		COUNT(*) FILTER (WHERE severity = 'high'),
		COUNT(*) FILTER (WHERE severity = 'medium'),
		COUNT(*) FILTER (WHERE severity = 'low')
		FROM findings`).Scan(&st.Total, &st.Pending, &st.Critical, &st.High, &st.Medium, &st.Low)
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT DISTINCT vulnclass FROM findings WHERE vulnclass <> '' ORDER BY vulnclass`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vc string
		if err := rows.Scan(&vc); err != nil {
			return nil, err
		}
		st.VulnClasses = append(st.VulnClasses, vc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 작업 드롭다운: 발견이 있는 작업. 설명(작업 삭제 후에는 비어 있고, 프론트엔드는 id 로 되돌아감)과 건수를 담고, 가장 최근에 발견이 생긴 것이 앞에 온다.
	trows, err := d.Query(`SELECT f.task_id, COALESCE(t.name, ''), COALESCE(t.description, ''), COUNT(*)
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id
		WHERE f.task_id IS NOT NULL
		GROUP BY f.task_id, t.name, t.description
		ORDER BY MAX(f.created_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var opt FindingTaskOption
		if err := trows.Scan(&opt.ID, &opt.Name, &opt.Description, &opt.Count); err != nil {
			return nil, err
		}
		st.Tasks = append(st.Tasks, opt)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	vulnclasses := make(map[string]bool, len(st.VulnClasses))
	for _, vulnclass := range st.VulnClasses {
		vulnclasses[vulnclass] = true
	}
	for _, aggregate := range archived {
		cold := aggregate.FindingStats
		st.Total += cold.Total
		st.Pending += cold.Pending
		st.Critical += cold.Critical
		st.High += cold.High
		st.Medium += cold.Medium
		st.Low += cold.Low
		for _, vulnclass := range cold.VulnClasses {
			if vulnclass != "" {
				vulnclasses[vulnclass] = true
			}
		}
	}
	st.VulnClasses = st.VulnClasses[:0]
	for vulnclass := range vulnclasses {
		st.VulnClasses = append(st.VulnClasses, vulnclass)
	}
	sort.Strings(st.VulnClasses)
	return st, nil
}

// GetFinding은 발견 한 줄을 돌려준다(task_description 조인과
// 마크다운 보고 전체). 그 id의 행이 없으면 nil이다. 목록 조회와 달리
// `report`도 고른다. 그 열은 상세 화면에만 필요하다.
func (d *DB) GetFinding(id int64) (*DBFinding, error) {
	f := &DBFinding{}
	var aidsJSON string
	err := d.QueryRow(`SELECT `+findingSelectCols+`, COALESCE(f.report, '')
		FROM findings f
		LEFT JOIN tasks t ON f.task_id = t.id
		WHERE f.id = $1`, id).Scan(
		&f.ID, &f.TaskID, &f.NodeID, &f.VulnClass, &f.Name, &f.Severity,
		&f.Summary, &f.Evidence, &f.Worker, &aidsJSON, &f.Status, &f.CreatedAt,
		&f.TaskDescription, &f.EvidenceVersion, &f.ReportEvidenceVersion, &f.TrafficCount, &f.Report)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(aidsJSON), &f.AssetIDs)
	return f, nil
}

// DeleteFinding 은 발견을 통째로 지운다. 독립 findings 행과
// 그 출발점인 탐색 노드(kind='finding')를 지우므로, 발견
// 목록, 작업별 발견 탭, 탐색 그래프에서 함께 사라진다. 노드를 지우면
// 그 edge 와 node_assets 가 연쇄 삭제되고, 그 노드를 가리키는 activity 는 null 이 된다. 반환값은
// 영향받은 행 수다(0 = 그 id 의 발견이 없음).
func (d *DB) DeleteFinding(id int64) (n int64, err error) {
	err = d.WithEvidenceTx(context.Background(), func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(tx, id, nil); err != nil {
			if errors.Is(err, ErrFindingNotFound) {
				return nil
			}
			return err
		}
		var nodeID sql.NullInt64
		if err := tx.QueryRow(`DELETE FROM findings WHERE id=$1 RETURNING node_id`, id).Scan(&nodeID); err != nil {
			return err
		}
		if nodeID.Valid {
			if _, err := tx.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND kind='finding'`, nodeID.Int64); err != nil {
				return err
			}
		}
		n = 1
		return nil
	})
	return
}

// DeleteFindingsByTask는 작업의 발견 행을 모두 지운다. 원래
// 탐색의 발견 노드는 작업의 탐색 부분 그래프를 버릴 때 따로 연쇄 삭제된다.
// 지운 행 수를 돌려준다.
func (d *DB) DeleteFindingsByTask(taskID int64) (int64, error) {
	res, err := d.Exec(`DELETE FROM findings WHERE task_id=$1`, taskID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetFindingStatus 는 발견 하나의 분류 상태를 바꾼다. 영향받은 행 수를 반환한다.
//
// 하위 setter: 상태만 바꾸고 전달 이벤트는 기록하지 않는다. 운영 코드에서 상태를 바꿀 때는
// SetFindingStatusWithNotify 를 탄다. 이 함수를 직접 호출하면 「상태 변경 전달」이 조용히 동작하지 않는다.
// 알림이 필요 없는 용도(인자 검증, 재검사 흐름)가 상태만 따로 돌릴 수 있도록 남겨 둔다.
func (d *DB) SetFindingStatus(id int64, status string) (int64, error) {
	res, err := d.Exec(`UPDATE findings SET status=$1 WHERE id=$2`, status, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetFindingReportByNodeID는 node_id가 맞는 독립 발견 행의 마크다운 보고를 넣는다.
// report_finding이 그 노드 id를 돌려주므로, 에이전트 도구가
// 방금 만든 발견을 가리킬 수 있다. 영향 받은 행 수를 돌려준다(행이 없으면 0).
func (d *DB) SetFindingReportByNodeID(nodeID int64, report string) (int64, error) {
	return d.SetFindingReportVersionByNodeID(context.Background(), nodeID, report, nil)
}

// setFindingCol 은 독립 발견 행의 텍스트 열 하나를 바꾸고, 새 값을
// 출발 탐색 노드의 payload 안 jsonKey 에도 그대로 반영한다. 그래서
// 작업별 발견 탭(이 테이블이 아니라 노드 payload 를 읽음)이 같은 값을 유지한다.
// 영향받은 행 수를 반환한다(그 id 의 발견이 없으면 0). 노드 동기화는 최선 노력이다.
// col 과 jsonKey 는 반드시 신뢰할 수 있는 상수여야 한다(SQL 에 그대로 끼워 넣음). 사용자 입력을
// 넘기면 안 된다.
func (d *DB) setFindingCol(id int64, col, jsonKey, val string) (int64, error) {
	var nodeID *int64
	err := d.QueryRow(`UPDATE findings SET `+col+`=$1 WHERE id=$2 RETURNING node_id`, val, id).Scan(&nodeID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if nodeID != nil {
		_, _ = d.Exec(`UPDATE exploration_nodes
			SET payload = jsonb_set(payload, '{`+jsonKey+`}', to_jsonb($1::text))
			WHERE id = $2`, val, *nodeID)
	}
	return 1, nil
}

// SetFindingSeverity는 발견 하나의 심각도를 고친다(노드 payload도 같이 맞춘다).
// 영향 받은 행 수를 돌려준다(그 id의 발견이 없으면 0).
func (d *DB) SetFindingSeverity(id int64, severity string) (int64, error) {
	return d.setFindingCol(id, "severity", "severity", severity)
}

// SetFindingName 은 발견 하나의 발견 이름을 바꾼다(노드 payload 동기화 포함). 빈 이름은
// 허용된다. 프론트엔드는 표시할 때 취약점 분류로 되돌아간다.
func (d *DB) SetFindingName(id int64, name string) (int64, error) {
	return d.setFindingCol(id, "name", "name", name)
}

// SetFindingVulnClass 는 발견 하나의 발견 분류를 바꾼다(노드 payload 동기화 포함).
func (d *DB) SetFindingVulnClass(id int64, vulnclass string) (int64, error) {
	return d.setFindingCol(id, "vulnclass", "vulnclass", vulnclass)
}

// FindingMeta는 독립 행 데이터(id, 분류 상태, 앵커된 자산)다.
// 작업별 보기가 탐색 노드 발견 위에 이 값을 붙인다.
type FindingMeta struct {
	TrafficCount int

	ID       int64
	Status   string
	AssetIDs []int64
}

// FindingMetaByNodeID는 작업의 발견 노드 id를 독립 행
// 메타데이터(상태 + 앵커된 자산 id)로 연결한다. 자산 저장소를 통하므로
// AssetStore만 가진 호출자(예: 에이전트 ToolSet)가 날 *DB 없이 닿을 수 있다.
func (a *AssetStore) FindingMetaByNodeID(taskID int64) (map[int64]FindingMeta, error) {
	return a.db.FindingMetaByNodeID(taskID)
}

// FindingMetaByNodeID 는 작업의 발견 노드 id 를 독립 행
// 메타데이터에 대응시킨다. 그래서 탐색 노드를 읽는 작업별 화면이
// 전역 발견 페이지와 같은 상태, 그리고 같은 앵커 자산을 보여주고 고칠 수 있다.
func (d *DB) FindingMetaByNodeID(taskID int64) (map[int64]FindingMeta, error) {
	out := map[int64]FindingMeta{}
	if taskID <= 0 {
		return out, nil
	}
	rows, err := d.Query(`SELECT node_id, id, COALESCE(status,'pending'), asset_ids, (SELECT count(*) FROM finding_traffic_bindings b WHERE b.finding_id=findings.id) FROM findings
		WHERE task_id=$1 AND node_id IS NOT NULL`, taskID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var nid int64
		var m FindingMeta
		var aidsJSON string
		if err := rows.Scan(&nid, &m.ID, &m.Status, &aidsJSON, &m.TrafficCount); err != nil {
			return out, err
		}
		_ = json.Unmarshal([]byte(aidsJSON), &m.AssetIDs)
		out[nid] = m
	}
	return out, rows.Err()
}
