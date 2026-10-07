package db

import (
	"sort"
	"strconv"
	"time"
)

// LLMUsage는 가벼운 LLM 호출 계량 한 줄이다. 항상 켜 두는 사용 장부이며,
// llm_records(요청·응답 본문 전체를 저장하는, 켜야 하는 디버그 기능)와는 다르다.
// 완료 호출마다 한 줄이고, 성공과 오류 모두 적는다. 그래서 끊기거나 실패한 실행도 토큰 계산이 빠지지 않는다.
// 토큰 사용을 자르는 데 필요한 축(모델 / 프로파일 / 작업 / 에이전트)만 담고,
// 프롬프트나 응답 내용은 담지 않는다.
type LLMUsage struct {
	TaskID        string `json:"task_id"`        // 작업 등록 id (llm_records.task_id와 같다)
	ExplorationID int64  `json:"exploration_id"` // 세션에서 읽은 탐색 id (0 = 모름/작업 아님)
	Worker        string `json:"worker"`         // 에이전트 줄: worker / planner / mainagent / goals
	Model         string `json:"model"`
	ProfileName   string `json:"profile_name"`
	LatencyMs     int    `json:"latency_ms"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CacheRead     int    `json:"cache_read"`
	CacheWrite    int    `json:"cache_write"`
	Status        string `json:"status"` // ok(성공) | error(오류)
}

const llmUsageSchema = `
CREATE TABLE IF NOT EXISTS llm_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    task_id        TEXT,
    exploration_id BIGINT,
    worker         TEXT,
    model          TEXT,
    profile_name   TEXT,
    latency_ms     INTEGER,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cache_write    INTEGER NOT NULL DEFAULT 0,
    status         TEXT
);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task  ON llm_usage(task_id);
CREATE INDEX IF NOT EXISTS idx_llm_usage_model ON llm_usage(task_id, model);
CREATE INDEX IF NOT EXISTS idx_llm_usage_exp   ON llm_usage(exploration_id);
`

// EnsureLLMUsageTable은 llm_usage 계량 테이블이 없으면 만든다.
func (d *DB) EnsureLLMUsageTable() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(llmUsageSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertLLMUsage는 계량 한 줄을 덧붙인다. 최선을 다할 뿐, 실패해도 호출자는 로그만 남기고 계속한다.
// 통계 한 줄을 잃어도 LLM 호출이 깨지면 안 된다.
func (d *DB) InsertLLMUsage(u *LLMUsage) error {
	var expID any
	if u.ExplorationID > 0 {
		expID = u.ExplorationID
	}
	_, err := d.Exec(`
INSERT INTO llm_usage(task_id, exploration_id, worker, model, profile_name, latency_ms, input_tokens, output_tokens, cache_read, cache_write, status)
VALUES (NULLIF($1,''),$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6,$7,$8,$9,$10,$11)`,
		u.TaskID, expID, u.Worker, u.Model, u.ProfileName,
		u.LatencyMs, u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheWrite, u.Status)
	return err
}

// TokenByModel은 작업의 LLM 토큰 사용을 모델별로 모아, 많이 쓴 순으로 돌려준다.
// 항상 켜 둔 llm_usage 장부가 출처다. taskID는 작업 등록 id다.
// 에이전트별 모델 연결, 풀 교대·장애 조치, 끊긴 실행이 있어도 정확하다.
// 성공이든 오류든 호출마다 계량하기 때문이다.
func (d *DB) TokenByModel(taskID string) ([]ModelTokenStat, error) {
	rows, err := d.Query(`
SELECT COALESCE(NULLIF(model,''),'(unknown)') AS model, COUNT(*) AS calls,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
WHERE COALESCE(task_id,'') = $1
GROUP BY model
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC, model`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelTokenStat{}
	for rows.Next() {
		var m ModelTokenStat
		if err := rows.Scan(&m.Model, &m.Calls, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProfileUsage는 LLM 프로파일 하나의 토큰 사용을 장부 전체에서 모은다
// (전역, 모든 작업). 대시보드의 프로파일별 토큰 카드가 이 새 출처를 쓴다.
type ProfileUsage struct {
	ProfileName      string `json:"profile_name"`
	Calls            int    `json:"calls"`
	Tasks            int    `json:"tasks"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
}

