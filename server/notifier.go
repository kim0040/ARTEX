package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 전역 설정 키다(settings 키-값 테이블에 두며, 테이블을 새로 만들 필요 없다).
const (
	// settingNotifyEnabled는 알림 전체 스위치다. 기본은 켜짐이다. 유지보수 때 한 번에 멈추려고 쓰며,
	// 기능의 사용 조건은 아니다. 진짜 사용 조건은 채널을 설정했느냐이다.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL은 발견(finding) 상세로 돌아가는 링크를 만들 외부 접속 주소다
	// (예: https://artex.example.com). 비워 두면 메시지에 되돌아가는 링크 버튼이 없다.
	// 프로젝트에 재사용할 외부 주소 설정이 없어서 여기에 항목을 하나 추가한다.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes는 요약 모드의 주기(분)다.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick은 전달 엔진의 폴링 간격이다. 3초는 이 엔진 실시간성의 상한이며,
	// 발견(finding)이 DB에 기록된 뒤 IM에 메시지가 도착하기까지의 주요 지연 원인이기도 하다.
	notifyTick = 3 * time.Second
	// notifyLease는 전달을 집어 갈 때의 임대 시간이다. 한 번 전달의 최악 소요 시간보다 분명히 커야 하며
	// (notify 패키지의 HTTP 클라이언트 타임아웃 15초), 그렇지 않으면 같은 행을 두
	// dispatcher가 동시에 전달하게 된다.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick은 한 회전에 나누는 이벤트 수를 제한한다. 채널을 처음 켤 때
	// 쌓인 이력을 한 번에 전부 전달 작업으로 펼치는 일을 막는다.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes는 요약 주기의 기본값이다.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick은 채널에 속도 제한이 없을 때 한 회전의 전달 상한이다.
	// 있는 이유는 채널 하나를 무제한으로 두고 한 번에 발견(finding)이 수천 개 나올 때
	// 한 회전 루프가 길게 막히는 일을 막기 위해서다.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick은 채널 하나가 한 회전에 최대 몇 건을 전달하는가다.
	//
	// 이 상한은 **임대 시간**에서 거꾸로 정한다. 집어 갈 때 행에 찍는 것은 임대(notifyLease = 3분)이며,
	// 한 회전에서 직렬로 보내는 건수가 많아 최악 소요 시간이 임대를 넘으면, 뒤의 몇 건은 보내기 전에 임대가 만료된다.
	// 프로세스 하나 안에서는 상관없다(Run은 goroutine 하나가 직렬로 돌고, tick은 재진입하지 않는다). 그러나 **두
	// 프로세스가 같은 DB에 붙으면** 상대가 임대가 만료된 행을 다시 집어 가 중복 전송하고, 또
	// attempts를 두 배로 올리고, 원래 프로세스가 아직 전달 중인데도 실패로 판정한다.
	//
	// 값: 3분 임대 / 30초 1회 타임아웃 = 6은 **임대를 딱 다 쓰고** 여유가 0이라
	// 쓸 수 없다. 5로 두면 최악 150초에 30초 여유가 남는다. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease가 고정한다. notifyLease,
	// notifySendTimeout, 또는 이 값 중 아무거나 바꾸면 그 단언이 실패한다.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout은 한 번 전달의 타임아웃이다. 바로 위 상수의 값도 같이 정하며,
	// 둘을 곱한 값이 notifyLease를 넘으면 안 된다. TestNotifyTickBudgetFitsWithinLease를 본다.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff는 실패 재시도의 백오프 수열이며, 첨자는 이미 시도한 횟수다.
// 3번의 기회(첫 시도 포함)는 db.MaxNotifyAttempts와 대응하며, 둘은 함께 바꿔야 한다.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier는 발견(finding) 알림의 전달 엔진이다. 자산 그래프와 탐색 그래프를 갱신하는 일과 따로, 기록된 발견을 밖으로 보낸다.
//
// Scheduler와 나란히, 독립 goroutine으로 돈다(server.New를 본다). 일부러
// Scheduler의 tick을 재사용하지 않는다. 알림의 실시간 요구(3초)와 트리거의 업무 리듬이 다르고,
// 둘의 실패는 서로 엮이지 않는다. 알림이 막혀도 agent 트리거에 영향을 주면 안 된다.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu는 buckets를 지킨다. 채널 수가 적고 경합이 낮아 뮤텍스 하나로 충분하며,
	// 더 잘게 나눈 구조를 들일 가치는 없다.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket은 채널 하나의 토큰 버킷이다. 엔진이 발견(finding)을 채널로 보낼 때의 속도 제한이며, 자산 그래프·탐색 그래프와는 별개의 알림 경로다.
//
// 「분마다 센 뒤 0으로 되돌리기」식 슬라이딩 윈도우 대신 토큰 버킷을 쓰는 이유는, 윈도우 쪽의 경계 효과가 나쁘기 때문이다:
// 윈도우 끝에 20건을 다 보내고 다음 순간에 다시 20건을 보내면, 플랫폼 입장에서는 1초 안에 40건이고,
// 속도 제한에 걸린다. 토큰 버킷은 일정한 속도로 보충되므로 이런 폭주를 자연스럽게 피한다.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run은 ctx가 끝날 때까지 돈다. server.New가 한 번 시작한다.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step은 한 바퀴를 돈다. 먼저 새 이벤트를 배분하고, 이어서 기한이 된 작업을 전달한다.
//
// 어느 단계가 실패해도 로그만 남기고 루프는 끊지 않는다. 알림 시스템의 고장이 프로세스 단위 문제로 커지면 안 된다.
// tick마다 독립이라, 다음 바퀴에서 자연스럽게 다시 시도한다.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 이벤트 분배 실패: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 채널 읽기 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 버킷의 단위는 **메시지 건수**(HTTP 요청 수와 같다)이며, 발견(finding) 건수가 아니다.
		// 실시간 모드에서는 둘이 같다(발견(finding) 하나당 메시지 하나). 요약 모드에서는 한 묶음의 발견(finding)을
		// 메시지 하나로 합치므로 토큰을 하나만 쓴다.
		//
		// 두 모드 모두 먼저 토큰 버킷에 묻고, 그다음 한도만큼 가져온다. 순서를 뒤집으면 안 된다. 그렇지 않으면 속도 제한에 막힌
		// 전달이 이미 재시도 횟수를 소모한 상태가 된다.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan은 요약 채널이 이번 바퀴에 쓸 토큰 소모와 배치 크기 상한을 반환한다.
//
// 반환값 둘은 **서로 다른 단위**이며, 그래서 함수로 떼어 둔다:
//
//   - tokens는 메시지 건수다. 발견(finding) 한 묶음이 메시지 하나, HTTP 요청 한 번이므로 항상 1이다.
//     그래서 rate_per_min은 digest에도 그대로 적용된다(분당 요약 메시지는 최대 이 건수).
//   - claimLimit는 이 배치에 발견(finding)을 최대 몇 건 담는지다. 메모리 상한만 따르며, 요청 예산과는 무관하다.
//
// 예전에는 rate_per_min이 digest에 적용되게 하려고, 바퀴마다의 요청 예산을
// (notifyMaxSendsPerChannelPerTick, 리스에서 거꾸로 계산한 값) 그대로 배치 크기로 넘겼다.
// 그 결과 rate_per_min=20인 채널은 3초 tick에서 토큰이 1개만 채워지고, 요약 메시지마다
// 발견(finding)을 1건만 담게 된다. digest가 「요약 문구가 붙은 실시간 푸시」로 퇴화하고, 읽는 사람은 이어진
// 「최근 30분에 발견(finding) 1건 추가」를 받는다. db.MaxDigestBatchSize에는 영원히 닿지 못한다.
//
// 이 증상은 엔드투엔드 테스트에서 잘 안 보인다(기존 케이스는 충분히 큰 limit를
// stepDigest에 직접 넘겨 step 안의 한도 계산을 우회한다). 그래서 결정을 여기에 모아
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget이 직접 고정한다.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime은 한 채널의 실시간 작업을 집어 전달한다. 발견(finding) 하나당 메시지 하나다.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전달 수령 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q 이(가) 등록되지 않았습니다", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더 실패는 로컬 데이터 문제라, 재시도해도 나아지지 않는다.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest는 배치 기한이 되면 한 채널의 대기 중인 전달을 메시지 하나로 모아 보낸다.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 요약 배치 판정 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 요약 배치 수령 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q 이(가) 등록되지 않았습니다", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 스냅샷이 깨져 메시지에 들어가지 못한 전달은 명시적으로 실패로 판정해야 한다. 그렇게 하지 않으면 그것들은
	// included 밖에 남고, 메시지에도 실패 목록에도 들어가지 않는다. 전송이 성공하면 그 상태는
	// 이후의 일괄 표시에서 빠지고, 리스가 만료되어 반복해서 집어 갈 때까지 sending에 영원히 머문다.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "이벤트 스냅샷을 해석할 수 없어, 이 발견(finding)을 메시지로 렌더링할 수 없습니다"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 손상된 스냅샷 전달 표시 실패 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅샷을 해석할 수 없는 전달 %d건을 건너뜁니다 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 들어간 것만 send에 넘긴다. included[i]와 msg.Items[i]는 엄격히 대응하고,
	// send는 이 대응에 기대어 「채널이 앞의 K건만 담았다고 알린 것」을 올바른 전달 행에 반영한다.
	n.send(ctx, channel, cfg, msg, included)
}

