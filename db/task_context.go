package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const IntentBlockedLLMQuota = "llm_quota_exhausted"

// TaskLLMProfile은 작업의 명시적 장애 조치 사슬에서 순서가 있는 항목 하나다.
type TaskLLMProfile struct {
	ProfileID   int64      `json:"profile_id"`
	Position    int        `json:"position"`
	Status      string     `json:"status"`
	LastError   string     `json:"last_error,omitempty"`
	ExhaustedAt *time.Time `json:"exhausted_at,omitempty"`
}

// TaskSource는 직접 연결된 작업 하나와 그 탐색을 가리킨다.
type TaskSource struct {
	TaskID        int64
	ExplorationID int64
	Description   string
	Goal          string
	Status        string
}

// TaskLLMTransition은 프로파일 하나를 할당량 소진으로 표시한, 작업 단위 공유 결과다.
// 명시적 사슬이 끝나면 NextProfileID는 nil이다.
type TaskLLMTransition struct {
	PreviousProfileID int64
	NextProfileID     *int64
	ChainExhausted    bool
	Advanced          bool
	// Stale는 실패한 호출이 provider를 고른 뒤에 사슬이 바뀌었다는 뜻이다.
	// 호출자는 새 사슬로 스트림 전 요청을 다시 시도할 수 있다.
	// 다만 이 결과의 전환을 보고하거나 저장하면 안 된다.
	Stale bool
}

func (d *DB) hydrateTaskContext(t *Task) error {
	if t == nil {
		return nil
	}
	// 사슬이 비어 있지 않은데 커서가 nil이면, 사슬 끝을 저장해 둔 표시다.
	// 앞의 준비된 항목으로 고치지 않는다. 사용자가 중간 프로파일을 직접 고르고
	// 그 뒤 후보를 모두 소진했을 수 있다.
	legacyProfileID, activeProfileID, revision, chain, err := d.taskLLMContext(t.ID)
	if err != nil {
		return err
	}
	sources, err := d.TaskSourceIDs(t.ID)
	if err != nil {
		return err
	}
	t.SourceTaskIDs = sources
	companies, err := d.TaskCompanyIDs(t.ID)
	if err != nil {
		return err
	}
	t.CompanyIDs = companies
	applyTaskLLMContext(t, legacyProfileID, activeProfileID, revision, chain)
	return nil
}

func applyTaskLLMContext(
	t *Task,
	legacyProfileID, activeProfileID *int64,
	revision int64,
	chain []TaskLLMProfile,
) {
	t.LLMProfileID = legacyProfileID
	t.ActiveLLMProfileID = activeProfileID
	t.LLMChainRevision = revision
	t.LLMProfileIDs = make([]int64, 0, len(chain))
	t.LLMFailoverState = "default"
	t.LLMFailoverReason = ""
	var latest *TaskLLMProfile
	activeReady := false
	for i := range chain {
		entry := chain[i]
		t.LLMProfileIDs = append(t.LLMProfileIDs, entry.ProfileID)
		if t.ActiveLLMProfileID != nil && entry.ProfileID == *t.ActiveLLMProfileID && entry.Status == "ready" {
			activeReady = true
		}
		if entry.ExhaustedAt != nil && (latest == nil || entry.ExhaustedAt.After(*latest.ExhaustedAt)) {
			copy := entry
			latest = &copy
		}
	}
	if len(chain) > 0 {
		if activeReady {
			t.LLMFailoverState = "ready"
		} else {
			t.LLMFailoverState = "chain_exhausted"
		}
	}
	if latest != nil {
		t.LLMFailoverReason = latest.LastError
	}
}

type taskBatchContext struct {
	legacyProfileID *int64
	activeProfileID *int64
	revision        int64
	chain           []TaskLLMProfile
	sourceIDs       []int64
	companyIDs      []int64
}

