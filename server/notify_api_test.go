package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 푸시 기능의 처음부터 끝까지 동작을 다룹니다: 발견(finding) 저장 → 이벤트 → 분배 → 실제 HTTP 전송.
// 발견(finding)은 엔진이 탐색 그래프에 남긴 기록이고, 푸시는 그 기록을 바깥 채널로 알리는 단계입니다.
//
// 보안상의 주의점: 이 테스트는 **전역 Notifier.step()을 호출하지 않고**, 자신이 만든
// 채널에만 stepRealtime/stepDigest를 호출합니다. step()은 저장소에 있는 모든 활성 채널을 돌기 때문입니다.
// 실제 딩딩/기업 위챗 봇이 이미 설정된 개발 데이터베이스에서 테스트를 돌리면, 전역 step은 테스트 동안
// 생긴 발견(finding)을 그 단체방에 실제로 보냅니다. 채널별로 호출하면 영향 범위가 테스트가 만든 가짜 수신단에만 엄격히 남습니다.
//
// 정리: 테스트가 끝나면 이 테스트가 만든 이벤트(전달 행은 연쇄 삭제)와 채널을 지워, 실제 채널에 적체가 남지 않게 합니다.
//
// 단언 기준: stepRealtime/stepDigest는 반환값이 없고 안에서 로그만 남기므로, 여기서 단언하는 것은
// **관찰 가능한 외부 동작**(가짜 수신단이 무엇을 받았는지, 전달 행이 어떤 상태가 되었는지)이지, 함수의
// 반환값이 아닙니다. 반환값을 스텁으로 맞추는 것보다 실제 호출 경로에 가깝습니다.

// notifyFixture는 이 파일 테스트의 공통 장치입니다.
// 여기서 만든 발견(finding)은 탐색 그래프의 작업에 붙고, 자산 그래프의 데이터와는 섞지 않습니다.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 직접 만든 task/exploration: 테스트는 발견(finding)을 여기에 기록해 다른 테스트 데이터와 격리합니다.
	taskID int64
	expID  int64
	// cleanupMark 이후에 생긴 이벤트는 정리할 때 함께 삭제합니다.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 이 파일의 모든 가짜 수신단은 127.0.0.1에서 돌고, 전달은 기본적으로 루프백 주소를 거부합니다.
	// (SSRF로 같은 머신의 서비스와 클라우드 메타데이터를 치는 것을 막기 위해). 테스트는 이 스위치를 명시적으로 켭니다.
	// 가드(guard)의 「기본 거부」 동작은 notify 패키지의 ssrf_test.go가 다룹니다.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// task를 직접 만듭니다. 공유 장치 trafficEvidenceServer가 만든 task에서는 exploration id를 얻을 수 없고,
	// 발견(finding)을 기록하려면 그것이 반드시 필요합니다.
	task, err := s.m.CreateTask("알림 푸시 테스트", "푸시 동작 검증", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 장치를 스스로 닫힌 고리로 만듭니다. fixture를 만들기 전에 이미 있던 이벤트를 한 번에 분배됨으로 표시합니다.
	//
	// 해야 하는 이유: FanOutPendingEvents는 **전역**이라, 저장소의 모든 미분배 이벤트를
	// 일치하는 모든 채널로 펼칩니다. 공유 장치 trafficEvidenceServer 자체가 발견(finding) 하나를 기록하고
	// (바로 그것이 반환하는 초기 finding입니다), 다른 테스트에도 잔여가 있을 수 있습니다. 격리하지 않으면
	// 이런 무관한 이벤트가 이 테스트의 채널로 분배되어, 「전달이 N건이어야 한다」 같은 단언이
	// 맞았다 틀렸다 합니다. 게다가 틀리는 방식이 테스트 실행 순서에 달려 있어, 바로 실패하는 것보다 추적하기 어렵습니다.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("알림 이벤트 정리 실패: %v", err)
		}
	})
	// 전체 스위치는 켜져 있어야 합니다(다른 테스트가 꺼 두었을 수 있습니다).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record는 실제 증거 기록 경로로 발견(finding) 하나를 저장하고 finding id를 반환합니다.