// send는 전달하고 결과에 따라 상태를 옮긴다.
//
// 같은 묶음의 전달(요약 모드에서는 수십 건일 수 있다)은 전송 결과 하나를 공유한다. 도달하거나, 묶음 전체가 재시도된다.
// 건별로 재시도하지 않는다. 요약 메시지는 하나라, 그중 일부만 다시 보내면 배치의 의미가 어긋난다.
//
// 유일한 예외는 **채널 길이 상한 때문에 나뉘는 경우**다. 채널이 실제로는 앞의 K건만 담았다고 알리면,
// K+1번째부터는 다음 배치에 남겨야 하며, 함께 성공으로 표시하면 안 된다. 그렇지 않으면 잘린
// 그 발견(finding)은 메시지에도 없고 실패 목록에도 없어, 완전히 사라진다.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 한 번의 전달에 상한을 두어, 한 채널이 멈추면 이번 바퀴의 나머지 채널까지 전부 끌려가지 않게 한다.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 채널이 알린 건수는 전달 건수를 넘을 수 없다. 실제로 넘으면 렌더 계층이 잘못 계산한 것이므로,
			// 전부 도달한 것으로 처리하고 문제를 기록하는 편이, 기록을 어지럽히는 것보다 낫다.
			log.Printf("[notify] 채널이 보고한 도달 건수 %d가 전달 수 %d를 초과합니다 channel=%s, 전부 도달한 것으로 처리합니다",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 도달 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 이 메시지가 채널 길이 상한에 닿았다. 나머지는 즉시 대기열로 돌려, 다음 tick이 이어서 보낸다.
			// RescheduleDeliveries가 아니라 DeferDeliveries를 쓴다. 이것은 실패가 아니라,
			// 재시도 예산을 쓰면 안 된다(집어 갈 때 이미 낙관적으로 +1 했고, 거기서 다시 뺀다).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("이 메시지가 채널 길이 한도에 도달하여, 앞의 %d건만 전달하고 나머지는 다음 배치에 남깁니다", delivered)); err != nil {
				log.Printf("[notify] 분할 이어 보내기 대기열 등록 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 채널이 오류도 알리지 않고, 몇 건이 도달했는지도 말하지 않았다. 실패로 처리하고(백오프를 타고),
		// 이 전달이 반복해서 집어 가면서도 영원히 표시가 안 끝나는 일을 막는다.
		err = fmt.Errorf("채널이 도달 건수를 보고하지 않았습니다(delivered=%d)", delivered)
	}

	// 실패 처리는 **건마다** 정한다. 묶음 전체의 최대 시도 횟수로 판단하지 않는다.
	//
	// 예전에는 `if maxAttempts(deliveries) >= MaxNotifyAttempts`로 묶음 전체를 실패로 확정했지만, 배치 안에서
	// 각 건의 시도 횟수는 같지 않다. 이미 두 번 재시도한 오래된 전달(attempts=2)이 같은 배치의
	// 새 전달(attempts=1)까지 failed로 끌어들인다. 새 발견(finding)은 재시도를 한 번도 쓰지 못하고 영구히 버려지고,
	// 「오래된 행이 새 행을 끌어내리지 않는다」는 처음 의도와 정반대다.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 중 오류 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d회 재시도 후에도 실패: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 중 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 지연별로 묶어 다시 정렬한다. 백오프는 3단뿐이라 묶음 수가 원래 작고, 건마다 따로
	// UPDATE를 보낼 필요가 없다(그러면 500건 배치가 왕복 500번이 된다).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 재정렬 전달 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전달 실패 channel=%d kind=%s 영구 실패=%d 재시도 소진=%d 재시도 대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries는 all 가운데 keep에 없는 것을 반환한다(포인터 정체성으로 비교).
// 「메시지에 들어가지 못한」 전달을 찾는 데 쓴다. 그것들은 명시적으로 처리해야 하며, 회색 지대에 두면 안 된다.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt는 채널 구현을 가져와 그 설정을 파싱한다.
// ok=false를 반환하면 타입이 등록되지 않았다는 뜻이며, 전달은 무한 재시도 없이 바로 실패로 판정해야 한다.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 파싱이 실패하면 빈 map을 준다. 채널 자신의 Validate가 「어느 필드가 빠졌는지」를 알려 주고,
		// 그 오류가 JSON 파싱 오류보다 사용자가 고치는 데 더 도움이 된다.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle은 발견(finding) 메시지 한 건을 렌더링한다.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch는 요약 메시지를 렌더링한다. 스냅샷을 한 건씩 파싱하며, 한 건이 깨지면 그 한 건만 건너뛰고,
// 깨진 한 건이 배치 전체 요약을 없애지 않게 한다.
//
// 반환값 included는 msg.Items와 **엄격히 일대일**이다(i번째 전달 ↔ i번째 항목).
// 이 대응은 강제 요건이다. 호출자는 「채널이 앞의 K건을 담았다고 보고했다」에 따라 앞의 K개 전달을
// 전달 완료로 표시한다. 여기서 깨진 스냅샷을 건너뛰면서 건너뛴 전달을 included에서 빼지 않으면,
// 인덱스가 어긋난다. 실패해야 할 깨진 항목이 전달 완료로 표시되고, 정상 항목은 미전달로 잘못 판정된다.
// 깨진 것들은 호출자가 명시적으로 실패로 표시한다. stepDigest를 본다.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 깨진 스냅샷은 메시지에도 included에도 넣지 않는다. 그 처리는 호출자가 담당하고
			// (전달 완료에 섞어 넘기지 않고, 명시적으로 실패로 표시한다).
			log.Printf("[notify] 요약 배치에서 해석할 수 없는 스냅샷을 건너뜀 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("요약 배치의 전달 %d건을 모두 해석할 수 없습니다", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor는 이벤트 스냅샷을 푸시 대기 항목으로 렌더링하고, 이어서 자산 이름과 상세 역링크를 파싱한다.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산 이름 파싱 실패가 푸시를 막아서는 안 된다. 이름을 못 읽는 편이 알림을 못 받는 것보다 훨씬 가볍고,
		// 메시지에서 자산 한 줄이 빠질 뿐이다.
		log.Printf("[notify] 자산 이름 해석 실패 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 상세 페이지 라우트는 web/src/app/(main)/function/findings/detail/page.tsx 를 본다.
		// 그 페이지는 query 파라미터 id에서 발견(finding) id를 읽는다.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens는 채널 토큰 버킷에서 **최대 want개**의 토큰을 가져가고, 실제로 가져온 개수를 반환한다.
//
// 토큰 하나 = 메시지 하나(HTTP 요청 한 번)이다. 실시간 모드에서는 호출자가 필요한 개수만큼 넘기고,
// 요약 모드에서는 발견(finding) 한 배치 전체를 메시지 하나로만 보내므로 1을 넘긴다.
//
// 버킷 용량은 그 채널의 분당 상한이며, 일정한 속도로 보충한다. ratePerMin<=0이면 속도 제한이 없고,
// 유한하지만 충분히 큰 값을 반환해, 한 라운드 루프가 무한 적체에 끌려가지 않게 한다.
//
// want라는 상한은 필수다. 이것이 없으면 버킷을 통째로 비울 수밖에 없는데, 호출자 쪽에도 라운드당 상한이 있어
// 더 가져온 토큰은 쓰이지도 않고 다음 보충 전에 허공으로 사라진다. 모아 둔 버스트 용량은 영원히 닿을 수 없고,
// 「이 라운드에 보낼 전달이 하나도 없는」 경우에도 한 번은 그대로 차감된다.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 실제로 흐른 시간만큼 보충하며, 속도는 초당 ratePerMin/60이다.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 아주 작은 epsilon을 더한 뒤 정수로 자른다. 토큰 수는 부동소수점으로 누적되므로, 두 번에 나눠 채울 때
	// 0.5 + 0.5가 0.9999999999가 될 수 있고, 그대로 int()하면 0으로 잘린다.
	// 수학적으로는 가득 찬 버킷인데 토큰을 꺼내지 못한다. 1e-9는 토큰 하나보다 훨씬 작아서, 진짜 부족분은 그냥 넘기지 않는다.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled는 전체 스위치를 읽는다.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL은 역링크에 쓸 외부 주소를 반환하며, 끝의 슬래시를 제거한다.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval은 요약 주기를 반환하며, 값이 잘못되었거나 설정되지 않았으면 기본값으로 돌아간다.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot은 전달에 해당하는 이벤트의 스냅샷을 파싱한다.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("전달 %d의 이벤트 스냅샷이 비어 있습니다", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("전달 %d의 이벤트 스냅샷 해석 실패: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// 이벤트 타입은 이벤트 행을 기준으로 하며, 스냅샷 안의 값은 옛 버전이 썼을 수 있다.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
