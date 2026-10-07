package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"unicode/utf8"
)

// =====================================================================
// 기업 주체 계층
// 이 계층의 기업 범위가 자산 그래프에 올릴 root_domain, subdomain, ip의 경계를 정한다.
// =====================================================================

// Company는 companies 테이블의 한 줄이다. 기업 범위가 자산 그래프에 올릴 자산의 경계를 정한다.
type Company struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	NKey      string  `json:"nkey"`
	Logo      *string `json:"logo,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// CompanyWithScope는 Company에 범위 규칙과 자산 수를 더한다.
type CompanyWithScope struct {
	Company
	Scope      []ScopeRule `json:"scope"`
	AssetCount int         `json:"asset_count"`
}

// ScopeRule은 company_scope 한 줄이다.
type ScopeRule struct {
	ID        int64  `json:"id"`
	CompanyID int64  `json:"company_id"`
	Kind      string `json:"kind"`
	Domain    string `json:"domain,omitempty"`
	Net       string `json:"net,omitempty"`
	Value     string `json:"value,omitempty"`
	Raw       string `json:"raw"`
	Reason    string `json:"reason,omitempty"`
}

// CompanyStore는 companies와 company_scope 테이블을 다룬다.
type CompanyStore struct{ db *DB }

var (
	ErrCompanyNameConflict = errors.New("같은 기업 이름이 이미 있습니다")
	ErrCompanyNotFound     = errors.New("기업을 찾을 수 없습니다")
)

const (
	// 기업 범위는 규칙 개수를 제한하지 않는다. IP / 도메인을 하나씩 넣는 범위는 금방 수천 건이 되고, 상한은 사용자를
	// 여러 기업으로 쪼개게 할 뿐이다. 요청 본문 크기(server 측 maxCompanyMutationBodyBytes)가 여전히 최종 한도다.
	//
	// 원문과 정규화한 범위 글은 유니코드 글자 수로 상한을 둔다.
	// 그래서 여러 바이트 입력을 API와 DB가 같이 다룬다.
	MaxCompanyScopeRawRunes   = 1024
	MaxCompanyScopeValueRunes = 1024
)

// CompanyScopeValidationError는 클라이언트가 고칠 수 있는 범위 오류다.
// 저장과 트랜잭션 실패는 그 대신 일반 오류로 돌려준다.
type CompanyScopeValidationError struct{ Message string }

func (e *CompanyScopeValidationError) Error() string { return e.Message }

// ValidateCompanyScopeInputBounds는 파싱 전에 요청 전체 한도를 적용한다.
// Store 메서드가 이를 다시 호출하므로 HTTP가 아닌 호출자도 한도를 우회할 수 없다.
// 규칙 한 건의 길이만 제약하고, 건수는 제한하지 않는다.
func ValidateCompanyScopeInputBounds(inputs []ScopeInput) error {
	for i, input := range inputs {
		if utf8.RuneCountInString(input.Value) > MaxCompanyScopeRawRunes {
			return &CompanyScopeValidationError{Message: fmt.Sprintf(
				"기업 범위 %d번째 원본 값이 너무 김: 최대 %d자", i+1, MaxCompanyScopeRawRunes,
			)}
		}
	}
	return nil
}

// 범위 쓰기는 계산된 자산 소유를 전역으로 다시 만든다. 그래서 직렬화해서
// 커밋된 귀속이 항상 가장 최근에 커밋된 규칙을 반영하게 한다.
// 이 키는 기업 변경용으로 남겨 둔다. 7337741001은 스키마 잠금이고
// 7337741002는 패키지를 가로지르는 테스트 잠금이다.
const companyScopeMutationLock int64 = 7337741003

// Companies는 기업 저장소를 돌려준다.
func (d *DB) Companies() *CompanyStore { return &CompanyStore{db: d} }

// companyNKey는 기업 이름을 정규화한다. 소문자, 앞뒤 공백 제거, 연속 공백을 하나로.
func companyNKey(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
}

// UpsertCompany는 이름으로 기업을 만들거나 고친다. id와 새 행인지 여부를 돌려준다.
func (s *CompanyStore) UpsertCompany(name, logo string) (id int64, created bool, err error) {
	nkey := companyNKey(name)
	var logoVal any
	if logo != "" {
		logoVal = logo
	}
	err = s.db.QueryRow(`
