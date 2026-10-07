package db

import "database/sql"

// Settings는 UI가 실행 중에 켜고 끄는 전역 설정을 담는 작은 키-값 저장소다
// (예: traffic_capture). 키가 없으면 호출자가 준 기본값을 쓴다.

// GetSetting은 저장된 값을 돌려준다. 키가 없으면 ok=false다.
func (d *DB) GetSetting(key string) (value string, ok bool, err error) {
	err = d.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting은 설정 값을 넣거나 같은 키를 갱신한다.
func (d *DB) SetSetting(key, value string) error {
	_, err := d.Exec(`
INSERT INTO settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return err
}

// GetBool은 불리언 설정을 돌려준다. 없거나 해석할 수 없으면 def를 쓴다.
func (d *DB) GetBool(key string, def bool) bool {
	v, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v == "true" || v == "1"
}

// SetBool은 불리언 설정을 "true" 또는 "false" 문자열로 저장한다.
func (d *DB) SetBool(key string, val bool) error {
	if val {
		return d.SetSetting(key, "true")
	}
	return d.SetSetting(key, "false")
}
