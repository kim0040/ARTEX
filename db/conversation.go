package db

import (
	"database/sql"
	"time"
)

// ConvTokenSummary는 대화 하나의 토큰 합이다(kind='result' 행의 합).
// 프로파일과 created_at을 함께 담아, 대화 사용량을 대시보드의
// 프로파일별·일별 토큰 통계에 합친다. 그 통계는 원래 작업만 담는다.
type ConvTokenSummary struct {
	LLMProfileID     *int64 `json:"llm_profile_id"`
	CreatedAt        string `json:"created_at"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// ConversationTokenSummaries는 대화마다 한 줄로, result 행 토큰 합을 돌려준다
// (아직 끝난 실행이 없는 대화는 0).
func (d *DB) ConversationTokenSummaries() ([]ConvTokenSummary, error) {
	rows, err := d.Query(`
SELECT c.llm_profile_id, c.created_at::text,
       COALESCE(sum(ca.input_tokens),0), COALESCE(sum(ca.output_tokens),0),
       COALESCE(sum(ca.cache_read_tokens),0), COALESCE(sum(ca.cache_write_tokens),0)
FROM conversations c
LEFT JOIN conversation_activities ca ON ca.conversation_id = c.id AND ca.kind = 'result'
GROUP BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConvTokenSummary{}
	for rows.Next() {
		var s ConvTokenSummary
		var pid sql.NullInt64
		if err := rows.Scan(&pid, &s.CreatedAt, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens); err != nil {
			return nil, err
		}
		if pid.Valid {
			v := pid.Int64
			s.LLMProfileID = &v
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Conversation은 에이전트 키에 묶인 채팅 스레드 하나다.
// 탐색 그래프와는 따로 산다. schema.sql §I를 본다.
type Conversation struct {
	ID           int64      `json:"id"`
	AgentKey     string     `json:"agent_key"`
	Title        string     `json:"title"`
	LLMProfileID *int64     `json:"llm_profile_id,omitempty"`
	Pinned       bool       `json:"pinned"`
	PinnedAt     *time.Time `json:"pinned_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ConversationPatch는 포인터가 nil이 아닌 필드만 고친다.
type ConversationPatch struct {
	Title  *string
	Pinned *bool
}

const convCols = `id, agent_key, title, llm_profile_id, pinned_at, created_at, updated_at`

func scanConv(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	var pinnedAt sql.NullTime
	err := row.Scan(&c.ID, &c.AgentKey, &c.Title, &c.LLMProfileID, &pinnedAt, &c.CreatedAt, &c.UpdatedAt)
	if pinnedAt.Valid {
		c.Pinned = true
		c.PinnedAt = &pinnedAt.Time
	}
	return c, err
}

// CreateConversation은 agentKey의 채팅 스레드를 처음 제목과 함께 연다.
// llmProfileID가 nil이면 전역으로 활성인 프로파일을 쓴다.
func (d *DB) CreateConversation(agentKey, title string, llmProfileID *int64) (*Conversation, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 먼저 프로파일 없이 자식 행을 넣는다. 새 행은 프로파일 잠금을 잡기 전에
	// 이 트랜잭션만 소유한다. DeleteProfile의 자식 행 → 프로파일 행 순서와 맞춘다.
	c, err := scanConv(tx.QueryRow(`
INSERT INTO conversations(agent_key, title, llm_profile_id) VALUES ($1, $2, NULL)
RETURNING `+convCols, agentKey, title))
	if err != nil {
		return nil, err
	}
	if err := lockLLMProfileForReference(tx, llmProfileID); err != nil {
		return nil, err
	}
	if llmProfileID != nil {
		c, err = scanConv(tx.QueryRow(`UPDATE conversations SET llm_profile_id=$2
WHERE id=$1 RETURNING `+convCols, c.ID, llmProfileID))
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &c, nil
}

// UpdateConversationProfile은 대화의 LLM 프로파일 덮어쓰기를 넣거나 지운다.
func (d *DB) UpdateConversationProfile(id int64, llmProfileID *int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var lockedID int64
	if err := tx.QueryRow(`SELECT id FROM conversations WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID); err != nil {
		if err == sql.ErrNoRows {
			// 예전 UPDATE와 같이, 모르는 id는 아무 일도 하지 않는다.
			return tx.Commit()
		}
		return err
	}
	if err := lockLLMProfileForReference(tx, llmProfileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE conversations SET llm_profile_id=$2 WHERE id=$1`, id, llmProfileID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListConversations는 모든 스레드를, 최근에 고친 것부터 돌려준다.
func (d *DB) ListConversations() ([]*Conversation, error) {
	rows, err := d.Query(`SELECT ` + convCols + ` FROM conversations
ORDER BY (pinned_at IS NOT NULL) DESC, pinned_at DESC NULLS LAST, updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Conversation{}
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetConversation은 스레드 하나를 돌려준다. 없으면 nil, nil이다.
func (d *DB) GetConversation(id int64) (*Conversation, error) {
	c, err := scanConv(d.QueryRow(`SELECT `+convCols+` FROM conversations WHERE id=$1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpdateConversation은 제목·고정만 일부 고치고, 고친 행을 돌려준다.
// 이미 고정된 대화를 다시 고정해도 원래 고정 순서는 남긴다.
func (d *DB) UpdateConversation(id int64, patch ConversationPatch) (*Conversation, error) {
	c, err := scanConv(d.QueryRow(`UPDATE conversations SET
	title = CASE WHEN $2::boolean THEN $3 ELSE title END,
	pinned_at = CASE
		WHEN $4::boolean IS NULL THEN pinned_at
		WHEN $4::boolean THEN COALESCE(pinned_at, now())
		ELSE NULL
	END
WHERE id=$1
RETURNING `+convCols, id, patch.Title != nil, patch.Title, patch.Pinned))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// RenameConversation은 스레드 제목을 정한다. 첫 메시지로 제목을 자동으로 붙이거나
// 기존 호출자와 맞추려고 남겨 둔다.
func (d *DB) RenameConversation(id int64, title string) error {
	_, err := d.UpdateConversation(id, ConversationPatch{Title: &title})
	return err
}

// TouchConversation은 updated_at을 올려, 스레드가 목록 맨 위로 가게 한다.
func (d *DB) TouchConversation(id int64) error {
	_, err := d.Exec(`UPDATE conversations SET updated_at=now() WHERE id=$1`, id)
	return err
}

// DeleteConversation은 스레드를 지운다. 활동 기록은 외래 키로 함께 지워진다.
func (d *DB) DeleteConversation(id int64) error {
	_, err := d.Exec(`DELETE FROM conversations WHERE id=$1`, id)
	return err
}

// DeleteConversations는 있는 스레드를 한 문으로 지우고, 실제로 있던 id를 돌려준다.
// 자식 활동과 트리거 실행은 함께 지워진다.
func (d *DB) DeleteConversations(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return []int64{}, nil
	}
	rows, err := d.Query(`DELETE FROM conversations WHERE id=ANY($1::bigint[]) RETURNING id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deleted := make([]int64, 0, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		deleted = append(deleted, id)
	}
	return deleted, rows.Err()
}

// AppendConvActivity는 대화의 한 단계(사람 메시지 또는 에이전트 실행 단계)를 기록하고 id를 돌려준다.
// ExplorationStore.AppendActivity와 같지만 conversation_id가 키다.
// Activity 구조체를 다시 쓴다. 여기서 NodeID는 무시한다.
func (d *DB) AppendConvActivity(convID int64, a Activity) (int64, error) {
	var id int64
	err := d.QueryRow(`
INSERT INTO conversation_activities(conversation_id, worker, kind, tool, tool_use_id, is_error, summary, detail, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES ($1,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6,NULLIF($7,''),NULLIF($8,''),$9,$10,$11,$12)
RETURNING id`, convID, utf8Clean(a.Worker), utf8Clean(a.Kind), utf8Clean(a.Tool), utf8Clean(a.ToolUseID), a.IsError,
		utf8Clean(a.Summary), utf8Clean(a.Detail), a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens).Scan(&id)
	return id, err
}

// ConvActivityList는 sinceID보다 뒤의 대화 단계를 돌려준다(sinceID 자체는 제외).
// 요약 열만 담는다. 자세한 내용은 ConvActivityDetail이 나중에 읽는다.
func (d *DB) ConvActivityList(convID, sinceID int64, limit int) ([]Activity, int64, error) {
	if limit <= 0 {
		limit = 500
	}
	const cols = `id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), created_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens`
	rows, err := d.Query(`SELECT `+cols+`
FROM conversation_activities WHERE conversation_id=$1 AND id>$2 ORDER BY id LIMIT $3`, convID, sinceID, limit)
	if err != nil {
		return nil, sinceID, err
	}
	defer rows.Close()
	out := []Activity{}
	cursor := sinceID
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens); err != nil {
			return nil, sinceID, err
		}
		if a.ID > cursor {
			cursor = a.ID
		}
		out = append(out, a)
	}
	return out, cursor, rows.Err()
}

// ConvActivityPage는 역방향(최신 먼저) 페이지 하나를 돌려준다. id가 before보다
// 앞인 단계를 최대 limit개 본다(before 자체는 제외. before가 0 이하면 최신 페이지).
// 돌려주는 순서는 id 오름차순이다. hasMore는 이 창보다 더 오래된 단계가 있는지를 알려,
// 클라이언트가 위로 스크롤할 때 더 옛 기록을 멈출 수 있다.
// 요약 열만 담는다. 자세한 내용은 ConvActivityDetail이 나중에 읽는다.
func (d *DB) ConvActivityPage(convID, before int64, limit int) ([]Activity, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	const cols = `id, COALESCE(worker,''), COALESCE(kind,''), COALESCE(tool,''), COALESCE(tool_use_id,''), is_error, COALESCE(summary,''), created_at, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens`
	// 한 줄을 더 읽어, 이 창보다 오래된 기록이 남았는지 본다.
	rows, err := d.Query(`SELECT `+cols+`
FROM conversation_activities
WHERE conversation_id=$1 AND ($2 <= 0 OR id < $2)
ORDER BY id DESC LIMIT $3`, convID, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	desc := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheWriteTokens); err != nil {
			return nil, false, err
		}
		desc = append(desc, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(desc) > limit
	if hasMore {
		desc = desc[:limit]
	}
	// 최신 먼저인 창을 화면에 맞게 id 오름차순으로 뒤집는다.
	out := make([]Activity, len(desc))
	for i, a := range desc {
		out[len(desc)-1-i] = a
	}
	return out, hasMore, nil
}

// ConvActivityDetail은 한 단계의 자세한 내용 전체를 필요할 때 돌려준다.
func (d *DB) ConvActivityDetail(convID, id int64) (string, error) {
	var s sql.NullString
	err := d.QueryRow(`SELECT detail FROM conversation_activities WHERE id=$1 AND conversation_id=$2`, id, convID).Scan(&s)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return s.String, err
}