// hydrateTasksContext는 모든 작업의 LLM 사슬, 원본 작업, 기업 범위를 조회 세 번으로 읽는다.
// ListTasks는 예전에는 작업마다 이 조회를 해서, 시작과 작업 목록 채우기가 3N+1번 왕복으로 늘었다.
func (d *DB) hydrateTasksContext(tasks []*Task) error {
	if len(tasks) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(tasks))
	contexts := make(map[int64]*taskBatchContext, len(tasks))
	byID := make(map[int64]*Task, len(tasks))
	for _, task := range tasks {
		if task == nil {
			continue
		}
		ids = append(ids, task.ID)
		contexts[task.ID] = &taskBatchContext{}
		byID[task.ID] = task
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := d.Query(`
SELECT t.id, t.llm_profile_id, t.active_llm_profile_id, t.llm_chain_revision,
       p.profile_id, p.position, p.status, COALESCE(p.last_error,''), p.exhausted_at
FROM tasks t
LEFT JOIN task_llm_profiles p ON p.task_id=t.id
WHERE t.id=ANY($1::bigint[])
ORDER BY t.id, p.position NULLS LAST`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var taskID, revision int64
		var legacy, active, profileID, position sql.NullInt64
		var status, lastError sql.NullString
		var exhaustedAt sql.NullTime
		if err := rows.Scan(
			&taskID, &legacy, &active, &revision,
			&profileID, &position, &status, &lastError, &exhaustedAt,
		); err != nil {
			rows.Close()
			return err
		}
		context := contexts[taskID]
		if context == nil {
			continue
		}
		context.revision = revision
		if legacy.Valid {
			id := legacy.Int64
			context.legacyProfileID = &id
		}
		if active.Valid {
			id := active.Int64
			context.activeProfileID = &id
		}
		if profileID.Valid {
			entry := TaskLLMProfile{
				ProfileID: profileID.Int64,
				Position:  int(position.Int64),
				Status:    status.String,
				LastError: lastError.String,
			}
			if exhaustedAt.Valid {
				ts := exhaustedAt.Time
				entry.ExhaustedAt = &ts
			}
			context.chain = append(context.chain, entry)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = d.Query(`
SELECT task_id, source_task_id
FROM task_relations
WHERE task_id=ANY($1::bigint[])
ORDER BY task_id, created_at, source_task_id`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var taskID, sourceID int64
		if err := rows.Scan(&taskID, &sourceID); err != nil {
			rows.Close()
			return err
		}
		if context := contexts[taskID]; context != nil {
			context.sourceIDs = append(context.sourceIDs, sourceID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rows, err = d.Query(`
SELECT task_id, company_id
FROM task_scope
WHERE task_id=ANY($1::bigint[]) AND kind='company' AND company_id IS NOT NULL
ORDER BY task_id, id`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var taskID, companyID int64
		if err := rows.Scan(&taskID, &companyID); err != nil {
			rows.Close()
			return err
		}
		if context := contexts[taskID]; context != nil {
			context.companyIDs = append(context.companyIDs, companyID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for id, context := range contexts {
		task := byID[id]
		task.SourceTaskIDs = context.sourceIDs
		task.CompanyIDs = context.companyIDs
		applyTaskLLMContext(
			task,
			context.legacyProfileID,
			context.activeProfileID,
			context.revision,
			context.chain,
		)
	}
	return nil
}

// taskLLMContext는 호환 프로파일, 현재 커서, 순서 있는 사슬을 한 문으로 읽는다.
// 실행 중 사슬 수정은 한 번에 커밋된다. SQL 한 문이라 채우기가 맞는 스냅샷 하나를 본다.
// 옛 작업 커서와 새로 바뀐 사슬이 잠깐 섞이지 않는다.
func (d *DB) taskLLMContext(taskID int64) (*int64, *int64, int64, []TaskLLMProfile, error) {
	rows, err := d.Query(`
SELECT t.llm_profile_id, t.active_llm_profile_id, t.llm_chain_revision,
       p.profile_id, p.position, p.status, COALESCE(p.last_error,''), p.exhausted_at
FROM tasks t
LEFT JOIN task_llm_profiles p ON p.task_id=t.id
WHERE t.id=$1
ORDER BY p.position NULLS LAST`, taskID)
	if err != nil {
		return nil, nil, 0, nil, err
	}
	defer rows.Close()
	var (
		legacyProfileID *int64
		activeProfileID *int64
		revision        int64
		chain           []TaskLLMProfile
		found           bool
	)
	for rows.Next() {
		var legacy, active, profileID, position sql.NullInt64
		var rowRevision int64
		var status, lastError sql.NullString
		var exhaustedAt sql.NullTime
		if err := rows.Scan(&legacy, &active, &rowRevision, &profileID, &position, &status, &lastError, &exhaustedAt); err != nil {
			return nil, nil, 0, nil, err
		}
		if !found {
			found = true
			revision = rowRevision
			if legacy.Valid {
				id := legacy.Int64
				legacyProfileID = &id
			}
			if active.Valid {
				id := active.Int64
				activeProfileID = &id
			}
		}
		if !profileID.Valid {
			continue
		}
		entry := TaskLLMProfile{
			ProfileID: profileID.Int64,
			Position:  int(position.Int64),
			Status:    status.String,
			LastError: lastError.String,
		}
		if exhaustedAt.Valid {
			ts := exhaustedAt.Time
			entry.ExhaustedAt = &ts
		}
		chain = append(chain, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, nil, err
	}
	if !found {
		return nil, nil, 0, nil, sql.ErrNoRows
	}
	return legacyProfileID, activeProfileID, revision, chain, nil
}

func (d *DB) TaskSourceIDs(taskID int64) ([]int64, error) {
	rows, err := d.Query(`SELECT source_task_id FROM task_relations WHERE task_id=$1 ORDER BY created_at, source_task_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TaskCompanyIDs는 이 작업이 쓸 수 있는 자산 범위를 가진 기업을 돌려준다.
// 기준은 여전히 task_scope 행이다.
func (d *DB) TaskCompanyIDs(taskID int64) ([]int64, error) {
	rows, err := d.Query(`
SELECT company_id FROM task_scope
WHERE task_id=$1 AND kind='company' AND company_id IS NOT NULL
ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *DB) TaskSources(taskID int64) ([]TaskSource, error) {
	rows, err := d.Query(`
SELECT t.id, t.exploration_id, t.description, t.goal, t.status
FROM task_relations r
JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL
WHERE r.task_id=$1
ORDER BY r.created_at, r.source_task_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskSource
	for rows.Next() {
		var source TaskSource
		if err := rows.Scan(&source.TaskID, &source.ExplorationID, &source.Description, &source.Goal, &source.Status); err != nil {
			return nil, err
		}
		out = append(out, source)
	}
	return out, rows.Err()
}

func (d *DB) TaskLLMProfiles(taskID int64) ([]TaskLLMProfile, error) {
	rows, err := d.Query(`SELECT profile_id, position, status, COALESCE(last_error,''), exhausted_at
FROM task_llm_profiles WHERE task_id=$1 ORDER BY position`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskLLMProfile
	for rows.Next() {
		var entry TaskLLMProfile
		if err := rows.Scan(&entry.ProfileID, &entry.Position, &entry.Status, &entry.LastError, &entry.ExhaustedAt); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ReplaceTaskLLMProfiles는 명시적 작업 체인을 원자적으로 교체하고 재설정한다.
// activeProfileID=0은 첫 항목을 선택한다. 빈 목록은 기존의
// agent-binding/global 폴백 동작을 복원한다.
// 종료 상태(done/failed/timeout) 작업도 체인 변경을 허용한다. 작업이 끝난 뒤에도 주 Agent 대화는 이 체인을 타므로,
// 체인의 모델을 쓸 수 없을 때는 바꿀 수 있어야 한다. 그렇지 않으면 이미 완료된 작업과는 다시 상호작용할 수 없다.
func (d *DB) ReplaceTaskLLMProfiles(taskID int64, profileIDs []int64, activeProfileID int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 작업 → 프로파일 잠금 순서를 모든 작업 LLM 변경과 DeleteProfile이 같이 쓴다.
	// 아래 INSERT는 외래 키 때문에 llm_profiles에 KEY SHARE 잠금을 잡을 수 있다.
	// 그래서 그 전에 작업 행을 잠가야 한다.
	var lockedID int64
	if err := tx.QueryRow(`SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, taskID).Scan(&lockedID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("작업 %d 을(를) 찾을 수 없습니다", taskID)
		}
		return err
	}
	if _, err := tx.Exec(`DELETE FROM task_llm_profiles WHERE task_id=$1`, taskID); err != nil {
		return err
	}
	if err := insertTaskLLMProfiles(tx, taskID, profileIDs); err != nil {
		return err
	}
	if len(profileIDs) == 0 {
		activeProfileID = 0
	} else if activeProfileID == 0 {
		activeProfileID = profileIDs[0]
	}
	if activeProfileID > 0 {
		found := false
		for _, id := range profileIDs {
			found = found || id == activeProfileID
		}
		if !found {
			return fmt.Errorf("active LLM profile %d is not in the task chain", activeProfileID)
		}
	}
	var active any
	if activeProfileID > 0 {
		active = activeProfileID
	}
	if _, err := tx.Exec(`UPDATE tasks
SET active_llm_profile_id=$2, llm_profile_id=$2, llm_chain_revision=llm_chain_revision+1
WHERE id=$1`, taskID, active); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkTaskLLMProfileQuotaExhausted는 공유 작업 커서를 한 번 앞으로 보낸다.
// 더 옛 프로파일의 늦은 오류는 그 항목만 소진으로 적고,
// 다른 호출이 이미 고른 프로파일 너머로는 커서를 밀지 않는다.
func (d *DB) MarkTaskLLMProfileQuotaExhausted(taskID, profileID int64, reason string) (TaskLLMTransition, error) {
	return d.markTaskLLMProfileQuotaExhausted(taskID, profileID, 0, false, reason)
}

// MarkTaskLLMProfileQuotaExhaustedAtRevision은 그 호출을 시작할 때 본
// 사슬 스냅샷에 속할 때만 provider 실패를 적용한다.
func (d *DB) MarkTaskLLMProfileQuotaExhaustedAtRevision(taskID, profileID, revision int64, reason string) (TaskLLMTransition, error) {
	return d.markTaskLLMProfileQuotaExhausted(taskID, profileID, revision, true, reason)
}

func (d *DB) markTaskLLMProfileQuotaExhausted(taskID, profileID, revision int64, checkRevision bool, reason string) (TaskLLMTransition, error) {
	var out TaskLLMTransition
	tx, err := d.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var (
		active          sql.NullInt64
		currentRevision int64
	)
	if err := tx.QueryRow(`SELECT active_llm_profile_id, llm_chain_revision FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&active, &currentRevision); err != nil {
		return out, err
	}
	out.PreviousProfileID = profileID
	if checkRevision && revision != currentRevision {
		out.Stale = true
		return out, tx.Commit()
	}
	var (
		position    int
		entryStatus string
	)
	if err := tx.QueryRow(`SELECT position, status FROM task_llm_profiles WHERE task_id=$1 AND profile_id=$2`, taskID, profileID).
		Scan(&position, &entryStatus); err != nil {
		if err == sql.ErrNoRows {
			// 이 요청이 나가는 동안 사슬이 고쳐졌다. provider
			// 오류는 호출자에게 여전히 유효하지만, 새 사슬을 고치면 안 된다.
			return out, tx.Commit()
		}
		return out, err
	}
	// 커서가 끝에 닿은 뒤의 늦은 실패는 같은 결과를 다시 내야 한다.
	// 더 옛 진행 중 요청이, 사람이 고른 시작 위치 앞의 준비된 항목을 되살리지 못하게 한다.
	if !active.Valid {
		out.ChainExhausted = true
		return out, tx.Commit()
	}
	reason = truncateUTF8(strings.TrimSpace(reason), 1000)
	if entryStatus != "quota_exhausted" {
		if _, err := tx.Exec(`UPDATE task_llm_profiles
SET status='quota_exhausted', last_error=$3, exhausted_at=now()
WHERE task_id=$1 AND profile_id=$2`, taskID, profileID, reason); err != nil {
			return out, err
		}
	}
	if active.Valid && active.Int64 != profileID {
		next := active.Int64
		out.NextProfileID = &next
		return out, tx.Commit()
	}
	var next int64
	err = tx.QueryRow(`SELECT profile_id FROM task_llm_profiles
WHERE task_id=$1 AND position>$2 AND status='ready'
ORDER BY position LIMIT 1`, taskID, position).Scan(&next)
	switch err {
	case nil:
		out.Advanced = true
		out.NextProfileID = &next
		if _, err := tx.Exec(`UPDATE tasks
				SET active_llm_profile_id=$2, llm_profile_id=$2, llm_chain_revision=llm_chain_revision+1
				WHERE id=$1`, taskID, next); err != nil {
			return out, err
		}
	case sql.ErrNoRows:
		out.Advanced = true
		out.ChainExhausted = true
		if _, err := tx.Exec(`UPDATE tasks
				SET active_llm_profile_id=NULL, llm_profile_id=NULL, llm_chain_revision=llm_chain_revision+1
				WHERE id=$1`, taskID); err != nil {
			return out, err
		}
	default:
		return out, err
	}
	return out, tx.Commit()
}

func truncateUTF8(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func (s *ExplorationStore) SetIntentBlockedReason(id int64, reason string) error {
	_, err := s.db.Exec(`UPDATE exploration_nodes
SET state='blocked', blocked_reason=NULLIF($1,''), completed_at=now()
WHERE id=$2 AND exploration_id=$3 AND kind='intent'`, reason, id, s.expID)
	return err
}

func (s *ExplorationStore) ReopenIntentsByBlockedReason(reason string) (int64, error) {
	res, err := s.db.Exec(`UPDATE exploration_nodes
SET state='open', blocked_reason=NULL, completed_at=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='blocked' AND blocked_reason=$2`, s.expID, reason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
