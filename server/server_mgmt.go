package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/skill"
)

// validSkillName은 agentskills.io 이름 규칙을 검사합니다. 스킬 이름을
// 중국어(또는 다른 글자)로도 지을 수 있게 넓혔습니다. ASCII는 소문자
// 영숫자와 하이픈만 되고, ASCII가 아닌 글자와 숫자는 그대로 받습니다.
// 1–64룬이고, 글자로 시작해야 하며, 앞이나 뒤나 연속된 하이픈은 안 됩니다.
// 이름은 skillDir 아래 디렉터리 이름이기도 해서, 경로를 실을 수 있는 것은
// (구분자, 점, 공백, 제어 문자) 통과하지 못합니다.
func validSkillName(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	rs := []rune(name)
	if len(rs) > 64 {
		return false
	}
	isLetter := func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r > unicode.MaxASCII && unicode.IsLetter(r))
	}
	if !isLetter(rs[0]) || rs[len(rs)-1] == '-' {
		return false
	}
	for _, r := range rs {
		switch {
		case isLetter(r), r >= '0' && r <= '9', r == '-':
		case r > unicode.MaxASCII && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return !strings.Contains(name, "--")
}

// reAgentKey는 agents.key DB 검사와 같습니다. 소문자로 시작하고, 그 뒤는
// 소문자, 숫자, 밑줄입니다.
var reAgentKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// pg는 PG 손잡이를 돌려줍니다. 쓸 수 없으면 503을 쓰고 nil을 돌려줍니다.
func (s *Server) pg(w http.ResponseWriter) *db.DB {
	if s.m.pg == nil {
		writeErr(w, 503, "관리 백엔드 데이터 소스(PostgreSQL)가 연결되지 않았습니다")
		return nil
	}
	return s.m.pg
}

func pathInt(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil
}

func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// ---------- 작업(삭제) ----------

const taskDeleteDrainTimeout = 10 * time.Second

func canonicalTaskID(raw string) (string, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

// beginTaskDelete는 삭제 장벽을 일시정지, 재개, 입장,
// FIFO 맞추기와 순서를 맞춥니다. 모든 수명 경로는 concMu를 잡은 뒤 deleting을 다시 봅니다.
// 그래서 이 함수가 돌아온 뒤에는 아무도 반대 상태를 확정할 수 없습니다.
// 초보용: 작업을 지우는 동안 엔진의 재개나 입장이 반대 상태를 확정하지 못하게 막습니다.
func (s *Server) beginTaskDelete(taskID string) bool {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	if s.engine.IsDeleting(taskID) {
		return false
	}
	return s.engine.BeginDelete(taskID)
}

// abortTaskDelete는 실행 장벽을, 삭제를 시작하기 전 메모리가 아니라
// 확정된 작업 행에서 되돌립니다. 행을 못 읽으면 일시정지로 두는 편이
// 안전합니다. 나중의 명시적 재개가 불확실한 삭제에서 실행이 새지 않게
// 하고도 그 작업을 되돌릴 수 있습니다.
// 초보용: 삭제가 실패하면 저장된 작업 행을 기준으로 실행 장벽을 되돌립니다.
func (s *Server) abortTaskDelete(taskID string) {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	keepPaused := true
	if id, err := strconv.ParseInt(taskID, 10, 64); err == nil && s.m != nil && s.m.pg != nil {
		if persisted, getErr := s.m.pg.GetTask(id); getErr == nil && persisted != nil {
			keepPaused = persisted.Paused || persisted.Queued
		} else if task, ok := s.m.Task(taskID); ok {
			state := task.lifecycleSnapshot()
			keepPaused = state.Paused || state.Queued
			if getErr != nil {
				log.Printf("[task-delete] task %s 영속 상태 읽기 실패, 메모리 상태로 장벽을 복구합니다: %v", taskID, getErr)
			}
		} else if getErr == nil {
			// 요청이 없는 작업을 가리켰습니다. 임시 삭제 장벽을 놓은 뒤에
			// 가짜 일시정지 항목을 남기지 않습니다.
			keepPaused = false
		}
	}
	s.engine.AbortDelete(taskID, keepPaused)
}

func (s *Server) pgDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := canonicalTaskID(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "작업 id가 유효하지 않다")
		return
	}
	var opts DeleteTaskOptions
	if err := decode(r, &opts); err != nil && err != io.EOF {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다: "+err.Error())
		return
	}
	if !s.beginTaskDelete(id) {
		writeErr(w, http.StatusConflict, "작업을 삭제하는 중입니다")
		return
	}
	deleted := false
	defer func() {
		if !deleted {
			s.abortTaskDelete(id)
		}
	}()

	// 메인 에이전트 실행은 플래너/워커와 다른 컨텍스트를 씁니다. 그것을 취소한 뒤,
	// 두 실행 영역이 돌아오기를 기다리고 나서 대화 기록을 지웁니다.
	s.cancelTaskChat(id, agent.AbortTaskDeleted)
	drainCtx, cancelDrain := context.WithTimeout(r.Context(), taskDeleteDrainTimeout)
	defer cancelDrain()
	if err := s.waitTaskQuiescent(drainCtx, id); err != nil {
		writeErr(w, http.StatusConflict, "작업에 아직 실행 중인 Agent가 있어 삭제가 취소되었습니다")
		return
	}

	if err := s.drainTaskSideQuestions(drainCtx, id); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	result, err := s.m.DeleteTask(id, opts)
	if err != nil {
		var committed *taskDeleteCommittedError
		if errors.As(err, &committed) {
			// PostgreSQL에서는 이미 없습니다. 런타임 해체를 끝내고,
			// 감사할 수 있는 개수와 확정 뒤 정리 경고를 같이 돌려줍니다.
			s.engine.StopTask(id)
			s.taskAgentMu.Lock()
			delete(s.taskAgents, id)
			s.taskAgentMu.Unlock()
			deleted = true
			writeCommittedTaskDelete(w, result, err)
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	// Manager가 목록에서 작업을 뺐으므로, 새 API 조작은
	// 그 작업을 찾을 수 없습니다. 이제 그 작업에 속한 Engine 고루틴을 모두 멈추고 합류한 뒤,
	// 수명 맵을 비우고 삭제 장벽을 놓습니다.
	s.engine.StopTask(id)
	s.taskAgentMu.Lock()
	delete(s.taskAgents, id)
	s.taskAgentMu.Unlock()
	deleted = true
	writeJSON(w, 200, result)
}

func writeCommittedTaskDelete(w http.ResponseWriter, result DeleteTaskResult, err error) {
	result.CleanupWarning = err.Error()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) waitTaskQuiescent(ctx context.Context, taskID string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.chatMu.Lock()
		chatBusy := s.chatBusy[taskID]
		s.chatMu.Unlock()
		if s.engine.inflightCount(taskID) == 0 && !chatBusy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ---------- 에이전트 ----------

func (s *Server) pgListAgents(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ags, err := pg.ListAgents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	dtos := agentDTOs(ags)
	// 에이전트별 바인딩 수를 겹쳐 그립니다(mcp/skill은 id, tools는 key). 실패해도 목록은 계속됩니다.
	if mcp, skill, tools, err := pg.AgentBindingCounts(); err == nil {
		for i := range dtos {
			dtos[i].McpCount = mcp[ags[i].ID]
			dtos[i].SkillCount = skill[ags[i].ID]
			dtos[i].ToolCount = tools[ags[i].Key]
		}
	}
	writeJSON(w, 200, map[string]any{"agents": dtos})
}

// pgCreateAgent는 사용자 정의 대화 에이전트를 만듭니다(builtin=false, role은
// assistant). 시작 프롬프트를 심어 편집기가 비지 않게 합니다.
func (s *Server) pgCreateAgent(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct{ Key, Name, Description string }
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Key, req.Name = strings.TrimSpace(req.Key), strings.TrimSpace(req.Name)
	if !reAgentKey.MatchString(req.Key) {
		writeErr(w, 400, "key는 소문자로 시작해야 하며, 소문자/숫자/밑줄만 포함합니다")
		return
	}
	if req.Name == "" {
		writeErr(w, 400, "이름은 비울 수 없습니다")
		return
	}
	if exist, _ := pg.GetAgentByKey(req.Key); exist != nil {
		writeErr(w, 409, "해당 key가 이미 있습니다")
		return
	}
	a, err := pg.CreateAgent(req.Key, req.Name, req.Description)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 시작 프롬프트입니다. 편집기가 처음부터 고칠 글을 보여 주게 합니다.
	if err := pg.SeedPromptIfEmpty(a.ID, agent.DefaultAssistantPrompt); err != nil {
		log.Printf("[agents] seed starter prompt for %s 실패: %v", a.Key, err)
	}
	writeJSON(w, 200, agentDTO(a))
}

// pgUpdateAgent는 사용자 정의 에이전트의 이름/설명을 고칩니다(내장 에이전트는 거절).
func (s *Server) pgUpdateAgent(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if a.Builtin {
		writeErr(w, 400, "내장 agent는 이름/설명을 수정할 수 없습니다")
		return
	}
	var req struct{ Name, Description string }
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, 400, "이름은 비울 수 없습니다")
		return
	}
	if err := pg.UpdateAgentMeta(a.Key, req.Name, req.Description); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgDeleteAgent는 사용자 정의 에이전트를 지웁니다(내장은 거절). 프롬프트/변수/보이기는
// FK로 같이 지워집니다. 도구 바인딩 정리는 실패해도 삭제는 계속됩니다.
func (s *Server) pgDeleteAgent(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if a.Builtin {
		writeErr(w, 400, "내장 agent는 삭제할 수 없습니다")
		return
	}
	if err := pg.DeleteAgent(a.Key); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := pg.RemoveAgentFromToolBindings(a.Key); err != nil {
		log.Printf("[agents] %s 도구 바인딩 정리 실패: %v", a.Key, err)
	}
	if err := pg.DeleteTriggersForAgent(a.Key); err != nil {
		log.Printf("[agents] %s 트리거 정리 실패: %v", a.Key, err)
	}
	writeJSON(w, 200, map[string]any{"deleted": a.Key})
}

func (s *Server) agentByKey(w http.ResponseWriter, r *http.Request) (*db.DB, *db.Agent, bool) {
	pg := s.pg(w)
	if pg == nil {
		return nil, nil, false
	}
	a, err := pg.GetAgentByKey(r.PathValue("key"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, nil, false
	}
	if a == nil {
		writeErr(w, 404, "agent 를 찾을 수 없습니다")
		return nil, nil, false
	}
	return pg, a, true
}

// pgSaveAgentConfig는 에이전트의 실행 설정을 고칩니다(지금은 max_turns). 그리고
// 살아 있는 LLM을 다시 적용해 변경이 바로 먹게 합니다.
func (s *Server) pgSaveAgentConfig(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	// 모든 필드는 선택입니다(포인터). 그래서 일부만 고치는 요청(예: 트리거 탭이
	// trigger_* 필드만 보냄)은 안 건드린 설정을 그대로 둡니다.
	// max_turns나 run_seconds를 0으로 되돌리지 않습니다.
	var req struct {
		MaxTurns         *int  `json:"max_turns"`
		RunSeconds       *int  `json:"run_seconds"`
		WebSearch        *bool `json:"web_search"`
		InteractiveShell *bool `json:"interactive_shell"`
		// llm_profile_id 의 세 상태: 필드 생략=변경 없음; 명시적 null=묶음 해제(작업/전역을 따름); 숫자=그 profile 에 묶음.
		LLMProfileID json.RawMessage `json:"llm_profile_id"`
		// P3 트리거 후처리 전략(셋은 함께 선택이며, 하나라도 주면 통째로 기록하고, 주지 않으면 그대로 둔다).
		TriggerRunMode     *string `json:"trigger_run_mode"`
		TriggerMergeMode   *string `json:"trigger_merge_mode"`
		TriggerMaxParallel *int    `json:"trigger_max_parallel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	profileChanged := false
	if req.LLMProfileID != nil { // key present (숫자 또는 null)
		var id *int64
		if err := json.Unmarshal(req.LLMProfileID, &id); err != nil {
			writeErr(w, 400, "llm_profile_id 형식이 올바르지 않습니다")
			return
		}
		if id != nil { // 바인딩: 대상 profile이 유효한지 검증
			if _, ok := s.loadProfileConfig(*id); !ok {
				writeErr(w, 400, "지정한 LLM 구성이 없거나 유효하지 않습니다")
				return
			}
		}
		if err := pg.SetAgentLLMProfile(a.Key, id); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		profileChanged = true
	}
	if req.MaxTurns != nil {
		mt := *req.MaxTurns
		if mt < 0 {
			mt = 0
		}
		if err := pg.SetAgentMaxTurns(a.Key, mt); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.RunSeconds != nil {
		rs := *req.RunSeconds
		if rs < 0 {
			rs = 0
		}
		if err := pg.SetAgentRunSeconds(a.Key, rs); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.WebSearch != nil {
		if err := pg.SetAgentWebSearch(a.Key, *req.WebSearch); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if req.InteractiveShell != nil {
		if err := pg.SetAgentInteractiveShell(a.Key, *req.InteractiveShell); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// P3 트리거 전략: 셋을 한 묶음으로 기록(SetAgentTriggerBehavior가 한 번에 세 열을 씀), 빠진 필드는
	// 현재 저장값으로 다시 채워서, 하나만 넘겨 나머지 둘이 기본값으로 덮이는 일을 막는다.
	if req.TriggerRunMode != nil || req.TriggerMergeMode != nil || req.TriggerMaxParallel != nil {
		runMode, mergeMode, maxPar := a.TriggerRunMode, a.TriggerMergeMode, a.TriggerMaxParallel
		if req.TriggerRunMode != nil {
			runMode = *req.TriggerRunMode
		}
		if req.TriggerMergeMode != nil {
			mergeMode = *req.TriggerMergeMode
		}
		if req.TriggerMaxParallel != nil {
			maxPar = *req.TriggerMaxParallel
		}
		if err := pg.SetAgentTriggerBehavior(a.Key, runMode, mergeMode, maxPar); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// 에이전트의 LLM 바인딩이 바뀌면 고정된 작업과 설정별 캐시도 무효가 됩니다.
	// 그래서 다음 라운드가 각 에이전트에 묶인 모델을 다시 찾습니다.
	if profileChanged {
		s.invalidateProfileAgents()
	}
	// 작업 묶음은 운용용 에이전트 설정을 담습니다(턴/시간 예산, 도구,
	// 웹 검색). 전역 설정이 없어도 다시 만듭니다.
	s.invalidateTaskAgents()
	// 살아 있는 에이전트를 다시 만듭니다. 새 max_turns, run_seconds, 바인딩이 재시작 없이 먹게 합니다.
	s.cfgMu.Lock()
	cfg, on := s.llmCfg, s.llmOn
	s.cfgMu.Unlock()
	if on {
		_ = s.applyLLM(cfg)
	}
	resp := map[string]any{"ok": true}
	if req.MaxTurns != nil {
		resp["max_turns"] = *req.MaxTurns
	}
	if req.RunSeconds != nil {
		resp["run_seconds"] = *req.RunSeconds
	}
	writeJSON(w, 200, resp)
}

func (s *Server) pgGetAgent(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	cur, _ := pg.CurrentPrompt(a.ID)
	vars, _ := pg.PromptVars(a.ID)
	vars = withGlobalVars(vars)
	vers, _ := pg.ListPromptVersions(a.ID)
	if vers == nil {
		vers = []db.PromptVersion{}
	}
	mcp, _ := pg.AgentVisible(a.ID, "mcp")
	sk, _ := pg.AgentSkillNames(a.ID)
	if sk == nil {
		sk = []string{}
	}
	// 선택 가능한 LLM 설정 목록(id/name/model/기본 여부). 프론트가 "기본 모델" 드롭다운을 그릴 때 쓴다. 현재 바인딩은 agent.llm_profile_id를 본다.
	profs, _ := pg.ListProfiles()
	llmProfiles := make([]map[string]any, 0, len(profs))
	for _, p := range profs {
		llmProfiles = append(llmProfiles, map[string]any{
			"id": p.ID, "name": p.Name, "model": p.Model, "is_default": p.IsDefault,
		})
	}
	writeJSON(w, 200, map[string]any{
		"agent": agentDTO(a), "prompt": cur, "variables": vars, "versions": vers,
		"visibility":   map[string]any{"mcp": mcp, "skill": sk},
		"llm_profiles": llmProfiles, // 바인딩할 수 있는 LLM 설정 후보

		"wrapup_prompt":            a.WrapupPrompt,                  // 저장된 마무리 프롬프트(비어 있으면 내장 기본값)
		"wrapup_default":           agent.WrapupDefault(a.Key),      // 내장 기본값(자리 표시/기본값 복원용)
		"wrapup_max_turns":         a.WrapupMaxTurns,                // 저장된 마무리 라운드 수(0이면 내장 기본값)
		"wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key), // 내장 기본 라운드 수("0=기본 N" 안내용)
		// 작업 단위 시간 초과 마무리 문구(워커/플래너만 내장 기본값이 있다. task_timeout_supported로 프론트가 이 구역을 보일지 정한다)
		"task_timeout_wrapup_supported":         agent.TaskTimeoutWrapupDefault(a.Key) != "",
		"task_timeout_wrapup_prompt":            a.TaskTimeoutWrapupPrompt,
		"task_timeout_wrapup_default":           agent.TaskTimeoutWrapupDefault(a.Key),
		"task_timeout_wrapup_max_turns":         a.TaskTimeoutWrapupMaxTurns,
		"task_timeout_wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key),
	})
}

func (s *Server) pgSavePrompt(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct{ Template, Note string }
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	vars, _ := pg.PromptVars(a.ID)
	if bad := validateTemplate(body.Template, withGlobalVars(vars)); bad != "" {
		writeErr(w, 400, bad)
		return
	}
	ver, err := pg.SavePrompt(a.ID, body.Template, body.Note, "ui")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"version": ver})
}

// pgResetPrompt는 에이전트 프롬프트 본문을 코드 안 내장 기본값으로 되돌립니다
// (구간 [A]). 내장 에이전트만 코드 기본값이 있고, 커스텀 에이전트는 없다.
func (s *Server) pgResetPrompt(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	tmpl, has := agent.BuiltinPromptSeeds()[a.Key]
	if !has {
		writeErr(w, 400, "이 agent에는 내장 기본 프롬프트가 없어 복원할 수 없습니다")
		return
	}
	ver, err := pg.ResetPromptToDefault(a.ID, tmpl)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"version": ver})
}

// pgSaveWrapup은 에이전트의 마무리(정산) 프롬프트를 저장합니다. 시간 초과나
// 단계 소진 때 넣는 글입니다. 본문이 비면 덮어쓰기를 지워 내장 기본값을 씁니다.
func (s *Server) pgSaveWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	// MaxTurns는 선택입니다(포인터). 빼면 저장된 턴 예산을 그대로 둡니다.
	var body struct {
		Prompt   string `json:"prompt"`
		MaxTurns *int   `json:"max_turns"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetAgentWrapupPrompt(a.Key, body.Prompt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if body.MaxTurns != nil {
		n := *body.MaxTurns
		if n < 0 {
			n = 0
		}
		if err := pg.SetAgentWrapupMaxTurns(a.Key, n); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgResetWrapup은 에이전트의 마무리 프롬프트 덮어쓰기를 지워, 코드 내장
// 기본값을 다시 쓰게 합니다.
func (s *Server) pgResetWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if err := pg.SetAgentWrapupPrompt(a.Key, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := pg.SetAgentWrapupMaxTurns(a.Key, 0); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":                       true,
		"wrapup_default":           agent.WrapupDefault(a.Key),
		"wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key),
	})
}

// pgSaveTaskTimeoutWrapup은 에이전트의 작업 시간 초과 마무리 프롬프트와 턴 예산을 저장합니다
// (워커/플래너만). 프롬프트가 비거나 턴이 0이면 덮어쓰기를 지워 내장 기본값을 씁니다.
func (s *Server) pgSaveTaskTimeoutWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Prompt   string `json:"prompt"`
		MaxTurns *int   `json:"max_turns"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	turns := a.TaskTimeoutWrapupMaxTurns // 넘기지 않으면 원래 값을 유지
	if body.MaxTurns != nil {
		turns = *body.MaxTurns
		if turns < 0 {
			turns = 0
		}
	}
	if err := pg.SetAgentTaskTimeoutWrapup(a.Key, body.Prompt, turns); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgResetTaskTimeoutWrapup은 작업 시간 초과 마무리 덮어쓰기를 지워 내장 기본값으로 돌립니다.
func (s *Server) pgResetTaskTimeoutWrapup(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	if err := pg.SetAgentTaskTimeoutWrapup(a.Key, "", 0); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":                                    true,
		"task_timeout_wrapup_default":           agent.TaskTimeoutWrapupDefault(a.Key),
		"task_timeout_wrapup_max_turns_default": agent.WrapupTurnsDefault(a.Key),
	})
}

func (s *Server) pgListPromptVersions(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	vers, err := pg.ListPromptVersions(a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"versions": vers})
}

func (s *Server) pgPromptVars(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	vars, err := pg.PromptVars(a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"variables": withGlobalVars(vars)})
}

func (s *Server) pgPreviewPrompt(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Template string            `json:"template"`
		Sample   map[string]string `json:"sample"`
	}
	_ = decode(r, &body)
	vars, _ := pg.PromptVars(a.ID)
	if body.Template == "" {
		body.Template, _ = pg.CurrentPrompt(a.ID)
	}
	rendered, err := renderPrompt(body.Template, withGlobalVars(vars), body.Sample)
	if err != nil {
		writeJSON(w, 200, map[string]any{"rendered": "", "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"rendered": rendered})
}

func (s *Server) pgGetAgentVisibility(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	mcp, _ := pg.AgentVisible(a.ID, "mcp")
	sk, _ := pg.AgentSkillNames(a.ID)
	if sk == nil {
		sk = []string{}
	}
	writeJSON(w, 200, map[string]any{"mcp": mcp, "skill": sk})
}

func (s *Server) pgSetAgentVisibility(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	var body struct {
		MCP   []int64  `json:"mcp"`
		Skill []string `json:"skill"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetAgentVisibilityKind(a.ID, "mcp", body.MCP); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := pg.SetAgentSkillVisibility(a.ID, body.Skill); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- tools (내장 도구 목록) ---------- 워커가 의도 하나를 실행할 때 호출하는 도구이며, 호출 흔적은 탐색 그래프에 남는다.

func (s *Server) pgListTools(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ts, err := pg.ListTools()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ts == nil {
		ts = []*db.Tool{}
	}
	// 사용 횟수는 되면 좋은 꾸밈입니다. 실행 중 도구 결정은 평범한
	// 목록 조회를 그대로 쓰므로, 에이전트 조립이 이 집계 비용을 내지 않습니다.
	counts, countErr := pg.ToolUsageCounts()
	if countErr != nil {
		log.Printf("[tools] 호출 통계 읽기 실패: %v", countErr)
	} else {
		for _, tool := range ts {
			tool.Calls = counts[tool.Key]
		}
	}
	writeJSON(w, 200, map[string]any{"tools": ts})
}

// pgUpdateTool은 내장 도구에서 화면이 고칠 수 있는 필드를 저장합니다. 설명,
// 매개변수 스키마(구조는 그대로 두고, 매개변수별 설명과
// 기본값만 움직임), 에이전트 바인딩, 사용 여부입니다. key는 경로에서 오고
// 절대 안 바뀝니다(Go 처리기에 붙어 있음).
func (s *Server) pgUpdateTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	cur, err := pg.GetTool(key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if cur == nil {
		writeErr(w, 404, "도구가 없습니다: "+key)
		return
	}
	var body struct {
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Agents      []string        `json:"agents"`
		Enabled     bool            `json:"enabled"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	agents, _ := json.Marshal(body.Agents)
	if err := pg.UpdateTool(key, body.Description, body.Schema, agents, body.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgResetTool은 도구 행을 코드가 정한 기본값으로 덮습니다(설명,
// schema, agent binding) 그리고 그것을 다시 켠다. 명시적인 "기본값 복원" 동작인데, 왜냐하면
// 시작 때 심기는 처음 한 번만 넣고 고친 내용을 덮지 않기 때문입니다.
func (s *Server) pgResetTool(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	key := r.PathValue("key")
	for _, sd := range agent.BuiltinToolSeeds() {
		if sd.Key != key {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		agents, _ := json.Marshal(sd.Agents)
		if err := pg.UpsertToolForce(sd.Key, sd.Desc, schema, agents); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// 오케스트레이션/플랫폼 도구(auto 에이전트). 기본으로 auto에 묶습니다.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range append(s.orchestrationTools(), s.platformTools()...) {
		if t.Name() != key {
			continue
		}
		schema, _ := json.Marshal(t.InputSchema())
		if err := pg.UpsertToolForce(t.Name(), t.Description(), schema, autoAgents); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeErr(w, 404, "내장 도구가 아니거나 없습니다: "+key)
}

// ---------- mcp ----------

func (s *Server) pgListMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ms, err := pg.ListMCP()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ms == nil {
		ms = []*db.MCPServer{}
	}
	writeJSON(w, 200, map[string]any{"servers": ms})
}

func (s *Server) pgSaveMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var m db.MCPServer
	if err := decode(r, &m); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	isNew := m.ID == 0
	id, err := pg.SaveMCP(&m)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 처음 추가할 때 도구 목록을 자동으로 찾아 캐시합니다. 화면이 바로
	// 보게 하려고요(느리거나 깨진 서버가 요청을 붙잡지 않게 시간을 จำกัด). 평범한
	// 갱신(예: 사용 스위치)에서는 건너뛰어, 매번 서버를 다시 띄우지 않습니다.
	if isNew && m.Enabled {
		m.ID = id
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		if derr := s.discoverAndCacheMCP(ctx, &m); derr != nil {
			log.Printf("[mcp] %s 추가 후 도구 디스커버리 실패: %v", m.Name, derr)
		}
		cancel()
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) pgDeleteMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	if err := pg.DeleteMCP(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// pgRefreshMCP는 MCP 하나의 도구를 요청 때 다시 찾아 캐시합니다. 그래서
// 설정 변경이나 앞선 찾기 실패를 재시작 없이 고칠 수 있습니다.
func (s *Server) pgRefreshMCP(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	all, err := pg.ListMCP()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var target *db.MCPServer
	for _, m := range all {
		if m.ID == id {
			target = m
			break
		}
	}
	if target == nil {
		writeErr(w, 404, "MCP가 없습니다")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.discoverAndCacheMCP(ctx, target); err != nil {
		writeErr(w, 502, "도구 디스커버리 실패: "+err.Error())
		return
	}
	tools, _ := pg.MCPToolsDetailed(id)
	writeJSON(w, 200, map[string]any{"tools": tools})
}

// pgMCPTools는 MCP 하나의 캐시된 도구(이름과 설명)를 상세 화면에 돌려줍니다.
func (s *Server) pgMCPTools(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	tools, err := pg.MCPToolsDetailed(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tools == nil {
		tools = []db.MCPTool{}
	}
	writeJSON(w, 200, map[string]any{"tools": tools})
}

// ---------- skills (파일 시스템) ---------- 플래너가 의도를 만들 때 읽는 설명 파일이며, 자산 그래프의 대상과는 따로 둔다.

type skillFileNode struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	License       string   `json:"license,omitempty"`
	Compatibility string   `json:"compatibility,omitempty"`
	MCPs          []string `json:"mcps,omitempty"`
	Files         []string `json:"files"`
	// 사용 장부입니다(db/skill_usage.go). 한 번도 안 부른 스킬은 Calls가 0이고 LastUsed가 nil입니다.
	// 장부에는 에이전트가 실제로 불러온 스킬 행만 있습니다.
	Calls    int        `json:"calls"`
	Tasks    int        `json:"tasks"`
	Agents   []string   `json:"usage_agents"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

func (s *Server) fsListSkills(w http.ResponseWriter, r *http.Request) {
	_ = os.MkdirAll(s.skillDir, 0o755)
	allReg, _ := skill.LoadDir(s.skillDir)
	metaByDir := map[string]skill.Skill{}
	if allReg != nil {
		for _, sk := range allReg.List() {
			if sk.Dir != "" {
				metaByDir[filepath.Base(sk.Dir)] = sk
			}
		}
	}
	entries, err := os.ReadDir(s.skillDir)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 사용 횟수는 되면 좋은 꾸밈입니다. 장부 읽기가 실패하면 횟수를
	// 0으로 두고, 스킬 목록 자체는 실패시키지 않습니다.
	statBySkill := map[string]db.SkillStat{}
	if s.m.pg != nil {
		stats, err := s.m.pg.SkillStats()
		if err != nil {
			log.Printf("[skills] 호출 통계 읽기 실패: %v", err)
		}
		for _, st := range stats {
			statBySkill[st.Skill] = st
		}
	}
	nodes := []skillFileNode{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		node := skillFileNode{Name: dirName, Agents: []string{}}
		if st, ok := statBySkill[dirName]; ok {
			node.Calls, node.Tasks, node.Agents, node.LastUsed = st.Calls, st.Tasks, st.Agents, st.LastUsed
		}
		if meta, ok := metaByDir[dirName]; ok {
			node.Description = meta.Description
			node.License = meta.License
			node.Compatibility = meta.Compatibility
			node.MCPs = meta.MCPs
		}
		node.Files, _ = walkSkillFiles(filepath.Join(s.skillDir, dirName))
		if node.Files == nil {
			node.Files = []string{}
		}
		nodes = append(nodes, node)
	}
	writeJSON(w, 200, map[string]any{"skills": nodes})
}

// fsSkillUsage는 스킬 하나의 최근 호출을 최신순으로 돌려줍니다(상세 패널).
func (s *Server) fsSkillUsage(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "잘못된 skill 이름")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	calls, err := pg.RecentSkillCalls(name, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"calls": calls})
}

// fsMissingSkills는 에이전트가 찾았지만 없는 스킬 이름을 나열합니다. 빈칸
// 목록입니다. 다음에 어떤 절차를 쓸 가치가 있는지입니다.
func (s *Server) fsMissingSkills(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	missing, err := pg.MissingSkillStats(limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"missing": missing})
}

// cleanStrs는 각 문자열의 공백을 자르고 빈 값을 버립니다.
func cleanStrs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) fsCreateSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string   `json:"name"`
		Description   string   `json:"description"`   // agentskills.io 규격상 필수
		License       string   `json:"license"`       // 선택
		Compatibility string   `json:"compatibility"` // 선택
		MCPs          []string `json:"mcps"`          // 선택. 이 스킬이 불러오면 열리는 MCP 서버
		Instructions  string   `json:"instructions"`  // 선택. 비어 있으면 뼈대를 만듦
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !validSkillName(body.Name) {
		writeErr(w, 400, "skill 이름은 1–64자의 소문자, 숫자, 하이픈만 쓸 수 있고, 하이픈으로 시작하거나 끝나거나 두 번 연속일 수 없습니다")
		return
	}
	if strings.TrimSpace(body.Description) == "" {
		writeErr(w, 400, "설명은 필수입니다")
		return
	}
	skillPath := filepath.Join(s.skillDir, body.Name)
	if _, err := os.Stat(skillPath); err == nil {
		writeErr(w, 409, "skill 이 이미 있습니다")
		return
	}
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 규격에 맞는 SKILL.md를 만듭니다(agentskills.io 형식).
	//   YAML 프론트매터: name(필수), description(필수),
	//                     license, compatibility(선택)
	//   Markdown 본문:    단계별 안내
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "name: %s\n", body.Name)
	fmt.Fprintf(&sb, "description: %s\n", body.Description)
	if body.License != "" {
		fmt.Fprintf(&sb, "license: %s\n", body.License)
	}
	if body.Compatibility != "" {
		fmt.Fprintf(&sb, "compatibility: %s\n", body.Compatibility)
	}
	// mcps: 이 스킬을 불러오면 열리는 MCP 서버(스킬로 막힌 지연 도구).
	if mcps := cleanStrs(body.MCPs); len(mcps) > 0 {
		fmt.Fprintf(&sb, "mcps: %s\n", strings.Join(mcps, ", "))
	}
	sb.WriteString("---\n")
	if strings.TrimSpace(body.Instructions) != "" {
		sb.WriteString(body.Instructions)
	} else {
		// 바로 쓸 수 있게 최소한의 Markdown 본문 뼈대를 만듭니다.
		fmt.Fprintf(&sb, "## %s\n\n", body.Name)
		sb.WriteString("<!-- Describe step-by-step instructions in Markdown. -->\n\n")
		sb.WriteString("1. \n2. \n3. \n")
	}
	if err := os.WriteFile(filepath.Join(skillPath, "SKILL.md"), []byte(sb.String()), 0o644); err != nil {
		_ = os.RemoveAll(skillPath)
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"name": body.Name})
}

