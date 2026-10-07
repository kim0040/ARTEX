package db

import "time"

// AssetInterceptRule은 asset_intercept_rules의 한 줄이다. 자산 그래프 전체를 대상으로 하는
// 전역 가로채기 규칙이다. intercept_rules는 도구 이름·입력을 맞추지만, 이 규칙은
// 대상 자산(정확한/비슷한 domain·ip·url, 또는 CIDR 범위)을 맞춘다.
// 이 계층은 규칙만 저장한다. 맞추고 막는 로직은 다른 곳에 있다.
type AssetInterceptRule struct {
	ID      int64  `json:"id"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"` // exact_domain|exact_ip|exact_url|fuzzy_domain|fuzzy_ip|fuzzy_url|cidr (맞출 대상의 종류)
	Pattern string `json:"pattern"`
	Note    string `json:"note"`
	Builtin bool   `json:"builtin"`
	// Action은 작업급 규칙에만 쓴다. block=가로채기 allow=허용(허용 목록).
	// 전역 규칙(asset_intercept_rules)에는 이 열이 없고, 항상 비어 있으며, 가로채기로 본다.
	Action    string    `json:"action,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const assetInterceptRuleCols = `id, enabled, kind, pattern, note, builtin, created_at, updated_at`

func scanAssetInterceptRule(row interface{ Scan(...any) error }) (AssetInterceptRule, error) {
	var r AssetInterceptRule
	err := row.Scan(&r.ID, &r.Enabled, &r.Kind, &r.Pattern, &r.Note, &r.Builtin, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ListAssetInterceptRules는 규칙을 모두 돌려준다. 내장 규칙이 먼저, 그다음 최신 순이다.
func (d *DB) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	rows, err := d.Query(`SELECT ` + assetInterceptRuleCols + ` FROM asset_intercept_rules ORDER BY builtin DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssetInterceptRule
	for rows.Next() {
		r, err := scanAssetInterceptRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateAssetInterceptRule은 사용자 규칙을 새로 넣는다. 여기서 builtin은 항상 false다.
func (d *DB) CreateAssetInterceptRule(kind, pattern, note string, enabled bool) (AssetInterceptRule, error) {
	row := d.QueryRow(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES ($1, $2, $3, $4, false)
RETURNING `+assetInterceptRuleCols,
		enabled, kind, pattern, note)
	return scanAssetInterceptRule(row)
}

// UpdateAssetInterceptRule은 기존 규칙에서 고칠 수 있는 필드를 바꾼다.
func (d *DB) UpdateAssetInterceptRule(id int64, kind, pattern, note string, enabled bool) (AssetInterceptRule, error) {
	row := d.QueryRow(`
UPDATE asset_intercept_rules
   SET enabled=$2, kind=$3, pattern=$4, note=$5
WHERE id=$1
RETURNING `+assetInterceptRuleCols,
		id, enabled, kind, pattern, note)
	return scanAssetInterceptRule(row)
}

// DeleteAssetInterceptRule은 규칙을 지운다. 내장 규칙도 지울 수 있다.
func (d *DB) DeleteAssetInterceptRule(id int64) error {
	_, err := d.Exec(`DELETE FROM asset_intercept_rules WHERE id=$1`, id)
	return err
}

// ToggleAssetInterceptRule은 규칙의 켜짐/꺼짐을 바꾼다.
func (d *DB) ToggleAssetInterceptRule(id int64, enabled bool) error {
	_, err := d.Exec(`UPDATE asset_intercept_rules SET enabled=$2 WHERE id=$1`, id, enabled)
	return err
}
