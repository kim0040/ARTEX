package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 알림 채널 설정과 발견 이벤트 층입니다. 탐색 그래프에 발견이 기록되면
// 여기서 이벤트가 생기고, 실제 전달과 전달 이력은 notification_delivery.go가 맡습니다.
//
// 두 가지 불변 조건입니다. 이 파일을 고칠 때 반드시 지키십시오.
//
//  1. 발견을 기록하는 트랜잭션(RecordFindingTx)은 InsertNotificationEventTx로 한 번만 무조건 삽입하고,
//     알림 관련 테이블을 읽지 않으며 필터 매칭도 하지 않습니다. 여기에 읽기를 넣으면
//     사용자가 잘못 넣은 필터 조건 때문에 발견 기록 트랜잭션이 오염되거나 중단될 수 있습니다.
//  2. 필터 매칭은 절대 오류를 내지 않습니다. 설정이 깨지면 항상 「일치」로 처리합니다(notify.Match 참고). 더 보내는 쪽을 택하고,
//     누락은 허용하지 않습니다.

// ErrNotificationChannelNotFound 알림 채널이 없습니다.
var ErrNotificationChannelNotFound = errors.New("알림 채널이 없습니다")

// 전달 상태.
const (
	NotifyStatePending = "pending" // 발송 대기
	NotifyStateSending = "sending" // 어떤 dispatcher가 가져갔고, 임대가 아직 만료되지 않음
	NotifyStateSent    = "sent"    // 전달됨
	NotifyStateFailed  = "failed"  // 재시도가 소진되었거나 영구 실패이며, 수동으로 다시 보낼 수 있음
	NotifyStateSkipped = "skipped" // 알림 채널이 중지되어 더 이상 보내지 않음
)

// 푸시 모드.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode는 푸시 모드를 허용 목록으로 검사합니다(findings.status와 같습니다. DB CHECK를 쓰지 않아
// 나중에 확장하기 쉽습니다).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel은 알림 채널 인스턴스 설정입니다. Config와 Filter는 원본 JSON을 그대로 두고,
// 해석은 notify 패키지에 맡깁니다. db 층은 그 필드 의미를 알지 못합니다.
// 초보자 안내: 이 설정은 어떤 발견을 알림 채널로 보낼지 정하고, 실제 보낸 결과는 전달 이력에 남습니다.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled를 포인터로 둔 이유는 「이 필드를 안 보냄」과 「명시적으로 false를 보냄」을 구분하기 위해서입니다.
	// 프론트엔드 스위치는 바뀐 필드만 제출합니다.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled는 알림 채널이 켜져 있는지 반환합니다. Enabled가 nil(아직 로드되지 않음)이면 켜진 것으로 처리합니다.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent는 하나의 이벤트 사실입니다. 초보자 안내: 발견이 기록되면 이 사실이 생기고, 알림 채널과 전달 이력이 나중에 이 사실을 읽어 사용자에게 보냅니다.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels는 모든 알림 채널 인스턴스를 반환하며, 켜진 것이 앞에 오고 같은 단계에서는 id 순입니다.
