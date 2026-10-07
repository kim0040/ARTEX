package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전달 작업의 수령과 상태 전이를 다룬다.
//
// 수령은 긴 트랜잭션이 아니라 「임대(lease)」를 쓴다. 행을 sending으로 두고 next_attempt_at을 미래로 밀어
// 임대 만료 시각으로 삼은 뒤, 트랜잭션을 커밋하고 나서 네트워크 전달을 한다. 이렇게 하면 전달 동안 데이터베이스 락을 잡지 않는다.
// 네트워크 요청은 수 초가 걸릴 수 있고(클라이언트 타임아웃 15초), 행 락을 붙잡고 있으면 같은 DB의 다른 쓰기가 멈춘다.
//
// 대가는 프로세스가 전달 도중에 죽으면 행이 sending에 남는다는 점이다. 이것은 **스스로 회복**된다. 임대가 만료되면
// next_attempt_at이 과거가 되고, 다음 수령 라운드가 같은 행을 다시 집어 올린다(수령 조건의
// state IN ('pending','sending') 참고). 재시도 횟수는 수령 시점에 이미 +1이므로, 크래시가
// 무한 재시도를 만들지는 않는다. MaxNotifyAttempts번의 기회가 끝나면 failed로 떨어져 사람이 처리한다.

// MaxNotifyAttempts는 한 건의 전달이 가질 수 있는 최대 시도 횟수(첫 시도 포함)이다.
// 전달 엔진이 아니라 여기에 정의한다. 상태 기계 자체의 정책이고, 엔진은 실행자일 뿐이다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize는 요약 배치 하나가 한 번에 최대 몇 건의 전달을 합치는지이다.
//
// 있는 이유는 자원이다. 한 요약 주기 안에 발견이 수만 건 나올 수 있고(충분히 가능하다. 전체 스캔 한 번으로
// 그렇게 된다), 상한이 없으면 수령이 모든 행을 메모리로 읽어 아주 긴 메시지 하나로 렌더링한 뒤
// 채널 길이 상한에 대부분이 잘린다. 메모리를 낭비하고, 잘린 발견을 **조용히 잃는다**.
// 상한을 두면 넘는 부분은 DB에 남아 다음 배치가 되고, 다음 주기에 자연히 나가므로 잃지 않는다.
//
// 500을 고른 근거: 메시지로 렌더링한 뒤에도 기업 위챗 4096바이트 상한 안에서 "읽을 내용이 남는" 규모이다.
// 더 크면 잘림이 더 뒤쪽 위치에서 일어날 뿐이다.
const MaxDigestBatchSize = 500