INSERT INTO companies(name, nkey, logo)
VALUES ($1, $2, $3)
ON CONFLICT (nkey) DO UPDATE SET
    name = EXCLUDED.name,
    logo = COALESCE(EXCLUDED.logo, companies.logo),
    updated_at = now()
RETURNING id, (xmax = 0)`, name, nkey, logoVal).Scan(&id, &created)
	return
}

// CreateCompanyWithScope는 이미 있는 행을 고치지 않고 기업을 만든다.
// 기업, 유효한 처음 범위 규칙, 계산된 자산 귀속을
// 한 번에 커밋한다. 잘못된 입력은 예전 부분 검사
// 계약을 유지하고, 유효한 규칙 저장을 막지 않은 채 보고한다.
func (s *CompanyStore) CreateCompanyWithScope(name, logo string, inputs []ScopeInput, reason string) (
	id int64, added, skipped, invalid int, validationErrors []string, err error,
) {
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return 0, 0, 0, 0, nil, err
	}
	rules, invalid, validationErrors := parseScopeInputs(inputs)
	if err := validateParsedScopeBounds(rules); err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}

	nkey := companyNKey(name)
	var logoVal any
	if logo != "" {
		logoVal = logo
	}
	if err := tx.QueryRow(`
INSERT INTO companies(name, nkey, logo)
VALUES ($1, $2, $3)
ON CONFLICT (nkey) DO NOTHING
RETURNING id`, name, nkey, logoVal).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, 0, invalid, validationErrors, ErrCompanyNameConflict
		}
		return 0, 0, 0, invalid, validationErrors, err
	}

	added, skipped, needsAttribution, err := insertScopeRulesTx(tx, id, rules, reason)
	if err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	if needsAttribution {
		warning, err := recomputeAttributionTx(tx)
		if err != nil {
			return 0, 0, 0, invalid, validationErrors, err
		}
		logAttributionWarning(warning)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, invalid, validationErrors, err
	}
	return id, added, skipped, invalid, validationErrors, nil
}

// GetCompany는 id로 기업 하나를 돌려준다. 없으면 nil이다.
func (s *CompanyStore) GetCompany(id int64) (*Company, error) {
	c := &Company{}
	err := s.db.QueryRow(`
SELECT id, name, nkey, logo, created_at::text, updated_at::text
FROM companies WHERE id = $1`, id).Scan(
		&c.ID, &c.Name, &c.NKey, &c.Logo, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// GetCompanyByName은 정규화한 이름으로 기업 하나를 돌려준다. 없으면 nil이다.
func (s *CompanyStore) GetCompanyByName(name string) (*Company, error) {
	nkey := companyNKey(name)
	c := &Company{}
	err := s.db.QueryRow(`
