package db

import (
	"database/sql"
	"strings"
	"time"
)

// SkillUsage는 Skill()을 한 번 부른 기록이다. 항상 켜 두는 스킬 호출 장부다.
// Skill 메타 도구의 OnInvoke 훅(server/assembly.go)에서 적재마다 한 줄을 쓴다.
// 어떤 스킬을, 어떤 에이전트가, 어느 작업·세션에서 불렀는지만 남기고, 인자 글은 저장하지 않는다.
// args_len만 남겨 프롬프트 없이 「빈 맥락인지, 내용이 있는 맥락인지」를 가를 수 있다. llm_usage와 같다.
//
// 행은 일부러 작업보다 오래 남는다. skill_usage에는 외래 키가 없어, 작업을 지워도
// 스킬 통계는 남는다(llm_usage와 같은 이유다).
type SkillUsage struct {
	Skill         string `json:"skill"`          // 스킬 디렉터리 이름(agent_skill_visibility.skill_name과 같다)
	AgentKey      string `json:"agent_key"`      // worker / planner / mainagent / 사용자 에이전트 키
	TaskID        int64  `json:"task_id"`        // 작업이 아닌 실행(채팅 세션)이면 0
	ExplorationID int64  `json:"exploration_id"` // 모르면 0
	IntentID      int64  `json:"intent_id"`      // 워커의 의도 노드. 플래너·메인 에이전트·채팅은 0
	SessionID     string `json:"session_id"`     // 채팅 대화 id. 작업 실행이면 비어 있다
	ArgsLen       int    `json:"args_len"`
	Found         bool   `json:"found"` // false = 모델이 없는 스킬 이름을 말했다
}

// InsertSkillUsage는 장부 한 줄을 덧붙인다. 최선을 다할 뿐, 실패해도 호출자는 로그만 남기고 계속한다.
// 통계 한 줄을 잃어도 스킬 호출이 깨지면 안 된다.
func (d *DB) InsertSkillUsage(u *SkillUsage) error {
	_, err := d.Exec(`
INSERT INTO skill_usage(skill, agent_key, task_id, exploration_id, intent_id, session_id, args_len, found)
VALUES ($1, NULLIF($2,''), $3, $4, $5, NULLIF($6,''), $7, $8)`,
		u.Skill, u.AgentKey, nullIfZero(u.TaskID), nullIfZero(u.ExplorationID),
		nullIfZero(u.IntentID), u.SessionID, u.ArgsLen, u.Found)
	return err
}

func nullIfZero(v int64) any {
	if v > 0 {
		return v
	}
	return nil
}

// SkillStat은 스킬 하나의 사용 합계다. 스킬 화면이 이 값을 보여 준다.
type SkillStat struct {
	Skill    string     `json:"skill"`
	Calls    int        `json:"calls"`
	Tasks    int        `json:"tasks"`     // 이 스킬을 불러 온 서로 다른 작업 수(채팅 실행은 제외)
	Agents   []string   `json:"agents"`    // 불러 온 에이전트 키. 많이 쓴 순
	LastUsed *time.Time `json:"last_used"` // 한 번도 안 불렀으면 nil
}

