package server

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestChatUnavailableReasonDistinguishesStates는 운영자에게 보이는 문장이
// 실제 설정 상태와 맞는지 고정합니다. 아무것도 없음과, 있지만 아직
// 켜지 않음은 다르게 읽혀야 사용자가 다음에 할 일을 압니다.
func TestChatUnavailableReasonDistinguishesStates(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)

	// 설정 표를 비우고 시작합니다. 공유 DB의 다른 테스트가 행을 남겼을 수 있습니다.
	existing, _ := m.pg.ListProfiles()
	for _, p := range existing {
		if p.IsDefault {
			// 활성 설정은 바로 지울 수 없습니다. 먼저 그 플래그를 끕니다.
			_, _ = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, p.ID)
		}
		_ = m.pg.DeleteProfile(p.ID)
	}

	if reason := s.chatUnavailableReason(); !strings.Contains(reason, "아직 없습니다") {
		t.Fatalf("no-profile reason=%q, want 아직 없습니다", reason)
	}

	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "p1", Format: "anthropic", Model: "claude-x", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		_ = m.pg.DeleteProfile(id)
	})

	// 설정은 있지만 켜져 있지 않습니다.
	if reason := s.chatUnavailableReason(); !strings.Contains(reason, "활성화된 LLM 설정이 없습니다") {
		t.Fatalf("inactive reason=%q, want 활성화된 LLM 설정이 없습니다", reason)
	}

	// 켜면, 이유가 더 이상 설정 없음/비활성을 말하지 않습니다.
	if err := m.pg.SetActiveProfile(id); err != nil {
		t.Fatal(err)
	}
	if reason := s.chatUnavailableReason(); strings.Contains(reason, "아직 없습니다") || strings.Contains(reason, "활성화된 LLM 설정이 없습니다") {
		t.Fatalf("active reason=%q should not report missing/inactive", reason)
	}
}

// TestResolveChatAgentHonoursConversationProfile은 보고된 버그의 회귀 가드입니다.
// 대화가 올바른 설정을 골랐으면, 전역 설정이 하나도 활성이지 않아도
// 채팅 에이전트가 풀려야 합니다. 그래서 보내기 전 검사가
// "LLM 이 설정되지 않음"으로 거절한다.
func TestResolveChatAgentHonoursConversationProfile(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)

	// 전역 폴백을 nil로 고정합니다. 그러면 nil이 아닌 결과는 오직
	// 그 대화 자신의 설정에서만 나올 수 있습니다. 사용자가 실제로 만난 상황이고
	// (쓸 수 있는 전역 설정이 없음), 테스트 기계의 환경과 고친 내용을 분리합니다.
	s.cfgMu.Lock()
	s.chatAgent = nil
	s.cfgMu.Unlock()

	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "conv-pick", Format: "anthropic", Model: "claude-x", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(id) })

	// 이 설정을 고정한 대화는 전역 폴백이 nil이어도 채팅 에이전트가 풀립니다.
	// 고치기 전에는 보내기 전 검사가 바로 거절했습니다.
	conv := &db.Conversation{AgentKey: "mainagent", LLMProfileID: &id}
	if s.resolveChatAgent(conv) == nil {
		t.Fatalf("resolveChatAgent(conversation with valid profile) = nil; global fallback wrongly gates the pick")
	}

	// 고른 설정도 없고 전역 폴백도 없으면, 정말로 돌릴 것이 없습니다.
	if s.resolveChatAgent(&db.Conversation{AgentKey: "mainagent"}) != nil {
		t.Fatalf("resolveChatAgent with neither pick nor global config should be nil")
	}
}