// fsUpdateSkillMeta는 SKILL.md 프론트매터에서 본문을 건드리지 않고 고쳐도 되는
// 필드를 다시 씁니다. mcps, license, compatibility,
// description입니다. 요청 본문에 있는 필드만 고칩니다. 빠진
// 필드는 그대로 둡니다(프론트매터 전체를 파싱한 값으로 다시 만들므로,
// 쓰기는 줄 단위 수정이 아니라 깨끗한 교체입니다).
func (s *Server) fsUpdateSkillMeta(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	var body struct {
		MCPs          *[]string `json:"mcps"`
		Description   *string   `json:"description"`
		License       *string   `json:"license"`
		Compatibility *string   `json:"compatibility"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	skillMD := filepath.Join(s.skillDir, name, "SKILL.md")
	raw, err := os.ReadFile(skillMD)
	if err != nil {
		writeErr(w, 404, "skill 을 찾을 수 없습니다")
		return
	}
	updated, err := rewriteSkillFrontmatter(raw, body.MCPs, body.Description, body.License, body.Compatibility)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := os.WriteFile(skillMD, updated, 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// rewriteSkillFrontmatter는 SKILL.md의 YAML 프론트매터를 파싱하고,
// nil이 아닌 포인터로 준 필드를 바꿉니다. 안내 본문(닫는 --- 뒤)은
// 그대로 둡니다. 알아볼 프론트매터가 없으면 오류를 돌려줍니다.
func rewriteSkillFrontmatter(content []byte, mcps *[]string, description, license, compatibility *string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("SKILL.md has no YAML frontmatter")
	}
	fmEnd := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			fmEnd = i
			break
		}
	}
	if fmEnd < 0 {
		return nil, fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	// 프론트매터의 기존 키와 값을 모읍니다(모르는 키는 유지).
	type kv struct{ k, v string }
	var pairs []kv
	for _, l := range lines[1:fmEnd] {
		if idx := strings.IndexByte(l, ':'); idx >= 0 {
			pairs = append(pairs, kv{strings.TrimSpace(l[:idx]), strings.TrimSpace(l[idx+1:])})
		} else if strings.TrimSpace(l) != "" {
			pairs = append(pairs, kv{"", l}) // 키가 아닌 줄은 그대로 둡니다.
		}
	}
	// 갱신을 적용합니다(nil 포인터면 변경 없음).
	applyStr := func(key string, val *string) {
		if val == nil {
			return
		}
		for i, p := range pairs {
			if p.k == key {
				pairs[i].v = strings.TrimSpace(*val)
				return
			}
		}
		pairs = append(pairs, kv{key, strings.TrimSpace(*val)})
	}
	applyStr("description", description)
	applyStr("license", license)
	applyStr("compatibility", compatibility)
	if mcps != nil {
		cleaned := cleanStrs(*mcps)
		// 기존 mcps 줄을 뺍니다.
		filtered := pairs[:0]
		for _, p := range pairs {
			if p.k != "mcps" {
				filtered = append(filtered, p)
			}
		}
		pairs = filtered
		if len(cleaned) > 0 {
			pairs = append(pairs, kv{"mcps", strings.Join(cleaned, ", ")})
		}
	}
	// 다시 조립합니다.
	var sb strings.Builder
	sb.WriteString("---\n")
	for _, p := range pairs {
		if p.k == "" {
			sb.WriteString(p.v)
		} else {
			fmt.Fprintf(&sb, "%s: %s", p.k, p.v)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("---\n")
	// 본문(닫는 --- 다음 줄)
	if fmEnd+1 < len(lines) {
		sb.WriteString(strings.Join(lines[fmEnd+1:], "\n"))
	}
	return []byte(sb.String()), nil
}

// zip 올리기 안전 상한입니다(zip 폭탄이나 폭주하는 아카이브를 막음).
const (
	maxSkillZipBytes   = 20 << 20  // 압축된 요청 본문 20MB
	maxSkillTotalBytes = 100 << 20 // 압축을 푼 전체 100MB
	maxSkillFileBytes  = 20 << 20  // 푼 파일 하나당 20MB
	maxSkillEntries    = 4000      // 아카이브 안 파일 수 상한
)

// skillNameFromFrontmatter는 SKILL.md의 YAML
// 프론트매터(처음 두 `---` 사이)에서 name 값을 꺼냅니다. 없으면 빈 문자열입니다.
func skillNameFromFrontmatter(md []byte) string {
	lines := strings.Split(string(md), "\n")
	inFM := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "---" {
			if !inFM {
				inFM = true
				continue
			}
			break // 프론트매터의 끝
		}
		if inFM && strings.HasPrefix(t, "name:") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "name:"))
			return strings.Trim(v, `"'`) // name: "한국어 스킬" 도 인식
		}
	}
	return ""
}