// SkillStats는 장부 전체를 스킬별로 모아, 많이 쓴 순으로 돌려준다.
// 한 번도 안 부른 스킬은 빠진다. 호출자가 디스크의 스킬 목록과 합친다.
// 실제로 찾은 호출만 센다. 못 찾은 이름은 MissingSkillStats가 따로 보여 준다.
func (d *DB) SkillStats() ([]SkillStat, error) {
	// 에이전트 키는 text[]가 아니라 쉼표로 이은 문자열로 받는다. pgx
	// 표준 드라이버는 배열을 database/sql로 Scan할 대상이 없고, 키는
	// [a-z0-9_-]라 쉼표로 이어 붙여도 헷갈리지 않는다.
	rows, err := d.Query(`
SELECT skill, COUNT(*) AS calls,
       COUNT(DISTINCT task_id) AS tasks,
       COALESCE(STRING_AGG(DISTINCT agent_key, ','), '') AS agents,
       MAX(ts) AS last_used
FROM skill_usage
WHERE found
GROUP BY skill
ORDER BY COUNT(*) DESC, skill`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillStat{}
	for rows.Next() {
		var s SkillStat
		var agents string
		var lastUsed sql.NullTime
		if err := rows.Scan(&s.Skill, &s.Calls, &s.Tasks, &agents, &lastUsed); err != nil {
			return nil, err
		}
		s.Agents = []string{}
		if agents != "" {
			s.Agents = strings.Split(agents, ",")
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			s.LastUsed = &t
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	return mergeArchivedSkillStats(out, archived, false), nil
}

// MissingSkillStats는 에이전트가 찾았지만 없는 스킬 이름을, 요청이 많은 순으로 돌려준다.
// 「있었으면 하는 이름」 목록이다. 이름은 모델이 말한 그대로 보여 준다(넣을 때 이미 길이를 잘랐다).
func (d *DB) MissingSkillStats(limit int) ([]SkillStat, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := d.Query(`
SELECT skill, COUNT(*) AS calls,
       COALESCE(STRING_AGG(DISTINCT agent_key, ','), '') AS agents,
       MAX(ts) AS last_used
FROM skill_usage
WHERE NOT found
GROUP BY skill
ORDER BY COUNT(*) DESC, skill`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillStat{}
	for rows.Next() {
		var s SkillStat
		var agents string
		var lastUsed sql.NullTime
		if err := rows.Scan(&s.Skill, &s.Calls, &agents, &lastUsed); err != nil {
			return nil, err
		}
		s.Agents = []string{}
		if agents != "" {
			s.Agents = strings.Split(agents, ",")
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			s.LastUsed = &t
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	archived, err := d.archivedTaskAggregates()
	if err != nil {
		return nil, err
	}
	out = mergeArchivedSkillStats(out, archived, true)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SkillCall은 스킬의 최근 호출 목록(상세 패널) 한 줄이다.
type SkillCall struct {
	TS        time.Time `json:"ts"`
	AgentKey  string    `json:"agent_key"`
	TaskID    int64     `json:"task_id"`
	SessionID string    `json:"session_id"`
	ArgsLen   int       `json:"args_len"`
}

// RecentSkillCalls는 스킬 하나의 최근 호출을 최신 순으로 돌려준다.
func (d *DB) RecentSkillCalls(skill string, limit int) ([]SkillCall, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.Query(`
SELECT ts, COALESCE(agent_key,''), COALESCE(task_id,0), COALESCE(session_id,''), args_len
FROM skill_usage
WHERE skill = $1 AND found
ORDER BY ts DESC
LIMIT $2`, skill, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillCall{}
	for rows.Next() {
		var c SkillCall
		if err := rows.Scan(&c.TS, &c.AgentKey, &c.TaskID, &c.SessionID, &c.ArgsLen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SkillCallsByTask는 한 작업이 불러 온 스킬을 많이 쓴 순으로 센다.
// 그 작업의 에이전트가 실제로 어떤 절차를 집었는지, 작업 화면이 이 수를 보여 준다.
func (d *DB) SkillCallsByTask(taskID int64) ([]SkillStat, error) {
	rows, err := d.Query(`
SELECT skill, COUNT(*) AS calls, MAX(ts) AS last_used
FROM skill_usage
WHERE task_id = $1 AND found
GROUP BY skill
ORDER BY COUNT(*) DESC, skill`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillStat{}
	for rows.Next() {
		var s SkillStat
		var lastUsed sql.NullTime
		if err := rows.Scan(&s.Skill, &s.Calls, &lastUsed); err != nil {
			return nil, err
		}
		s.Agents = []string{}
		if lastUsed.Valid {
			t := lastUsed.Time
			s.LastUsed = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
