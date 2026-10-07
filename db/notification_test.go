package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일의 케이스는 모두 PostgreSQL에 실제로 연결한다(데이터베이스가 없으면 건너뛴다). 이 SQL은
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, 여러 행 IN(...) 자리 표시자 이어 붙이기를 쓰며,
// 모두 「컴파일은 통과하지만 실행 때 오류가 날 수 있는」 작성법이므로, 실제로 실행해야 검증된 것이다.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel은 알림 채널을 하나 만들고, 테스트가 끝나면 자동으로 삭제한다.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "테스트 알림 채널-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("알림 채널 생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent는 finding을 거치지 않고 이벤트를 한 건 직접 쓰며, 분배와 전달을 테스트하는 데 쓴다.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("이벤트 기록 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 세 종류의 자산은 각자 표시 기준이 다르다. 도메인, IP, URL.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 넣는 순서를 일부러 섞었고, 존재하지 않는 id를 하나 포함한다.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("자산 이름 해석 실패: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("자산 이름 수가 일치하지 않음, 기대 %v 실제 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("순서/값이 일치하지 않음, 기대 %v 실제 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure는 저장점 메커니즘의 핵심 케이스다.
// 트랜잭션 안에서 notification_events 쓰기가 반드시 실패하게 만든 뒤(항상 false인 제약을 임시로 추가),
// ① 이 함수가 false를 보고하고 ② 트랜잭션이 aborted 상태에 들어가지 않아 이후 문장이 계속 실행되는지 단언한다.
//
// 저장점이 없으면 PostgreSQL은 트랜잭션 전체를 무효로 만들고, 이후 어떤 문장도
// "current transaction is aborted"로 실패한다. 그것이 바로 「알림 테이블 하나의 문제 때문에
// 발견을 데이터베이스에 넣지 못하는」 장애 경로다.
//
// 여기서는 일부러 **COMMIT이 아니라 ROLLBACK으로 끝낸다**. ALTER TABLE은 PG에서 트랜잭션에 속하므로,
// 한 번 커밋하면 그 임시 제약이 schema에 영구히 남아 이후 모든 케이스를 함께 망가뜨린다.
// 롤백은 DDL을 자동으로 되돌리므로 수동 정리가 필요 없다. 단언에는 「트랜잭션이 아직 살아 있음」만 필요하고,
// 실제로 커밋할 필요는 없다.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 방어적 정리: 이전 실행이 이 제약을 남겨 두었으면 먼저 제거한다.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 임시 제약을 되돌린다. 함수 주석을 본다.

	// NOT VALID: 이후 기록되는 행만 제약하고, 데이터베이스에 이미 있는 과거 이벤트는 검사하지 않는다.
	// (그렇지 않으면 기존 행의 위반 때문에 제약 조건을 걸 수 없다).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("임시 제약 추가 실패: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("반드시 실패해야 하는 제약인데도 쓰기 성공을 보고함")
	}
	// 핵심 단언: 트랜잭션을 아직 쓸 수 있다.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("트랜잭션이 오염됨(세이브포인트가 적용되지 않음): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("롤백 실패: %v", err)
	}
	// DDL이 롤백과 함께 되돌려졌는지 확인하고, 이후 테스트 케이스에 함정을 남기지 않는다.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("임시 제약이 롤백으로 되돌려지지 않아 이후 테스트 케이스를 오염시킨다")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL 인젝션"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"전체 수신 채널이 high를 받음", highSQL, all.ID, true},
		{"전체 수신 채널이 low를 받음", lowXSS, all.ID, true},
		{"심각 전용 채널은 high를 건너뜀", highSQL, onlyCritical.ID, false},
		{"심각 전용 채널이 critical을 받음", criticalXSS, onlyCritical.ID, true},
		{"SQL 전용 채널이 SQL을 받음", highSQL, sqlOnly.ID, true},
		{"SQL 전용 채널은 XSS를 건너뜀", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("전달 존재 여부: 기댓값 %v, 실제값 %v", tc.want, exists)
			}
		})
	}

	// 한 번 더 분배해도 중복 전달이 생기면 안 된다(fanned_out 멱등).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("이미 분배된 이벤트는 다시 처리되면 안 되는데, events=%d deliveries=%d 를 얻었다", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel 은 「이벤트가 어떤 채널에도 맞지 않는」 경우를 다룬다.