SELECT id, name, nkey, logo, created_at::text, updated_at::text
FROM companies WHERE nkey = $1`, nkey).Scan(
		&c.ID, &c.Name, &c.NKey, &c.Logo, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// UpsertByName은 기업이 없으면 만든 뒤 id를 돌려준다.
func (s *CompanyStore) UpsertByName(name string) (int64, error) {
	id, _, err := s.UpsertCompany(name, "")
	return id, err
}

// DeleteCompany는 기업을 지우고, 같은 트랜잭션에서 남은 기업을 기준으로
// 자동 소유를 다시 계산한다. 명시적으로 소유한 자산은
// 외래 키가 떼고, 그다음 남은 범위 맞춤으로 내려갈 수 있다.
func (s *CompanyStore) DeleteCompany(id int64) error {
	_, err := s.DeleteCompanyWithAssets(id, false)
	return err
}

// DeleteCompanyWithAssets는 기업과, 선택하면 그 자산을 모두
// 한 트랜잭션에서 지운 뒤, 남은 기업을 기준으로 소유를 다시 계산한다.
func (s *CompanyStore) DeleteCompanyWithAssets(id int64, deleteAssets bool) (assetsDeleted int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, err
	}
	if deleteAssets {
		res, err := tx.Exec(`DELETE FROM assets WHERE company_id = $1`, id)
		if err != nil {
			return 0, err
		}
		assetsDeleted, err = res.RowsAffected()
		if err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`DELETE FROM companies WHERE id = $1`, id)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if deleted == 0 {
		return 0, ErrCompanyNotFound
	}
	// 이 경로에는 요청별 경고 통로가 없다. 그래서 해석할 수 없는 ip 행은
	// 로그만이 운영자에게 알린다.
	warning, err := recomputeAttributionTx(tx)
	if err != nil {
		return 0, err
	}
	logAttributionWarning(warning)
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return assetsDeleted, nil
}

// ListCompanies는 모든 기업을 범위와 자산 수와 함께 돌려준다.
func (s *CompanyStore) ListCompanies() ([]*CompanyWithScope, error) {
	rows, err := s.db.Query(`
SELECT c.id, c.name, c.nkey, c.logo, c.created_at::text, c.updated_at::text,
       COUNT(DISTINCT a.id) AS asset_count