// fsUploadSkill은 올린 .zip으로 스킬을 설치합니다. 아카이브에는
// SKILL.md가 있어야 합니다(루트 또는 맨 위 디렉터리 하나 아래). 스킬 이름은
// 그 파일의 name 프론트매터에서 가져옵니다(없으면 맨 위 디렉터리나 zip 이름).
// Zip-slip은 모든 항목 경로를 skillRelPath로 검사해 막습니다. 크기와 개수
// 상한이 zip 폭탄을 막습니다. POST ?overwrite=true면 같은 이름의
// 기존 스킬을 바꿉니다.
func (s *Server) fsUploadSkill(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSkillZipBytes)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "업로드 파일이 없습니다(폼 필드 file). 또는 크기 제한을 초과했습니다")
		return
	}
	defer file.Close()
	buf, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	zr, err := newSkillZipReader(buf)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 항목은 UTF-8로 디코딩된 이름을 담는다(GBK 패키지도 읽을 수 있다). archiver 찌꺼기는 제외한다.
	entriesAll := skillZipEntries(zr)
	if err := checkSkillZipMethods(entriesAll); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	// 가장 얕은 SKILL.md를 찾습니다. 그 디렉터리가 zip 안 스킬 루트입니다.
	var skillMD *skillZipEntry
	for i := range entriesAll {
		e := &entriesAll[i]
		if path.Base(e.name) != "SKILL.md" {
			continue
		}
		if skillMD == nil || strings.Count(e.name, "/") < strings.Count(skillMD.name, "/") {
			skillMD = e
		}
	}
	if skillMD == nil {
		writeErr(w, 400, "압축 파일에서 SKILL.md를 찾지 못했습니다")
		return
	}
	root := path.Dir(skillMD.name) // SKILL.md가 zip 루트면 "."
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}

	// SKILL.md 프론트매터에서 스킬 이름을 만들고 검사합니다.
	md, err := readZipEntry(skillMD.f)
	if err != nil {
		writeErr(w, 400, "SKILL.md 읽기 실패: "+err.Error())
		return
	}
	name := skillNameFromFrontmatter(md)
	if name == "" && prefix != "" {
		name = path.Base(strings.TrimSuffix(prefix, "/"))
	}
	if name == "" {
		base := path.Base(filepath.ToSlash(hdr.Filename))
		name = strings.TrimSuffix(base, path.Ext(base))
	}
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 유효하지 않습니다(SKILL.md의 name 필드에서 가져옴): "+name+
			"（64자 이하, 글자로 시작, 소문자/숫자/하이픈 또는 한글 등 비 ASCII 글자만 사용할 수 있으며, 공백·점·경로 구분자는 사용할 수 없습니다）")
		return
	}

	skillPath := filepath.Join(s.skillDir, name)
	overwrite := r.URL.Query().Get("overwrite") == "true"
	if _, err := os.Stat(skillPath); err == nil && !overwrite {
		writeErr(w, 409, "skill이 이미 있습니다: "+name+"（덮어쓰려면 확인한 뒤 다시 시도하세요）")
		return
	}

	// 먼저 임시 디렉터리에 푼 뒤 원자적으로 바꿉니다. 나쁜 항목이 있으면
	// 올리기 전체를 멈추고, 반만 쓴 스킬을 남기지 않습니다.
	_ = os.MkdirAll(s.skillDir, 0o755)
	tmp, err := os.MkdirTemp(s.skillDir, ".upload-*")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(tmp) // 이름 바꾸기가 성공하면 아무 일도 안 함

	var total int64
	entries := 0
	for _, e := range entriesAll {
		f := e.f
		// 스킬 루트 아래 파일만. 그 밖은 건너뜁니다.
		if prefix != "" && !strings.HasPrefix(e.name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(e.name, prefix)
		if rel == "" {
			continue
		}
		clean, msg := skillRelPath(rel)
		if msg != "" {
			writeErr(w, 400, "압축 파일에 잘못된 경로가 있습니다 "+e.name+"："+msg)
			return
		}
		if entries++; entries > maxSkillEntries {
			writeErr(w, 400, "압축 파일의 파일 수가 너무 많습니다")
			return
		}
		if f.UncompressedSize64 > maxSkillFileBytes {
			writeErr(w, 400, "파일이 너무 큽니다: "+rel)
			return
		}
		dst := filepath.Join(tmp, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		rc, err := f.Open()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			writeErr(w, 500, err.Error())
			return
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxSkillFileBytes+1))
		out.Close()
		rc.Close()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		total += n
		if total > maxSkillTotalBytes {
			writeErr(w, 400, "압축 해제 후 크기가 너무 큽니다")
			return
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "SKILL.md")); err != nil {
		writeErr(w, 400, "압축 해제 후 SKILL.md가 없습니다")
		return
	}

	if overwrite {
		_ = os.RemoveAll(skillPath)
	}
	if err := os.Rename(tmp, skillPath); err != nil {
		writeErr(w, 500, "설치 실패: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"name": name, "files": entries})
}

// readZipEntry는 항목 하나의 바이트를 읽습니다(상한이 있음). 이름 대신 *zip.File을 받습니다.
// zr.Open은 올바른 UTF-8이 아닌 이름을 거절하기 때문입니다(GBK 이름 아카이브).
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxSkillFileBytes))
}

