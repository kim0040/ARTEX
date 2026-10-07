package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("가린 값이 자격 증명 전체를 흘렸습니다: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("가린 값이 주소 본체를 보여 주면 안 됩니다: %q", got)
	}
	// 끝 6자는 남겨, 사용자가 어느 로봇인지 알아보게 합니다.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("식별 힌트로 끝 6자를 남겨야 합니다: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("가린 값은 IsMasked가 알아봐야 합니다: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 짧은 자격 증명까지 끝 6자를 보여 주면 자격 증명 전체가 나갑니다.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("길이 %d 인 자격 증명은 꼬리 힌트를 주면 안 됩니다. 결과 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("가린 값에 원값이 들어 있습니다: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s 는 가려져야 합니다. 결과 %q", k, s)
		}
	}
	// 자격 증명이 아닌 필드는 그대로 둬야 UI가 보여 줄 수 있습니다.
	if masked["port"] != float64(587) {
		t.Errorf("자격 증명이 아닌 필드 port는 바뀌면 안 됩니다: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// 채널 종류를 모르면, 자격 증명이 있을 수 있는 원문을 돌려주지 않고 UI에는 빈 설정을 보여 줍니다.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("모르는 채널 종류는 빈 설정을 반환해야 합니다. 결과 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// 가리기는 표시 계층의 동작입니다. 저장소의 진짜 값을 거꾸로 바꾸면 안 됩니다.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig가 인자를 수정했습니다. 진짜 자격 증명이 가린 값으로 덮입니다")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// 사용자는 method만 바꿨고, 브라우저는 가린 값과 새 method를 제출합니다.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("가린 필드는 저장소 원값을 유지해야 합니다. 결과 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("수정한 필드는 반영되어야 합니다. 결과 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("빈 문자열은 그 필드를 비워야 합니다. 결과 %v", got)
	}
	// 언급하지 않은 필드는 유지합니다(부분 갱신).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("언급하지 않은 필드는 남아야 합니다. 결과 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("언급하지 않은 필드는 남아야 합니다. 결과 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("언급한 필드는 갱신되어야 합니다. 결과 %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap은 이 패키지에서 가장 중요한 보안 불변 조건입니다.
// **대상 주소를 바꿀 때 옛 자격 증명을 함께 가져가면 안 됩니다.**
//
// 이 케이스들은 방어가 막아야 하는 입력 모양(주소만 바꾸고 자격 증명은 말하지 않음)을 그대로 씁니다.
// 방어가 통과하는 입력만 보면, 방어가 꺼져 있어도 전부 초록이 됩니다.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing은 오류가 지목해야 하는 자격 증명 키입니다.
		wantMissing string
	}{
		{
			name: "일반 Webhook이 주소를 바꾸며 Authorization 헤더를 유지하려 함",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram이 base_url을 바꾸며 Bot Token을 그 끝점으로 보내려 함",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "메일이 SMTP 호스트를 바꾸며 비밀번호를 넘기려 함",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "메일이 TLS를 꺼도 비밀번호를 다시 밝혀야 함",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// 가린 값 = 「옛 자격 증명 유지」. 주소가 바뀐 맥락에서는 이것도 거절해야 합니다.
			name:        "가린 자격 증명을 돌려주며 새 주소를 줌",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "딩톡이 Webhook을 바꾸며 서명 비밀을 유지하려 함",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("주소를 바꾸면서 자격 증명을 다시 밝히지 않았는데 통과했습니다. 설정 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("API가 고칠 방법을 안내할 수 있게 전용 오류 타입이어야 합니다. 결과 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("빠진 자격 증명 키 %q 를 지목해야 합니다. 결과 %v", tc.wantMissing, target.Missing)
			}
			// 오류 문구가 운영자에게 고칠 키를 알려 줘야 합니다.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("오류 문구가 %q 를 언급해야 합니다: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits는 반대 케이스입니다. 정상적인 편집을 오탐으로 막으면
// 이 방어가 「너무 귀찮다」는 이유로 우회되거나 지워집니다.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "이름만 변경(설정은 그대로 회신)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "요청 메서드만 변경, 주소와 자격 증명은 그대로",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "주소를 바꾸면서 새 자격 증명을 함께 줌",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "주소를 바꾸면서 자격 증명이 더 이상 필요 없다고 명시",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram chat_id 변경(목적지가 아님)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "메일 수신자 변경(목적지가 아님)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("합법적인 편집이 막혔습니다: %v", err)
			}
			if merged == nil {
				t.Fatal("합친 결과를 반환해야 합니다")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance는 오탐하기 쉬운 세부입니다.
// 프론트가 제출하는 포트는 JSON number(float64)이고, 저장소에서 읽은 값도 float64인데
// 두 값의 타입이 다를 수 있습니다(int 대 float64). == 로 비교하면 「안 바꿈」이 「바꿈」이 되어,
// 이름만 고친 사용자에게 「비밀번호를 다시 입력하세요」가 뜹니다. 오탐은 이 방어를 믿지 않게 만듭니다.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 같은 포트를 int로 제출합니다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("포트 값이 같고 타입만 다르면 주소 변경이 아닙니다: %v", err)
	}
	// 포트를 정말로 바꾸면 막아야 합니다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("포트 변경은 막혀야 합니다")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination은 「비워 둘 수 있는
// 목적지 필드」 경로를 덮습니다. Telegram의 base_url을 비우면 공식 API 주소를 쓴다는 뜻입니다.
//
// 한때는 두 번째 저장부터 채널을 영구히 저장하지 못했습니다.
//
//	새로 만들 때 저장소에 base_url:"" 이 들어감(생성 경로는 프론트 config를 그대로 저장하고 MergeConfig를 타지 않음)
//	→ 첫 저장에서 MergeConfig가 빈 문자열을 명시적 비우기로 보고 그 키를 delete
//	→ 두 번째 저장에서 incoming은 여전히 "" 인데 stored에는 키가 없어 「주소가 바뀌었다」로 판정
//	→ bot_token은 가린 회신 값 → 400「대상 주소가 바뀌었습니다. 자격 증명 필드도 다시 입력하세요」
//
// 사용자는 아무것도 안 바꿨는데 다시는 저장하지 못하고, Bot Token을 다시 붙여 넣어야 했습니다.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// 프론트 buildConfig()는 그 채널의 필드 정의마다 값을 제출합니다. 자격 증명은 가린 값으로 채우고,
	// 빈 입력칸은 빈 문자열을 냅니다. 여기서는 「바뀐 키만」이 아니라 그 출력을 그대로 재현합니다.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 첫 저장: 채널 이름만 바꿨고 config는 그대로 회신합니다.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("첫 저장이 오탐으로 막혔습니다: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("전제가 깨졌습니다. 빈 문자열은 MergeConfig가 지워야 합니다. 이 케이스는 「키가 사라진 뒤」를 덮습니다")
	}

	// 두 번째 저장: 제출 내용은 이전과 같고, 사용자는 아무것도 안 바꿨습니다.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("두 번째 저장이 오탐으로 막혔습니다(사용자는 아무것도 안 바꿈): %v", err)
	}
	// 세 번째. 「한 번만 틀리는」 것이 아니라 안정적으로 저장되는지 확인합니다.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("세 번째 저장이 오탐으로 막혔습니다: %v", err)
	}
	// 자격 증명은 끝까지 남아야 하고, 빈 문자열 로직이 같이 지우면 안 됩니다.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token은 원값을 유지해야 합니다. 결과 %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges는 위 케이스의 짝입니다.
