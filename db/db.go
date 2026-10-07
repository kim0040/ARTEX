// Package db는 ARTEX의 PostgreSQL 데이터 원본이다.
// 예전 그래프는 SQLite 파일 하나였다. 지금은 PostgreSQL이 자산 그래프(작업들이
// 공유하는 자산 장부)와 탐색 그래프(작업마다 있는 목표·의도·사실·발견)를 담는다.
// 이 패키지는 연결을 열고, schema를 적용하고, 내장 에이전트와 변수 목록을 심는다.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx의 database/sql 드라이버("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock은 세션 잠금을 꺼내 둔 연결 하나에 고정한다.
// *sql.DB로 pg_advisory_lock을 돌리면 안 된다. 나중 스키마나 잠금 해제가
// 풀의 다른 PostgreSQL 세션을 쓸 수 있다.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration은 길고 여러 테이블에 걸친 보관 트랜잭션이
// 시작 DDL과 동시에 돌지 않게 한다. 평범한 실행 중 조회는 그대로 계속된다.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN은 PostgreSQL 연결 문자열을 찾고, 어디서 왔는지도 알려 준다.
// 우선순위: 환경 변수 ARTEX_PG_DSN > 설정 파일(config.json). 내장 기본값은 없다.
// 둘 다 없으면 오류다.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// DB는 공유 *sql.DB를 감싼다. PG가 연결 풀과 동시성(MVCC)을 스스로 다루므로,
// 예전 SQLite 저장소와 달리 프로세스 전체 쓰기 잠금이 없다.
type DB struct{ *sql.DB }

// ensureDatabase는 postgres 시스템 데이터베이스에 연결하고, 대상
// 데이터베이스가 없으면 만든다. dsn은 postgres:// URL이어야 한다.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // 해석할 수 없는 DSN. 일반 Open이 분명한 오류로 실패하게 둔다
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// 대신 postgres 유지보수 데이터베이스에 연결한다
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // 최선을 다할 뿐. 진짜 오류는 Open이 보여 주게 둔다
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open은 연결하고, 스키마를 적용하고(반복 실행 가능), 내장 행을 심는다.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx는 인자가 없으면 단순 프로토콜로 여러 문 Exec를 돌린다.
	// DDL과 심기가 끝날 때까지 전용 잠금 연결을 꺼내 둔다.
	// 그래야 동시에 뜨는 인스턴스가 순서를 어기고 초기화하지 못한다.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent는 고정 에이전트 하나와 그 프롬프트 변수 목록을 설명한다.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 행을 만들 때의 기본 대화형 shell. ON CONFLICT 는 사용자가 나중에 끈 값을 덮지 않는다.
	runSeconds            *int // 행을 만들 때의 한 번 실행 벽시계 상한(초). nil=시드 기본(1200), 0=제한 없음.
}

type promptVar struct{ name, desc, example, source string }

// intp는 v의 포인터를 돌려준다. builtinAgent의 선택 필드(runSeconds 등)에 값을 넣기 위해서다.
func intp(v int) *int { return &v }