// 이런 이벤트도 분배됨으로 표시해야 한다. 그렇지 않으면 대기 분배 집합에 영원히 남고 매 tick 마다 다시 훑는다.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["절대 매칭되지 않는 유형"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("전달이 생기면 안 되는데 %d건을 얻었다", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("채널에 맞지 않은 이벤트도 분배됨으로 표시해야 한다. 그렇지 않으면 끝없이 다시 훑는다")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 실시간 수령은 realtime 채널의 그 한 건만 가져와야 하고, digest 채널의 것은 건드리면 안 된다.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("수령 실패: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1건을 수령해야 하는데 %d건을 얻었다", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("수령 후에는 sending 이고 attempts=1 이어야 하는데 state=%s attempts=%d 를 얻었다", got[0].State, got[0].Attempts)
	}
	// 함께 불러온 렌더링 컨텍스트가 모두 있어야 한다(채널 설정 + 이벤트 스냅샷 + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("수령 결과에 채널 설정이 없어 렌더링이 실패한다")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id가 이벤트에서 따라오지 않았고 %d 를 얻었다", got[0].FindingID)
	}

	// 리스가 만료되지 않았으면 두 번째 수령은 비어야 한다. 이는 「같은 행을 두 dispatcher가 동시에 전달하지 않는다」는 뜻이다
	// 의 보장.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("임대 기간 안에는 중복 수령되면 안 되며, %d건을 얻었다", len(again))
	}

	// digest 알림 채널의 전달은 실시간 수령에 걸리면 안 된다.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("실시간 수령은 digest 알림 채널의 전달을 가져가면 안 되며, %d건을 얻었다", len(left))
	}
}