// 정렬을 SQL에 둔 이유는 UI와 dispatcher가 같은 안정된 순서를 보게 하기 위해서입니다.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID 는 알림 채널 하나를 가져온다.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel 는 알림 채널을 새로 만들거나 갱신한다.
//
// 갱신할 때는 호출자가 명시한 필드(nil 이 아니고 비어 있지 않은 값)만 덮어쓴다. 그래서 프런트엔드는 일부만
// 고친 서랍 폼을 보낼 수 있고, 화면에 없는 config 필드를 다시 보내지 않아도 된다. 그 필드를 다시 보내면
// 「가린 값이 실제 비밀 키를 덮어쓰는」 사고가 난다.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// 여기서는 0 을 일부러 **전혀** 가공하지 않는다. 0 은 합법적인 설정이며 뜻은 「속도 제한 없음」이다.
	//
	// 예전에는 `if c.RatePerMin <= 0 { c.RatePerMin = 기본값 }` 으로 썼고, 의도는 「지정하지 않았을 때
	// 안전한 기본값을 주자」였지만, 그 식은 「명시적으로 0」까지 함께 삼켜 버렸다. 문서, UI 안내와
	// takeTokens 는 모두 0 을 속도 제한 없음으로 해석하는데, 여기만 조용히 20(DingTalk/WeCom/Telegram)
	// 또는 100(Feishu)으로 바꿨다. 운영자는 속도 제한을 푼 줄 알았지만 실제로는 분당 20 에 막히고 아무 안내도 없었다.
	//
	// 「미지정」과 「명시적 0」의 차이는 호출자만 안다(요청 본문에서 필드가 빠짐 vs 0 을 분명히 전달).
	// 그래서 기본값은 server 층이 필드가 빠졌을 때 채운다. notifyCreateChannel 을 본다.
	if c.RatePerMin < 0 {
		return 0, errors.New("속도 제한 값은 음수일 수 없습니다")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled 는 알림 채널의 사용과 중지를 바꾼다.
//
// 채널을 중지할 때 아직 보내지 않은 전달도 함께 skipped 로 표시한다. 그러지 않으면 다시 켠 뒤
// 「중지 동안 쌓인」 옛 발견이 한꺼번에 도착한다. 시효는 지났고 신규로 오해하기 쉽다.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "알림 채널이 중지되었습니다", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel 는 알림 채널을 삭제한다. 그 전달 이력은 외래 키 연쇄 삭제와 함께 지워진다
// (채널 설정이 없으면 이력을 해석할 수 없다).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx 는 호출자의 트랜잭션 안에서 푸시 이벤트를 **최대한** 한 건 기록한다.
//
// 이것은 발견을 쓰는 경로에서 알림과 관련된 유일한 변경이다. INSERT 한 번이며, 어떤 테이블도 읽지 않고, 채널을 알지 않으며,
// 필터도 돌리지 않는다. 트랜잭션이 커밋되면 「발견 저장」과 「푸시 작업 존재」가 원자적으로 함께 보장되어,
// 커밋은 됐는데 큐에 없거나 메시지가 영원히 사라지는 구간이 없다.
//
// 핵심 설계는 두 가지이며, 어느 쪽도 즉흥으로 적은 것이 아니다.
//
//  1. **왜 SAVEPOINT 를 쓰는가**: PostgreSQL 에서는 트랜잭션 안의 문장 하나가 실패하면 트랜잭션 전체가
//     aborted 상태가 되고, 그 뒤의 모든 문장(COMMIT 포함)이 실패한다. 그래서 「이 INSERT
//     오류는 무시하고 호출자가 계속 커밋하게 하기」는 PG 에서는 불가능하다. 저장점으로 오류를
//     이 문장 하나에 가두지 않으면 그렇다. 저장점이 없으면 「전체 롤백」만 남는다.
//
//  2. **왜 전체 롤백이 틀린가**: 푸시는 편의 기능이고, 발견 기록이 제품 자체다. 알림
//     테이블의 문제(옛 데이터베이스 미이전, 디스크의 순간 장애) 때문에 고위험 발견이 저장되지 못하면 안 된다. 그래서 여기서 오류를 격리하고,
//     로그를 남기고, false 를 반환해 발견 쓰기는 그대로 커밋한다. 대가는 이 푸시 한 건을 버리는 것이다.
//     error 가 아니라 bool 을 반환하는 것은 의도적이다. 호출자가 이것을 쓰기 성공 여부를 가르는 오류로 보면 안 된다.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 푸시 이벤트 직렬화 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 저장점 생성 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 푸시 이벤트 기록 실패 finding=%d(발견 기록은 영향 없음): %v", findingID, err)
		// 저장점으로 롤백해 트랜잭션을 aborted 상태에서 되돌린다.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 저장점 롤백 실패 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 저장점을 해제해 긴 트랜잭션에 쓸모없는 저장점이 쌓이지 않게 한다.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent 는 InsertNotificationEventTx 의 독립 트랜잭션 버전이며, 기존
// 트랜잭션 밖에 있는 호출 지점에서 쓴다(예: 채널의 「테스트 메시지 보내기」. 여기에는 실제 finding 이 없다).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("알림 이벤트 스냅샷 직렬화 실패: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents 는 아직 배분되지 않은 발견 이벤트를 현재 켜진 알림 채널별로 전달 작업으로 펼치고,
// 이번 차례에 처리한 이벤트 수와 새로 만든 전달 수를 반환한다.
//
// 한 차례 전체는 하나의 트랜잭션이다. 이벤트는 FOR UPDATE SKIP LOCKED 로 가져가며, 여러 프로세스가 동시에 돌아도
// 각자 다른 행을 받는다(이 프로젝트의 아카이브 큐 수령도 같은 방식이다.
// db/task_archives.go 의 completeNextArchiveJob 을 본다).
//
// 필터 일치는 일부러 SQL 이 아니라 Go 쪽에서 한다. 채널의 필터 조건은 선택 필드들의 JSONB 이고,
// 여섯 가지 조합의 일치를 SQL 로 쓰면 쿼리를 유지하기 어렵다. 채널 수는 「사람이 직접 넣는 몇 개」라서
// 전부 읽어 메모리에서 한 줄씩 비교하는 편이 더 빠르고 테스트하기도 좋다.
//
// 어떤 채널에도 맞지 않은 이벤트도 fanned_out 으로 표시한다. 그러지 않으면 배분 대기 집합에 영원히 남고,
// tick 마다 다시 훑게 된다.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋이 성공한 뒤에는 no-op이다

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// 스냅샷은 우리가 직접 쓴 것이라 이론상 반드시 파싱된다. 파싱에 실패해도 전달 흐름은 막지 않으며,
		// 이 이벤트는 필드가 모두 비어 필터 조건이 있는 모든 채널이 건너뛴다. 한 건을 덜 푸시하더라도
		// 나쁜 행 하나가 큐 전체를 멈추게 두지는 않는다.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind 는 행 안의 값을 기준으로 한다. 스냅샷 쪽은 렌더링용 복사본이라 옛 버전이 썼을 수 있다.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 이번 차례의 이벤트를 배분 완료로 표시한다. 어떤 채널에도 맞지 않은 이벤트도 함께 표시한다(함수 주석을 본다).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx 는 트랜잭션 안에서 사용 중인 알림 채널을 가져온다. 개수가 적어
// 페이지도 캐시도 두지 않는다. 캐시는 「설정을 바꾼 뒤 언제 적용되는가」라는 별도의 시점 문제를 만든다.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames 는 자산 id 를 짧은 표시 이름으로 풀어 푸시 메시지에 쓴다.
//
// 반환 순서는 인자와 같고, 길이는 인자보다 짧을 수 있다(없는 id 는 건너뛴다). 인자 순서를 유지하는 이유는
// 같은 발견의 메시지가 여러 번 전달되어도 자산 순서가 같게 하기 위해서다. 그렇지 않으면 재시도 후 받은 메시지에서
// 자산 순서가 바뀌어 「자산이 바뀌었다」로 오해된다.
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName 은 자산 유형에 따라 가장 알아보기 쉬운 식별자를 고른다.
// 마지막에는 빈 문자열을 돌려주고, 이름을 얻지 못한 자산을 어떻게 보여줄지는 호출자가 정한다. 이 함수는 자리 표시자를 만들어 내지 않는다.
// 그렇지 않으면 자산#42 같은 잡음이 푸시 메시지에 섞여, 읽는 사람이 실제 도메인으로 오해한다.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify 는 발견 처리 상태를 갱신하고, 같은 트랜잭션 안에서 상태 변경
// 푸시 이벤트를 하나 등록한다.
//
// 반환: from=변경 전 상태, found=발견이 존재하는지, notified=이벤트 등록에 성공했는지.
//
// 의도적으로 정한 동작 세 가지:
//   - 상태가 실제로 바뀌지 않으면 이벤트를 등록하지 않는다. 프론트 서랍이 같은 값을 다시 제출하거나, 자동화 스크립트가
//     멱등하게 재생해도 푸시 잡음이 나면 안 된다.
//   - 발견이 없으면 found=false 를 반환하고 아무것도 쓰지 않는다. 호출자가 이를 404 로 옮긴다.
//   - 이벤트 등록 실패는 상태 갱신에 영향을 주지 않는다(RecordNotificationEventTx 의 저장점 설명 참고).
//     그래서 notified=false 여도 상태는 이미 바뀌어 있으며, 호출자는 이것 때문에 오류를 내면 안 된다.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx 는 **호출자의 트랜잭션** 안에서 발견 상태를 갱신하고 상태 변경 푸시 이벤트를 등록한다.
//
// 트랜잭션 단위 함수로 뺀 이유는 상태를 바꾸는 모든 경로가 같은 의미를 공유하게 하려는 것이다. 이전에는
// patchFinding 만 알림이 있는 버전을 탔고, **재테스트 결론이 「수정됨」일 때**(finding_retests
// 의 `UPDATE findings SET status=...`)는 저장소에 직접 써서, `on_status_change` 를 켠
// 채널은 이런 상태 흐름의 푸시를 전혀 받지 못했다. 화면의 상태는 조용히 바뀌고,
// 운영자는 플랫폼을 열어야 알게 되었다.
//
// 반환: from=변경 전 상태, found=발견이 존재하는지, changed=상태가 정말 바뀌었는지,
// notified=이벤트 등록에 성공했는지(등록 실패는 상태 갱신에 영향을 주지 않는다. RecordNotificationEventTx 참고).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 상태가 정말로 바뀌지 않았으면 이벤트를 등록하지 않는다. 같은 값을 다시 제출하거나 멱등 재생을 해도
		// 푸시 잡음이 나면 안 된다.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats 는 알림 페이지 상단의 요약 카운트다. 이 숫자는 알림 채널과 전달 이력이 지금 어떤지 UI 에 보여 준다.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 가장 오래된 미발송 전달이 지금까지 지난 밀리초
}

// NotificationStatsSnapshot 은 알림 시스템의 건강도를 모은다.
// BacklogAgeMS 는 「푸시가 멈춰 있는지」를 가장 직접 보여 주는 지표다. pending 개수보다 훨씬 쓸모 있다.
// 적체가 3건이든 적체가 3건이든, 그 차이는 3초에서 3시간까지 될 수 있다.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
