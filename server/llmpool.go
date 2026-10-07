package server

import (
	"log"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/norma/llm"
)

// LLM 장애 조치(페일오버)를 서버에 연결합니다. 설계 메모는 docs/ 아래 LLM 페일오버 문서입니다.
// 초보용: 플래너나 워커가 모델을 부를 때, 지금 설정이 죽으면 다음 설정으로 넘기는 줄입니다.
// 자산 그래프·탐색 그래프의 노드와는 별개이고, 에이전트 호출만 담당합니다.
//   - 전역으로 켠 설정 경로(에이전트 미바인딩, 작업 미고정)만 순회합니다.
//   - 바인딩/고정 경로는 기본적으로 그 설정만 쓰고, 실패하면 그대로 실패합니다(llm_pool_bind_fallback으로 폴백을 켤 수 있음).
//   - 순서 = 활성 설정 → 나머지는 priority DESC. pool_exclude는 빠집니다.
//   - 차단 상태는 프로세스 안에서 공유합니다(s.llmHealth). 풀을 다시 만들어도 비우지 않습니다.

// newLLMHealthRegistry는 프로세스 전체의 차단기 레지스트리를 만듭니다. 상태를
// PG에도 비춰, 식히는 시간이 재시작 뒤에도 남게 합니다. 쓰기는 비동기이고
// 실패해도 흐름은 계속합니다. 기준은 메모리 쪽입니다.
// 초보용: 플래너나 워커가 쓰다 실패한 모델 설정을 잠깐 쉬게 하고, 그 쉼을 재시작 뒤에도 기억합니다.
func newLLMHealthRegistry(pg *db.DB) *llmpool.Registry {
	if pg == nil {
		return llmpool.NewRegistry(nil, nil)
	}
	persist := func(id int64, st llmpool.State) {
		h := db.LLMHealth{ProfileID: id, Fails: st.Fails, Trips: st.Trips, LastError: st.LastError}
		if !st.OpenUntil.IsZero() {
			t := st.OpenUntil
			h.OpenUntil = &t
		}
		go func() {
			if err := pg.SaveLLMHealth(h); err != nil {
				log.Printf("[llmpool] 차단 상태를 DB에 저장하지 못했습니다: %v", err)
			}
		}()
	}
	forget := func(id int64) {
		go func() { _ = pg.ClearLLMHealth(id) }()
	}
	reg := llmpool.NewRegistry(persist, forget)
	// 아직 안 끝난 쉼만 복구합니다(LoadLLMHealth가 거름). 그래서 우리가 꺼져 있는 동안
	// 식힘이 끝난 설정은 건강한 상태로 돌아옵니다.
	if rows, err := pg.LoadLLMHealth(); err == nil {
		for _, h := range rows {
			st := llmpool.State{Fails: h.Fails, Trips: h.Trips, LastError: h.LastError, LastAt: h.LastAt}
			if h.OpenUntil != nil {
				st.OpenUntil = *h.OpenUntil
			}
			reg.Restore(h.ProfileID, st)
			log.Printf("[llmpool] 차단 상태 복구: 설정 #%d 냉각 종료 %s", h.ProfileID, st.OpenUntil.Format(time.RFC3339))
		}
	}
	return reg
}

// poolMember는 설정 하나로 체인 구성원을 만듭니다. 설정마다 프로바이더 캐시를 재사용해
// 같은 설정을 가리키는 에이전트는 프로바이더 하나(그래서 속도 제한도 하나)를 나눕니다.
// 설정을 만들 수 없으면 nil입니다.
func (s *Server) poolMember(p *db.LLMProfile, rank int) *llmpool.Member {
	prov, cfg, ok := s.providerForProfile(p.ID)
	if !ok {
		return nil
	}
	return &llmpool.Member{
		ID: p.ID, Name: p.Name, Model: p.Model, Format: p.Format,
		Priority: p.Priority, Active: p.IsDefault, Rank: rank,
		WindowTokens: cfg.CompactionWindow(), Prov: prov,
	}
}

// poolChain은 DB에서 장애 조치 체인을 읽습니다. headID/headProv/headCfg는
// 체인 맨 앞에 설 구성원입니다. 보통 경로는 전역 활성 설정이고,
// 바인딩 폴백이 켜지면 명시적으로 묶인 설정입니다. 머리는
// 캐시로 다시 풀지 않고 인자로 받습니다. applyLLM이 이미 만든
// 프로바이더를 다시 만들지 않고 재사용하려고요.
//
// 장애 조치가 꺼졌거나 쓸 수 있는 구성원이 둘 미만이면 nil입니다.
// 그러면 호출자는 맨 프로바이더를 씁니다. 바이트까지 예전 동작과 같습니다.
func (s *Server) poolChain(headID int64, headProv llm.Provider, headCfg agent.Config) *llmpool.Pool {
	if s.m == nil || s.m.pg == nil || !s.m.LLMPoolEnabled() {
		return nil
	}
	profs, err := s.m.pg.PoolProfiles()
	if err != nil {
		log.Printf("[llmpool] 페일오버 연쇄를 읽지 못했습니다: %v", err)
		return nil
	}
	var head *db.LLMProfile
	for _, p := range profs {
		if p.ID == headID {
			head = p
			break
		}
	}
	if head == nil { // 머리는 체인에서 빠질 수 있습니다(바인딩 + pool_exclude). 그래도 맨 앞에 섭니다.
		if p, err := s.m.pg.ProfileByID(headID); err == nil && p != nil {
			head = p
		} else {
			return nil
		}
	}
	members := []*llmpool.Member{{
		ID: head.ID, Name: head.Name, Model: head.Model, Format: head.Format,
		Priority: head.Priority, Active: head.IsDefault, Rank: llmpool.RankActive,
		WindowTokens: headCfg.CompactionWindow(), Prov: headProv,
	}}
	for _, p := range profs {
		if p.ID == headID {
			continue
		}
		// 전역 활성 설정은 자기가 머리가 아니면 다른 설정보다 앞입니다
		// (바인딩 폴백 체인). 그래서 첫 폴백으로 먼저 시도됩니다.
		rank := p.Priority
		if p.IsDefault {
			rank = llmpool.RankActive - 1
		}
		if m := s.poolMember(p, rank); m != nil {
			members = append(members, m)
		}
	}
	if len(members) < 2 {
		return nil // 넘어갈 곳이 없습니다.
	}
	return llmpool.New(members, s.llmHealth)
}

