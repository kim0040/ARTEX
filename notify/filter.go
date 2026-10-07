package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter 는 notification_channels.filter JSONB 열의 계약입니다. 채널 인스턴스의 필터입니다.
// 모든 필드는 선택이고, 없으면 「필터 없음」입니다. 깨진 설정의 안전망입니다. ParseFilter를 보세요.
type Filter struct {
	// MinSeverity 는 최저 심각도 문턱(low/medium/high/critical)입니다. 빈 값은 문턱 없음.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs 가 빈 배열이면 제한 없음. 비어 있지 않으면 이벤트와 교집합이 있어야 합니다.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude 가 비면 전부 받습니다. 비어 있지 않으면 vulnclass가 키워드 중 하나를 포함해야 합니다.
	// VulnClassExclude 는 키워드 중 하나라도 맞으면 제외합니다(제외가 포함보다 우선).
	// 대소문자 무시 부분 문자열입니다. 정규식보다 안전합니다. 정규식을 잘못 쓰면 채널이 조용히 죽습니다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange 는 이 채널이 발견 상태 변경 이벤트를 받을지입니다. realtime 모드에서만 의미가 있습니다.
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter 는 채널 필터 설정을 해석합니다.
//
// **error를 반환하지 않습니다.** 의도입니다. 필터 설정이 깨지면 영 값 Filter
// (= 필터 없음 = 전부 맞춤)로 물러납니다. 발견 알림에서는 **한 건을 더 보내는 것이
// 심각한 한 건을 조용히 빠뜨리는 것보다 낫습니다.** 해석 실패를 「보내지 않음」으로
// 만들면, 설정은 된 것처럼 보이는데 아무것도 안 보내는 채널이 됩니다. 가장 나쁜 실패입니다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 해석에 실패하면 f는 영 값, 즉 필터 없음입니다.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity 는 s가 올바른 심각도 문턱인지 보고합니다. 빈 문자열은 문턱 없음입니다.
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate 는 필터 설정에서 **값이 제한된** 필드를 검사합니다. 채널을 저장할 때 호출합니다.
//
// 쓸 때 막아야 하는 이유: Match는 모르는 문턱을 `rank >= 0`으로 봐서 항상 참입니다.
// 즉 min_severity를 한 글자 틀리면("hgih") 필터가 **조용히 꺼지고** 「전부 보냄」이 됩니다.
// 이 패키지의 「빠뜨리기보다 더 보내기」와 방향은 같습니다(빠지지는 않음).
// 그러나 사용자는 등급별로 보낸다고 생각하는데 실제로는 발견이 전부 방으로 들어가고,
// 잘못 설정했다는 신호도 없습니다. 이런 「조용한 강등」은 입구에서 막아야 합니다.
//
// Validate는 **쓰기** 경로에만 씁니다. 읽기 경로는 ParseFilter의 관대한 의미를 유지합니다.
// 그래야 이미 저장된 나쁜 값 때문에 채널 전체를 읽지 못하는 일이 없습니다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("최소 심각도 %q이(가) 올바르지 않습니다. 선택: low / medium / high / critical, 비우면 제한 없음", f.MinSeverity)
	}
	return nil
}

// Match 는 이벤트를 이 필터가 있는 채널로 보낼지 판단합니다.
//
// **error를 반환하지 않습니다.** 이유는 ParseFilter와 같습니다. 내부 이상은 「맞음」으로 처리합니다.
// 순서: 이벤트 종류 → 심각도 문턱 → 작업/자산 범위 → 유형 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경은 명시적으로 켠 채널만 받습니다. 기본은 꺼짐입니다. 대부분
	// 「알림」은 「새 발견」이지, 상태 이동을 장부처럼 따라가는 것이 아닙니다.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 제외가 우선입니다. 제외 키워드 중 하나라도 맞으면 탈락이고, 포함 목록에 같이 맞아도 그렇습니다.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 작은 집합은 선형 탐색으로 충분합니다. 양쪽 다 「사람이 고른 수십 개」 규모라
	// map을 만드는 비용이 이득보다 큽니다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold 는 s가 keywords 중 하나를 포함하는지 보고합니다(대소문자 무시).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