// TestClaimExpiredLeaseRecovers는 비정상 종료 후 자가 복구를 다룬다. 프로세스가 전달 도중에 죽으면 sending
// 행이 남고, 임대가 만료된 뒤에는 다시 수령할 수 있어야 한다. 그렇지 않으면 이 전달은 영원히 멈춘다.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("첫 수령 실패: %v (%d건)", err, len(first))
	}
	// 임대를 수동으로 과거로 밀어 「임대가 이미 만료됨」을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("임대가 만료된 sending 행은 다시 수령할 수 있어야 하며, %d건을 얻었다", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("다시 수령하면 시도 횟수가 누적되어야 하며, %d를 얻었다", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 비활성화하면 남아 있는 발송 대기 전달을 함께 skipped로 표시한다.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("비활성화된 알림 채널의 기존 발송 대기 전달은 skipped로 표시되어야 하며, %s를 얻었다", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("이미 비활성화된 알림 채널은 수령할 수 없어야 하며, %d건을 얻었다", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 배치를 방금 만들었고 나이가 0이면, 30분 주기에서는 만료되면 안 된다.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("배치 만료 판정 실패: %v", err)
	}
	if due {
		t.Fatal("방금 만든 배치는 즉시 만료되면 안 된다")
	}

	// 전달 3건의 생성 시각을 함께 과거로 밀어, 주기를 채운 배치를 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("주기를 넘긴 배치는 만료로 판정되어야 한다")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("요약 배치 수령 실패: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("요약은 한 번에 3건을 모두 가져가야 하며, %d건을 얻었다", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("요약 배치는 batch_id를 반드시 기록해야 한다. 그렇지 않으면 전달 이력에서 이 몇 건이 함께 발송됐는지 알 수 없다")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("같은 배치는 batch_id를 공유해야 하며, %v 대 %d를 얻었다", dl.BatchID, firstBatchID)
		}
	}

	// 이 배치 **전체**를 실패로 재배치한 뒤 다시 수령하면, batch_id는 원래 값을 유지해야 한다(COALESCE의 역할):
	// 그렇지 않으면 재시도 한 번으로 「이 배치는 함께 발송됐다」는 사실이 지워진다.
	//
	// 한 건만 재배치하면 안 되고 배치 전체를 재배치해야 한다. 전달 엔진이 요약 메시지를 보낼 때 이렇게 처리한다
	// (메시지 하나가 배치 전체를 나타내고, 성공과 실패를 함께한다). 한 건만 재배치하면 나머지는 아직 임대 기간 안이라,
	// 다시 수령하면 자연히 그 한 건만 받는다.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "실패 흉내"); err != nil {
		t.Fatal(err)
	}
	// 임대를 과거로 밀어, 백오프 시간이 이미 지났음을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("다시 수령하면 3건을 모두 받아야 하며, %d를 얻었다", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("재시도 뒤 batch_id는 원래 값 %d를 유지해야 하며, %v를 얻었다", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("가져오기 실패: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "네트워크 불안정"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "네트워크 불안정" {
		t.Fatalf("다시 대기열에 넣은 뒤 상태는 pending이어야 하고 이유가 기록되어야 하는데, state=%s err=%q를 받았습니다", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "재시도 소진"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed여야 하는데, %s를 받았습니다", state)
	}

	// 수동 재발송은 재시도 횟수를 0으로 만들고 즉시 만료해야 합니다. 그렇지 않으면 이전 실패 예산을 그대로 물려받습니다.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("재발송 실패: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("재발송 뒤 상태는 pending이고 attempts=0이어야 하는데, state=%s attempts=%d를 받았습니다", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("재발송하면 즉시 가져올 수 있어야 합니다")
	}

	// 전달이 완료된 건은 재발송할 수 없습니다.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("전달이 완료된 건은 재발송을 허용하면 안 됩니다")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "페이지네이션 테스트"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if total != 5 {
		t.Fatalf("총개수는 5여야 하는데, %d를 받았습니다", total)
	}
	if len(page1) != 2 {
		t.Fatalf("페이지당 2건이어야 하는데, %d를 받았습니다", len(page1))
	}
	// 최신이 앞입니다. 첫 페이지 첫 항목의 id는 둘째 페이지 첫 항목보다 커야 합니다.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("페이지 순서는 최신이 앞이어야 하는데, page1[0]=%d page2[0]=%d를 받았습니다", page1[0].ID, page2[0].ID)
	}
	// 렌더링 맥락은 이력과 함께 반환되어야 합니다. 그렇지 않으면 목록에서 무엇을 푸시했는지 보여줄 수 없습니다.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("이력 항목에 표시 필드가 없습니다: %+v", page1[0])
	}

	// 상태별 필터: pending인 항목이 없습니다.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending 전달이 있으면 안 되는데, %d건이 있습니다 (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("알림 상태 변경 테스트", "목표", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 인젝션", Name: "상태 변경 사례",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 저장할 때 finding_created 이벤트를 하나 등록해 두었으니, 먼저 그것을 세어 기준선으로 삼습니다.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("발견을 저장할 때는 같은 트랜잭션에서 푸시 이벤트 하나를 등록해야 합니다")
	}

	// 같은 상태로 바꾸면 이벤트가 생기면 안 됩니다(반복 제출로 푸시 소음이 나지 않게).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("상태가 바뀌지 않았으면 푸시 이벤트를 등록하면 안 됩니다")
	}
	if from != "pending" {
		t.Fatalf("변경 전 상태인 pending을 반환해야 하는데, %q를 받았습니다", from)
	}

	// 실제 변경: 이벤트를 등록하고 from/to를 기록해야 한다.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("상태가 실제로 바뀌면 푸시 이벤트를 등록해야 한다")
	}
	if from != "pending" {
		t.Fatalf("from은 pending이어야 하는데 %q를 받았다", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("상태 변경 이벤트를 찾지 못했다: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("스냅샷의 상태 전이가 잘못되었다: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 스냅샷에는 렌더링에 필요한 필드를 넣어야 한다. 그렇지 않으면 상태 변경 메시지가 빈 껍데기가 된다.
	if snap.VulnClass != "SQL 인젝션" || snap.Severity != "high" || snap.Name != "상태 변경 사례" {
		t.Fatalf("스냅샷에 렌더링 필드가 없다: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("상태는 fixed로 갱신되었어야 하는데 %s를 받았다", status)
	}

	// 존재하지 않는 발견: found=false이며 오류를 내지 않는다.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("존재하지 않는 발견은 found=false와 오류 없음을 반환해야 하는데 found=%v err=%v를 받았다", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("집계 실패: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("알림 채널 건수가 맞지 않다: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("발송 대기 전달이 집계되어야 한다: %+v", stats)
	}
	// 방금 만든 전달의 적체 연령은 0에 가까워야 하며, 음수나 아주 큰 값이면 안 된다.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("적체 연령이 올바르지 않다: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 왕복",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("생성 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 왕복" {
		t.Fatalf("왕복 필드가 일치하지 않는다: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("기본값은 활성화여야 한다")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("설정이 올바르게 저장되지 않았다: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("필터 조건이 올바르게 저장되지 않았다: %+v", filter)
	}

	// 갱신한 뒤 다시 읽는다.
	got.Name = "이름을 바꿨다"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "이름을 바꿨다" || after.IsEnabled() {
		t.Fatalf("갱신이 반영되지 않았다: %+v", after)
	}

	// 삭제 후에는 조용히 성공하면 안 되고 「없음」을 알려야 한다.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("ErrNotificationChannelNotFound를 기대했는데 %v를 받았다", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("중복 삭제는 없음을 알려야 하는데 %v를 받았다", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate는 예전에 잘못 쓰였던 지점을 고정한다:
// **0은 유효한 설정이며 의미는 「속도 제한 없음」이다. db 계층이 「미지정」으로 보고 기본값으로 덮어쓰면 안 된다**.
//
// 과거 버그: SaveNotificationChannel에 `if RatePerMin <= 0 { 기본값을 취함 }`이 있었다.
// 그래서 문서, UI 안내, takeTokens는 모두 「0=속도 제한 없음」으로 해석했는데, 저장 계층만 몰래
// 20(딩톡/기업위챗/Telegram) 또는 100(페이슈)으로 바꿨다. 조작자는 제한을 푼 줄 알았지만 실제로는 막혀 있었고
// 어떤 안내도 없었다. 「미지정」과 「명시적 0」의 차이는 요청 본문만 표현할 수 있으므로
// 기본값은 server 계층에서 채운다(notifyCreateChannel 참고). db 계층은 저장만 한다.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 명시적 0(속도 제한 없음): 그대로 저장해야 한다.
	unlimited := &NotificationChannel{
		Name: "제한 없음", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("명시적 0은 제한 없음을 뜻하며, 그대로 저장되어야 한다. 얻은 값 %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("기본 모드는 realtime이어야 한다. 얻은 값 %s", got.Mode)
	}

	// 음수는 잘못된 입력이며, 조용히 다른 값으로 바뀌지 않고 거부되어야 한다.
	bad := &NotificationChannel{
		Name: "음수 제한", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("음수 제한은 거부되어야 한다")
	}
}

// TestDeleteChannelCascadesDeliveries는 외래 키 동작을 고정한다. 채널을 삭제하면 그 전달 이력도 함께 사라진다
// (설정이 없어지면 이력을 해석할 수 없다). 이벤트 자체는 남겨 둔다. 다른 채널이 아직 참조할 수 있기 때문이다.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("사전 조건이 성립하지 않음: 전달이 생성되지 않음")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("채널 삭제 후 그 전달은 연쇄 삭제되어야 하는데, 아직 %d건이 남아 있다", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("채널을 삭제해도 이벤트 자체는 함께 삭제되면 안 된다")
	}
}

// TestClaimDigestBatchHonorsCallerLimit는 감사에서 지적한 빈틈을 덮는다.
// 요약 채널은 이전까지 토큰 버킷을 완전히 우회했다. allow는 takeTokens로 차감되었지만 아무도 쓰지 않았고,
// rate_per_min은 digest 모드에 아무 작용도 하지 않았다. 이제 limit도 제약에 참여한다.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3으로 두면 3건만 수령할 수 있고, 나머지는 저장소에 남긴다.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("수령 실패: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("호출자 제한 한도대로 3건만 수령해야 한다. 얻은 값 %d", len(got))
	}
	// limit=0은 이번 회차 한도가 소진되었음을 뜻한다. 한 건도 수령하면 안 되고, 오류를 내서도 안 된다.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("한도가 0이면 0건을 수령하고 오류가 없어야 한다. 얻은 값 %d건 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange는 감사에서 지적한 무결성 빈틈을 덮는다.