// NotificationDelivery는 전달 작업 한 건이며, 렌더에 필요한 채널 설정과 이벤트 스냅샷을 담는다.
// 알림 채널과 전달 이력을 잇는 한 줄이라, 알림이 어디로 나갔는지 UI에서 따라갈 수 있다.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 함께 적재한 렌더 컨텍스트. JSON에는 넣지 않는다(server 층이 DTO를 조립한다).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind는 이벤트에서 가져오며, 이력 목록에서 발견 상세로 바로 이동하는 데 쓴다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind는 목록 표시용 중복 필드이며, 프론트의 두 번째 조회를 줄인다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery는 전달 행의 통합 읽기 형태이다. 전달 + 이벤트 스냅샷 + 채널 설정.
// 메시지 하나를 렌더하려면 셋이 모두 필요하고, 나눠 조회하면 왕복이 세 번이 된다.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery는 수령 한 번을 기술한다. 먼저 sel로 후보를 고르고 잠근 뒤, sending으로 두고
// 임대를 늘린다. sel 안의 lease 자리는 호출자가 $n 플레이스홀더로 두고 직접 인자를 넘긴다.
// 이 조회는 전달 이력의 행을 집어 알림 채널로 보내기 직전의 상태를 고정한다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries는 한 채널에서 만료된 실시간 전달을 최대 limit건 수령한다.
//
// 일부러 **채널 하나** 단위로 수령하고, 「전역에서 한 묶음을 받은 뒤 골라 보내기」는 하지 않는다. 속도 제한 게이트는 전달 엔진이 채널별로
// 유지하므로, 이 채널이 이번 라운드에 몇 건을 더 보낼 수 있는지 먼저 알고 같은 수만큼 행을 받아야 재시도 횟수를 소모하지 않는다. 반대로 먼저 받고 버리면, 제한에 막힌 행도 이미 attempts가 한 번 올라
// 3회 예산이 순수한 대기로 소진된 뒤 failed로 떨어진다.
//
// 조건에는 「임대가 만료된 sending」이 들어 있다. 그것이 크래시 자가 회복의 자리이다. lease는 한 번의
// 전달이 최악의 경우 걸리는 시간(채널 HTTP 클라이언트 타임아웃 15초)보다 충분히 커야 한다. 그렇지 않으면 같은 행을 두 dispatcher가
// 동시에 전달한다. 동시에 이미 중지된 채널은 막는다. 중지 작업이 기존 전달을 skipped로 표시했고,
// 여기서 한 번 더 막아 중지와 수령이 동시에 일어날 때의 누락을 피한다.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue는 해당 채널이 만료된 배치를 모았는지 보고한다. 보낼 전달이 있고, **가장 오래된 것**의
// 나이가 요약 주기에 도달했을 때이다.
//
// 판정 기준은 벽시계가 아니라 가장 오래된 전달의 나이이다. 이렇게 하면 막 만든 채널이 정각에 맞춰
// 항목 하나뿐인 「요약」을 바로 내보내지 않고, 오래 쌓인 배치가 한 주기를 더 헛되이 기다리지 않는다.
//
// ClaimDigestBatch와 나눈 이유는 의미가 다르기 때문이다. 이 함수는 「보내야 하는가」만 답하고,
// 수령은 그 채널의 **모든** 대기 행(아직 나이가 차지 않은 것까지)을 가져간다. 그렇지 않으면 한 주기가
// 여러 메시지로 쪼개져 요약의 의미가 사라진다.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch는 한 채널에서 지금 만료된 대기 전달을 요약 배치로 수령하며,
// 한 배치는 최대 MaxDigestBatchSize건이다.
//
// 같은 배치의 모든 전달은 batch_id를 공유하고, 집합의 최소 id를 배치 번호로 쓴다(안정적이고, 읽을 수 있으며,
// 추가 시퀀스가 필요 없다). 재시도 때는 COALESCE로 원래 배치 번호를 유지해 「이 N건은 함께 보낸다」가
// 여러 번 재시도한 뒤에도 성립하게 한다.
//
// 무작위로 고르지 않고 id 오름차순으로 앞 N건을 취한다. 먼저 생긴 전달이 먼저 나가므로, 적체 때
// 「새 발견이 먼저 나가고 오래된 발견은 계속 뒤에 남는」 기아가 생기지 않는다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit은 **메모리 상한**이며, 호출자가 MaxDigestBatchSize를 넘긴다. 여기서 한 번 더 집어,
	// 호출자가 더 큰 값을 넣는 것을 막는다.
	//
	// 일부러 「속도 제한 할당량」을 배치 크기로 받지 않는다. 속도 제한의 단위는 메시지 건수라, 배치 하나는
	// 메시지 하나당 토큰 하나를 소비하며, server 계층의 takeTokens가 차감한다——「한 묶음에 몇 건의
	// 발견」을 담는지와는 서로 다른 단위다. 예전에 rate_per_min이 digest에 적용되도록 매 라운드의
	// 요청 예산을 묶음 크기로 넘겼더니, rate=20/min인 알림 채널은 묶음마다 발견을 1건만 담게 되어
	// digest가 요약 문구가 붙은 실시간 푸시로 퇴화했다. 속도 제한을 바꾸려면 takeTokens의 want를 바꾸고,
	// 여기를 건드리지 마라.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries는 「선정 + sending으로 두고 임대 연장 + 전체 행 읽기」를 한 트랜잭션에서 실행한다.