func (s *Server) fsDeleteSkill(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	if err := pg.DeleteSkillVisibility(name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.RemoveAll(filepath.Join(s.skillDir, name)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": name})
}

// skillPathBlocked는 스킬 상대 경로에 있으면 안 되는 ASCII 문자입니다.
// 백슬래시는 윈도우 구분자입니다. %는 두 번 URL 인코딩한 속임수가 남지 않게 합니다
// (Go의 net/http PathValue는 한 번만 URL 디코딩합니다. %2F는 /, %2e는 .가 됩니다. 그래서 공격자가
// %252e%252e를 보내면 여기에는 글자 그대로 %2e%2e가 도착하고, %에서 거절됩니다). #와
// ?는 경로가 다시 URL을 탈 때 경로를 잘라 버립니다. 나머지는
// 윈도우 파일시스템에서 예약된 문자입니다.
const skillPathBlocked = `\%#?*:"<>|`

// skillPathRune은 r이 클라이언트가 준 스킬 경로에 나와도 되는지 알립니다.
// ASCII 허용 목록이 아니라 유니코드 차단 목록이라서 한글(그리고 어떤
// 다른 글자) 파일 이름도 됩니다. 경로 검사를 어렵게 하는 것은
// 여전히 거절합니다. 제어 문자, 형식 문자, 비슷해 보이는 공백, 구분자입니다.
func skillPathRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r == utf8.RuneError:
		return false // NUL과 제어 문자, 잘못된 UTF-8
	case strings.ContainsRune(skillPathBlocked, r):
		return false
	case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r):
		return false // 제로폭 결합자, 양방향 오버라이드(RLO 파일 이름 위장), 사용자 정의
	case r != ' ' && unicode.IsSpace(r):
		return false // NBSP / 전각 공백 같은 것: 공백처럼 보이지만 공백이 아니다
	}
	return true
}