// poolForActive는 전역 활성 프로바이더를 장애 조치 체인으로 감쌉니다.
// 장애 조치가 꺼졌거나 폴백이 없으면 prov를 그대로 돌려줍니다.
func (s *Server) poolForActive(activeID int64, prov llm.Provider, cfg agent.Config) llm.Provider {
	pool := s.poolChain(activeID, prov, cfg)
	if pool == nil {
		return prov
	}
	names := make([]string, 0, len(pool.Members()))
	for _, m := range pool.Members() {
		names = append(names, m.Name+"/"+m.Model)
	}
	log.Printf("[llmpool] LLM 페일오버가 켜졌습니다. 연쇄(%d): %v", len(names), names)
	return pool
}

// poolForBinding은 묶인 설정의 프로바이더를 감싸, 체인으로 넘어가게 합니다.
// 장애 조치 전체 스위치와 바인딩 폴백 스위치가 둘 다 켜져야 합니다.
// 아니면 묶인 설정은 독점입니다(실패하면 그대로 실패). 이것이
// 기본이고, 문서에 적힌 우선순위입니다.
func (s *Server) poolForBinding(id int64, prov llm.Provider, cfg agent.Config) llm.Provider {
	if s.m == nil || !s.m.LLMPoolEnabled() || !s.m.LLMPoolBindFallback() {
		return prov
	}
	if pool := s.poolChain(id, prov, cfg); pool != nil {
		return pool
	}
	return prov
}

// LLMPoolMemberStatus는 화면에 보이는 체인 항목 하나입니다.
type LLMPoolMemberStatus struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Format    string `json:"format"`
	Priority  int    `json:"priority"`
	Active    bool   `json:"active"`   // 전역으로 켠 설정인지
	Excluded  bool   `json:"excluded"` // pool_exclude — 장애 조치 대상이 아님
	// 건강: state는 "ok" | "degraded"(실패 중이지만 아직 차단 전) | "tripped"(차단됨)입니다.
	State        string `json:"state"`
	Fails        int    `json:"fails"`
	Trips        int    `json:"trips"`
	CooldownSecs int    `json:"cooldown_secs"` // 남은 식힘 초. 0이면 없음
	LastError    string `json:"last_error,omitempty"`
	LastAt       string `json:"last_at,omitempty"`
}

// llmPoolStatus는 LLM 화면에 전체를 보여 줍니다. 장애 조치가
// 켜졌는지, 풀린 체인 순서, 각 설정의 차단기 상태. 빠진
// 설정도 표시해서, 왜 줄에 없는지 사용자가 보게 합니다.
func (s *Server) llmPoolStatus() map[string]any {
	out := map[string]any{
		"enabled":       false,
		"bind_fallback": false,
		"chain":         []LLMPoolMemberStatus{},
	}
	if s.m == nil || s.m.pg == nil {
		return out
	}
	out["enabled"] = s.m.LLMPoolEnabled()
	out["bind_fallback"] = s.m.LLMPoolBindFallback()

	all, err := s.m.pg.ListProfiles()
	if err != nil {
		return out
	}
	health := s.llmHealth.Snapshot()
	now := time.Now()
	// 체인 순서: 활성 먼저, 그다음 priority 내림차순 / id 오름차순. PoolProfiles와
	// 같은 순서입니다. 여기서 다시 계산해 빠진 것도 그 자리에 보여 줍니다.
	chain := make([]LLMPoolMemberStatus, 0, len(all))
	for _, p := range all {
		st := health[p.ID]
		m := LLMPoolMemberStatus{
			ProfileID: i64s(p.ID), Name: p.Name, Model: p.Model, Format: p.Format,
			Priority: p.Priority, Active: p.IsDefault, Excluded: p.PoolExclude,
			State: "ok", Fails: st.Fails, Trips: st.Trips,
			LastError: st.LastError,
		}
		if st.Open() {
			m.State = "tripped"
			m.CooldownSecs = int(st.OpenUntil.Sub(now).Seconds()) + 1
		} else if st.Fails > 0 {
			m.State = "degraded"
		}
		if !st.LastAt.IsZero() {
			m.LastAt = st.LastAt.Format(time.RFC3339)
		}
		chain = append(chain, m)
	}
	sortPoolStatus(chain)
	out["chain"] = chain
	return out
}

// sortPoolStatus는 상태 행을 실제 체인과 같게 정렬합니다. 활성 먼저,
// 그다음 priority 내림차순, 그다음 id 오름차순. 들어온 조각은 이미 id 순이라
// 앞의 두 키만 안정 삽입 정렬하면 됩니다.
func sortPoolStatus(in []LLMPoolMemberStatus) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && poolLess(in[j], in[j-1]); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func poolLess(a, b LLMPoolMemberStatus) bool {
	if a.Active != b.Active {
		return a.Active
	}
	return a.Priority > b.Priority
}
