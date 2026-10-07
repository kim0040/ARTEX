package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/artex/sidequestion"
)

const (
	maxConversationRequestBytes  = 64 << 10
	maxConversationAgentKeyRunes = 120
	maxConversationTitleRunes    = 200
)

func decodeConversationRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxConversationRequestBytes)
	if err := decode(r, value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "요청 본문이 너무 큽니다")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return false
	}
	return true
}

// ---------- 대화(채팅 화면) ----------
//
// 대화는 에이전트 키에 묶인 ChatGPT 스타일 스레드입니다.
// 탐색 그래프와는 별개입니다. 턴은 ChatAgent에서 돌고, 단계는
// conversation_activities에 남습니다. 브라우저는 ?since=커서로 폴링해 실시간 갱신을 받습니다
// (대화마다 SSE 방송은 필요 없습니다).
// 초보용: 이 대화는 탐색 그래프의 작업과 별개입니다. 화면이 폴링으로 턴을 받아 그립니다.

type conversationListItem struct {
	*db.Conversation
	Running bool `json:"running"`
}

func (s *Server) pgListConversations(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	cs, err := pg.ListConversations()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	items := make([]conversationListItem, 0, len(cs))
	s.chatMu.Lock()
	for _, c := range cs {
		items = append(items, conversationListItem{Conversation: c, Running: s.chatBusy[s.convBusyKey(c.ID)]})
	}
	s.chatMu.Unlock()
	writeJSON(w, 200, map[string]any{"conversations": items})
}

func (s *Server) pgCreateConversation(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		AgentKey     string `json:"agent_key"`
		Title        string `json:"title"`
		LLMProfileID *int64 `json:"llm_profile_id"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	req.AgentKey = strings.TrimSpace(req.AgentKey)
	if req.AgentKey == "" {
		writeErr(w, 400, "agent_key는 비울 수 없습니다")
		return
	}
	if utf8.RuneCountInString(req.AgentKey) > maxConversationAgentKeyRunes {
		writeErr(w, 400, fmt.Sprintf("agent_key는 최대 %d자입니다", maxConversationAgentKeyRunes))
		return
	}
	a, err := pg.GetAgentByKey(req.AgentKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if a == nil {
		writeErr(w, 404, "에이전트가 없습니다")
		return
	}
	if req.LLMProfileID != nil {
		if _, ok := s.loadProfileConfig(*req.LLMProfileID); !ok {
			writeErr(w, 400, "지정한 LLM 설정이 없거나 API Key가 설정되지 않았습니다")
			return
		}
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "새 대화"
	}
	if utf8.RuneCountInString(title) > maxConversationTitleRunes {
		writeErr(w, 400, fmt.Sprintf("제목은 최대 %d자입니다", maxConversationTitleRunes))
		return
	}
	c, err := pg.CreateConversation(req.AgentKey, title, req.LLMProfileID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) pgUpdateConversation(w http.ResponseWriter, r *http.Request) {
	pg, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	var req struct {
		LLMProfileID *int64 `json:"llm_profile_id"` // null이면 이 대화의 LLM 설정 고정을 지웁니다.
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	if req.LLMProfileID != nil {
		if _, ok := s.loadProfileConfig(*req.LLMProfileID); !ok {
			writeErr(w, 400, "지정한 LLM 설정이 없거나 API Key가 설정되지 않았습니다")
			return
		}
	}
	if err := pg.UpdateConversationProfile(c.ID, req.LLMProfileID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// convByID는 경로의 {id}를 대화로 찾습니다. 없으면 404.
func (s *Server) convByID(w http.ResponseWriter, r *http.Request) (*db.DB, *db.Conversation, bool) {
	pg := s.pg(w)
	if pg == nil {
		return nil, nil, false
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "대화 id가 올바르지 않습니다")
		return nil, nil, false
	}
	c, err := pg.GetConversation(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, nil, false
	}
	if c == nil {
		writeErr(w, 404, "대화를 찾을 수 없습니다")
		return nil, nil, false
	}
	return pg, c, true
}

func (s *Server) pgRenameConversation(w http.ResponseWriter, r *http.Request) {
	pg, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	var req struct {
		Title  *string `json:"title"`
		Pinned *bool   `json:"pinned"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	if req.Title == nil && req.Pinned == nil {
		writeErr(w, 400, "title 또는 pinned 중 적어도 하나는 필요합니다")
		return
	}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			writeErr(w, 400, "제목은 비울 수 없습니다")
			return
		}
		if utf8.RuneCountInString(title) > maxConversationTitleRunes {
			writeErr(w, 400, fmt.Sprintf("제목은 최대 %d자입니다", maxConversationTitleRunes))
			return
		}
		req.Title = &title
	}
	updated, err := pg.UpdateConversation(c.ID, db.ConversationPatch{Title: req.Title, Pinned: req.Pinned})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if updated == nil {
		writeErr(w, 404, "대화를 찾을 수 없습니다")
		return
	}
	writeJSON(w, 200, updated)
}