FROM companies c
LEFT JOIN assets a ON a.company_id = c.id
GROUP BY c.id
ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*CompanyWithScope
	for rows.Next() {
		cws := &CompanyWithScope{}
		if err := rows.Scan(&cws.ID, &cws.Name, &cws.NKey, &cws.Logo,
			&cws.CreatedAt, &cws.UpdatedAt, &cws.AssetCount); err != nil {
			return nil, err
		}
		out = append(out, cws)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 기업마다 범위 규칙을 가져온다
	for _, cws := range out {
		cws.Scope, err = s.GetScope(cws.ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetScope는 기업의 범위 규칙을 모두 돌려준다.
func (s *CompanyStore) GetScope(companyID int64) ([]ScopeRule, error) {
	rows, err := s.db.Query(`
SELECT id, company_id, kind,
       COALESCE(domain,''), COALESCE(net::text,''), COALESCE(value,''), raw, COALESCE(reason,'')
FROM company_scope
WHERE company_id = $1
ORDER BY id`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ScopeRule, 0)
	for rows.Next() {
		var r ScopeRule
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.Kind, &r.Domain, &r.Net, &r.Value, &r.Raw, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddScope는 기업의 범위 줄을 파싱해 넣고, 자산 귀속을 다시 계산한다.
// 추가·건너뜀·잘못된 줄의 수를 돌려준다.
func (s *CompanyStore) AddScope(companyID int64, lines []string, reason string) (added, skipped, invalid int, errors []string) {
	inputs := make([]ScopeInput, 0, len(lines))
	for _, line := range lines {
		inputs = append(inputs, ScopeInput{Value: line})
	}
	return s.AddScopeInputs(companyID, inputs, reason)
}

// AddScopeInputs는 구조화한 범위 규칙을 넣는다. kind가 비어 있으면
// AddScope와 같은 CIDR/IP/ICP/도메인/키워드 자동 분류를 쓴다.
func (s *CompanyStore) AddScopeInputs(companyID int64, inputs []ScopeInput, reason string) (added, skipped, invalid int, errors []string) {
	added, skipped, invalid, validationErrors, err := s.AddScopeInputsChecked(companyID, inputs, reason)
	if err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	return added, skipped, invalid, validationErrors
}

// AddScopeInputsChecked는 구조화한 범위 규칙을 넣되, 입력
// 검사를 저장·트랜잭션 오류와 분리한다.
func (s *CompanyStore) AddScopeInputsChecked(companyID int64, inputs []ScopeInput, reason string) (
	added, skipped, invalid int, validationErrors []string, err error,
) {
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return 0, 0, 0, nil, err
	}
	rules, invalid, errors := parseScopeInputs(inputs)
	if err := validateParsedScopeBounds(rules); err != nil {
		return 0, 0, invalid, errors, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, invalid, errors, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, 0, invalid, errors, err
	}
	if err := ensureCompanyExistsTx(tx, companyID); err != nil {
		return 0, 0, invalid, errors, err
	}
	if len(rules) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, 0, invalid, errors, err
		}
		return 0, 0, invalid, errors, nil
	}
	added, skipped, needsAttribution, err := insertScopeRulesTx(tx, companyID, rules, reason)
	if err != nil {
		return 0, 0, invalid, errors, err
	}
	if needsAttribution {
		warning, err := recomputeAttributionTx(tx)
		if err != nil {
			return 0, 0, invalid, errors, fmt.Errorf("기업 귀속 재계산 실패: %w", err)
		}
		logAttributionWarning(warning)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, invalid, errors, err
	}
	return added, skipped, invalid, errors, nil
}

func parseScopeInputs(inputs []ScopeInput) (rules []ParsedScope, invalid int, validationErrors []string) {
	rules = make([]ParsedScope, 0, len(inputs))
	for _, input := range inputs {
		rule, err := ParseScopeInput(input)
		if err != nil {
			invalid++
			validationErrors = append(validationErrors, fmt.Sprintf("%s: %v", input.Value, err))
			continue
		}
		rules = append(rules, rule)
	}
	return rules, invalid, validationErrors
}

func validateParsedScopeBounds(rules []ParsedScope) error {
	for i, rule := range rules {
		if utf8.RuneCountInString(rule.Raw) > MaxCompanyScopeRawRunes {
			return &CompanyScopeValidationError{Message: fmt.Sprintf(
				"기업 범위 %d번째 원본 값이 너무 김: 최대 %d자", i+1, MaxCompanyScopeRawRunes,
			)}
		}
		if utf8.RuneCountInString(rule.Value) > MaxCompanyScopeValueRunes {
			return &CompanyScopeValidationError{Message: fmt.Sprintf(
				"기업 범위의 %d번째 정규화 값이 너무 깁니다: 최대 %d자", i+1, MaxCompanyScopeValueRunes,
			)}
		}
	}
	return nil
}

func ensureCompanyExistsTx(tx *sql.Tx, companyID int64) error {
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM companies WHERE id = $1)`, companyID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrCompanyNotFound
	}
	return nil
}

func lockCompanyScopeMutation(tx *sql.Tx) error {
	_, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, companyScopeMutationLock)
	return err
}

func insertScopeRulesTx(tx *sql.Tx, companyID int64, rules []ParsedScope, reason string) (
	added, skipped int, needsAttribution bool, err error,
) {
	for _, rule := range rules {
		inserted, insertErr := insertScopeRuleTx(tx, companyID, rule, reason)
		if insertErr != nil {
			return 0, 0, false, insertErr
		}
		if !inserted {
			skipped++
			continue
		}
		added++
		needsAttribution = needsAttribution || rule.Kind != "keyword"
	}
	return added, skipped, needsAttribution, nil
}

// insertScopeRuleTx는 범위 규칙을 넣는다. inserted=false는 중복을
// ON CONFLICT가 무시한 것이지 오류가 아니다.
func insertScopeRuleTx(tx *sql.Tx, companyID int64, rule ParsedScope, reason string) (inserted bool, err error) {
	var res interface{ RowsAffected() (int64, error) }
	switch rule.Kind {
	case "domain":
		res, err = tx.Exec(`
INSERT INTO company_scope(company_id, kind, domain, raw, reason)
VALUES ($1, 'domain', $2, $3, $4)
ON CONFLICT ON CONSTRAINT uq_sv2_domain DO NOTHING`,
			companyID, rule.Domain, rule.Raw, reason)
	case "ip", "cidr":
		res, err = tx.Exec(`
INSERT INTO company_scope(company_id, kind, net, raw, reason)
VALUES ($1, $2, $3::cidr, $4, $5)
ON CONFLICT ON CONSTRAINT uq_sv2_net DO NOTHING`,
			companyID, rule.Kind, rule.Net, rule.Raw, reason)
	case "icp", "keyword":
		res, err = tx.Exec(`
INSERT INTO company_scope(company_id, kind, value, raw, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (company_id, kind, value) WHERE kind IN ('icp','keyword') DO NOTHING`,
			companyID, rule.Kind, rule.Value, rule.Raw, reason)
	default:
		return false, fmt.Errorf("unsupported company scope kind %q", rule.Kind)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecomputeAttribution은 범위에서 계산된 소유만 다시 만든다. 명시적 기업
// 연결은 범위를 고쳐도 바뀌지 않는다. 우선순위는 도메인, IP/CIDR, 그다음
// 정규화한 정확한 ICP다. 키워드 규칙은 자산을 귀속하지 않는다.
func (s *CompanyStore) RecomputeAttribution() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return err
	}
	warning, err := recomputeAttributionTx(tx)
	if err != nil {
		return err
	}
	logAttributionWarning(warning)
	return tx.Commit()
}

// recomputeAttributionTx는 범위에서 계산된 소유를 다시 만든다. ip 열을
// 해석할 수 없는 자산은 경고로 돌려준다. try_inet은 문을 멈추지 않고 그 행을 건너뛴다.
// 이 경고가 없으면 그 자산은 네트워크 기업을 조용히 영원히 못 받는다.
// 호출자가 경고를 보여 주고, 항상 로그에도 남긴다.
func recomputeAttributionTx(tx *sql.Tx) (string, error) {
	// 계산된 행만 지운다. 출처 없이 옮겨 온 옛 행은
	// schema.sql이 explicit로 표시한다. 지우지 않는 쪽이 기본이다.
	if _, err := tx.Exec(`
UPDATE assets
SET company_id = NULL, company_source = 'scope'
WHERE company_source = 'scope'`); err != nil {
		return "", err
	}

	// 도메인 기준 귀속(root_domain 정확히 맞춤).
	if _, err := tx.Exec(`
WITH matched AS (
    SELECT DISTINCT ON (a.id) a.id AS asset_id, cs.company_id
    FROM assets a
    JOIN company_scope cs ON cs.kind = 'domain' AND a.root_domain = cs.domain
    WHERE a.company_id IS NULL
      AND a.type IN ('root_domain','subdomain','service','endpoint')
      AND a.root_domain IS NOT NULL
    ORDER BY a.id, length(cs.domain) DESC, cs.company_id
)
UPDATE assets a
SET company_id = matched.company_id, company_source = 'scope'
FROM matched
WHERE a.id = matched.asset_id`); err != nil {
		return "", err
	}

	// 아직 소유자가 없는 자산의 IP/CIDR 귀속.
	if _, err := tx.Exec(`
WITH matched AS (
    SELECT DISTINCT ON (a.id) a.id AS asset_id, cs.company_id
    FROM assets a
    JOIN company_scope cs ON cs.kind IN ('ip','cidr') AND cs.net >>= try_inet(a.ip)
    WHERE a.company_id IS NULL
      AND a.type IN ('ip','subdomain','service','endpoint')
      AND a.ip IS NOT NULL
    ORDER BY a.id, masklen(cs.net) DESC, cs.company_id
)
UPDATE assets a
SET company_id = matched.company_id, company_source = 'scope'
FROM matched
WHERE a.id = matched.asset_id`); err != nil {
		return "", err
	}

	// 도메인·네트워크 우선순위 다음의, 정규화한 ICP 정확 맞춤.
	if _, err := tx.Exec(`
WITH matched AS (
    SELECT DISTINCT ON (a.id) a.id AS asset_id, cs.company_id
    FROM assets a
    JOIN company_scope cs ON cs.kind = 'icp'
      AND (
        lower(regexp_replace(COALESCE(a.icp,''), '[[:space:]]+', '', 'g')) = cs.value
        OR lower(regexp_replace(COALESCE(a.app_icp,''), '[[:space:]]+', '', 'g')) = cs.value
      )
	WHERE a.company_id IS NULL
      AND (COALESCE(a.icp,'') <> '' OR COALESCE(a.app_icp,'') <> '')
    ORDER BY a.id, cs.company_id
)
UPDATE assets a
SET company_id = matched.company_id, company_source = 'scope'
FROM matched
WHERE a.id = matched.asset_id`); err != nil {
		return "", err
	}
	return malformedIPAssetWarning(tx)
}

// malformedIPAssetsSampled는 경고 하나가 이름을 적는 문제 id 수의 상한이다.
// 나쁜 행이 많아도 토스트와 로그에서 읽을 수 있게 한다.
const malformedIPAssetsSampled = 5

// logAttributionWarning은 재계산 경고를 서버 로그에 남긴다. 모든
// 재계산 경로가 이것을 부르므로, 요청별 응답이 없는 트리거
// (기업 삭제, scopesentry 동기화, 에이전트 자산 쓰기)에서도 경고가 남는다.
func logAttributionWarning(warning string) {
	if warning != "" {
		log.Printf("[assets] %s", warning)
	}
}

// malformedIPAssetQueryer는 *sql.Tx와 *DB가 모두 만족한다. 그래서 경고를
// 재계산 트랜잭션 안에서 만들거나, API가 따로 읽을 수 있다.
type malformedIPAssetQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// MalformedIPAssetWarning은 변경 밖에서, ip를 해석할 수 없는 자산을 보고한다.
// API가 변경 함수 서명을 넓히지 않고 범위 응답에 경고를 붙일 수 있다.
// 이 요청과 무관한 데이터 문제는 이 요청의 검사 오류가 아니다.
func (s *CompanyStore) MalformedIPAssetWarning() (string, error) {
	return malformedIPAssetWarning(s.db)
}

// malformedIPAssetWarning은 ip 열이 올바른 주소가 아닌 자산을 설명한다.
// 네트워크 귀속에는 보이지 않으므로, 운영자에게 어느 행을 고칠지
// 알려야 한다. 조용히 건너뛰면 범위 규칙이 그냥 안 되는 것처럼 보인다.
func malformedIPAssetWarning(q malformedIPAssetQueryer) (string, error) {
	rows, err := q.Query(`
SELECT id, ip, count(*) OVER () AS total
FROM assets
WHERE ip IS NOT NULL AND ip <> '' AND try_inet(ip) IS NULL
  AND type IN ('ip','subdomain','service','endpoint')
ORDER BY id
LIMIT $1`, malformedIPAssetsSampled)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var total int
	samples := make([]string, 0, malformedIPAssetsSampled)
	for rows.Next() {
		var id int64
		var ip string
		if err := rows.Scan(&id, &ip, &total); err != nil {
			return "", err
		}
		samples = append(samples, fmt.Sprintf("#%d %s", id, ip))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if total == 0 {
		return "", nil
	}
	warning := fmt.Sprintf(
		"%d개 자산의 ip 필드가 올바른 IP가 아니어서 IP/CIDR 범위 일치를 건너뛰었습니다(이 자산은 대역 규칙으로 기업에 귀속되지 않습니다): %s",
		total, strings.Join(samples, "、"),
	)
	if total > len(samples) {
		warning += fmt.Sprintf(" 외 %d개", total)
	}
	return warning, nil
}

// UpdateScope는 기업의 범위 규칙을 모두 바꾸고 귀속을 다시 계산한다.
func (s *CompanyStore) UpdateScope(companyID int64, lines []string, reason string) (added, invalid int, errs []string) {
	inputs := make([]ScopeInput, 0, len(lines))
	for _, line := range lines {
		inputs = append(inputs, ScopeInput{Value: line})
	}
	return s.UpdateScopeInputs(companyID, inputs, reason)
}

// UpdateScopeInputs는 모든 규칙을 구조화한 묶음으로 바꾼다.
func (s *CompanyStore) UpdateScopeInputs(companyID int64, inputs []ScopeInput, reason string) (added, invalid int, errs []string) {
	added, invalid, validationErrors, err := s.UpdateScopeInputsChecked(companyID, inputs, reason)
	if err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	return added, invalid, validationErrors
}

// UpdateScopeInputsChecked는 모든 규칙을 바꾸되, 검사
// 피드백을 저장·트랜잭션 실패와 분리한다.
func (s *CompanyStore) UpdateScopeInputsChecked(companyID int64, inputs []ScopeInput, reason string) (
	added, invalid int, validationErrors []string, err error,
) {
	if err := ValidateCompanyScopeInputBounds(inputs); err != nil {
		return 0, 0, nil, err
	}
	rules, invalid, errs := parseScopeInputs(inputs)
	if invalid > 0 {
		return 0, invalid, errs, &CompanyScopeValidationError{Message: fmt.Sprintf(
			"기업 범위에 유효하지 않은 규칙 %d개가 있어 기존 범위를 덮어쓰지 않았습니다", invalid,
		)}
	}
	if err := validateParsedScopeBounds(rules); err != nil {
		return 0, invalid, errs, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, invalid, errs, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := lockCompanyScopeMutation(tx); err != nil {
		return 0, invalid, errs, err
	}
	if err := ensureCompanyExistsTx(tx, companyID); err != nil {
		return 0, invalid, errs, err
	}
	if _, err := tx.Exec(`DELETE FROM company_scope WHERE company_id = $1`, companyID); err != nil {
		return 0, invalid, errs, err
	}
	added, _, _, err = insertScopeRulesTx(tx, companyID, rules, reason)
	if err != nil {
		return 0, invalid, errs, err
	}
	// 빈 교체여도 다시 계산한다. 옛 규칙을 지우면
	// 범위에서 온 자산이 떨어지거나, 더 낮은 우선순위의 기업 맞춤이 드러날 수 있다.
	warning, err := recomputeAttributionTx(tx)
	if err != nil {
		return 0, invalid, errs, fmt.Errorf("기업 귀속 재계산 실패: %w", err)
	}
	logAttributionWarning(warning)
	if err := tx.Commit(); err != nil {
		return 0, invalid, errs, err
	}
	return added, invalid, errs, nil
}

// ResolveCompany는 준 root_domain 그리고/또는 ip의 company_id를 돌려준다.
// 맞는 범위 규칙이 없으면 nil이다. 자산을 넣을 때의 귀속 논리와 같다.
func (s *CompanyStore) ResolveCompany(rootDomain, ipStr string) (*int64, error) {
	return s.ResolveCompanyWithICP(rootDomain, ipStr, "")
}

// ResolveCompanyWithICP는 넣을 때의 소유에 RecomputeAttribution을 그대로 쓴다.
// ICP는 도메인과 IP/CIDR이 맞지 않은 뒤에만 본다.
func (s *CompanyStore) ResolveCompanyWithICP(rootDomain, ipStr, icp string) (*int64, error) {
	return resolveCompanyWithICP(s.db, rootDomain, ipStr, icp)
}

type companyScopeQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func resolveCompanyWithICP(q companyScopeQueryer, rootDomain, ipStr, icp string) (*int64, error) {
	if rootDomain != "" {
		var cid int64
		err := q.QueryRow(`
SELECT company_id FROM company_scope
WHERE kind = 'domain'
  AND domain = $1
ORDER BY length(domain) DESC, company_id
LIMIT 1`, rootDomain).Scan(&cid)
		if err == nil {
			return &cid, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	if ipStr != "" {
		if net.ParseIP(ipStr) != nil {
			var cid int64
			err := q.QueryRow(`
SELECT company_id FROM company_scope
WHERE kind IN ('ip','cidr')
  AND net >>= $1::inet
ORDER BY masklen(net) DESC, company_id
LIMIT 1`, ipStr).Scan(&cid)
			if err == nil {
				return &cid, nil
			}
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
	}
	if normalized := NormalizeICP(icp); normalized != "" {
		var cid int64
		err := q.QueryRow(`
SELECT company_id FROM company_scope
WHERE kind = 'icp' AND value = $1
ORDER BY company_id
LIMIT 1`, normalized).Scan(&cid)
		if err == nil {
			return &cid, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	return nil, nil
}