// maxSkillPathLen은 상대 경로 길이를 막아, 병적인 이름이 시스템 호출까지 가지 않게 합니다.
const maxSkillPathLen = 512

// skillRelPath는 클라이언트가 준 상대 파일 경로를 검사합니다.
// 성공하면 정리된 경로와 빈 오류 문자열을 돌려줍니다.
// 검사 순서가 중요합니다. 글자 검사를 Clean보다 먼저 해서, 인코딩 속임수가
// 정규화를 통과하지 못하게 합니다.
func skillRelPath(file string) (string, string) {
	// 1. 글자 검사. 정규화 전입니다. 널 바이트, 백슬래시,
	//    %, 제어 문자, 유니코드로 비슷해 보이는 문자를 막습니다. CJK 이름은 통과합니다.
	if file == "" || len(file) > maxSkillPathLen {
		return "", "invalid path: empty or too long"
	}
	if !utf8.ValidString(file) {
		return "", "invalid path: not valid UTF-8"
	}
	for _, r := range file {
		if !skillPathRune(r) {
			return "", "invalid path: illegal character " + strconv.QuoteRune(r)
		}
	}
	// 2. ".."를 명시적으로 거절합니다. 위의 허용 목록으로는 인코딩 우회가 이미
	//    불가능하지만, 의도가 보이게 이 검사를 남깁니다.
	if strings.Contains(file, "..") {
		return "", "invalid path: '..' not allowed"
	}
	// 3. 앞의 슬래시와 빈 구간(슬래시 두 번)을 거절합니다.
	//    앞의 슬래시는 filepath.Clean 뒤에도 절대 경로로 남습니다.
	if strings.HasPrefix(file, "/") || strings.Contains(file, "//") {
		return "", "invalid path: must be relative with no empty segments"
	}
	// 4. 정규화한 뒤 Clean 이후를 마지막으로 다시 확인합니다.
	//    filepath.Clean은 남는 구분자를 없애고 점 하나를 풉니다.
	clean := filepath.Clean(file)
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", "invalid path"
	}
	return clean, ""
}