func (s *Server) pgDeleteConversation(w http.ResponseWriter, r *http.Request) {
	pg, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	s.cancelConversation(c.ID)
	s.cancelSideWhere(func(p sidequestion.Parent) bool { return p.ConversationID == c.ID })
	if err := pg.DeleteConversation(c.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": c.ID})
}

const maxConversationDeleteBatch = 100

type conversationDeleteItem struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) cancelConversation(id int64) {
	busyKey := s.convBusyKey(id)
	s.chatMu.Lock()
	cancel := s.chatCancel[busyKey]
	s.chatMu.Unlock()
	if cancel != nil {
		cancel(agent.AbortChatStoppedByUser)
	}
}

// pgDeleteConversationsBatch는 고른 대화를 최대 100개, 한 번의
// DB 문으로 지웁니다. 없는 id는 항목마다 알려, 낡은 목록이
// 실제로 지운 것을 숨기지 않게 합니다.
func (s *Server) pgDeleteConversationsBatch(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var request struct {
		IDs []int64 `json:"ids"`
	}
	if !decodeConversationRequest(w, r, &request) {
		return
	}
	ids := make([]int64, 0, len(request.IDs))
	seen := make(map[int64]struct{}, len(request.IDs))
	for _, id := range request.IDs {
		if id <= 0 {
			writeErr(w, http.StatusBadRequest, "대화 id가 유효하지 않습니다")
			return
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > maxConversationDeleteBatch {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("ids 개수는 1-%d여야 합니다", maxConversationDeleteBatch))
		return
	}
	for _, id := range ids {
		s.cancelConversation(id)
		s.cancelSideWhere(func(p sidequestion.Parent) bool { return p.ConversationID == id })
	}
	deleted, err := pg.DeleteConversations(ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	deletedSet := make(map[int64]struct{}, len(deleted))
	for _, id := range deleted {
		deletedSet[id] = struct{}{}
	}
	items := make([]conversationDeleteItem, 0, len(ids))
	for _, id := range ids {
		_, ok := deletedSet[id]
		item := conversationDeleteItem{ID: id, OK: ok}
		if !ok {
			item.Error = "대화를 찾을 수 없습니다"
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) convBusyKey(id int64) string { return "conv-" + strconv.FormatInt(id, 10) }

// pgConversationMessages는 ?since=커서 이후의 단계와, 턴이
// 아직 도는지(클라이언트가 폴링을 계속할지)를 돌려줍니다. 항목 모양은 작업
// 활동 스트림과 같아서, 화면의 대화 기록 그리기를 그대로 씁니다.
func (s *Server) pgConversationMessages(w http.ResponseWriter, r *http.Request) {
	pg, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 200)
	s.chatMu.Lock()
	running := s.chatBusy[s.convBusyKey(c.ID)]
	s.chatMu.Unlock()

	// 증분 꼬리: ?since=N은 id N 이후 단계를 오름차순으로 줍니다. 실시간
	// 폴링과 보낸 직후 가져오기에 씁니다. 한도가 넉넉해 몰려도 안 버립니다.
	if sv := q.Get("since"); sv != "" {
		since, _ := strconv.ParseInt(sv, 10, 64)
		items, cursor, err := pg.ConvActivityList(c.ID, since, max(limit, 1000))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"items": activityDTOs(items), "cursor": cursor, "running": running, "hasMore": false})
		return
	}

	// 이전 기록 페이지(거꾸로): ?since가 없으면 최신 페이지, ?before=N이면
	// id N 앞에서 끝나는 더 옛 단계 페이지입니다. hasMore로 클라이언트가
	// 스레드 맨 위에 닿으면 더 옛 기록을 그만 읽게 합니다.
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	items, hasMore, err := pg.ConvActivityPage(c.ID, before, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	cursor := before
	if n := len(items); n > 0 {
		cursor = items[n-1].ID
	}
	writeJSON(w, 200, map[string]any{"items": activityDTOs(items), "cursor": cursor, "running": running, "hasMore": hasMore})
}

// pgConversationMsgDetail은 단계 하나의 상세 본문을 필요할 때 돌려줍니다.
func (s *Server) pgConversationMsgDetail(w http.ResponseWriter, r *http.Request) {
	pg, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	seq, ok := pathInt(r, "seq")
	if !ok {
		writeErr(w, 400, "순번이 올바르지 않습니다")
		return
	}
	detail, err := pg.ConvActivityDetail(c.ID, seq)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"detail": detail})
}

