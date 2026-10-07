package db

import (
	"testing"
)

// 원문 본문 열은 릴리스 뒤에 추가됐다. 기존 설치는
// llmRecordsMigrate로만 그 열을 받는다. 이 테스트는 개발
// PG에서 업그레이드 전 모양으로 되돌린 표에 실제 마이그레이션을 돌린다. 트랜잭션 안이고
// 항상 롤백한다. PG는 DDL도 트랜잭션이라 아무것도 남지 않는다.
func TestLLMRecordsMigrateAddsRawColumnsToOldTable(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	tx, err := d.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // the test never commits

	// 표를 업그레이드 전 설치 모습으로 되돌린다.
	if _, err := tx.Exec(`ALTER TABLE llm_records DROP COLUMN IF EXISTS raw_request, DROP COLUMN IF EXISTS raw_response`); err != nil {
		t.Fatalf("simulate old table: %v", err)
	}
	if _, err := tx.Exec(llmRecordsMigrate); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, col := range []string{"raw_request", "raw_response"} {
		var n int
		if err := tx.QueryRow(
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_name='llm_records' AND column_name=$1`, col).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("column %s missing after migrate", col)
		}
	}

	// 다시 돌려도 아무 일도 없어야 한다(마이그레이션은 시작할 때마다 돈다).
	if _, err := tx.Exec(llmRecordsMigrate); err != nil {
		t.Fatalf("migrate is not idempotent: %v", err)
	}

	// 원문 본문을 담은 insert는 마이그레이션된 표에서 왕복해야 한다.
	var got string
	if err := tx.QueryRow(
		`INSERT INTO llm_records(model, raw_request, raw_response) VALUES ('m','{"a":1}','data: x')
		 RETURNING raw_response`).Scan(&got); err != nil {
		t.Fatalf("insert into migrated table: %v", err)
	}
	if got != "data: x" {
		t.Errorf("raw_response=%q want %q", got, "data: x")
	}
}
