package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---------- LLM 프로필 ----------

type LLMProfile struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Format        string  `json:"format"`
	BaseURL       string  `json:"base_url,omitempty"`
	Proxy         string  `json:"proxy,omitempty"` // LLM 아웃바운드 프록시(http/https/socks5);빈 값=환경 변수 사용
	Model         string  `json:"model"`
	APIKey        string  `json:"-"` // UI로 직렬화하지 않는다
	APIKeyHint    string  `json:"api_key_hint,omitempty"`
	RatePerSecond float64 `json:"rate_per_second"`
	RatePerMinute float64 `json:"rate_per_minute"`
	// ContextWindowK는 모델 컨텍스트 창이다. 단위는 K 토큰이고, 압축 임계값을 정할 때 쓴다.
	// 0이면 기본 200K를 쓰고, 상한은 1000(1M)이다.
	ContextWindowK int `json:"context_window_k"`
	// ThinkingType 이 사고 「스위치」를 따로 제어한다(thinking.type):"" = 보내지 않음(기본);
	// "disabled" = 명시적으로 끔; "enabled" = 켬. ReasoningEffort 와 분리된다.
	ThinkingType string `json:"thinking_type"`
	// ReasoningEffort 가 사고 「강도」를 따로 제어한다:"" = 보내지 않음(기본);
	// "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도. agent.Config.NewProvider 를 본다.
	ReasoningEffort string `json:"reasoning_effort"`
	IsDefault       bool   `json:"is_default"`
	// Priority는 장애 조치 사슬의 순서다. 클수록 앞이다. 활성 프로필
	// (IsDefault)은 이 값과 상관없이 항상 사슬의 맨 앞에 선다.
	Priority int `json:"priority"`
	// PoolExclude=true면 이 프로필은 장애 조치 사슬에 넣지 않는다. 에이전트나 작업이
	// 직접 묶으면 그대로 쓸 수 있고, 대체 대상으로는 뽑히지 않는다.
	PoolExclude bool `json:"pool_exclude"`
	// Streaming은 전송 방식을 고른다. true(기본)는 스트리밍(SSE)이고,
	// false는 진짜 비스트리밍이다(stream:false, Provider.Complete의 JSON 응답 하나).
	// 비스트리밍은 불안정한 게이트웨이 SSE를 피하지만, 실행 중 실시간 진행은 없다.
	// agent.Config.Stream과 같다.
	Streaming bool `json:"streaming"`
	// MaxTokens는 답변 하나의 출력 토큰 상한이다. 0이면 상한을 보내지 않고
	// 엔드포인트 기본값을 쓴다(예전 동작). ContextWindowK는 모델 전체 용량이라
	// 로컬에서 압축 크기를 정할 때만 쓰고, 이 값은 요청마다 따라간다.
	MaxTokens int `json:"max_tokens"`
	// MaxTokensField는 MaxTokens를 실어 보내는 요청 키를 고른다. format이
	// "openai"일 때만 쓴다. ""는 max_tokens(기본)이고, "max_completion_tokens"는
	// 새 키다. OpenAI 추론 모델이 이 키를 요구하고, 예산은 추론 토큰과 보이는 출력을 같이 덮는다.
	// anthropic과 openai-responses는 필드 이름을 스스로 정하므로 이 값을 무시한다.
	MaxTokensField string `json:"max_tokens_field"`
	// SessionHeaderKey가 비어 있지 않으면, 이 프로필로 만드는 요청마다 그 이름의 HTTP 헤더를 보낸다.
	// 값은 이번 실행의 세션 id다(채팅 대화 / 워커 의도). 세션 id 헤더로 프롬프트 캐시나
	// 고정 라우팅을 하는 게이트웨이용이다. ""이면 보내지 않는다.
	// agent.Config.SessionHeaderKey와 같다.
	SessionHeaderKey string `json:"session_header_key"`
	// Retry는 이 프로필이 재시도 사다리에서 맡는 몫을 덮어쓴다. 0이면
	// 전역 정책(LLMRetryPolicy)을 물려받아, 손대지 않은 프로필은 예전과 같다.
	// RetryOverride를 본다.
	Retry RetryOverride `json:"retry"`
}

// RetryOverride 는 한 프로필이 엔드포인트별 세 재시도 계층을 선택적으로 덮어쓰는 값이다.
// 계층은 연결 수립(connect) / 빈 응답(empty) / 같은 provider 안전 창
// (stream) 이다. 각 규칙의 0 값은 "전역 정책을 상속" 을 뜻한다.
// -1 / 0 / >0 의미는 RetryRule 을 본다. 이 값은 워커와 플래너가 LLM 호출을 이어 탐색 그래프에 사실과 발견을 남기게 한다.
type RetryOverride struct {
	Connect RetryRule `json:"connect"`
	Empty   RetryRule `json:"empty"`
	Stream  RetryRule `json:"stream"`
}

// profileCols는 목록 쿼리가 같이 쓰는 읽기 열이다(힌트만, api 키 없음).
// profileColsKey는 한 행을 읽을 때 api_key를 포함한 같은 목록이다.
const profileRetryCols = `COALESCE(retry_connect_attempts,0),COALESCE(retry_connect_interval_ms,0),COALESCE(retry_empty_attempts,0),COALESCE(retry_empty_interval_ms,0),COALESCE(retry_stream_attempts,0),COALESCE(retry_stream_interval_ms,0)`
const profileCols = `id,name,format,COALESCE(base_url,''),COALESCE(proxy,''),model,COALESCE(api_key_hint,''),rate_per_second,rate_per_minute,context_window_k,COALESCE(reasoning_effort,''),is_default,priority,pool_exclude,COALESCE(thinking_type,''),COALESCE(streaming,true),COALESCE(max_tokens,0),COALESCE(max_tokens_field,''),COALESCE(session_header_key,''),` + profileRetryCols
const profileColsKey = `id,name,format,COALESCE(base_url,''),COALESCE(proxy,''),model,COALESCE(api_key,''),rate_per_second,rate_per_minute,context_window_k,COALESCE(reasoning_effort,''),is_default,priority,pool_exclude,COALESCE(thinking_type,''),COALESCE(streaming,true),COALESCE(max_tokens,0),COALESCE(max_tokens_field,''),COALESCE(session_header_key,''),` + profileRetryCols