// pgStopConversation은 대화 하나의 진행 중 실행을 끊습니다(수동 정지 —
// 채팅 화면의 실행/정지 버튼). 이 세션의 에이전트 실행만 취소합니다.
// P3 트리거 대기열은 그대로라, 그 에이전트의 다음 대기 울림은 여전히 시작합니다.
func (s *Server) pgStopConversation(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	cancel := s.chatCancel[busyKey]
	s.chatMu.Unlock()
	if cancel == nil {
		writeJSON(w, 200, map[string]any{"status": "idle"}) // 도는 것이 없습니다.
		return
	}
	cancel(agent.AbortChatStoppedByUser)
	writeJSON(w, 200, map[string]any{"status": "stopping"})
}

// pgSendConversationMessage는 사람 턴을 저장한 뒤 에이전트를
// 백그라운드에서 돌립니다(단계는 conversation_activities로 흐르고, 클라이언트가 폴링). 대화마다
// 진행 중 턴은 하나입니다.
func (s *Server) pgSendConversationMessage(w http.ResponseWriter, r *http.Request) {
	pg, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	var req struct {
		Message     string           `json:"message"`
		Attachments []chatAttachment `json:"attachments,omitempty"` // 방식 1로 올린 파일(경로는 세션 작업 디렉터리에 상대적)
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" && len(req.Attachments) == 0 {
		writeErr(w, 400, "메시지는 비울 수 없습니다")
		return
	}
	agentMessage, ok := s.prepareChatMentionMessage(w, msg)
	if !ok {
		return
	}
	// 백그라운드 실행과 같은 고르기입니다. 에이전트 바인딩과 이
	// 대화가 고른 설정을 전역보다 먼저 봅니다. 그래서
	// 올바른 LLM을 고른 대화가, 전역 설정이 없다는 이유만으로
	// 거절되지 않습니다.
	if s.resolveChatAgent(c) == nil {
		writeErr(w, 503, s.chatUnavailableReason())
		return
	}

	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	if s.chatBusy[busyKey] {
		s.chatMu.Unlock()
		writeErr(w, 409, "이 세션은 이전 메시지를 처리 중입니다. 잠시 기다려 주세요")
		return
	}
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()

	// 사람 턴을 저장하고 맨 위로 올립니다. 첫 메시지로 스레드 제목을 자동으로 짓습니다.
	// Worker는 에이전트 키입니다("user"가 아님). 대화 기록이 한 줄로 남고
	// (워커 칩 없음). kind가 user라 사람 말풍선으로 오른쪽에 붙습니다.
	// 첨부가 있으면 Detail에 {text, attachments} JSON이 들어가 대화 기록이
	// 첨부 카드를 그립니다. 없으면 Detail이 전문입니다(Summary는 잘리고,
	// 대화 기록은 Detail을 나중에 읽어 잘리지 않은 메시지를 보여 줌).
	ua := userActivityWithAttachments(c.AgentKey, msg, req.Attachments)
	ua.Summary = firstLine(msg, 200)
	if ua.Detail == "" {
		ua.Detail = msg
	}
	if _, err := pg.AppendConvActivity(c.ID, ua); err != nil {
		log.Printf("[conv %d] append user msg failed: %v", c.ID, err)
	}
	if c.Title == "" || c.Title == "새 대화" {
		title := firstLine(msg, 40)
		if title == "" {
			title = "첨부 메시지"
		}
		_ = pg.RenameConversation(c.ID, title)
	}
	_ = pg.TouchConversation(c.ID)

	// 올린 파일의 절대 경로를 붙여, ChatAgent가 Read/Bash로 열게 합니다.
	// baseDir는 에이전트의 세션별 현재 디렉터리입니다(<workDir>/sessions/
	// conv-<id>/). chatUpload가 두는 곳, agent/chat.go의 sessionWorkDir와 같습니다.
	// busyKey == convBusyKey(c.ID) == "conv-<id>" == 그 세션 id.
	baseDir := filepath.Join(s.m.dir, "sessions", busyKey)
	s.runConversation(c, composeAgentMessage(agentMessage, req.Attachments, baseDir), busyKey, msg)
	writeJSON(w, 202, map[string]any{"status": "accepted"})
}