// 빈 문자열과 「키 없음」을 같게 보더라도, 진짜 주소 변경까지 통과시키면 안 됩니다.
// 두 방향 모두 자격 증명이 밖으로 나가는 실제 경로입니다. Telegram Bot Token은 URL 경로에 있고,
// base_url이 바뀌면 Token이 새 주소로 갑니다.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 방향 하나: 「빈 값」(공식 주소)에서 자체 주소로 바꿈.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("공식 주소에서 자체 주소로 바꾸면 Token을 다시 입력해야 합니다")
	}

	// 방향 둘: 자체 주소를 비우는 것(= 공식 API로 되돌림)도 주소 변경입니다.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("자체 주소를 비우는 것(공식 API로 되돌림)도 주소 변경이라 Token을 다시 입력해야 합니다")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys와 같습니다. 채널이 목적지 키를 선언하지 않으면 PrepareConfigUpdate가 그 채널을 보호하지 못합니다.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("채널 %s 가 목적지 키를 선언하지 않았습니다. 주소를 바꾸며 자격 증명을 가져가는 방어가 이 채널에는 없습니다", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s 가 자격 증명 키를 선언하지 않았습니다", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// 컴파일러가 이미 모든 채널에 SecretKeys를 강제합니다. 여기서는 「가리기를 빈 답으로 내는 채널이 없는지」를
	// 한 번 더 확인합니다. 빈 슬라이스를 반환하는 채널은 자격 증명을 브라우저에 평문으로 보여 줍니다.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("채널 %s 가 테스트의 가리기 기대에 등록되지 않았습니다", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s 가 자격 증명 필드를 하나도 선언하지 않아 설정이 평문으로 보입니다", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer는 감사가 지적한 구멍을 덮습니다.
// 가리기 센티널을 **문자열이 아닌** 구조(webhook.headers 같은 객체) 안에 넣으면,
// MergeConfig는 「문자열이고 접두가 있음」만 가린 값으로 봅니다. 리터럴 "__masked__"가
// 진짜 헤더 값으로 저장되고, 이후 인증이 조용히 실패하며 오류도 없습니다.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// 객체 안에 가리기 센티널을 끼워 넣습니다.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("구조 안에 가리기 센티널이 있으면 거절해야 합니다(아니면 리터럴이 저장소에 들어갑니다)")
	}
	// 객체 전체를 진짜 새 값으로 제출하는 것은 그대로 받아들입니다.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("정상적인 새 요청 헤더 제출은 막히면 안 됩니다: %v", err)
	}
}