// scanProfile은 profileCols / profileColsKey 열 순서대로 한 행을 읽는다.
// 7번째 열은 호출자가 어느 목록을 썼는지에 따라 APIKeyHint 또는 APIKey에 들어간다.
func scanProfile(sc interface{ Scan(...any) error }, into *string, p *LLMProfile) error {
	return sc.Scan(&p.ID, &p.Name, &p.Format, &p.BaseURL, &p.Proxy, &p.Model, into,
		&p.RatePerSecond, &p.RatePerMinute, &p.ContextWindowK, &p.ReasoningEffort, &p.IsDefault, &p.Priority, &p.PoolExclude, &p.ThinkingType, &p.Streaming,
		&p.MaxTokens, &p.MaxTokensField, &p.SessionHeaderKey,
		&p.Retry.Connect.Attempts, &p.Retry.Connect.IntervalMS,
		&p.Retry.Empty.Attempts, &p.Retry.Empty.IntervalMS,
		&p.Retry.Stream.Attempts, &p.Retry.Stream.IntervalMS)
}

func (d *DB) ListProfiles() ([]*LLMProfile, error) {
	rows, err := d.Query(`SELECT ` + profileCols + ` FROM llm_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LLMProfile
	for rows.Next() {
		var p LLMProfile
		if err := scanProfile(rows, &p.APIKeyHint, &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ActiveProfile은 기본(활성) 프로필을 api 키와 함께 돌려준다. 없으면 nil.
func (d *DB) ActiveProfile() (*LLMProfile, error) {
	var p LLMProfile
	err := scanProfile(d.QueryRow(`SELECT `+profileColsKey+` FROM llm_profiles WHERE is_default LIMIT 1`), &p.APIKey, &p)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// ProfileByID는 id로 프로필 하나와 api 키를 돌려준다. 없으면 nil.
// 기본이 아닌 LLM 프로필로 작업을 돌릴 때 쓴다.
func (d *DB) ProfileByID(id int64) (*LLMProfile, error) {
	var p LLMProfile
	err := scanProfile(d.QueryRow(`SELECT `+profileColsKey+` FROM llm_profiles WHERE id=$1`, id), &p.APIKey, &p)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// PoolProfiles는 실행 순서의 장애 조치 사슬을 api 키와 함께 돌려준다.
// 활성 프로필이 먼저이고, 그다음 제외되지 않은 키가 있는 프로필을
// priority 내림차순(같으면 id 오름차순으로 안정)으로 붙인다. api 키가 없으면
// 요청을 처리할 수 없어 사슬에 넣지 않는다. 이 순서가 곧 정책이다. 호출자는 앞부터 걷는다.
func (d *DB) PoolProfiles() ([]*LLMProfile, error) {
	rows, err := d.Query(`SELECT ` + profileColsKey + ` FROM llm_profiles
WHERE COALESCE(api_key,'') <> '' AND (is_default OR NOT pool_exclude)
ORDER BY is_default DESC, priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LLMProfile
	for rows.Next() {
		var p LLMProfile
		if err := scanProfile(rows, &p.APIKey, &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// SaveProfile은 프로필을 넣거나(id==0) 고친다. 수정할 때 apiKey가 비어 있으면 기존 키를 유지한다.
func (d *DB) SaveProfile(p *LLMProfile) (int64, error) {
	hint := p.APIKeyHint
	if len(p.APIKey) >= 4 {
		hint = "…" + p.APIKey[len(p.APIKey)-4:]
	}
	r := p.Retry.Clamped()
	if p.ID == 0 {
		var id int64
		err := d.QueryRow(`INSERT INTO llm_profiles(name,format,base_url,proxy,model,api_key,api_key_hint,rate_per_second,rate_per_minute,context_window_k,reasoning_effort,priority,pool_exclude,thinking_type,streaming,max_tokens,max_tokens_field,session_header_key,retry_connect_attempts,retry_connect_interval_ms,retry_empty_attempts,retry_empty_interval_ms,retry_stream_attempts,retry_stream_interval_ms)
VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,NULLIF($6,''),NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24) RETURNING id`,
			p.Name, p.Format, p.BaseURL, p.Proxy, p.Model, p.APIKey, hint, p.RatePerSecond, p.RatePerMinute, p.ContextWindowK, p.ReasoningEffort, p.Priority, p.PoolExclude, p.ThinkingType, p.Streaming, p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey,
			r.Connect.Attempts, r.Connect.IntervalMS, r.Empty.Attempts, r.Empty.IntervalMS, r.Stream.Attempts, r.Stream.IntervalMS).Scan(&id)
		return id, err
	}
	if p.APIKey == "" {
		_, err := d.Exec(`UPDATE llm_profiles SET name=$1,format=$2,base_url=NULLIF($3,''),proxy=NULLIF($4,''),model=$5,rate_per_second=$6,rate_per_minute=$7,context_window_k=$8,reasoning_effort=$9,priority=$10,pool_exclude=$11,thinking_type=$12,streaming=$13,max_tokens=$14,max_tokens_field=$15,session_header_key=$16,retry_connect_attempts=$17,retry_connect_interval_ms=$18,retry_empty_attempts=$19,retry_empty_interval_ms=$20,retry_stream_attempts=$21,retry_stream_interval_ms=$22 WHERE id=$23`,
			p.Name, p.Format, p.BaseURL, p.Proxy, p.Model, p.RatePerSecond, p.RatePerMinute, p.ContextWindowK, p.ReasoningEffort, p.Priority, p.PoolExclude, p.ThinkingType, p.Streaming, p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey,
			r.Connect.Attempts, r.Connect.IntervalMS, r.Empty.Attempts, r.Empty.IntervalMS, r.Stream.Attempts, r.Stream.IntervalMS, p.ID)
		return p.ID, err
	}
	_, err := d.Exec(`UPDATE llm_profiles SET name=$1,format=$2,base_url=NULLIF($3,''),proxy=NULLIF($4,''),model=$5,api_key=$6,api_key_hint=$7,rate_per_second=$8,rate_per_minute=$9,context_window_k=$10,reasoning_effort=$11,priority=$12,pool_exclude=$13,thinking_type=$14,streaming=$15,max_tokens=$16,max_tokens_field=$17,session_header_key=$18,retry_connect_attempts=$19,retry_connect_interval_ms=$20,retry_empty_attempts=$21,retry_empty_interval_ms=$22,retry_stream_attempts=$23,retry_stream_interval_ms=$24 WHERE id=$25`,
		p.Name, p.Format, p.BaseURL, p.Proxy, p.Model, p.APIKey, hint, p.RatePerSecond, p.RatePerMinute, p.ContextWindowK, p.ReasoningEffort, p.Priority, p.PoolExclude, p.ThinkingType, p.Streaming, p.MaxTokens, p.MaxTokensField, p.SessionHeaderKey,
		r.Connect.Attempts, r.Connect.IntervalMS, r.Empty.Attempts, r.Empty.IntervalMS, r.Stream.Attempts, r.Stream.IntervalMS, p.ID)
	return p.ID, err
}

var (
	ErrActiveLLMProfileDelete      = errors.New("현재 활성화된 LLM 구성은 삭제할 수 없습니다. 먼저 다른 구성을 활성화하세요")
	ErrLLMProfileReferencesChanged = errors.New("LLM profile references changed while deleting; retry the request")
	ErrLLMProfileNotFound          = errors.New("LLM 구성을 찾을 수 없습니다")
)

func (d *DB) DeleteProfile(id int64) error {
	return d.DeleteProfileContext(context.Background(), id)
}

// DeleteProfileContext는 기본이 아닌 프로필을 지우면서 작업의 장애 조치 커서는 남긴다.
// 처음 훑는 사이와 프로필 행을 잠그기 전에 작업, 에이전트, 대화가 그 프로필을 가리키기 시작할 수 있어 참조 변경은 다시 시도한다.
// 상한을 두어, 계속 바뀌는 작업이 HTTP 요청을 영원히 붙잡지 않게 한다.
func (d *DB) DeleteProfileContext(ctx context.Context, id int64) error {
	const (
		maxAttempts = 8
		maxDuration = 15 * time.Second
	)
	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := d.deleteProfile(ctx, id)
		if err != nil || !retry {
			return err
		}
	}
	return fmt.Errorf("%w: profile %d", ErrLLMProfileReferencesChanged, id)
}

func (d *DB) deleteProfile(ctx context.Context, id int64) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 참조를 바꾸는 쪽은 외래 키 검사가 llm_profiles 행을 잠그기 전에
	// 작업/에이전트/대화 행을 먼저 잠근다. 삭제도 같은 순서를 지킨다. 예전처럼
	// 프로필 → 자식을 잠그면, 설정 쪽의 자식 → 프로필 순서와 교착된다.
	// 정렬해 두면 사슬이 여러 작업에 겹쳐도 동시에 지우는 순서가 안정된다.
	lockedTasks, err := lockProfileReferenceRows(ctx, tx, `SELECT t.id
FROM tasks t
WHERE t.llm_profile_id=$1
   OR t.active_llm_profile_id=$1
   OR EXISTS (
       SELECT 1 FROM task_llm_profiles x
       WHERE x.task_id=t.id AND x.profile_id=$1
   )
ORDER BY t.id
FOR UPDATE OF t`, id)
	if err != nil {
		return false, err
	}
	lockedAgents, err := lockProfileReferenceRows(ctx, tx, `SELECT id FROM agents
WHERE llm_profile_id=$1
ORDER BY id
FOR UPDATE`, id)
	if err != nil {
		return false, err
	}
	lockedConversations, err := lockProfileReferenceRows(ctx, tx, `SELECT id FROM conversations
WHERE llm_profile_id=$1
ORDER BY id
FOR UPDATE`, id)
	if err != nil {
		return false, err
	}

	var isDefault bool
	if err := tx.QueryRowContext(ctx, `SELECT is_default FROM llm_profiles WHERE id=$1 FOR UPDATE`, id).Scan(&isDefault); err != nil {
		if err == sql.ErrNoRows {
			return false, ErrLLMProfileNotFound
		}
		return false, err
	}
	if isDefault {
		return false, ErrActiveLLMProfileDelete
	}

	type affectedTask struct {
		id        int64
		position  int
		wasActive bool
	}
	// 첫 문장이 스냅샷을 찍은 뒤, 이 트랜잭션이 프로필 잠금을 얻기 전에
	// 작업이 새 참조를 커밋했을 수 있다. 지금은 프로필 잠금이 추가 참조를 막는다.
	// 그 커밋된 작업이 작업 우선 잠금 집합에 없었으면 다시 시도한다.
	// 프로필 잠금을 쥔 채 새 작업 잠금을 잡지 않는다. 그러면 순서가 다시 뒤집힌다.
	rows, err := tx.QueryContext(ctx, `SELECT ref_kind, ref_id FROM (
	SELECT 'task'::text AS ref_kind, t.id AS ref_id
FROM tasks t
WHERE t.llm_profile_id=$1
   OR t.active_llm_profile_id=$1
   OR EXISTS (
       SELECT 1 FROM task_llm_profiles x
       WHERE x.task_id=t.id AND x.profile_id=$1
   )
	UNION ALL
	SELECT 'agent', a.id FROM agents a WHERE a.llm_profile_id=$1
	UNION ALL
	SELECT 'conversation', c.id FROM conversations c WHERE c.llm_profile_id=$1
) refs
ORDER BY ref_kind, ref_id`, id)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var (
			kind  string
			rowID int64
		)
		if err := rows.Scan(&kind, &rowID); err != nil {
			rows.Close()
			return false, err
		}
		locked := false
		switch kind {
		case "task":
			_, locked = lockedTasks[rowID]
		case "agent":
			_, locked = lockedAgents[rowID]
		case "conversation":
			_, locked = lockedConversations[rowID]
		}
		if !locked {
			rows.Close()
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT x.task_id, x.position, COALESCE(t.active_llm_profile_id=$1, false)
FROM task_llm_profiles x
JOIN tasks t ON t.id=x.task_id
WHERE x.profile_id=$1
ORDER BY x.task_id`, id)
	if err != nil {
		return false, err
	}
	var affected []affectedTask
	for rows.Next() {
		var task affectedTask
		if err := rows.Scan(&task.id, &task.position, &task.wasActive); err != nil {
			rows.Close()
			return false, err
		}
		affected = append(affected, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM llm_profiles WHERE id=$1`, id); err != nil {
		return false, err
	}
	// 지운 커서 뒤의 후속만 후보가 된다. 없으면
	// 명시 사슬을 비워 작업이 에이전트/전역 제공자로 돌아간다.
	// 사람이 고른 커서보다 앞의 프로필은 되살리지 않는다.
	for _, task := range affected {
		if !task.wasActive {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET llm_chain_revision=llm_chain_revision+1 WHERE id=$1`, task.id); err != nil {
				return false, err
			}
			continue
		}
		var next int64
		err := tx.QueryRowContext(ctx, `SELECT profile_id FROM task_llm_profiles
WHERE task_id=$1 AND position>$2 AND status='ready'
ORDER BY position
LIMIT 1`, task.id, task.position).Scan(&next)
		if err != nil && err != sql.ErrNoRows {
			return false, err
		}
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, `DELETE FROM task_llm_profiles WHERE task_id=$1`, task.id); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks
SET active_llm_profile_id=NULL, llm_profile_id=NULL, llm_chain_revision=llm_chain_revision+1
WHERE id=$1`, task.id); err != nil {
				return false, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks
SET active_llm_profile_id=$2, llm_profile_id=$2, llm_chain_revision=llm_chain_revision+1
WHERE id=$1`, task.id, next); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func lockProfileReferenceRows(ctx context.Context, tx *sql.Tx, query string, profileID int64) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, query, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	locked := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		locked[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return locked, nil
}

// SetActiveProfile은 프로필 하나를 전역 기본으로 만든다. 기본은 항상 하나다.
func (d *DB) SetActiveProfile(id int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE llm_profiles SET is_default=true WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLLMProfileNotFound
	}
	return tx.Commit()
}

// ---------- 에이전트 / 프롬프트 ----------

type Agent struct {
	ID               int64  `json:"id"`
	Key              string `json:"key"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Role             string `json:"role"`
	Builtin          bool   `json:"builtin"`
	Enabled          bool   `json:"enabled"`
	LLMProfileID     *int64 `json:"llm_profile_id"`    // 바인딩된 LLM 설정;nil=작업/세션 pin 을 따르고, 이어서 전역 활성으로 폴백
	MaxTurns         int    `json:"max_turns"`         // 한 번 실행의 최대 라운드;0=제한 없음
	RunSecs          int    `json:"run_seconds"`       // worker 한 번 실행의 벽시계 상한(초);0=제한 없음
	WebSearch        bool   `json:"web_search"`        // 네트워크 검색 사용 여부(시스템 전역 스위치의 제어를 받음)
	InteractiveShell bool   `json:"interactive_shell"` // 대화형 shell 사용 여부(지속 PTY 세션 도구 계열)
	WrapupPrompt     string `json:"wrapup_prompt"`     // 마무리 프롬프트(시간 초과/단계 수 소진 시의 settlement 안내);빈 값=코드 내장 기본값
	WrapupMaxTurns   int    `json:"wrapup_max_turns"`  // 마무리 단계 자신의 라운드 예산;0=코드 내장 기본값(agent 별)
	// 작업급 시간 초과 마무리 문구(per-run 과 별도인 두 벌;워커/플래너만 사용);빈 값/0=코드 내장 기본값.
	TaskTimeoutWrapupPrompt   string `json:"task_timeout_wrapup_prompt"`
	TaskTimeoutWrapupMaxTurns int    `json:"task_timeout_wrapup_max_turns"`
	// P3 트리거 후처리 전략(사용자 정의 agent 에만 의미 있음):
	// TriggerRunMode  serial|parallel — 직렬 대기열 / 트리거마다 각자 세션 하나를 동시 실행
	// TriggerMergeMode by_task|all|none — serial 에서만 사용:같은 작업 병합 / 전부 병합 / 병합 안 함
	// TriggerMaxParallel — parallel 에서만 쓰는 agent 별 동시 실행 상한;0=제한 없음
	TriggerRunMode     string `json:"trigger_run_mode"`
	TriggerMergeMode   string `json:"trigger_merge_mode"`
	TriggerMaxParallel int    `json:"trigger_max_parallel"`
}

const agentCols = `id,key,name,COALESCE(description,''),role,builtin,enabled,COALESCE(max_turns,0),COALESCE(run_seconds,600),COALESCE(web_search,false),COALESCE(interactive_shell,false),COALESCE(wrapup_prompt,''),COALESCE(wrapup_max_turns,0),COALESCE(task_timeout_wrapup_prompt,''),COALESCE(task_timeout_wrapup_max_turns,0),COALESCE(trigger_run_mode,'serial'),COALESCE(trigger_merge_mode,'all'),COALESCE(trigger_max_parallel,5),llm_profile_id`

func scanAgent(sc interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var prof sql.NullInt64 // llm_profile_id 는 비울 수 있음:바인딩되지 않으면 NULL
	err := sc.Scan(&a.ID, &a.Key, &a.Name, &a.Description, &a.Role, &a.Builtin, &a.Enabled, &a.MaxTurns, &a.RunSecs, &a.WebSearch, &a.InteractiveShell, &a.WrapupPrompt, &a.WrapupMaxTurns, &a.TaskTimeoutWrapupPrompt, &a.TaskTimeoutWrapupMaxTurns, &a.TriggerRunMode, &a.TriggerMergeMode, &a.TriggerMaxParallel, &prof)
	if err == nil && prof.Valid {
		v := prof.Int64
		a.LLMProfileID = &v
	}
	return &a, err
}

func (d *DB) ListAgents() ([]*Agent, error) {
	rows, err := d.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) GetAgentByKey(key string) (*Agent, error) {
	a, err := scanAgent(d.QueryRow(`SELECT `+agentCols+` FROM agents WHERE key=$1`, key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// AgentBindingCounts 는 몇 개의 묶음 쿼리로 agent 별 바인딩 개수를 반환한다
// (NO N+1): agent id 로 키된 보이는 MCP 서버와 skill, 그리고 agent key 로 키된 바인딩된 도구
// (tools.agents 는 agent key 의 JSONB 배열). 빠진 키는
// 0 을 뜻한다. 에이전트 카드에 "MCP N · Skill N · 도구 N" 을 표시하는 데 쓴다.
func (d *DB) AgentBindingCounts() (mcp map[int64]int, skill map[int64]int, tools map[string]int, err error) {
	mcp, skill, tools = map[int64]int{}, map[int64]int{}, map[string]int{}
	byID := func(q string, into map[int64]int) error {
		rows, e := d.Query(q)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var n int
			if e := rows.Scan(&id, &n); e != nil {
				return e
			}
			into[id] = n
		}
		return rows.Err()
	}
	if err = byID(`SELECT agent_id, count(DISTINCT resource_id) FROM agent_visibility WHERE resource_kind='mcp' AND enabled GROUP BY agent_id`, mcp); err != nil {
		return
	}
	if err = byID(`SELECT agent_id, count(*) FROM agent_skill_visibility WHERE enabled GROUP BY agent_id`, skill); err != nil {
		return
	}
	rows, e := d.Query(`SELECT elem, count(*) FROM tools, jsonb_array_elements_text(agents) AS elem GROUP BY elem`)
	if e != nil {
		err = e
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if e := rows.Scan(&k, &n); e != nil {
			err = e
			return
		}
		tools[k] = n
	}
	err = rows.Err()
	return
}

// CreateAgent는 역할이 'assistant'인 사용자 정의(builtin=false) 대화 에이전트를 넣는다.
// 새 행을 돌려준다. key/name 검사는 호출자가 먼저 하고, DB는 키 문자 집합, 유일함, 역할 제약을 지킨다.
func (d *DB) CreateAgent(key, name, description string) (*Agent, error) {
	a, err := scanAgent(d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled)
VALUES ($1, $2, NULLIF($3,''), 'assistant', false, true)
RETURNING `+agentCols, key, name, description))
	if err != nil {
		return nil, err
	}
	return a, nil
}

// UpdateAgentMeta는 사용자 정의 에이전트의 표시 이름과 설명을 고친다.
// 내장 에이전트는 건드리지 않는다(호출자와 builtin 플래그가 막는다).
func (d *DB) UpdateAgentMeta(key, name, description string) error {
	_, err := d.Exec(`UPDATE agents SET name=$2, description=NULLIF($3,'') WHERE key=$1 AND builtin=false`, key, name, description)
	return err
}

// DeleteAgent는 사용자 정의 에이전트를 지운다. 내장 에이전트는 builtin=false 조건으로 보호된다.
// agent_prompts / agent_prompt_vars / 가시성 행은 외래 키로 함께 지워지고, tools.agents 바인딩은 호출자가 정리한다.
func (d *DB) DeleteAgent(key string) error {
	_, err := d.Exec(`DELETE FROM agents WHERE key=$1 AND builtin=false`, key)
	return err
}

// SetAgentMaxTurns는 에이전트의 max_turns를 고친다(0 = 제한 없음).
func (d *DB) SetAgentMaxTurns(key string, maxTurns int) error {
	if maxTurns < 0 {
		maxTurns = 0
	}
	_, err := d.Exec(`UPDATE agents SET max_turns=$1 WHERE key=$2`, maxTurns, key)
	return err
}

// SetAgentLLMProfile은 에이전트를 특정 LLM 프로필에 묶거나(id != nil), 묶음을 풀어(id == nil)
// 작업/대화 핀을 따르게 한다. 그것도 없으면 전역 활성 프로필을 쓴다.
// 실행 우선순위: 에이전트 묶음 → 작업/대화 핀 → 활성 프로필.
func (d *DB) SetAgentLLMProfile(key string, id *int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// DeleteProfile은 프로필 행보다 참조 행을 먼저 잠근다. 여기도 같은 순서를 지켜
	// 동시에 다시 묶을 때 자식/프로필 교착이 나지 않게 한다.
	var agentID int64
	if err := tx.QueryRow(`SELECT id FROM agents WHERE key=$1 FOR UPDATE`, key).Scan(&agentID); err != nil {
		if err == sql.ErrNoRows {
			// 예전 UPDATE와 같다. 모르는 키는 아무 일도 하지 않는다.
			return tx.Commit()
		}
		return err
	}
	if err := lockLLMProfileForReference(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agents SET llm_profile_id=$1 WHERE id=$2`, id, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// lockLLMProfileForReference는 공유 잠금 순서에서 프로필 쪽을 분명히 한다.
// 호출자는 이미 참조하는 자식 행을 잠그고 있어야 한다.
func lockLLMProfileForReference(tx *sql.Tx, profileID *int64) error {
	if profileID == nil {
		return nil
	}
	var lockedID int64
	if err := tx.QueryRow(`SELECT id FROM llm_profiles WHERE id=$1 FOR KEY SHARE`, *profileID).Scan(&lockedID); err != nil {
		if err == sql.ErrNoRows {
			return ErrLLMProfileNotFound
		}
		return err
	}
	return nil
}

// SetAgentWebSearch는 에이전트가 네트워크 검색을 쓸지 바꾼다.
// 전역 웹 검색 스위치와 백엔드/키 설정은 그대로 위에 있다.
func (d *DB) SetAgentWebSearch(key string, on bool) error {
	_, err := d.Exec(`UPDATE agents SET web_search=$1 WHERE key=$2`, on, key)
	return err
}

// SetAgentInteractiveShell 은 agent 가 대화형 shell 을 받을지 바꾼다.
// (지속 PTY 세션) 도구 계열과 Bash 프롬프트 연동(docs 의 대화형 shell 설계 §14.2 를 본다).
func (d *DB) SetAgentInteractiveShell(key string, on bool) error {
	_, err := d.Exec(`UPDATE agents SET interactive_shell=$1 WHERE key=$2`, on, key)
	return err
}

// SetAgentWrapupPrompt는 에이전트의 마무리(정산) 프롬프트를 저장한다. 빈 문자열은
// "코드 내장 기본값을 쓴다"는 뜻이고, 실행 때 resolveWrapup이 정한다.
func (d *DB) SetAgentWrapupPrompt(key, prompt string) error {
	_, err := d.Exec(`UPDATE agents SET wrapup_prompt=$1 WHERE key=$2`, prompt, key)
	return err
}

// SetAgentWrapupMaxTurns는 마무리 단계 자체의 턴 예산을 저장한다. 0은
// "코드 내장 기본값을 쓴다"는 뜻이고, 실행 때 resolveWrapupTurns가 정한다.
func (d *DB) SetAgentWrapupMaxTurns(key string, n int) error {
	_, err := d.Exec(`UPDATE agents SET wrapup_max_turns=$1 WHERE key=$2`, n, key)
	return err
}

// SetAgentTaskTimeoutWrapup은 작업 시간 초과 마무리 프롬프트(빈 값 =
// 코드 내장 기본값, 워커/플래너만 있음)와 턴 예산(0 = 기본)을 저장한다.
// 실행 때 resolveTaskTimeoutWrapup / …Turns가 정한다.
func (d *DB) SetAgentTaskTimeoutWrapup(key, prompt string, maxTurns int) error {
	_, err := d.Exec(`UPDATE agents SET task_timeout_wrapup_prompt=$1, task_timeout_wrapup_max_turns=$2 WHERE key=$3`, prompt, maxTurns, key)
	return err
}

// SetAgentRunSeconds는 에이전트의 run_seconds 벽시계 예산을 고친다(0 = 제한 없음).
func (d *DB) SetAgentRunSeconds(key string, runSecs int) error {
	if runSecs < 0 {
		runSecs = 0
	}
	_, err := d.Exec(`UPDATE agents SET run_seconds=$1 WHERE key=$2`, runSecs, key)
	return err
}

// SetAgentTriggerBehavior 는 agent 의 P3 트리거 후처리 전략을 저장한다:
// runMode(serial|parallel) / mergeMode(by_task|all|none) / maxParallel(parallel 에서 사용,0=제한 없음).
// 열거형은 화이트리스트로 검사하고, 잘못된 값은 기본값으로 되돌려 오염된 데이터가 스케줄 pump 를 빗나가지 않게 한다.
func (d *DB) SetAgentTriggerBehavior(key, runMode, mergeMode string, maxParallel int) error {
	switch runMode {
	case "serial", "parallel":
	default:
		runMode = "serial"
	}
	switch mergeMode {
	case "by_task", "all", "none":
	default:
		mergeMode = "by_task"
	}
	if maxParallel < 0 {
		maxParallel = 0
	}
	_, err := d.Exec(`UPDATE agents SET trigger_run_mode=$1, trigger_merge_mode=$2, trigger_max_parallel=$3 WHERE key=$4`,
		runMode, mergeMode, maxParallel, key)
	return err
}

type PromptVar struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Source      string `json:"source"`
}

func (d *DB) PromptVars(agentID int64) ([]PromptVar, error) {
	rows, err := d.Query(`SELECT var_name,COALESCE(description,''),COALESCE(example,''),source FROM agent_prompt_vars WHERE agent_id=$1 ORDER BY var_name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptVar{}
	for rows.Next() {
		var v PromptVar
		if err := rows.Scan(&v.Name, &v.Description, &v.Example, &v.Source); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CurrentPrompt는 에이전트의 현재 템플릿 글을 돌려준다. 아직 없으면 "".
func (d *DB) CurrentPrompt(agentID int64) (string, error) {
	var tmpl sql.NullString
	err := d.QueryRow(`SELECT p.template_text FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id WHERE a.id=$1`, agentID).Scan(&tmpl)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return tmpl.String, err
}

// SeedPromptIfEmpty는 아직 프롬프트가 없을 때만(current_prompt_id IS NULL) 코드 기본 템플릿을 첫 버전으로 쓴다.
// SeedTool처럼 처음 한 번만 넣는다. 재시작해도 사람이 고친 프롬프트는 덮지 않는다.
// 버전이 하나라도 있으면 아무 일도 하지 않는다.
func (d *DB) SeedPromptIfEmpty(agentID int64, tmpl string) error {
	var cur sql.NullInt64
	if err := d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, agentID).Scan(&cur); err != nil {
		return err
	}
	if cur.Valid {
		return nil // 이미 심었거나 사람이 고쳤으면 그대로 둔다
	}
	_, err := d.SavePrompt(agentID, tmpl, "내장 기본값", "system")
	return err
}

// ResetPromptToDefault 는 코드 기본 템플릿을 새 버전으로 추가하고
// current 가 그 버전을 가리키게 한다. 명시적인 "내장 기본값으로 복원" 동작이다.
func (d *DB) ResetPromptToDefault(agentID int64, tmpl string) (int, error) {
	return d.SavePrompt(agentID, tmpl, "내장 기본값으로 복원", "system")
}

// SavePrompt는 새 버전을 뒤에 붙이고 current_prompt_id가 그 버전을 가리키게 한다.
func (d *DB) SavePrompt(agentID int64, template, note, by string) (int, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var ver int
	if err := tx.QueryRow(`SELECT COALESCE(max(version),0)+1 FROM agent_prompts WHERE agent_id=$1`, agentID).Scan(&ver); err != nil {
		return 0, err
	}
	var pid int64
	if err := tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by) VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,'')) RETURNING id`,
		agentID, ver, template, note, by).Scan(&pid); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, agentID); err != nil {
		return 0, err
	}
	return ver, tx.Commit()
}

type PromptVersion struct {
	Version   int       `json:"version"`
	Template  string    `json:"template_text"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"ts"`
}

func (d *DB) ListPromptVersions(agentID int64) ([]PromptVersion, error) {
	rows, err := d.Query(`SELECT version,template_text,COALESCE(note,''),created_at FROM agent_prompts WHERE agent_id=$1 ORDER BY version DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptVersion{}
	for rows.Next() {
		var v PromptVersion
		if err := rows.Scan(&v.Version, &v.Template, &v.Note, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------- MCP 서버 ----------

type MCPServer struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Command   string          `json:"command,omitempty"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	URL       string          `json:"url,omitempty"`
	Enabled   bool            `json:"enabled"`
	Insecure  bool            `json:"insecure"`        // http: TLS 인증서 검사를 건너뛴다(자체 서명 서버, 이슈 #108)
	Tools     []string        `json:"tools,omitempty"` // 캐시된 도구 이름(mcp_tools_cache)
}

func (d *DB) ListMCP() ([]*MCPServer, error) {
	rows, err := d.Query(`SELECT id,name,transport,COALESCE(command,''),args,env,COALESCE(url,''),enabled,insecure FROM mcp_servers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []*MCPServer
	for rows.Next() {
		var m MCPServer
		var args, env []byte
		if err := rows.Scan(&m.ID, &m.Name, &m.Transport, &m.Command, &args, &env, &m.URL, &m.Enabled, &m.Insecure); err != nil {
			rows.Close()
			return nil, err
		}
		m.Args, m.Env = json.RawMessage(args), json.RawMessage(env)
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // 아래 서버별 도구 캐시 쿼리 전에 연결을 돌려준다
	// 서버마다 캐시된 도구 이름을 붙인다. 발견 전에는 비어 있을 수 있다.
	for _, m := range out {
		m.Tools, _ = d.MCPToolNames(m.ID)
	}
	return out, nil
}

// MCPToolNames는 서버의 캐시된 도구 이름을 돌려준다. 발견 전에는 비어 있다.
func (d *DB) MCPToolNames(serverID int64) ([]string, error) {
	rows, err := d.Query(`SELECT tool_name FROM mcp_tools_cache WHERE server_id=$1 ORDER BY tool_name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MCPTool은 MCP 서버에 캐시된 도구 하나다(이름과 설명).
type MCPTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// MCPToolsDetailed는 서버의 캐시된 도구(이름과 설명)를 돌려준다.
func (d *DB) MCPToolsDetailed(serverID int64) ([]MCPTool, error) {
	rows, err := d.Query(`SELECT tool_name, COALESCE(description,'') FROM mcp_tools_cache WHERE server_id=$1 ORDER BY tool_name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPTool
	for rows.Next() {
		var t MCPTool
		if err := rows.Scan(&t.Name, &t.Description); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveMCPTools는 서버의 캐시된 도구 목록을 통째로 바꾼다. 발견 뒤에 호출한다.
func (d *DB) SaveMCPTools(serverID int64, tools []MCPTool) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM mcp_tools_cache WHERE server_id=$1`, serverID); err != nil {
		return err
	}
	for _, t := range tools {
		if _, err := tx.Exec(`INSERT INTO mcp_tools_cache(server_id, tool_name, description) VALUES ($1,$2,$3)
ON CONFLICT (server_id, tool_name) DO UPDATE SET description=EXCLUDED.description`, serverID, t.Name, t.Description); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) SaveMCP(m *MCPServer) (int64, error) {
	args, env := string(m.Args), string(m.Env)
	if args == "" {
		args = "[]"
	}
	if env == "" {
		env = "{}"
	}
	if m.ID == 0 {
		var id int64
		err := d.QueryRow(`INSERT INTO mcp_servers(name,transport,command,args,env,url,enabled,insecure) VALUES ($1,$2,NULLIF($3,''),$4,$5,NULLIF($6,''),$7,$8) RETURNING id`,
			m.Name, m.Transport, m.Command, args, env, m.URL, m.Enabled, m.Insecure).Scan(&id)
		return id, err
	}
	_, err := d.Exec(`UPDATE mcp_servers SET name=$1,transport=$2,command=NULLIF($3,''),args=$4,env=$5,url=NULLIF($6,''),enabled=$7,insecure=$8 WHERE id=$9`,
		m.Name, m.Transport, m.Command, args, env, m.URL, m.Enabled, m.Insecure, m.ID)
	return m.ID, err
}

func (d *DB) DeleteMCP(id int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_visibility WHERE resource_kind='mcp' AND resource_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mcp_servers WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- 스킬 가시성 (에이전트 × skill_name) ----------

// AgentSkillNames는 에이전트에게 보이는 스킬 디렉터리 이름을 돌려준다.
func (d *DB) AgentSkillNames(agentID int64) ([]string, error) {
	rows, err := d.Query(`SELECT skill_name FROM agent_skill_visibility WHERE agent_id=$1 AND enabled ORDER BY skill_name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// SkillAgents는 그 스킬을 볼 수 있는 에이전트 id를 돌려준다.
func (d *DB) SkillAgents(skillName string) ([]int64, error) {
	rows, err := d.Query(`SELECT agent_id FROM agent_skill_visibility WHERE skill_name=$1 AND enabled`, skillName)
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

// SetAgentSkillVisibility는 에이전트의 스킬 가시성을 통째로 바꾼다.
func (d *DB) SetAgentSkillVisibility(agentID int64, names []string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_skill_visibility WHERE agent_id=$1`, agentID); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES ($1,$2,true)
ON CONFLICT (agent_id,skill_name) DO UPDATE SET enabled=true`, agentID, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ToggleSkillVisibility는 (에이전트, skill_name) 가시성 하나를 켜거나 끈다.
func (d *DB) ToggleSkillVisibility(agentID int64, skillName string, on bool) error {
	if on {
		_, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES ($1,$2,true)
ON CONFLICT (agent_id,skill_name) DO UPDATE SET enabled=true`, agentID, skillName)
		return err
	}
	_, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name=$2`, agentID, skillName)
	return err
}

// DeleteSkillVisibility는 스킬의 가시성 행을 모두 지운다. 스킬을 지울 때 호출한다.
func (d *DB) DeleteSkillVisibility(skillName string) error {
	_, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE skill_name=$1`, skillName)
	return err
}

// ---------- 가시성 (에이전트 × mcp) ----------

// AgentVisible은 에이전트에게 보이는 종류별 리소스 id를 돌려준다.
func (d *DB) AgentVisible(agentID int64, kind string) ([]int64, error) {
	rows, err := d.Query(`SELECT resource_id FROM agent_visibility WHERE agent_id=$1 AND resource_kind=$2 AND enabled`, agentID, kind)
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

// ResourceAgents는 그 리소스를 볼 수 있는 에이전트 id를 돌려준다.
func (d *DB) ResourceAgents(kind string, resourceID int64) ([]int64, error) {
	rows, err := d.Query(`SELECT agent_id FROM agent_visibility WHERE resource_kind=$1 AND resource_id=$2 AND enabled`, kind, resourceID)
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

// SetAgentVisibilityKind는 에이전트에게 보이는 한 종류의 리소스 집합을 통째로 바꾼다(에이전트 쪽 일괄 쓰기).
// 리소스 쪽 화면과 같은 agent_visibility 행을 읽고 쓴다.
func (d *DB) SetAgentVisibilityKind(agentID int64, kind string, resourceIDs []int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_kind=$2`, agentID, kind); err != nil {
		return err
	}
	for _, rid := range resourceIDs {
		if _, err := tx.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled) VALUES ($1,$2,$3,true)
ON CONFLICT (agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=true`, agentID, kind, rid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ToggleVisibility는 (에이전트, 종류, 리소스) 가시성 하나를 켜거나 끈다. 같은 값을 다시 넣어도 결과가 같다.
func (d *DB) ToggleVisibility(agentID int64, kind string, resourceID int64, on bool) error {
	if on {
		_, err := d.Exec(`INSERT INTO agent_visibility(agent_id,resource_kind,resource_id,enabled) VALUES ($1,$2,$3,true)
ON CONFLICT (agent_id,resource_kind,resource_id,mcp_tool_name) DO UPDATE SET enabled=true`, agentID, kind, resourceID)
		return err
	}
	_, err := d.Exec(`DELETE FROM agent_visibility WHERE agent_id=$1 AND resource_kind=$2 AND resource_id=$3`, agentID, kind, resourceID)
	return err
}
