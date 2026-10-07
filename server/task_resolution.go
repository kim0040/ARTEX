package server

import (
	"fmt"
	"net/http"

	"github.com/Autumn-27/artex/db"
)

type taskLLMResolution struct {
	ProfileID *int64 `json:"profile_id,omitempty"`
	Name      string `json:"name"`
	Format    string `json:"format"`
	Model     string `json:"model"`
	Source    string `json:"source"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Server) resolutionFromProfile(p *db.LLMProfile, source string) taskLLMResolution {
	if p == nil {
		return taskLLMResolution{Source: source, Reason: "LLM 설정이 없습니다"}
	}
	id := p.ID
	result := taskLLMResolution{
		ProfileID: &id,
		Name:      p.Name,
		Format:    p.Format,
		Model:     p.Model,
		Source:    source,
	}
	if p.APIKey == "" {
		result.Reason = "LLM 설정에 API Key가 없습니다"
		return result
	}
	if _, _, ok := s.providerForProfile(p.ID); !ok {
		result.Reason = "LLM 설정 형식이나 매개변수가 올바르지 않습니다"
		return result
	}
	result.Available = true
	return result
}

// resolveTaskRoleLLM은 프로바이더를 만들지 않고 taskLLMRuntime.current와 같은 순서로 고릅니다.
// 역할 에이전트 바인딩 → 작업에 명시한 체인 → 활성/전역 환경 설정.
// DB 오류는 '모델을 쓸 수 없음'으로 바꾸지 않고 그대로 돌려줍니다.
// 초보용: 플래너, 워커, 메인 에이전트가 엔진에서 쓸 모델 설정을 고르기만 합니다.
func (s *Server) resolveTaskRoleLLM(t *Task, agentKey string) (taskLLMResolution, error) {
	if s == nil || s.m == nil || s.m.pg == nil {
		return taskLLMResolution{}, fmt.Errorf("데이터베이스를 쓸 수 없습니다")
	}
	a, err := s.m.pg.GetAgentByKey(agentKey)
	if err != nil {
		return taskLLMResolution{}, fmt.Errorf("load agent %q: %w", agentKey, err)
	}
	if a != nil && a.LLMProfileID != nil {
		p, err := s.m.pg.ProfileByID(*a.LLMProfileID)
		if err != nil {
			return taskLLMResolution{}, fmt.Errorf("load agent LLM profile: %w", err)
		}
		if p != nil {
			resolved := s.resolutionFromProfile(p, "agent_binding")
			if resolved.Available {
				return resolved, nil
			}
		}
	}
	state := t.llmStateSnapshot()
	if len(state.ProfileIDs) > 0 {
		if state.ActiveID == nil {
			return taskLLMResolution{Source: "task_chain", Reason: "작업 LLM 설정 연쇄의 한도가 소진되었습니다"}, nil
		}
		p, err := s.m.pg.ProfileByID(*state.ActiveID)
		if err != nil {
			return taskLLMResolution{}, fmt.Errorf("load task LLM profile: %w", err)
		}
		return s.resolutionFromProfile(p, "task_chain"), nil
	}
	p, err := s.m.pg.ActiveProfile()
	if err != nil {
		return taskLLMResolution{}, fmt.Errorf("load global LLM profile: %w", err)
	}
	if p != nil {
		resolved := s.resolutionFromProfile(p, "global_profile")
		if resolved.Available {
			return resolved, nil
		}
	}
	s.cfgMu.Lock()
	on, name, cfg := s.llmOn, s.llmProf, s.llmCfg
	s.cfgMu.Unlock()
	if on {
		if name == "" {
			name = "전역 설정"
		}
		return taskLLMResolution{
			Name: name, Format: cfg.Provider(), Model: cfg.Model,
			Source: "environment", Available: true,
		}, nil
	}
	return taskLLMResolution{Source: "global", Reason: "쓸 수 있는 LLM 설정이 없습니다"}, nil
}

func (s *Server) taskLLMResolutionHandler(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	roles := map[string]taskLLMResolution{}
	for responseKey, agentKey := range map[string]string{
		"mainagent": "mainagent",
		"planner":   "planner",
		"worker":    "worker",
	} {
		resolved, err := s.resolveTaskRoleLLM(t, agentKey)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		roles[responseKey] = resolved
	}
	writeJSON(w, http.StatusOK, roles)
}
