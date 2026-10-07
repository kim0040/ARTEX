package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

const maxWorkerMessageBytes = 64 << 10

func validWorkerMessageRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

// sendWorkerMessage는 일시정지된 워커 의도를, 사람이 쓴 말로 이어 갑니다.
// 그 말은 다음 턴의 입력으로 들어갑니다. 워커가 보통 재개할 때 쓰는
// 기록에서 이어 가기(ExecuteWithMessage)와 같은 경로입니다.
// 그래서 따로 프로토콜을 만들지 않고, 있는 일시정지/재개를 재사용합니다.
// 실행은 워커 풀 밖의 전용 고루틴입니다(runDetachedIntent).
// 풀 자리가 모두 바빠도 말이 바로 집힙니다. 메인 에이전트 채팅 처리기가
// 실행을 직접 시작하는 것과 같습니다. 메모리에만 있습니다. 프로세스가
// 재시작하면 그 말 없이 기록부터 의도를 다시 돌립니다. 이 드문
// 끊었다가 이어 가는 동작에는 그 정도로 충분합니다.
// 초보용: 화면에서 멈춘 워커 의도에 사람이 쓴 말을 넣어, 엔진이 그 의도를 이어서 돌립니다.
func (s *Server) sendWorkerMessage(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "작업을 찾을 수 없습니다")
		return
	}
	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if err != nil || iid <= 0 {
		writeErr(w, http.StatusBadRequest, "의도 id가 올바르지 않습니다")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerMessageBytes)
	var req struct {
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "요청 본문이 너무 큽니다")
			return
		}
		writeErr(w, http.StatusBadRequest, "JSON 이 올바르지 않습니다: "+err.Error())
		return
	}
	message := strings.TrimSpace(req.Message)
	requestID := strings.TrimSpace(req.RequestID)
	if message == "" {
		writeErr(w, http.StatusBadRequest, "메시지는 비울 수 없습니다")
		return
	}
	if len([]rune(message)) > 4000 {
		writeErr(w, http.StatusBadRequest, "메시지는 4000자를 넘을 수 없습니다")
		return
	}
	if !validWorkerMessageRequestID(requestID) {
		writeErr(w, http.StatusBadRequest, "request_id는 1–128자의 영문, 숫자, -, _, ., : 만 쓸 수 있습니다")
		return
	}

	// 돌릴 수 없는 작업 생명주기는 앞에서 거절합니다. 호출자가 이유를 보게 하고,
	// 조용히 아무 일도 안 하는 일을 막습니다. 의도 자체는 일시정지여야 합니다. 화면 흐름은
	// 먼저 끼어들어 멈추고(일시정지), 그다음 보냅니다.
	if s.engine.IsDeleting(t.ID) {
		writeErr(w, http.StatusConflict, "작업을 삭제하는 중이라 워커에 메시지를 보낼 수 없습니다")
		return
	}
	lifecycle := t.lifecycleSnapshot()
	switch {
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		writeErr(w, http.StatusConflict, "작업이 일시정지되어 있습니다. 재개한 뒤 워커에 메시지를 보내세요")
		return
	case lifecycle.Queued:
		writeErr(w, http.StatusConflict, "대기 중인 작업에는 워커 메시지를 보낼 수 없습니다")
		return
	case isTerminalStatus(lifecycle.Status):
		writeErr(w, http.StatusConflict, "이미 끝난 작업에는 워커 메시지를 보낼 수 없습니다")
		return
	case s.engine.isSettling(t.ID):
		writeErr(w, http.StatusConflict, "작업이 마무리 중이라 워커에 메시지를 보낼 수 없습니다")
		return
	}

	node, err := t.Store.GetNode(iid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if node == nil {
		if inherited, sourceErr := t.Store.GetNodeWithSources(iid); sourceErr == nil && inherited != nil && inherited.Inherited {
			writeErr(w, http.StatusConflict, "상속된 의도는 읽기 전용이라 워커 메시지를 보낼 수 없습니다")
			return
		}
		writeErr(w, http.StatusNotFound, "의도를 찾을 수 없습니다")
		return
	}
	if node.Kind != db.KindIntent {
		writeErr(w, http.StatusConflict, "이 노드는 의도가 아닙니다")
		return
	}
	if node.State != "paused" {
		writeErr(w, http.StatusConflict, "일시정지된 워커만 메시지를 받을 수 있습니다. 먼저 일시정지하세요")
		return
	}
	agentMessage, ok := s.prepareChatMentionMessage(w, message)
	if !ok {
		return
	}

	// runDetachedIntent는 일시정지를 실행 중으로 바꾸고, 사용자 턴을 낸 뒤 전용 실행을 시작합니다.
	// 실행의 뿌리는 s.ctx입니다. 브라우저 연결이 끊겨도 실행이 허공에 남지 않게 하고,
	// 작업 일시정지/삭제/종료는 여전히 멈출 수 있습니다.
	if err := s.engine.runDetachedIntent(s.ctx, t, iid, requestID, message, agentMessage); err != nil {
		switch {
		case errors.Is(err, db.ErrIntentStateConflict):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         iid,
		"state":      "running",
		"accepted":   true,
		"request_id": requestID,
	})
}