// 이 경로는 **같은 트랜잭션** 안에서 푸시 이벤트를 등록합니다. 바로 이 기능이 걸리는 지점입니다.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 의 요약",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("발견(finding) 기록 실패: %v", err)
	}
	return out.FindingID
}

// channel은 채널 설정을 다시 읽습니다(채널별 stepX 호출에 씁니다).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("채널 읽기 실패: %v", err)
	}
	return ch
}

// deliver는 이벤트를 분배하고 지정한 채널에만 전달 한 라운드를 돌립니다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("배정 실패: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel은 HTTP 인터페이스로 채널을 만들고, 그 과정에서 인터페이스 자체의 검증 경로도 덮습니다.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("채널 만들기 실패 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("채널 만들기 응답 이상: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook은 받은 요청 본문을 기록하는 가짜 수신단입니다.
// 엔진이 채널로 보낸 본문만 여기서 확인하며, 자산 그래프나 탐색 그래프에는 쓰지 않습니다.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("가짜 수신단은 요청을 %d건만 받아, %d번째를 가져올 수 없다", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("가짜 수신단이 요청을 하나도 받지 못했다")
	}
	return f.body(t, f.count()-1)
}

// markdownText는 요청 본문에서 본문을 꺼냅니다. 제품마다 다른 필드 이름을 맞춰 줍니다.
// 딩딩 markdown은 `text`, ActionCard는 `text`, 기업 위챗 markdown은 `content`를 씁니다.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("요청 본문에 알아볼 수 있는 본문이 없다: %v", body)
	return ""
}