// builtinAgents는 고정 에이전트와 프롬프트 변수 목록이다. 내장 도구는 저장하지 않고
// 에이전트와 변수 목록만 심는다. 플래너만 탐색 그래프에 의도를 만들고, 워커는 의도 하나를 실행한다.
// planner/worker/mainagent/auto의 대화형 shell 기본값은 아래 interactive_shell_default_v1
// 블록이 한꺼번에 true로 둔다(이후 토글은 존중). 여기 interactiveShell은 만들자마자 켜야 하는 새 에이전트용이다.
var builtinAgents = []builtinAgent{
	{"goals", "목표 분해", "goals", "작업 목표를 독립적이고 확인할 수 있는 하위 목표 여러 개로 나눈다.", []promptVar{
		{"EngagementDescription", "작업 설명(대상과 배경)", "example.com 사이트", "exploration"},
		// Now는 전역 runtime 변수다(server.globalPromptVars). 에이전트 목록에 다시 정의하면
		// withGlobalVars가 붙일 때 전역 항목과 이름이 부딪힌다.
	}, false, nil},
	{"planner", "플래너", "planner", "상황을 읽고 목표를 판정한다. 아직 덮이지 않은 새 방향이 있을 때만 탐색 의도를 보탠다(작업마다 계획 루프 하나).", []promptVar{
		{"Goal", "작업의 전체 목표", "example.com 관리자 권한", "exploration"},
		{"AssetSummary", "자산 개수와 유형 분포 요약(선택)", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "메인", "main", "사람과의 접점. 진행을 보고, 사람의 뜻을 힌트나 높은 우선순위 의도로 남긴다.", []promptVar{
		{"Goal", "현재 작업 목표", "example.com 관리자 권한", "exploration"},
		{"AssetSummary", "시작 상황 요약(선택)", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "확인된 발견 요약(선택)", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "실행", "worker", "의도 하나를 집어 실행하고, 사실과 발견을 탐색 그래프에 쓴 뒤 멈춘다.", []promptVar{
		{"ProxyAddr", "기록 프록시 주소(문구가 둘로 갈린다)", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "워커 자기 이름(선택)", "worker-1", "runtime"},
	}, false, nil},
	// Auto는 내장 플랫폼 조작 에이전트다. 탐색 루프에 들어가지 않고, 대화 페이지에서 도구로 플랫폼을 다룬다.
	{"auto", "Auto", "assistant", "플랫폼 조작 도우미. 도구로 작업(만들기/보기/일시정지/힌트)과 자산을 관리하고, skill, 사용자 도구, MCP를 만들거나 고친다.", nil, false, nil},
	// pentest는 내장 독립 에이전트다. 대화 페이지에서 돌고, 한 에이전트가 계획하고 실행하고 확인한다. 대화형 shell은 기본으로 켜진다.
	{"pentest", "독립 점검", "assistant", "독립 에이전트. 한 명이 계획하고, 실행하고, 결과를 확인한다.", nil, true, intp(0)},
}

// seedBuiltins는 고정 내장 에이전트와 변수 목록을 넣는다. 반복 실행해도 된다.
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// 이름이 바뀐 변수의 카탈로그 항목을 지운다. 화이트리스트가
	// 템플릿이 풀 수 없는 이름을 더 알리지 않게 한다(EngagementTitle→Description).
	// 'Now'를 각 에이전트 목록에서 전역 runtime 변수로 올린 뒤에도, 옛 저장소의 goals에는
	// 'Now'가 남아 전역 항목과 이름이 부딪힌다(프론트 변수 목록 key 중복). 같이 지운다.
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// 실행 에이전트(플래너/워커/메인 에이전트/auto)의 interactive_shell을 한 번만 기본으로 켠다.
	// 나중에 사용자가 끄면 그 선택을 존중한다(설정 플래그가 지킨다). goals(한 번 도는
	// 분해기)는 꺼 둔다. 열이 생긴 뒤에 돈다(심기 전에 스키마를 적용한다).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// 내장 브라우저(Playwright) MCP는 한 번만 심고, 기본은 꺼 둔다(필요할 때 사용자가 켠다).
	// 프록시도 기본은 없다. 트래픽 캡처 토글이 실행 중에 기록 프록시와 CA를 넣거나 뺀다
	// 실행 중에 기록 프록시와 CA를 넣거나 뺀다(server.Manager.syncBrowserMCPProxy).
	// 없을 때만 넣는다. 재시작할 때 사람이 고친 값(args/env/enabled/
	// 가시성)을 덮지 않으려는 것이다.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// 참고: 자리표시용 ScopeSentry 데이터 원본 MCP(빈 URL + 빈 X-API-Key,
	// 꺼짐)는 schema.sql §F에서 직접 심는다. 그래서 `psql < schema.sql`만으로 초기화해도 생긴다.
	// schema.sql은 시작마다 Exec되므로 반복 실행해도 된다.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	return nil
}

// seedDefaultAssetInterceptRules는 내장 자산 가로채기 목록(정부·교육 사이트의
// 비슷한 도메인 맞춤)을 첫 시작 때 한 번 넣는다. 설정 플래그로 막아,
// 사용자가 나중에 끄거나 지운 것이 재시작 때 되살아나지 않게 한다. 가로채기 규칙 심기와 같은 정책이다.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[내장] 정부 웹사이트 (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[내장] 정부 웹사이트 (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[내장] 교육 웹사이트 (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[내장] 교육 웹사이트 (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility는 함께 배포된 스킬 디렉터리 이름 → 기본으로 그것을 볼
// 내장 에이전트 키다. 스킬 파일 자체는 파일 시스템(SkillDir, 실행 때 norma가 읽음)에 있고,
// DB는 이 보임 연결만 담는다. 여기에 없는 스킬(예: playwright-cli, scopesentry)은
// 기본으로 보이지 않는다. 필요할 때 사용자가 에이전트마다 켠다. scopesentry는 추가로
// `mcps: ScopeSentry`를 선언하는데, 보이게 하고 그 MCP를 켜고 설정한 뒤에만 효과가 있다.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility는 함께 배포된 내장 스킬을 기본 에이전트에 묶는다.
// 없을 때만 넣는다(ON CONFLICT DO NOTHING). 사용자가 나중에 끈 것이
// 재시작 때 되살아나지 않는다. 브라우저 MCP·가로채기 규칙 심기와 같은 정책이다.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules는 내장 안전 가로채기 규칙을 첫 시작 때 한 번 넣는다.
// 설정 플래그로 막아, 사용자가 끄거나 지우거나 순서를 바꾼 것이
// 이후 재시작 때 덮어쓰이지 않게 한다.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name(도구 이름) | tool_input(도구 입력)
		typ      string // string(문자열) | regex(정규식)
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── 시스템을 망가뜨리는 명령 (priority 100) ──
		{
			name:     "[내장] 재귀 강제 삭제 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "재귀 강제 삭제(rm -rf / rm --recursive)는 거절됩니다. 시스템이나 대상 환경을 영구히 망가뜨릴 수 있습니다",
			priority: 100,
		},
		{
			name:     "[내장] 시스템 핵심 디렉터리 삭제",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "시스템 핵심 경로 삭제는 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 포맷 mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "디스크 포맷(mkfs)은 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 장치 덮어쓰기 dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd로 디스크 장치를 덮어쓰는 것은 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 포크 폭탄",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "포크 폭탄 실행은 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 종료 / 재시작",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "종료나 재시작 명령은 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 모든 프로세스 종료",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 또는 killall -9(모든 프로세스 종료)는 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 디스크 지우기 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "디스크 장치에 shred/wipe를 실행하는 것은 거절됩니다",
			priority: 100,
		},
		{
			name:     "[내장] 방화벽 규칙 비우기",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "방화벽 규칙을 비우는 것은 거절됩니다(iptables -F / nft flush)",
			priority: 100,
		},
		// ── 데이터베이스를 망가뜨리는 조작 (priority 90) ──
		{
			name:     "[내장] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP 실행은 거절됩니다. 데이터베이스 객체를 되돌릴 수 없이 없앨 수 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE 실행은 거절됩니다. 표의 데이터를 모두 비울 수 있습니다",
			priority: 90,
		},
		{
			name:     "[내장] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB drop 실행은 거절됩니다",
			priority: 90,
		},
		{
			name:     "[내장] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis FLUSHALL / FLUSHDB 실행은 거절됩니다. 캐시 데이터를 모두 비울 수 있습니다",
			priority: 90,
		},
		// ── HTTP 파괴 요청 (priority 80) ──
		// 에이전트가 DELETE 요청을 보내는 흔한 세 가지:
		//   1. curl -X DELETE / --request DELETE (bash 도구가 직접 실행하거나 스크립트에 씀)
		//   2. Python HTTP 클라이언트의 .delete() 메서드
		//   3. JS나 일반 스크립트의 method: 'DELETE' / method="DELETE"
		{
			name:     "[내장] curl / wget DELETE 요청",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget으로 HTTP DELETE를 보내는 것은 거절됩니다. 대상 시스템 데이터를 지울 수 있습니다",
			priority: 80,
		},
		{
			name:     "[내장] Python HTTP 클라이언트 DELETE (requests/httpx/aiohttp)",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python HTTP 클라이언트로 DELETE를 보내는 것은 거절됩니다",
			priority: 80,
		},
		{
			name:     "[내장] 스크립트의 HTTP DELETE 메서드 선언 (JS/일반)",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "스크립트에서 HTTP DELETE를 선언해 보내는 것은 거절됩니다",
			priority: 80,
		},
		{
			name:     "[내장] 일괄 비우기 / 제거 경로",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "일괄 비우기나 제거 경로 호출은 거절됩니다(/clear /wipe /flush /purge 등)",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2는 예전에
// guard.go에 박혀 있던 두 안전 패턴(파괴적 셸과 데이터 반출 파이프)을 일반
// 가로채기 규칙으로 옮긴다. 자기 플래그로 막아, v1을 이미 돌린 DB에도
// 들어간다. 예전 guard.go 바닥과 달리 평범한 [내장] 규칙이라 사용자가
// 끄거나 지울 수 있다. 반출 규칙은 기본이 꺼져 있다(그
// curl/wget/nc 파이프 패턴은 정당한 역방향 셸과
// 데이터 전송 파이프에도 잘못 맞는다). 반출 차단이 필요할 때만 직접 켠다.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[내장] 파괴적인 시스템 명령",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "파괴적인 명령은 거절됩니다(rm -rf / / mkfs / dd / fork bomb / 종료·재시작 / 디스크 장치 덮어쓰기)",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[내장] 데이터 유출 파이프",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "데이터 유출로 보이는 파이프는 거절됩니다(명령 출력을 curl/wget/nc로 내보냄)",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3는 삭제 endpoint 경로 규칙을 더한다. v1 HTTP 규칙은
// DELETE *메서드*만 잡는다(curl -X DELETE, requests.delete(, method:'DELETE').
// v1 경로 규칙은 /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all만 덮는다. 그래서 `curl 'http://t/api/user/delete?id=1'`처럼
// GET/POST로 닿는 삭제 endpoint(대부분의 웹 앱이 삭제를 이렇게 연다)는
// 내장 규칙을 모두 빠져나갔다. 별도 플래그를 둬, v1/v2를 이미 돌린 DB에도 들어간다.
// v1 심기를 고쳐도 그 DB에는 효과가 없기 때문이다.
//
// 패턴은 동사 뒤에 구분자가 있게 일부러 만들었다. /delivery,
// /details, /delta, /delegate는 맞지 않고, /deleteAll, /delete_user,
// /delete-user는 맞는다. destroy를 여기서 다시 덮는 이유는 v1 규칙이
// 접미를 허용하지 않아서다(/destroyAll을 놓쳤다).
//
// 패키지 상수로 내보내는 이유는, DB 없이 심긴 정규식을 단위 테스트하기 위해서다.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[내장] 삭제류 인터페이스 경로"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name = $1)`,
		name,
		deleteEndpointPathPattern,
		"삭제류 경로 호출은 거절됩니다(/delete /remove /unlink /erase 등). HTTP 메서드와 관계없습니다. 많은 앱의 삭제 경로는 GET/POST로도 동작해 대상 데이터를 실제로 지웁니다",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
