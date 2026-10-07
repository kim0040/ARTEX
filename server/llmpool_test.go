package server

import "testing"

// 상태 목록은 UI의 「페일오버 순서」 줄을 그립니다. PoolProfiles가 실제로 도는
// 순서와 같아야 합니다. 활성 설정이 먼저, 그다음 priority DESC, 그다음 id ASC
// (입력은 id 순이라, priority가 같으면 상대 순서를 유지해야 합니다).
func TestSortPoolStatusMatchesChainOrder(t *testing.T) {
	in := []LLMPoolMemberStatus{
		{ProfileID: "1", Name: "low", Priority: 0},
		{ProfileID: "2", Name: "active", Priority: 0, Active: true},
		{ProfileID: "3", Name: "high", Priority: 10},
		{ProfileID: "4", Name: "mid-a", Priority: 5},
		{ProfileID: "5", Name: "mid-b", Priority: 5},
	}
	sortPoolStatus(in)

	want := []string{"active", "high", "mid-a", "mid-b", "low"}
	for i, w := range want {
		if in[i].Name != w {
			got := make([]string, len(in))
			for j, m := range in {
				got[j] = m.Name
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// 활성 설정은 자기 우선순위가 아무리 낮아도 체인 맨 앞에 섭니다.
// 문서의 우선순위이고, 화면이 다르게 보이면 안 됩니다.
func TestActiveProfileHeadsStatusList(t *testing.T) {
	in := []LLMPoolMemberStatus{
		{ProfileID: "1", Name: "loud", Priority: 999},
		{ProfileID: "2", Name: "active", Priority: -5, Active: true},
	}
	sortPoolStatus(in)
	if in[0].Name != "active" {
		t.Fatalf("head = %q, want the active profile regardless of priority", in[0].Name)
	}
}