// agePendingBatch는 그 채널의 대기 중인 전달을 오래되게 만들어, 요약 배치가 만료되는 경우를 테스트합니다.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "실시간 푸시",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL 인젝션", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("메시지 1건이 나가야 하는데, 실제 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL 인젝션", "높음", "요약"} {
		if !strings.Contains(text, want) {
			t.Fatalf("메시지 본문에 %q가 없다:\n%s", want, text)
		}
	}
	// 전달은 sent로 바뀌어야 합니다.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("전달 후에도 sent로 표시되지 않은 항목이 %d건 있다", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "마스크 케이스",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("채널 목록 실패 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("API 응답이 자격 증명을 노출했다: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("새로 만든 채널이 목록에 없다")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("자격 증명 필드는 마스크 값이어야 한다: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API는 프론트에 어느 필드가 자격 증명인지 알려야 한다")
	}

	// PATCH는 이름만 바꾸고 마스킹된 자격 증명을 되돌려 보냅니다. 진짜 자격 증명은 그대로 남아야 합니다.
	body, _ := json.Marshal(map[string]any{
		"name":   "이름 변경 후",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("업데이트 실패 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("마스크를 다시 보내 진짜 자격 증명을 덮었다: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("마스크를 다시 보내 secret을 덮었다: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "이름 변경 후" {
		t.Fatal("이름이 갱신되지 않았다")
	}

	// secret을 명시적으로 비우면 적용되어야 합니다(「마스킹된 값을 되돌려 보냄 = 그대로 유지」와 다릅니다).
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret 비우기 실패 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("빈 문자열은 secret을 비워야 한다")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"타입이 올바르지 않다", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "채널 유형이 올바르지 않습니다"},
		{"이름 없음", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름이 없습니다"},
		{"webhook 없음", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook 프로토콜이 잘못됨", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook 주소가 올바르지 않습니다"},
		{"모드가 잘못됨", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "푸시 방식이 올바르지 않습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400을 반환해야 하는데 %d를 받음: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("오류 메시지에 %q가 있어야 하는데 %s를 받음", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("없는 채널을 삭제하면 404여야 하는데 %d를 받음", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "심각만",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "저위험 문제", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("임계값 미만의 발견(finding)은 전달을 만들면 안 되는데 %d건을 받음", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("필터된 발견(finding)은 메시지를 보내면 안 됨")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "요약 푸시",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("요약 발견(finding)%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 아직 만료되지 않음: 보내지 않습니다.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("요약 배치가 만기 전에 발송됨")
	}

	// 배치를 오래되게 한 뒤: 세 건이 메시지 하나로 합쳐집니다.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("세 건은 메시지 하나로 합쳐져야 하는데 실제로는 %d건이 발송됨", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "최근") || !strings.Contains(text, "발견 3건") {
		t.Fatalf("요약 메시지에 건수/시간 창 문구가 없음:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("요약 발견(finding)%d", i)) {
			t.Fatalf("요약 메시지에 %d번째 항목이 없음:\n%s", i, text)
		}
	}
	// 같은 배치는 batch_id를 공유해야 합니다.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("세 건의 전달은 하나의 batch_id를 공유해야 하는데 distinct=%d total=%d를 받음", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "비활성 채널",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "비활성 기간의 발견(finding)", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("비활성 채널은 전달을 만들면 안 되는데 %d건을 받음", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "상태 변경 케이스", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 두 건이어야 합니다. fixed 쪽은 상태 변경이고, finding_created 쪽도 같은 라운드에 나갈 수 있습니다.
	// 상태 변경 쪽이 실제로 더 늦게 만들어지지만, 순서에 의존하지 않고 전부 찾습니다.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("「상태 변경 → 수정됨」이 포함된 메시지를 받지 못함 (총 %d건)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경을 구독하지 않음",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "변경을 구독하지 않음", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("상태 변경을 구독하지 않은 채널은 상태 변경 전달을 받으면 안 되는데 %d건을 받음", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "테스트 전송",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("테스트 전송 실패 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("가짜 수신단은 테스트 메시지 1건을 받아야 하는데 %d를 받음", hook.count())
	}
	// 테스트 메시지는 한눈에 테스트임을 알 수 있어야 하며, 실제 발견(finding)으로 오해되면 안 됩니다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("테스트 메시지는 테스트임을 표시해야 함: %s", text)
	}
	// 설정이 깨졌을 때는 채널의 원래 오류를 사용자에게 그대로 돌려줘야 합니다.
	badID := f.createChannel(t, map[string]any{
		"name":   "잘못된 주소",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("전달 실패는 502를 반환해야 하는데 %d를 받음: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소를 가리켜 failed 전달을 만듭니다.
	chID := f.createChannel(t, map[string]any{
		"name":   "실패 재시도",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "실패할 푸시", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 예산을 다 쓸 때까지 이어서 전달합니다.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("재시도를 모두 쓴 뒤에는 failed여야 하는데 %s를 받음", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("이력 조회 실패 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("실패한 전달 1건을 조회해야 하는데 total=%d len=%d를 받음", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("이력에 실패 이유가 있어야 함. 없으면 사용자가 원인을 찾을 수 없음")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("시도 횟수가 기록되어야 하는데 %d를 받음", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "실패할 푸시" {
		t.Fatalf("이력에 발견(finding) 제목이 있어야 하는데 %q를 받음", hist.Deliveries[0].Title)
	}

	// 수동 재전송: pending으로 돌아가고 횟수는 0이 되어야 합니다.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("재전송 실패 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("재전송 뒤에는 pending이고 attempts=0이어야 하는데 %s/%d를 받음", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 실패: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta는 채널 %d개를 모두 나열해야 하는데 %d를 받음", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("채널 %s이(가) 자격 증명 필드를 보고하지 않음", k.Kind)
		}
	}

	// 전역 설정 세 항목을 왕복합니다. 끝의 슬래시는 정규화로 제거되어야 합니다. 그렇지 않으면 되돌아가는 링크가 "//function/..."로 이어집니다.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("설정 저장 실패 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("복귀 링크 주소가 정규화되지 않음: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("요약 주기가 적용되지 않음: %v", payload["notify_digest_interval_min"])
	}

	// 잘못된 값은 거부되어야 합니다.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s은(는) 400을 반환해야 하는데 %d를 받음", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL은 되돌아가는 링크 조립을 다룹니다. public_base_url이 설정되어 있으면
// 단일 메시지는 버튼이 있는 ActionCard여야 하고, 링크는 발견(finding) 상세 페이지를 가리켜야 합니다.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "복귀 링크",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "복귀 링크가 있는 발견(finding)", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("복귀 링크가 있으면 ActionCard를 써야 하는데 msgtype=%v를 받음", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("복귀 링크가 다름\n기대 %s\n받음 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL은 반대 경우를 다룹니다. 외부 주소가 없으면 잘못된 링크가 생기면 안 되고
// (localhost나 상대 경로를 가리키는 경우 등) 순수 markdown으로 돌아가야 합니다.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "복귀 링크 없음",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "복귀 링크가 없는 발견(finding)", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("외부 주소를 설정하지 않았으면 markdown을 보내야 하는데 %v를 받음", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "자세히 보기") {
		t.Fatalf("외부 주소를 설정하지 않았으면 상세 링크가 나오면 안 됨:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder는 「조용히 유실」 수정의 처음부터 끝까지 증거입니다.
//
// 요약 메시지는 채널 길이 상한(기업 위챗 4096바이트)을 받습니다. 한 배치에 다 들어가지 않으면 **항목 전체 단위로** 나눠야 합니다.
// 이 메시지에 들어간 항목은 전달됨으로 표시하고, 나머지는 큐로 돌아가 다음 메시지를 기다립니다. 예전 구현은 배치 전체를
// 성공으로 표시했습니다. 잘린 항목은 메시지에도 없고 실패 목록에도 없으며, 전달 이력은 성공으로 남아
// 발견(finding)이 그대로 사라졌습니다.
//
// 네 가지를 단언합니다. ① 실제로 들어간 건수만 표시했다 ② 나머지는 여전히 대기이다 ③ 미뤄진 항목
// **재시도 횟수를 소모하지 않았다** ④ 한 라운드를 더 돌리면 나머지를 보낼 수 있다(고착되지 않는다).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// 기업 위챗을 쓴다: markdown 상한은 4096바이트이며, 여섯 채널 가운데 가장 빡빡하다.
	chID := f.createChannel(t, map[string]any{
		"name":   "분할 요약",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 제목을 조금 길게 잡아, 60건이 4096바이트를 훨씬 넘어 반드시 나뉘게 한다.
	longName := strings.Repeat("너무 긴 발견(finding) 이름", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("메시지 한 건만 보내야 하는데 %d를 받음", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("항목 중 일부가 전달됨으로 표시되어야 함")
	}
	if pending == 0 {
		t.Fatalf("한 묶음 %d건은 4096바이트에 모두 들어갈 수 없으므로 남은 대기 발송이 있어야 함; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("항목 수가 맞지 않음: sent=%d pending=%d total=%d (전달되지도 않고 대기 발송도 아님=유실)", sent, pending, total)
	}
	// 메시지 본문은 이 건에 들어가지 않은 항목이 몇 건인지 사실대로 알려야 한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("메시지에 이 건에 포함되지 않은 항목이 더 있다고 밝혀야 함:\n%.400s", text)
	}

	// 미뤄진 항목은 재시도 예산을 소모하면 안 된다: 수령할 때 attempts가 이미 낙관적으로 +1되었으므로, 미룰 때 다시 빼야 한다.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("미뤄진 항목은 재시도 횟수를 소모하면 안 됨(그렇지 않으면 몇 건 뒤에 실패로 판정됨), attempts=%d를 받음", maxAttempts)
	}

	// 수렴할 때까지 반복해서 돌린다. 단언하는 것은 **최종적으로 전부 전달**되고, 도중에 실제로 여러 라운드로 나뉘었다는 점이다—
	// 이것은 「두 번째 라운드에서 다 보낸다」보다 더 강하다: 나누어 보내기가 고착되지 않고, 남은 항목을 버리지 않는다는 것을 증명한다.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("분할 전달이 수렴하지 않음: %d라운드를 돌았는데도 %d건이 미결", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d라운드에서 진전이 없어 남은 %d건이 영구히 멈춤", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096바이트 메시지 하나에는 긴 제목의 발견(finding) %d건이 들어가지 않으므로 여러 라운드로 나눠 보내야 하는데 실제로는 %d라운드만 사용됨", total, rounds)
	}
	// 첫 라운드 이후의 각 라운드는 **순수 이어 보내기**여야 하며, 채널이 거부한 항목은 없어야 한다.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("가짜 수신단은 항상 성공을 반환하므로 실패 항목이 있으면 안 되는데 %d를 받음", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget는 드리프트 방지 단언이다.
//
// 재시도 예산(db.MaxNotifyAttempts)과 백오프 수열 표(notifyBackoff)는 두 패키지에 나뉘어 있다:
// 전자는 상태 기계의 정책이고, 후자는 엔진의 실행 박자이다. 하나만 고치면—예를 들어 예산을 5
// 회로 올리면서 백오프 단계를 추가하는 것을 잊으면—코드는 오류를 내지 않고, 4번째와 5번째 재시도가 마지막 단계의 간격을 그대로 쓰게 할 뿐이며,
// 「재시도 리듬이 이유 없이 느려진다」로 나타나서, 조사할 때 여기가 원인임을 떠올리기 어렵다.
// 둘의 길이가 같음을 단언해서, 이런 드리프트가 CI에서 드러나게 한다.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("백오프 단계 수(%d)와 최대 시도 횟수(%d)가 일치하지 않음. 하나를 바꾸면 다른 하나도 함께 바꿔야 함",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 백오프 간격은 단조 비감소여야 한다. 그렇지 않으면 재시도가 갈수록 급해져 제한을 더 악화시킨다.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("백오프 간격은 단조 비감소여야 함: %d단계 %v < %d단계 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget는 「먼저 토큰을 취한 뒤 수령」하는 순서를 고정한다.
// 반대로 하면(먼저 수령한 뒤 버리면), 제한에 막힌 전달이 이미 attempts를 한 번 센 상태가 되고,
// 예산이 순수한 대기로 소진되어 결국 failed에 떨어진다.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷 자체만 시험한다. Server는 필요 없다(그것을 위해 하나 만들면 안 된다).
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1건: 버킷이 가득 찼을 때 최대 1건이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("버킷이 가득 찰 때 분당 1건은 토큰 1개를 가져와야 하는데 %d를 받음", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("토큰이 소진되면 즉시 0을 반환해야 하는데 %d를 받음", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("절반 구간에서는 토큰 하나를 채우면 안 되는데 %d를 받음", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("한 주기가 차면 토큰 1개를 보충해야 하는데 %d를 받음", got)
	}
	// 제한이 없는 채널은 유한한 상한을 타서, 한 라운드가 무한 적체에 끌려가지 않게 한다.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("속도 제한이 없으면 라운드당 상한 %d를 반환해야 하는데 %d를 받음", notifyUnlimitedBurstPerTick, got)
	}
	// 채널 사이의 토큰 버킷은 서로 독립이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("채널 1의 버킷은 여전히 비어 있어야 하며, 결과는 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens는 「want개만 취한다」는 의미를 고정한다.
//
// 예전 구현은 버킷을 통째로 비운 뒤에야 호출 측이 잘라 냈다. 그래서 rate=100/min 채널이 버킷을 가득 모으고,
// 한 라운드에 5건만 쓰면 남은 95개 토큰을 바로 버린다. 그 라운드에 보낼 전달이 없는 채널도 똑같이 차감한다.
// 그 결과 주석이 말하는 「적체 때 rate_per_min건을 한 번에 밀어낼 수 있다」는 어떤 경우에도 되지 않는다.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 버킷은 처음에 가득 차 있다(100). 이번 라운드는 5개만 필요하다.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5일 때 정확히 토큰 5개를 가져와야 하며, 결과는 %d", got)
	}
	// 핵심 단언: 남은 95개는 버킷에 그대로 있어야 하며, 비워서 버리면 안 된다.
	// 시간을 진행하지 않아, 취한 것이 보충이 아니라 기존 재고에서만 올 수 있게 한다.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("남은 토큰은 여전히 가져올 수 있어야 하며(기대값 95), 결과는 %d——버킷이 한 라운드 통째로 비워졌다", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("버킷을 모두 소진했으면 0을 반환해야 하며, 결과는 %d", got)
	}
	// want<=0이면 어떤 토큰도 차감하면 안 된다(빈 라운드는 과금하지 않는다).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0은 0을 반환해야 하며, 결과는 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0인 호출은 토큰을 소비하면 안 되며, 여전히 20개를 가득 가져올 수 있어야 하고, 결과는 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget는 요약 모드의 두 척도를 고정한다.
//
// 요약 배치 크기가 라운드당 요청 예산에 묶이면, rate_per_min=20 채널은 각
// 3초 tick마다 토큰 1개만 보충되어, 요약 메시지마다 발견(finding) 1개만 담는다—기능상 요약이
// 없는 것과 같고, 메시지 머리에는 「최근 30분간 발견(finding) 1건 추가」라고 적힌다. 이 퇴화는 오류를 내지 않고,
// 기존 엔드투엔드 용례도 알아채지 못한다(그것들은 stepDigest에 충분히 큰 limit를 수동으로 넘기고,
// step 안의 한도 계산을 우회한다). 그래서 여기서 결정 자체를 직접 단언한다.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 한 배치 = 메시지 하나 = 요청 한 번 = 토큰 하나. 토큰의 단위는 메시지이며, 발견(finding)이 아니다.
	if tokens != 1 {
		t.Fatalf("한 묶음을 요약할 때는 메시지 하나만 보내므로, 토큰을 정확히 1개 소비해야 하며, 결과는 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("요약 배치 크기는 메모리 상한 db.MaxDigestBatchSize=%d이어야 하며, 결과는 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 핵심 관계: 배치 크기는 라운드당 요청 예산보다 훨씬 커야 한다. 둘이 같은 규모가 되면,
	// 「메시지를 몇 건 보낼지」와 「한 배치에 발견(finding)을 몇 건 담을지」를 다시 하나의 수로 섞었다는 뜻이다.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("요약 배치 크기 %d은 라운드당 요청 예산 %d의 제약을 받으면 안 된다——"+
			"요청 예산은 리스에서 거꾸로 계산한 「요청을 몇 번 보낼지」이며, 「한 배치에 발견(finding)을 몇 건 담을지」와는 서로 다른 차원이다",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease는 또 하나의 드리프트 방지 단언이다.
//
// 단일 채널의 라운드당 전달 건수 상한(notifyMaxSendsPerChannelPerTick)은 리스 지속 시간에서 거꾸로 계산한다:
// 한 라운드에서 직렬 전달의 최악 소요 시간은 리스보다 작아야 한다. 그렇지 않으면 뒤의 몇 건을 다 보내기 전에 리스가 만료되고,
// 다중 인스턴스 배포에서는 상대가 그것들을 다시 수령해 중복 전송한다. 이 세 상수는 서로 다른 위치에 있고,
// 어느 하나를 고쳐도 관계는 깨지면서 어떤 오류도 나지 않을 수 있다—그래서 여기서 고정한다.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("단일 채널 한 라운드의 최악 소요 시간 %v은 리스 %v에 도달하거나 넘어서면 안 된다"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"이 세 상수 중 어느 하나를 바꾸면 나머지 둘도 함께 확인해야 한다",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