// runConversation은 대화에서 에이전트 턴을 하나, 백그라운드 고루틴으로 돌립니다
// (대화는 서로 독립이라 병렬). 단계는
// conversation_activities로 흐릅니다. 채팅 HTTP 처리기와 P3 스케줄러가 같이 씁니다.
// busyKey는 실행이 끝나면 지웁니다(진행 중 표시, 실패해도 흐름은 계속).
func (s *Server) runConversation(c *db.Conversation, msg, busyKey string, userMessage ...string) {
	ctx, cancel := s.conversationRunContext(c.ID, busyKey)
	// 이 필드는 사람 메시지 처리기만, 첨부 목록을 붙이기 전에 넣습니다.
	// 스케줄러/재시험 프롬프트를 사용자 말로 표시하면 안 됩니다.
	if len(userMessage) == 1 {
		ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: intercept.BackgroundUserMessage, Text: userMessage[0]})
	}
	go s.runConversationTurn(ctx, cancel, c, msg, busyKey)
}

// runConversationSync는 대화에서 에이전트 턴을 하나 돌리고, 끝날 때까지
// 막습니다(busyKey를 지움). runConversation은 채팅의 보내고 잊어버리기 경로에서
// 이것을 고루틴으로 감쌉니다. P3 트리거 대기열은 직접 불러,
// 같은 에이전트의 다음 대기 울림 전에 끝나길 기다립니다.
func (s *Server) runConversationSync(c *db.Conversation, msg, busyKey string) {
	ctx, cancel := s.conversationRunContext(c.ID, busyKey)
	s.runConversationTurn(ctx, cancel, c, msg, busyKey)
}

// 202를 돌려주기 전에 취소를 등록합니다. 바로 오는 정지/삭제가
// 아직 시작 안 한 백그라운드 고루틴을 놓치지 않게 합니다.
func (s *Server) conversationRunContext(id int64, busyKey string) (context.Context, context.CancelCauseFunc) {
	// 실행마다 취소 가능한 컨텍스트입니다. 수동 정지(pgStopConversation)가
	// 이 세션만 끊게 합니다. chatMu 아래에 등록해 정지 처리기가 찾게 합니다.
	ctx, cancel := context.WithCancelCause(intercept.WithConvID(s.ctx, id))
	s.chatMu.Lock()
	s.chatCancel[busyKey] = cancel
	s.chatMu.Unlock()
	return ctx, cancel
}