// UsageByProfile은 전역 토큰 사용을 프로파일 이름별로, 많이 쓴 순으로 돌려준다.
// 환경 변수나 저장되지 않은 설정으로 부른 호출은 profile_name이 비어 있을 수 있다.
func (d *DB) UsageByProfile() ([]ProfileUsage, error) {
	rows, err := d.Query(`
SELECT COALESCE(profile_name,'') AS profile_name, COUNT(*) AS calls,
       COUNT(DISTINCT task_id) AS tasks,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
GROUP BY profile_name
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileUsage{}
	for rows.Next() {
		var p ProfileUsage
		if err := rows.Scan(&p.ProfileName, &p.Calls, &p.Tasks,
			&p.InputTokens, &p.OutputTokens, &p.CacheReadTokens, &p.CacheWriteTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]ProfileUsage, len(out))
	for _, current := range out {
		byName[current.ProfileName] = current
	}
	for _, aggregate := range archived {
		for _, cold := range aggregate.TokenProfiles {
			current := byName[cold.ProfileName]
			current.ProfileName = cold.ProfileName
			current.Calls += cold.Calls
			current.Tasks += cold.Tasks
			current.InputTokens += cold.InputTokens
			current.OutputTokens += cold.OutputTokens
			current.CacheReadTokens += cold.CacheReadTokens
			current.CacheWriteTokens += cold.CacheWriteTokens
			byName[cold.ProfileName] = current
		}
	}
	out = out[:0]
	for _, current := range byName {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i].InputTokens + out[i].OutputTokens
		right := out[j].InputTokens + out[j].OutputTokens
		if left != right {
			return left > right
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out, nil
}

// ProfileDayUsage는 일별 차트의 (프로파일, UTC 달력 날) 토큰 묶음 하나다.
// 활동 기록 기반 차트와 달리 ts는 실제 호출 시각이다. 그래서 작업 생성일로 묶은 값이 아니라
// 그날 실제로 쓴 토큰이다.
type ProfileDayUsage struct {
	ProfileName     string `json:"profile_name"`
	Date            string `json:"date"` // YYYY-MM-DD (UTC 날짜)
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
}

// UsageDaily는 지난 days일의 (프로파일, 날) 토큰 묶음을 돌려준다
// (days가 0 이하면 기본 365일). 대시보드가 프로파일과 기간으로 자를 수 있다.
func (d *DB) UsageDaily(days int) ([]ProfileDayUsage, error) {
	if days <= 0 {
		days = 365
	}
	rows, err := d.Query(`
SELECT COALESCE(profile_name,'') AS profile_name,
       to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read),0)
FROM llm_usage
WHERE ts >= now() - ($1 * interval '1 day')
GROUP BY profile_name, day
ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileDayUsage{}
	for rows.Next() {
		var p ProfileDayUsage
		if err := rows.Scan(&p.ProfileName, &p.Date, &p.InputTokens, &p.OutputTokens, &p.CacheReadTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	byKey := make(map[string]ProfileDayUsage, len(out))
	for _, current := range out {
		byKey[current.ProfileName+"\x00"+current.Date] = current
	}
	for _, aggregate := range archived {
		for _, cold := range aggregate.TokenDaily {
			if cold.Date < cutoff {
				continue
			}
			key := cold.ProfileName + "\x00" + cold.Date
			current := byKey[key]
			current.ProfileName = cold.ProfileName
			current.Date = cold.Date
			current.InputTokens += cold.InputTokens
			current.OutputTokens += cold.OutputTokens
			current.CacheReadTokens += cold.CacheReadTokens
			byKey[key] = current
		}
	}
	out = out[:0]
	for _, current := range byKey {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].ProfileName < out[j].ProfileName
	})
	return out, nil
}

// JudgeDayUsage는 가로채기 폴백 판정기의 UTC 하루 토큰 묶음이다.
// 설정 화면의 최근 사용 스파크라인이 이 값을 쓴다.
type JudgeDayUsage struct {
	Date         string `json:"date"` // YYYY-MM-DD (UTC 날짜)
	Calls        int    `json:"calls"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// JudgeUsage는 가로채기 폴백 판정기가 누적해 쓴 토큰이다
// (항상 켜 둔 장부의 worker='judge' 행). 최근 일별 수열도 함께 담는다.
type JudgeUsage struct {
	Calls            int             `json:"calls"`
	InputTokens      int             `json:"input_tokens"`
	OutputTokens     int             `json:"output_tokens"`
	CacheReadTokens  int             `json:"cache_read_tokens"`
	CacheWriteTokens int             `json:"cache_write_tokens"`
	Daily            []JudgeDayUsage `json:"daily"`
}

// JudgeUsageStats는 폴백 판정기의 전체 기간 토큰 합과
// 지난 days일의 일별 수열을 돌려준다(기본 30일). 항상 켜 둔 llm_usage 장부가 출처라
// 풀 교대와 끊기거나 실패한 판정 호출까지 맞다. days는 일별 수열만 제한하고, 합계는 전체 기간이다.
func (d *DB) JudgeUsageStats(days int) (JudgeUsage, error) {
	if days <= 0 {
		days = 30
	}
	var u JudgeUsage
	err := d.QueryRow(`
SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM llm_usage
WHERE worker = 'judge'`).Scan(&u.Calls, &u.InputTokens, &u.OutputTokens,
		&u.CacheReadTokens, &u.CacheWriteTokens)
	if err != nil {
		return JudgeUsage{}, err
	}
	rows, err := d.Query(`
SELECT to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
       COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
FROM llm_usage
WHERE worker = 'judge' AND ts >= now() - ($1 * interval '1 day')
GROUP BY day
ORDER BY day`, days)
	if err != nil {
		return JudgeUsage{}, err
	}
	defer rows.Close()
	u.Daily = []JudgeDayUsage{}
	for rows.Next() {
		var day JudgeDayUsage
		if err := rows.Scan(&day.Date, &day.Calls, &day.InputTokens, &day.OutputTokens); err != nil {
			return JudgeUsage{}, err
		}
		u.Daily = append(u.Daily, day)
	}
	if err := rows.Err(); err != nil {
		return JudgeUsage{}, err
	}
	return u, nil
}

// ParseExpID는 세션 문자열에서 읽은 탐색 id 조각을 int64로 바꾼다
// (비었거나 숫자가 아니면 0. 예: 대화 id가 키인 채팅 세션).
func ParseExpID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