// walkSkillFiles는 root 아래의 모든 파일을 root 기준 상대 경로로, 정렬해 돌려줍니다.
// 하위 디렉터리의 파일도 포함합니다(scripts/, references/, assets/ 등).
// walkSkillFiles는 root 아래의 모든 항목을 root 기준으로 돌려줍니다.
// 디렉터리는 끝에 /를 붙여, 화면이 파일과 구분하고
// 빈 폴더도 트리에 그리게 합니다.
func walkSkillFiles(root string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 읽을 수 없는 항목은 건너뜁니다.
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			entries = append(entries, rel+"/")
		} else {
			entries = append(entries, rel)
		}
		return nil
	})
	return entries, err
}

func (s *Server) fsListFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	dirPath := filepath.Join(s.skillDir, name)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		writeErr(w, 404, "skill 을 찾을 수 없습니다")
		return
	}
	files, err := walkSkillFiles(dirPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if files == nil {
		files = []string{}
	}
	writeJSON(w, 200, map[string]any{"files": files})
}

func (s *Server) fsReadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	file, errMsg := skillRelPath(r.PathValue("file"))
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.skillDir, name, file))
	if os.IsNotExist(err) {
		writeErr(w, 404, "파일을 찾을 수 없습니다")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"content": string(data), "file": file})
}

func (s *Server) fsWriteFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	file, errMsg := skillRelPath(r.PathValue("file"))
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	skillPath := filepath.Join(s.skillDir, name)
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		writeErr(w, 404, "skill 을 찾을 수 없습니다")
		return
	}
	fullPath := filepath.Join(skillPath, file)
	// 필요하면 부모 디렉터리를 만듭니다(예: scripts/, references/, assets/).
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(fullPath, []byte(body.Content), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) fsCreateDir(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dir, errMsg := skillRelPath(body.Path)
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	skillPath := filepath.Join(s.skillDir, name)
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		writeErr(w, 404, "skill 을 찾을 수 없습니다")
		return
	}
	if err := os.MkdirAll(filepath.Join(skillPath, dir), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"dir": dir})
}

func (s *Server) fsDeletePath(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, 400, "skill 이름이 올바르지 않습니다")
		return
	}
	file, errMsg := skillRelPath(r.PathValue("file"))
	if errMsg != "" {
		writeErr(w, 400, errMsg)
		return
	}
	fullPath := filepath.Join(s.skillDir, name, file)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		writeErr(w, 404, "not found")
		return
	}
	if err := os.RemoveAll(fullPath); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": file})
}

// ---------- 스킬 보이기 ----------

func (s *Server) pgSkillVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	agents, err := pg.SkillAgents(r.PathValue("name"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"agents": idStrings(agents)})
}

