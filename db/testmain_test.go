package db

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain은 db 테스트 모음 전체가 PostgreSQL 권고 잠금(7337741002)을 잡는다.
// agent와 server 패키지도 같은 잠금을 잡는다. 그래서 병렬
// `go test ./...`는 공유 개발 DB에서 패키지끼리 차례로 돌고
// 패키지 사이 정리 경합을 피한다(예: 한 패키지의 DELETE FROM assets WHERE id > X가
// 다른 패키지가 만든 자산을 지우는 일).
func TestMain(m *testing.M) {
	dsn, _, err := DSN()
	if err != nil {
		// DB가 설정되지 않았다. PG가 필요한 테스트는 스스로 건너뛴다.
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}
