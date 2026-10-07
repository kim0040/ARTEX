// Package notify 는 발견을 IM·메일 알림 채널로 보내는 어댑터입니다.
//
// 탐색 그래프에 발견이 쌓이면 사람은 UI를 열지 않아도 알려야 합니다. 이 패키지는
// 채널별 렌더와 전송만 하고, 알림 채널 설정과 전달 이력은 db가 PostgreSQL에 둡니다.
//
// 계층: 잎 패키지라 표준 라이브러리만 의존합니다. 데이터베이스와 server를 모릅니다.
// 채널 설정은 map[string]any(notification_channels.config JSONB)로, 보낼 내용은
// Message로 받습니다. 서명, UTF-8 자르기, 필터 매칭처럼 틀리기 쉬운 부분을
// PostgreSQL 없이 단위 테스트하고, 호출 순서는 server가 맡습니다.
//
// 동시성: Channel 구현은 상태가 없어야 합니다. 같은 인스턴스를 여러 채널 설정
// (같은 채널의 로봇 여러 개 포함)이 동시에 재사용하므로, 자격 증명은 항상 cfg로만 받습니다.
package notify

// 채널 종류. 값은 notification_channels.kind의 허용 집합이기도 하며, server가
// 화이트리스트로 검사합니다(findings.status와 같이 DB CHECK는 쓰지 않아 채널 추가가 쉽습니다).
const (
	KindDingTalk = "dingtalk" // 딩톡 커스텀 로봇
	KindFeishu   = "feishu"   // 페이슈(Lark 포함) 커스텀 로봇
	KindWeCom    = "wecom"    // 기업 위챗 그룹 로봇
	KindWebhook  = "webhook"  // 범용 Webhook: 메서드/헤더/JSON 템플릿을 직접 지정
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP 메일
)

// 이벤트 종류. notification_events.kind와 대응합니다.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind 는 config의 kind가 비었을 때 쓰는 기본값입니다.
const InitKind = KindDingTalk

// severityRank 는 발견 심각도를 비교 가능한 순서로 바꿉니다. 모르는 심각도는 0이라
// min_severity가 있으면 항상 걸러집니다. 애매하면 보내지 않아 오탐이 채널을 도배하지 않게 합니다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank 는 심각도 순서를 반환합니다. 모르면 0입니다.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel 은 이모지가 붙은 한국어 심각도 이름입니다. 메시지 제목과 카드 색에 씁니다.
// 모르는 값은 그대로 돌려주고, 없는 이름을 지어내지 않습니다.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 심각"
	case "high":
		return "🟠 높음"
	case "medium":
		return "🟡 중간"
	case "low":
		return "🔵 낮음"
	default:
		return severity
	}
}

// StatusLabel 은 처리 상태를 한국어로 바꿉니다. 상태 변경 메시지에 씁니다.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "대기"
	case "in_progress":
		return "처리 중"
	case "confirmed":
		return "확인됨"
	case "resolved":
		return "처리됨"
	case "fixed":
		return "수정됨"
	case "false_positive":
		return "오탐"
	case "ignored":
		return "무시"
	case "duplicate":
		return "중복"
	case "risk_accepted":
		return "위험 수용"
	default:
		return status
	}
}

// AtLeast 는 severity가 min 이상인지 봅니다. min이 비면 문턱이 없어 항상 통과합니다.
// 모르는 severity의 순서는 0이라, min이 비어 있지 않으면 거부됩니다(severityRank 주석).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
