package db

import "time"

// LLM 장애 조치 상태: 프로파일마다 한 줄로 회로 차단기 상태를 남긴다.
// 기준 원본은 메모리(llmpool.Registry)에 있다. 이 테이블은 재시작 때
// 냉각 시간이 소리 없이 초기화되지 않게 상태를 넘긴다.

// LLMHealth는 프로파일 하나의 회로 차단기 상태다.
type LLMHealth struct {
	ProfileID int64      `json:"profile_id"`
	Fails     int        `json:"fails"`      // 연속 실패 횟수. 성공하면 0으로 지운다
	Trips     int        `json:"trips"`      // 차단이 열린 총횟수. 대기 시간 계단을 정한다
	OpenUntil *time.Time `json:"open_until"` // nil이거나 지난 시각이면 닫힘(정상)
	LastError string     `json:"last_error"`
	LastAt    time.Time  `json:"last_at"`
}

// LoadLLMHealth는 냉각 시간이 아직 끝나지 않은 프로파일만 돌려준다.
// 시간이 지난 행은 일부러 건너뛴다. 재시작 뒤 냉각이 끝난 프로파일은
// 다시 정상으로 보고, 다음 호출에서 확인한다. 고장난 채로 되살리지 않는다.
func (d *DB) LoadLLMHealth() ([]LLMHealth, error) {
	rows, err := d.Query(`SELECT profile_id,fails,trips,open_until,COALESCE(last_error,''),last_at
FROM llm_profile_health WHERE open_until IS NOT NULL AND open_until > now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMHealth
	for rows.Next() {
		var h LLMHealth
		if err := rows.Scan(&h.ProfileID, &h.Fails, &h.Trips, &h.OpenUntil, &h.LastError, &h.LastAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveLLMHealth는 프로파일의 회로 차단기 상태를 넣거나 갱신한다.
func (d *DB) SaveLLMHealth(h LLMHealth) error {
	_, err := d.Exec(`
INSERT INTO llm_profile_health(profile_id,fails,trips,open_until,last_error,last_at)
VALUES ($1,$2,$3,$4,$5,now())
ON CONFLICT (profile_id) DO UPDATE SET
  fails=EXCLUDED.fails, trips=EXCLUDED.trips, open_until=EXCLUDED.open_until,
  last_error=EXCLUDED.last_error, last_at=now()`,
		h.ProfileID, h.Fails, h.Trips, h.OpenUntil, h.LastError)
	return err
}

// ClearLLMHealth는 프로파일 상태를 지운다. UI의 「지금 복구」가 이 함수를 부른다.
func (d *DB) ClearLLMHealth(profileID int64) error {
	_, err := d.Exec(`DELETE FROM llm_profile_health WHERE profile_id=$1`, profileID)
	return err
}