func (s *Server) runConversationTurn(ctx context.Context, cancel context.CancelCauseFunc, c *db.Conversation, msg, busyKey string) {
	defer func() {
		cancel(agent.AbortChatTurnFinished)
		s.chatMu.Lock()
		delete(s.chatBusy, busyKey)
		delete(s.chatCancel, busyKey)
		s.chatMu.Unlock()
	}()
	// 옛 재시험은 첫 턴만 실행합니다. 이어지는 대화
	// 턴은 봉인된 결과를 설명할 수 있고, 결과 도구는 덮어쓰기를 거절합니다.
	finishStatus, finishReason := "failed", "재테스트를 시작하지 못했습니다"
	if c.AgentKey == db.FindingRetestAgentKey {
		// 실행 취소를 빼고 읽습니다. 바로 멈춰도 대기를 봉인하게 하려고요.
		r, err := s.m.pg.FindingRetestForConversation(context.Background(), c.ID)
		if err != nil {
			// 아래 봉인 defer는 r.ID가 필요한데, 여기서는 없습니다. 대신
			// 대화로 봉인합니다. 안 그러면 행이 영원히 pending으로 남습니다.
			log.Printf("[conv %d] load retest: %v", c.ID, err)
			if err := s.m.pg.FailPendingRetestForConversation(c.ID, "재테스트 상태를 읽지 못했습니다. 다시 시작해 주세요"); err != nil {
				log.Printf("[conv %d] seal retest: %v", c.ID, err)
			}
			return
		}
		if r != nil && r.Status == "pending" {
			defer func() {
				if ctx.Err() != nil {
					finishStatus, finishReason = "stopped", "재테스트가 중지되었거나 서비스가 종료되었습니다"
				}
				s.finishRetest(r.ID, finishStatus, finishReason)
			}()
			if ctx.Err() != nil {
				return
			}
			started, err := s.m.pg.StartFindingRetest(ctx, r.ID)
			if err != nil {
				finishReason = err.Error()
				return
			}
			if !started {
				return
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	// 순서: 대화 에이전트 자신의 바인딩 → 이 대화의 고정 → 전역.
	ca := s.resolveChatAgent(c)
	if ca == nil {
		finishReason = s.chatUnavailableReason()
		return
	}
	pg := s.m.pg
	maxTurns := s.agentMaxTurns(c.AgentKey)
	maxDuration := time.Duration(s.agentRunSeconds(c.AgentKey)) * time.Second
	webSearch := false
	if a, err := pg.GetAgentByKey(c.AgentKey); err == nil && a != nil {
		webSearch = a.WebSearch
	}
	sessionID := s.convBusyKey(c.ID) // "conv-<id>" 대화 기록 세션
	emit := func(rec db.Activity) {
		if _, err := pg.AppendConvActivity(c.ID, rec); err != nil {
			log.Printf("[conv %d] append activity failed: %v", c.ID, err)
		}
	}
	// 수동 정지에서는 ctx 가 취소된다. Chat 은 이미 깨끗한 "수동으로 중지됨"을 낸다
	// 그 단계를 이미 냈으므로, 날것 오류 항목은 건너뜁니다. 진짜 실패만 드러냅니다.
	if _, err := ca.Chat(ctx, c.AgentKey, sessionID, msg, maxTurns, maxDuration, webSearch, emit); err != nil {
		finishReason = err.Error()
		if ctx.Err() == nil {
			_, _ = pg.AppendConvActivity(c.ID, db.Activity{Worker: c.AgentKey, Kind: "text", IsError: true,
				Summary: "(오류: " + err.Error() + "）", Detail: err.Error()})
		}
	} else {
		finishStatus, finishReason = "completed", ""
	}
	_ = pg.TouchConversation(c.ID)
}

// triggerBehavior 는 에이전트가 캐시한 P3 트리거 후처리 전략이다 (다음을 보라
// agents 표의 trigger_* 열). StartTriggeredRun이 울림마다 한 번 읽어,
// pump가 queueMu를 잡은 채 DB를 건드리지 않게 합니다.
type triggerBehavior struct {
	runMode     string // serial=하나씩 | parallel=동시에
	mergeMode   string // by_task | all | none (serial 에서 사용; parallel 은 무시하고, 각 항목이 세션을 하나씩 가진다)
	maxParallel int    // parallel 용 에이전트별 동시 실행 상한; <=0=제한 없음
}

// readTriggerBehavior 는 에이전트의 전략을 불러오고, 없으면 안전한 기본값으로 돌아간다
// (serial / all / 5). 오류가 나거나 모르는 enum이면 그 기본값입니다.
func (s *Server) readTriggerBehavior(agentKey string) triggerBehavior {
	b := triggerBehavior{runMode: "serial", mergeMode: "all", maxParallel: 5}
	if s.m.pg == nil {
		return b
	}
	a, err := s.m.pg.GetAgentByKey(agentKey)
	if err != nil || a == nil {
		return b
	}
	if a.TriggerRunMode == "parallel" {
		b.runMode = "parallel"
	}
	switch a.TriggerMergeMode {
	case "by_task", "all", "none":
		b.mergeMode = a.TriggerMergeMode
	}
	b.maxParallel = a.TriggerMaxParallel
	return b
}

// StartTriggeredRun은 agentKey의 P3 트리거 울림을 대기열에 넣고 펌프를 돌립니다.
// 에이전트의 전략이 동시성과 병합을 정한다: serial → 한 번에 하나씩 실행한다 (선택적으로
// 작업별 / 전체 / 안 합침으로 합칠 수 있음). parallel이면 울림마다 동시에
// 자기 대화를 돌립니다. 상한은 trigger_max_parallel입니다. 다른 에이전트는 항상 동시에 돕니다.
// 초보용: 스케줄러가 울린 트리거를 그 에이전트 대기열에 넣고, 엔진이 대화를 돌리게 합니다.
func (s *Server) StartTriggeredRun(agentKey, title, message string, taskID int64, mergeable bool, taskDesc, taskGoal string) {
	if s.m.pg == nil || s.chatAgentRef() == nil {
		return
	}
	cfg := s.readTriggerBehavior(agentKey) // 잠금 전에 DB를 읽습니다(queueMu 안에서는 절대 아님).
	s.queueMu.Lock()
	s.triggerCfg[agentKey] = cfg
	s.triggerQ[agentKey] = append(s.triggerQ[agentKey], triggeredRun{agentKey: agentKey, title: title, message: message, taskID: taskID, taskDesc: taskDesc, taskGoal: taskGoal, mergeable: mergeable})
	s.pumpLocked(agentKey)
	s.queueMu.Unlock()
}

// pumpLocked는 한 에이전트의 대기 울림을 동시성 상한까지 띄운 뒤
// 돌아옵니다. serial이면 상한 1, parallel이면 상한은 maxParallel(0 이하면 무제한). 띄운
// 실행은 끝나면(runAndPump) 다시 펌프해 빈 자리를 채웁니다. 호출자가
// queueMu를 잡고 있습니다.
func (s *Server) pumpLocked(agentKey string) {
	if s.ctx.Err() != nil {
		return
	}
	cfg := s.triggerCfg[agentKey]
	limit := 1
	if cfg.runMode == "parallel" {
		if cfg.maxParallel <= 0 {
			limit = math.MaxInt
		} else {
			limit = cfg.maxParallel
		}
	}
	for s.triggerActive[agentKey] < limit && len(s.triggerQ[agentKey]) > 0 {
		item := s.nextTriggerRun(agentKey, cfg)
		s.triggerActive[agentKey]++
		go s.runAndPump(agentKey, item)
	}
}

// runAndPump는 울림 하나를 끝까지 돌린 뒤 활성 수를 줄이고
// 다시 펌프해 빈 자리를 채웁니다(완전히 쉬면 그 에이전트 맵을 치움).
func (s *Server) runAndPump(agentKey string, item triggeredRun) {
	s.runTriggeredRun(item)
	s.queueMu.Lock()
	s.triggerActive[agentKey]--
	if s.triggerActive[agentKey] <= 0 && len(s.triggerQ[agentKey]) == 0 {
		delete(s.triggerActive, agentKey)
		delete(s.triggerQ, agentKey)
		delete(s.triggerCfg, agentKey)
	} else {
		s.pumpLocked(agentKey)
	}
	s.queueMu.Unlock()
}

// nextTriggerRun은 합치기 모드에 따라 에이전트 대기열에서 다음 실행을 꺼냅니다.
//
//	parallel / none → 머리 항목 하나, 합치지 않음
//	by_task         → 머리와, 같은 작업의 합칠 수 있는 대기 울림을 하나로
//	all             → 대기열 전체를 실행 하나로 합침(작업/종류와 무관)
//
// 호출자가 queueMu를 잡고 있습니다.
func (s *Server) nextTriggerRun(agentKey string, cfg triggerBehavior) triggeredRun {
	q := s.triggerQ[agentKey]
	if cfg.runMode == "parallel" || cfg.mergeMode == "none" {
		s.triggerQ[agentKey] = q[1:]
		return q[0]
	}
	if cfg.mergeMode == "all" {
		s.triggerQ[agentKey] = q[:0:0]
		return mergeAllRuns(q)
	}
	// by_task: 머리와 같은 작업의 합칠 수 있는 울림.
	head := q[0]
	if !head.mergeable || head.taskID == 0 {
		s.triggerQ[agentKey] = q[1:]
		return head
	}
	group := []triggeredRun{head}
	rest := q[:0:0] // 안 맞는 항목은 순서를 유지합니다.
	for _, it := range q[1:] {
		if it.mergeable && it.taskID == head.taskID {
			group = append(group, it)
		} else {
			rest = append(rest, it)
		}
	}
	s.triggerQ[agentKey] = rest
	return mergeTriggeredRuns(group)
}

// taskContextHeader는 작업의 설명/목표를 한 번 그립니다. 같은 작업의 울림이
// 이 블록을 나눠 쓰므로, 스케줄러가 이벤트마다 반복하지 않습니다(긴 작업 목표가
// 울림 수만큼 반복되던 것이 부풀림의 대부분이었습니다). 간격/없음 트리거는 ""입니다
// (taskID==0, 작업 맥락 없음). desc/goal은 잘라, 한 부만 있어도
// 길이가 한없이 커지지 않게 합니다.
func taskContextHeader(taskID int64, desc, goal string) string {
	if desc == "" && goal == "" {
		// 보여줄 작업 맥락이 없습니다(간격/없음 울림, 또는 합친 실행이 이미
		// 작업별 머리를 안에 넣고 이 필드를 비운 경우).
		return ""
	}
	if goal != "" {
		return fmt.Sprintf("【작업 #%d %s (목표: %s)】", taskID, trunc(desc, 200), trunc(goal, 500))
	}
	return fmt.Sprintf("【작업 #%d %s】", taskID, trunc(desc, 200))
}

// finalTriggerMessage는 합치지 않은 울림 하나에서 에이전트에게 실제로 보낼 글을 만듭니다.
// 작업 맥락 머리(한 번) 뒤에 이벤트
// 본문입니다. 합친 실행은 작업별 머리를 안에 넣고 taskDesc/taskGoal을 비우므로,
// 그 메시지는 그대로 돌려줍니다.
func finalTriggerMessage(item triggeredRun) string {
	if h := taskContextHeader(item.taskID, item.taskDesc, item.taskGoal); h != "" {
		return h + "\n" + item.message
	}
	return item.message
}

// mergeTriggeredRuns는 같은 작업의 이벤트 울림 여러 개를 실행 하나로 접습니다.
// 공유 작업 맥락 머리는 한 번만 쓰고, 그다음 각 울림의 이벤트 본문입니다. 에이전트가
// 그 작업의 몰림을 대화 하나에서 처리하고, (길 수 있는)
// 작업 설명/목표를 이벤트마다 반복하지 않습니다.
func mergeTriggeredRuns(items []triggeredRun) triggeredRun {
	if len(items) == 1 {
		return items[0]
	}
	first := items[0]
	var b strings.Builder
	fmt.Fprintf(&b, "【이 세션은 작업 #%d의 트리거 이벤트 %d건을 합쳤습니다. 함께 처리하세요】\n", first.taskID, len(items))
	if h := taskContextHeader(first.taskID, first.taskDesc, first.taskGoal); h != "" {
		fmt.Fprintf(&b, "%s\n", h) // 같은 작업이라 작업 맥락은 한 번만 나옵니다.
	}
	for i, it := range items {
		fmt.Fprintf(&b, "\n── 트리거 %d ──\n%s\n", i+1, it.message)
	}
	return triggeredRun{
		agentKey:  first.agentKey,
		title:     fmt.Sprintf("합친 트리거 · task#%d · %d건", first.taskID, len(items)),
		message:   b.String(),
		taskID:    first.taskID,
		mergeable: true,
		// taskDesc/taskGoal은 비웁니다. 머리는 이미 위에 넣었습니다.
	}
}

// mergeAllRuns는 대기 중인 몰림 전체를 실행 하나로 접습니다(merge_mode가 all).
// 종류와 무관합니다. 이벤트는 작업별로 묶습니다(작업은 처음 나타난 순서,
// 작업 안 이벤트는 원래 순서). 그래서 각 작업의 맥락 머리와
// 길 수 있는 설명/목표는, 다른 작업의 울림이 대기열에서 섞여도
// 정확히 한 번만 씁니다. 작업 사이의 시간 순서는 지키지 않습니다
// (이벤트 본문에 시각이 없어, 작업별로 묶는 편이 에이전트에게 더 읽힙니다).
func mergeAllRuns(items []triggeredRun) triggeredRun {
	if len(items) == 1 {
		return items[0]
	}
	first := items[0]
	order := []int64{}
	groups := map[int64][]triggeredRun{}
	for _, it := range items {
		if _, seen := groups[it.taskID]; !seen {
			order = append(order, it.taskID)
		}
		groups[it.taskID] = append(groups[it.taskID], it)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【이 세션은 대기열의 트리거 이벤트 %d건(작업 총 %d개)을 합쳤습니다. 함께 처리하세요】\n", len(items), len(order))
	seq := 0
	for _, tid := range order {
		g := groups[tid]
		if h := taskContextHeader(tid, g[0].taskDesc, g[0].taskGoal); h != "" {
			fmt.Fprintf(&b, "\n%s\n", h) // 같은 작업이면 맥락은 한 번만 나옵니다. 섞여 있어도요.
		}
		for _, it := range g {
			seq++
			fmt.Fprintf(&b, "\n── 트리거 %d(task#%d)──\n%s\n", seq, tid, it.message)
		}
	}
	return triggeredRun{
		agentKey:  first.agentKey,
		title:     fmt.Sprintf("합친 트리거 · 전체 · %d건", len(items)),
		message:   b.String(),
		taskID:    first.taskID,
		mergeable: true,
		// taskDesc/taskGoal은 비웁니다. 작업별 머리는 이미 위에 넣었습니다.
	}
}

// runTriggeredRun은 대기 울림 하나의 대화를 만들고, 사람
// 턴을 기록한 뒤 동기적으로 돌립니다(끝날 때까지 막음). recover가
// 패닉이 빼기 루프를 죽이고 에이전트 대기열을 막지 않게 합니다.
func (s *Server) runTriggeredRun(item triggeredRun) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[trigger] run for %s panicked: %v", item.agentKey, r)
		}
	}()
	pg := s.m.pg
	c, err := pg.CreateConversation(item.agentKey, firstLine(item.title, 60), nil)
	if err != nil {
		log.Printf("[trigger] create conversation for %s failed: %v", item.agentKey, err)
		return
	}
	msg := finalTriggerMessage(item) // 합치지 않은 울림에는 작업 맥락 머리를 앞에 붙입니다.
	if _, err := pg.AppendConvActivity(c.ID, db.Activity{Worker: item.agentKey, Kind: "user", Summary: firstLine(msg, 200), Detail: msg}); err != nil {
		log.Printf("[trigger] append msg failed: %v", err)
	}
	busyKey := s.convBusyKey(c.ID)
	s.chatMu.Lock()
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()
	s.runConversationSync(c, msg, busyKey)
}

// firstLine은 한 줄로 자른 미리보기를 돌려줍니다(요약과 같이 씀).
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}
