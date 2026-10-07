package notify

// Snapshot 은 notification_events.snapshot JSONB 열의 계약입니다. 쓰는 쪽은 db의
// 발견 저장 트랜잭션이고, 읽는 쪽은 server의 전달 엔진과 필터입니다. 이 패키지에
// 둔 이유는 알림 영역의 적재물이기 때문입니다. db는 직렬화만 하고 필드 의미는 모릅니다.
//
// 발견 필드를 중복 저장하고 렌더 때 다시 조회하지 않는 이유: 발견은 나중에 이름,
// 심각도, 상태가 바뀝니다. 알림은 그때의 결론을 보여 줘야 합니다. 다시 조회하면
// 「나중에 low로 바뀐」 위험한 인상이 됩니다. fan-out과 렌더도 findings/tasks/assets를 JOIN하지 않습니다.
type Snapshot struct {
	// 이벤트 종류: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed일 때만 비어 있지 않습니다.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item 은 채널이 그릴 발견 한 건입니다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets 는 풀어 쓴 자산 표시 이름(도메인/IP 등)입니다. server가 채웁니다.
	// 이 패키지는 데이터베이스를 건드리지 않아 이름을 모릅니다.
	Assets []string
	// DetailURL 은 발견 상세로 돌아가는 링크입니다. 비어 있으면 public_base_url이
	// 없어 렌더에서 생략합니다.
	DetailURL string
	// 상태 변경 이벤트 전용. 둘 다 비어 있지 않으면 「대기 → 수정됨」처럼 그립니다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange 는 이 항목이 상태 변경 이벤트인지 보고합니다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title 은 표시 제목입니다. 사람이 붙인 name을 우선하고, 없으면 유형 vulnclass,
// 둘 다 비면 자리 표시를 씁니다. 빈 제목은 내지 않습니다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 발견)"
}

// Message 는 채널로 한 번 보내는 전체 내용입니다.
type Message struct {
	// 단건이면 길이 1, 요약(digest)이면 한 묶음입니다.
	// 빈 슬라이스는 잘못된 입력이며, 호출자는 최소 한 건을 보장해야 합니다.
	Items []Item
	// Batch=true이면 요약 메시지로 그립니다(제목을 바꾸고 시간 창과 건수를 붙입니다).
	Batch bool
	// WindowMinutes 는 요약 주기(분)입니다. Batch=true일 때 「최근 N분」 문구에 씁니다.
	// 렌더 시점에 time.Since로 계산하지 않고 설정에서 넘깁니다. 렌더가 결정적이어야 테스트가 됩니다.
	WindowMinutes int
	// HomeURL 은 플랫폼 화면 주소(전역 public_base_url)입니다. 비면 입장 링크를 뺍니다.
	HomeURL string
}