// postClaim은 선택적 추가 단계다(요약 묶음이 이것으로 batch_id를 기록한다).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋이 성공한 뒤에는 no-op이다

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending으로 두고 next_attempt_at을 미래로 민다. 이 미래 시각이 곧 임대 만료 시각이며,
	// 「임대가 만료되지 않음」과 「재시도 시각이 되지 않음」이 같은 조건으로 표현되므로 열을 새로 둘 필요가 없다.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent는 한 묶음의 전달을 전달 완료로 표시한다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries는 한 묶음의 전달을 pending으로 되돌리고 재시도 시각을 뒤로 민다.
//
// pending으로 되돌리고 새 중간 상태를 두지 않는 이유는 「남은 기회 횟수」를 한곳
// (MaxNotifyAttempts)에서만 표현하여, 재시도 전략에 따라 상태 기계의 분기가 늘어나는 것을 막기 위해서다.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries는 한 묶음의 전달을 pending으로 되돌리고 즉시 다시 가져갈 수 있게 하며, **가져갈 때 센 그 한 번의 시도를 취소**한다.
//
// 용도는 하나뿐이다. 요약 메시지를 채널 길이 상한에 맞춰 나누어 보낼 때, 이 메시지에 담기지 않은 항목은 다음 묶음에 남긴다.
// 그것은 실패가 아니므로 재시도 예산을 소비하면 안 된다. 가져갈 때 attempts는 이미 낙관적으로 +1 되었으니
// 여기서 반드시 다시 빼야 한다. 그렇지 않으면 500건의 적체가 구간당 20건으로 25구간이 되고,
// 꼬리 항목은 3번째 구간에서 MaxNotifyAttempts에 의해 failed로 판정되는데, 그 항목들은 한 번도 오류를 낸 적이 없다.
//
// GREATEST(...,0)은 「누군가 수동 재발송으로 attempts를 0으로 만든 뒤 여기로 다시 오는」 경우를 막아
// 카운트가 음수가 되지 않게 한다.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries는 한 묶음의 전달을 최종 실패로 표시하고, 사람이 전달 이력에서 다시 보내기를 기다린다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 자리표시자는 $3부터 시작한다. $1은 state, $2는 last_error이다.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery는 전달 하나를 수동으로 다시 보낸다. pending으로 되돌리고, 재시도 횟수를 0으로 만들고,
// 즉시 만료시킨다. 횟수를 지우는 것은 의도적이다. 사람이 「재발송」을 누르면 이전 실패 원인은 이미 처리된 것이므로,
// 옛 횟수로 다시 제한할 이유가 없다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전달 %d이 없거나 현재 상태에서는 재발송할 수 없습니다", id)
	}
	return nil
}

// NotificationDeliveryFilter는 전달 이력의 조회 조건이다. UI는 이 조건으로 알림 채널이 남긴 전달 이력을 걸러 보여 준다.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries는 전달 이력을 페이지로 반환하며, 최신이 앞에 온다.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError는 오류 메시지를 열이 받아들일 수 있는 길이로 자른다. 채널이 돌려준 응답 본문은 길 수 있고
// (범용 Webhook이 자체 서비스에 닿을 때 특히 그렇다), 자르지 않으면 이력 목록의 적재량이 불어난다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 문자 경계까지 되돌려, UTF-8 문자가 반쪽만 남아 UI에 깨진 글자로 보이지 않게 한다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders는 start부터 시작하는 $n 자리표시 문자열과 대응 인자를 만들어 IN (...)에 쓴다.
// 예를 들어 start=3, ids=[7,8]이면 "$3,$4", [7,8]이다.
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