// 재검사 결론이 「수정됨」이면 상태는 실제로 바뀌지만, 그 UPDATE는 알림이 있는 버전을 우회해 저장소에 직접 기록한다.
// 그래서 on_status_change를 설정한 채널은 이런 상태 전이를 전혀 전달받지 못하고, 화면의 상태는 조용히 바뀌어 운영자가 플랫폼을 열어야 알게 된다.
//
// 이 테스트는 「상태를 바꾸는 모든 경로가 상태 변경 이벤트를 등록해야 한다」는 점을 고정한다.
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("재검사 전달 테스트", "목표", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 인젝션", Name: "재검사 대상",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 재검사 기록을 하나 만들고 바로 완료 상태로 밀어 넣는다.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "재검토")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("재검사는 세션 하나와 연결되어야 한다")
	}
	// 재검사는 결론을 내리기 전에 먼저 running에 들어가야 한다(실제 흐름과 같다).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("재검사 시작 실패: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "수정됨", "증거"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("재검사 종료 실패: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("재검사에서 수정됨으로 판정한 뒤 상태는 fixed여야 한다. 얻은 값 %s", status)
	}

	// 핵심 단언: 상태 변경 이벤트가 하나 있어야 하고, from/to가 맞아야 한다.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("재검사에서 수정됨으로 판정하면 상태 변경 전달 이벤트를 등록해야 한다(그렇지 않으면 on_status_change를 설정한 채널이 받지 못한다): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("스냅샷의 상태 전이가 맞지 않다: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 스냅샷에는 렌더링에 필요한 필드가 있어야 한다. 그렇지 않으면 전달 결과가 빈 껍데기가 된다.
	if snap.Name != "재검사 대상" || snap.Severity != "high" {
		t.Fatalf("스냅샷에 렌더링 필드가 없다: %+v", snap)
	}
}