func (s *Server) pgToggleSkillVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		AgentID   int64  `json:"agent_id,string"`
		SkillName string `json:"skill_name"`
		Visible   bool   `json:"visible"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleSkillVisibility(body.AgentID, body.SkillName, body.Visible); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 보이기(MCP 자원 쪽과 스위치) ----------

func (s *Server) pgResourceVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	agents, err := pg.ResourceAgents(r.PathValue("kind"), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"agents": idStrings(agents)})
}

func (s *Server) pgToggleVisibility(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		AgentID    int64  `json:"agent_id,string"`
		Kind       string `json:"kind"`
		ResourceID int64  `json:"resource_id"`
		Visible    bool   `json:"visible"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleVisibility(body.AgentID, body.Kind, body.ResourceID, body.Visible); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- LLM 설정 ----------

func (s *Server) pgListProfiles(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ps, err := pg.ListProfiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"profiles": llmProfileDTOs(ps)})
}

func (s *Server) pgSaveProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	// db.LLMProfile의 APIKey는 json:"-"라 읽을 때 화면으로 새지 않습니다.
	// 만들고 고칠 때는 옆 필드로 받습니다. Streaming은
	// 포함된 json:"streaming"을 가리는 *bool입니다. 필드가 없으면 스트리밍(true)이
	// 기본이어야 합니다. 그냥 bool의 영(false, 스트리밍 아님)은
	// 틀립니다. 이 필드를 안 보내는 옛 클라이언트나 일부 클라이언트는 스트리밍을 유지합니다.
	var body struct {
		db.LLMProfile
		APIKey    string `json:"api_key"`
		Streaming *bool  `json:"streaming"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := body.LLMProfile
	p.APIKey = body.APIKey
	p.Streaming = body.Streaming == nil || *body.Streaming
	// 출력 상한: 음수는 의미가 없어 0으로 둔다(= 그 필드를 보내지 않음). 필드 이름 스위치는 Chat Completions에서만
	// 쓸모가 있다. anthropic과 openai-responses는 필드 이름이 각각 고정되어 있어, 저장하면 나중 독자만 오해하고,
	// 그래서 openai 형식이 아니면 모두 비운다. 알 수 없는 값도 비워서, DB CHECK 오류를 사용자에게 넘기지 않는다.
	if p.MaxTokens < 0 {
		p.MaxTokens = 0
	}
	if p.Format != "openai" || p.MaxTokensField != llm.MaxTokensFieldCompletion {
		p.MaxTokensField = ""
	}
	id, err := pg.SaveProfile(&p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 설정을 고치면, 그것에 고정된 작업은 다음 라운드에 다시 만들어집니다. 활성
	// 설정도 다시 적용합니다. 전역 폴백과 명시한 작업 사슬이
	// 같은 제공자 캐시 항목을 다시 채워야, 제한기를 하나 같이 씁니다.
	s.invalidateProfileAgents()
	// 바로 적용합니다. 활성 설정이 바뀔 때만은 아닙니다. 장애 조치가 켜져 있으면
	// 어떤 설정의 키, 모델, 우선순위, pool_exclude를 고쳐도 사슬 모양이 바뀌므로,
	// 어느 쪽이든 엔진의 제공자를 다시 만들어야 합니다.
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"id": id})
}

// pgGetLLMRetryPolicy는 전역 재시도 전략(다섯 층 각각의 횟수+간격)을 반환한다. 설정된 적 없으면 → 모두 0,
// 프론트는 0을 「기본」으로 보여 준다.
func (s *Server) pgGetLLMRetryPolicy(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	writeJSON(w, 200, pg.LLMRetryPolicy())
}

// pgSaveLLMRetryPolicy는 전역 재시도 전략을 저장한다. 「엔드포인트를 따라가는」 세 층(연결/빈 응답/같은
// provider 안전 창)은 provider의 생성 인자 또는 호출 인자라, 바꾼 뒤에는 캐시 속 provider를
// 다시 만들어야 한다. 서킷 브레이커 인자는 프로세스 단위 Registry에 바로 보낸다.
func (s *Server) pgSaveLLMRetryPolicy(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var pol db.LLMRetryPolicy
	if err := decode(r, &pol); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetLLMRetryPolicy(pol); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.applyRetryPolicy()
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
	writeJSON(w, 200, pg.LLMRetryPolicy())
}

func (s *Server) pgDeleteProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	if err := pg.DeleteProfileContext(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrActiveLLMProfileDelete):
			writeErr(w, 409, "현재 활성화된 LLM 구성은 삭제할 수 없습니다. 먼저 다른 구성을 활성화하세요")
		case errors.Is(err, db.ErrLLMProfileReferencesChanged):
			writeErr(w, 409, "LLM 구성을 작업 또는 세션이 수정 중입니다. 다시 시도하세요")
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, 409, "LLM 구성 참조가 해제되기를 기다리다 시간이 초과되었습니다. 다시 시도하세요")
		case errors.Is(err, db.ErrLLMProfileNotFound):
			writeErr(w, 404, err.Error())
		default:
			writeErr(w, 500, err.Error())
		}
		return
	}
	s.invalidateProfileAgents() // 지운 설정의 캐시된 에이전트를 버립니다.
	s.llmHealth.Reset(id)       // 이제 차단기 상태는 의미가 없습니다(행이 FK로 같이 사라짐).
	s.restoreTasksAfterProfileDelete(pg)
	// 캐시 무효는 활성 설정의 공유 제공자 항목도 뺐습니다.
	// 작업 상태를 맞춘 뒤에 다시 적용해, 깨울 때 삭제 뒤의 사슬을 보게 합니다.
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) restoreTasksAfterProfileDelete(pg *db.DB) {
	for _, task := range s.m.List() {
		if taskID, err := strconv.ParseInt(task.ID, 10, 64); err == nil {
			if pt, err := pg.GetTask(taskID); err == nil && pt != nil {
				s.syncTaskLLMState(pt)
				// 활성 항목을 지우면 작업이 준비된 다음 설정으로 갈 수 있고,
				// 또는 명시한 사슬을 비우고 에이전트/전역 폴백으로 돌아갑니다.
				// 바닥난 사슬 때문에 막힌 의도만 재개합니다.
				// 일시정지된 작업은 그대로 두고, 끝난 작업은 바꾸지 않습니다.
				if !isTerminalStatus(task.lifecycleSnapshot().Status) && s.taskRuntimeAvailable(task, "planner", "worker") {
					if _, reopenErr := task.Store.ReopenIntentsByBlockedReason(db.IntentBlockedLLMQuota); reopenErr != nil {
						log.Printf("[llm-profile] task %s reopen quota-blocked intents after profile delete: %v", task.ID, reopenErr)
					}
				}
				task.Notify()
			}
		}
	}
}

func (s *Server) pgActivateProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetActiveProfile(body.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateProfileAgents() // 활성 변경은 고정된 작업의 폴백에 영향을 줄 수 있습니다.
	s.reapplyActiveProfile()    // 돌고 있는 엔진을 새로 활성화한 설정으로 바꿉니다.
	writeJSON(w, 200, map[string]any{"ok": true})
}

// pgLLMPoolStatus는 페일오버("순환") 전환, 결정된 체인 순서,
// 그리고 설정마다의 회로 차단기 상태입니다. LLM 화면이 다음으로 그리는
// "순환 순서" 띠와 카드별 건강 배지를 보고한다.
func (s *Server) pgLLMPoolStatus(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	writeJSON(w, 200, s.llmPoolStatus())
}

// pgLLMPoolReset은 끊긴 설정의 회로 차단기를 비워, 다음 호출이
// 바로 다시 시도한다("즉시 복구"). id=0이면 모든 profile을 비운다.
func (s *Server) pgLLMPoolReset(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"` // 0이거나 생략이면 전부
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.ID > 0 {
		s.llmHealth.Reset(body.ID)
	} else {
		for id := range s.llmHealth.Snapshot() {
			s.llmHealth.Reset(id)
		}
	}
	writeJSON(w, 200, s.llmPoolStatus())
}

// pgListModels는 제공자 API 끝점에서 쓸 수 있는 모델을 가져옵니다.
// OpenAI 형식(GET /models)과 Anthropic 형식(GET /v1/models)을 받습니다.
// 모델 목록이 OpenAI 경로에만 있는 Anthropic 호환 제3자(예: DeepSeek)는
// 루트를 잘라 OpenAI 끝점으로 물러섭니다.
func (s *Server) pgListModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider  string `json:"provider"` // 형식: openai 또는 anthropic
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
		Proxy     string `json:"proxy"`
		ProfileID *int64 `json:"profile_id"` // 폴백: 이 설정에 저장된 키를 씀
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// API 키를 정합니다. 폼 입력, 그다음 설정에 저장된 키.
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" && req.ProfileID != nil {
		if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
			apiKey = p.APIKey
		}
	}
	if apiKey == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "API Key가 제공되지 않음"})
		return
	}

	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	provider := strings.TrimSpace(req.Provider)

	// 순서대로 시도할 후보 끝점입니다. 어떤 Anthropic 호환 제공자는
	// (예: DeepSeek) /v1/messages를 /anthropic 경로 아래에 두지만,
	// 모델 목록은 OpenAI 형식 경로에만 둡니다. 그래서 anthropic이면
	// 루트를 잘라 OpenAI 끝점으로 물러섭니다.
	type candidate struct {
		url string
		hdr http.Header
	}
	bearerHdr := func() http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+apiKey)
		return h
	}
	anthropicHdr := func() http.Header {
		h := http.Header{}
		h.Set("x-api-key", apiKey)
		h.Set("anthropic-version", "2023-06-01")
		return h
	}

	var candidates []candidate
	switch provider {
	case "openai", "openai-responses":
		// Responses API는 OpenAI 모델 목록을 /v1/models에서 같이 씁니다.
		// 어느 형식이든 전체 끝점 URL을 허용합니다.
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		b := strings.TrimRight(strings.TrimSuffix(strings.TrimSuffix(baseURL, "/chat/completions"), "/responses"), "/")
		candidates = append(candidates, candidate{b + "/models", bearerHdr()})
	default: // anthropic 형식
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		b := strings.TrimRight(strings.TrimSuffix(baseURL, "/v1/messages"), "/")
		candidates = append(candidates, candidate{b + "/v1/models", anthropicHdr()})
		// Anthropic 호환 제3자용 폴백입니다. 모델 목록이
		// OpenAI 경로에 있으면, 끝의 /anthropic을 자르고 OpenAI 끝점을 시도합니다.
		if root := strings.TrimRight(strings.TrimSuffix(b, "/anthropic"), "/"); root != b {
			candidates = append(candidates,
				candidate{root + "/models", bearerHdr()},
				candidate{root + "/v1/models", bearerHdr()},
			)
		}
	}

	// 선택적 프록시가 있는 HTTP 클라이언트를 만듭니다.
	transport := &http.Transport{}
	if p := strings.TrimSpace(req.Proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	// 후보를 하나씩 시도하고, 비어 있지 않은 모델 목록을 처음 준 것을 돌려줍니다.
	var lastErr string
	emptyOK := false
	for _, c := range candidates {
		httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, c.url, nil)
		if err != nil {
			lastErr = "요청 구성 실패: " + err.Error()
			continue
		}
		httpReq.Header = c.hdr
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = "요청 실패: " + err.Error()
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("API 반환 %d: %s", resp.StatusCode, string(body[:min(len(body), 512)]))
			continue
		}
		// OpenAI와 Anthropic 모두 {"data": [{"id": "..."}, ...]}를 돌려줍니다.
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			lastErr = "응답 파싱 실패: " + err.Error()
			continue
		}
		models := make([]string, 0, len(parsed.Data))
		for _, m := range parsed.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
		if len(models) > 0 {
			writeJSON(w, 200, map[string]any{"ok": true, "models": models})
			return
		}
		emptyOK = true // 200인데 모델 id가 없습니다. 다른 후보를 계속 시도합니다.
	}
	if emptyOK {
		writeJSON(w, 200, map[string]any{"ok": true, "models": []string{}})
		return
	}
	if lastErr == "" {
		lastErr = "모델 목록을 가져오지 못했습니다"
	}
	writeJSON(w, 200, map[string]any{"ok": false, "error": lastErr})
}

// --- prompt template helpers (Go text/template + catalog 허용 목록) --- 엔진이 UI용 문구 틀을 고를 때 쓰며, 자산 그래프와 탐색 그래프의 행을 직접 쓰지 않는다.

// globalPromptVars는 모든 에이전트(내장과
// 사용자 정의)가 에이전트별 목록과 무관하게 쓸 수 있는 실행 중 변수입니다. 각 에이전트의 그리기 경로가
// 채웁니다(agent.nowStr, 턴마다 새로). 그래서 프롬프트는 항상
// {{.Now}}를 가리킬 수 있습니다. 예를 들어 고정된 시작 시각에서 빼 경과 시간을 따집니다.
// 초보용: 플래너와 워커를 포함한 모든 에이전트 프롬프트가 {{.Now}} 같은 값을 쓸 수 있습니다.
var globalPromptVars = []db.PromptVar{
	{Name: "Now", Description: "서버 현재 시각（실행할 때마다 실시간으로 갱신되며, 고정된 시작 시각과 빼서 경과 시간을 판단할 수 있습니다）", Example: "2026-08-11 14:30:00 CST", Source: "runtime"},
	{Name: "DataDir", Description: "서버 데이터 루트 디렉터리（모든 작업/세션 산출물의 루트. 각 agent의 실제 디스크 쓰기는 그 아래 하위 디렉터리에서 이루어집니다. 예: <DataDir>/<taskID>）. 초보자 참고: 이 파일 산출물은 엔진이 PostgreSQL 자산 그래프와 탐색 그래프에 남기는 기록과 함께 작업 결과를 이룹니다.", Example: "/app/data", Source: "runtime"},
}

// withGlobalVars는 전역 런타임 변수를 에이전트 자신의 목록 뒤에 붙입니다.
// 그래서 검사, 화면의 변수 목록, 미리보기가 {{.Now}} 등을 알아봅니다.
// 전역 이름과 겹치는 저장된 목록 항목은 버립니다. 전역
// 런타임 변수가 기준입니다(그리기 경로가 실제로 푸는 값). 그리고
// 돌려주는 목록의 이름을 유일하게 해, 화면이 같은 키를 두 번 보지 않게 합니다.
func withGlobalVars(vars []db.PromptVar) []db.PromptVar {
	globalNames := make(map[string]bool, len(globalPromptVars))
	for _, g := range globalPromptVars {
		globalNames[g.Name] = true
	}
	out := make([]db.PromptVar, 0, len(vars)+len(globalPromptVars))
	for _, v := range vars {
		if globalNames[v.Name] {
			continue // 기준이 되는 전역 런타임 변수가 가립니다.
		}
		out = append(out, v)
	}
	return append(out, globalPromptVars...)
}

// validateTemplate는 템플릿을 파싱하고, 목록에 없는 {{.Var}}를 거절합니다.
// 맞으면 빈 문자열, 아니면 오류 글을 돌려줍니다.
func validateTemplate(tmpl string, catalog []db.PromptVar) string {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "템플릿 구문 오류: " + err.Error()
	}
	allowed := map[string]bool{}
	for _, v := range catalog {
		allowed[v.Name] = true
	}
	for _, name := range templateFields(t) {
		if !allowed[name] {
			return "변수 {{." + name + "}} 이(가) 해당 agent 허용 목록에 없습니다"
		}
	}
	return ""
}

// renderPrompt는 예시 값으로 그립니다(목록의 예시를 sample이 덮음).
func renderPrompt(tmpl string, catalog []db.PromptVar, sample map[string]string) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	data := map[string]any{}
	for _, v := range catalog {
		data[v.Name] = v.Example
	}
	for k, val := range sample {
		data[k] = val
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// templateFields는 템플릿이 가리키는 맨 위 {{.X}} 필드 이름을
// 중복 없이 돌려줍니다(목록 허용 목록과 맞추는 검사에 씀).
func templateFields(t *template.Template) []string {
	seen := map[string]bool{}
	var out []string
	collect := func(p *parse.PipeNode) {
		if p == nil {
			return
		}
		for _, cmd := range p.Cmds {
			for _, arg := range cmd.Args {
				if f, ok := arg.(*parse.FieldNode); ok && len(f.Ident) > 0 && !seen[f.Ident[0]] {
					seen[f.Ident[0]] = true
					out = append(out, f.Ident[0])
				}
			}
		}
	}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch x := n.(type) {
		case *parse.ListNode:
			if x == nil {
				return
			}
			for _, c := range x.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			collect(x.Pipe)
		case *parse.IfNode:
			collect(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.RangeNode:
			collect(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.WithNode:
			collect(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		}
	}
	if t.Tree != nil {
		walk(t.Tree.Root)
	}
	return out
}
