package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

const maxBatchControlIDs = 100

type taskControlResult struct {
	ID     string `json:"id"`
	Paused bool   `json:"paused"`
	Queued bool   `json:"queued"`
	Status string `json:"status"`
}

type intentControlResult struct {
	ID      int64             `json:"id"`
	State   string            `json:"state"`
	Deleted *db.IntentCleanup `json:"deleted,omitempty"`
}

// parsedTaskID는 요청한 일괄 id 하나와, 숫자로 읽혔는지를 담습니다.
// 잘못된 id도 버리지 않습니다. 응답이 그 이름을 말하게 하려고요.
type parsedTaskID struct {
	id    string
	valid bool
}

// normalizeBatchTaskIDs는 일괄 요청의 id를 다듬고, 정규화하고, 중복을 뺍니다.
// 호출자 순서는 유지합니다. 모든 일괄
// 엔드포인트가 같이 써서, 무엇이 중복인지 의견이 같습니다.
func normalizeBatchTaskIDs(raw []string) []parsedTaskID {
	seen := map[string]bool{}
	seenInvalid := map[string]bool{}
	taskIDs := make([]parsedTaskID, 0, len(raw))
	for _, item := range raw {
		trimmed := strings.TrimSpace(item)
		id, valid := canonicalTaskID(trimmed)
		if !valid {
			if seenInvalid[trimmed] {
				continue
			}
			seenInvalid[trimmed] = true
			taskIDs = append(taskIDs, parsedTaskID{id: trimmed})
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		taskIDs = append(taskIDs, parsedTaskID{id: id, valid: true})
	}
	return taskIDs
}

type batchControlItem struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Status string `json:"status,omitempty"`
	Queued bool   `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) resumeAdmissionMode(t *Task) string {
	if t == nil {
		return "resume"
	}
	lifecycle := t.lifecycleSnapshot()
	if lifecycle.QueueMode == "bootstrap" {
		return "bootstrap"
	}
	if lifecycle.FirstRunAt == 0 {
		goals, err := t.Store.ListByKind(db.KindGoal, 1)
		if err == nil && len(goals) == 0 {
			return "bootstrap"
		}
	}
	return "resume"
}

// applyTaskControl은 단일과 일괄 엔드포인트가 같이 씁니다. 작업 재개는
// 일부러 일시정지된 작업만 됩니다. 다시 실행과 발견 이어하기는
// 종료된 작업을 되살려야 할 때 admitTask를 직접 씁니다.
func (s *Server) applyTaskControl(t *Task, action string) (taskControlResult, error) {
	return s.applyTaskControlWithCause(t, action, agent.AbortPausedByUser)
}

func (s *Server) applyTaskControlWithCause(t *Task, action string, pauseCause error) (taskControlResult, error) {
	if t == nil {
		return taskControlResult{}, fmt.Errorf("작업을 찾을 수 없습니다")
	}
	out := taskControlResult{ID: t.ID}
	switch action {
	case "pause":
		s.concMu.Lock()
		defer s.concMu.Unlock()
		current, exists := s.m.Task(t.ID)
		if !exists || current != t || s.engine.IsDeleting(t.ID) {
			return out, fmt.Errorf("작업을 삭제하는 중이라 제어할 수 없습니다")
		}
		if !s.engine.beginTaskOperation(t.ID) {
			return out, fmt.Errorf("작업을 삭제하는 중이라 제어할 수 없습니다")
		}
		defer s.engine.decInflight(t.ID)
		lifecycle := t.lifecycleSnapshot()
		if isTerminalStatus(lifecycle.Status) {
			return out, fmt.Errorf("종료 상태 작업은 일시 중지할 수 없습니다")
		}
		if lifecycle.Paused {
			return out, fmt.Errorf("작업이 이미 일시 중지되었습니다")
		}
		wasQueued := lifecycle.Queued
		wasEnginePaused := s.engine.IsPaused(t.ID)
		if pauseCause == nil {
			pauseCause = agent.AbortPausedByUser
		}
		s.engine.Pause(t.ID, pauseCause)
		if err := s.m.ApplyTaskPause(t.ID); err != nil {
			if !wasEnginePaused && !wasQueued {
				s.engine.Resume(t)
			}
			return out, err
		}
		// 메인 에이전트는 따로 취소할 수 있습니다. 현재 턴은
		// 영구 일시정지가 확정된 뒤에만 취소합니다. 실패한 제어 요청은 완전히
		// 되돌리고, 아직 유효한 대화 턴을 잃지 않습니다.
		s.cancelTaskChat(t.ID, agent.AbortChatPausedWithTask)
		out.Paused, out.Status = true, "paused"
		go s.reconcileConcurrency()
	case "resume":
		queued, err := s.admitPausedTask(t)
		if err != nil {
			return out, err
		}
		out.Queued = queued
		out.Status = map[bool]string{true: "queued", false: "running"}[queued]
	default:
		return out, fmt.Errorf("action 은 pause 또는 resume 이어야 합니다")
	}
	log.Printf("[task] #%s %s", t.ID, map[string]string{"pause": "일시 중지됨", "resume": "재개됨"}[action])
	return out, nil
}

// intentSummaryOf는 의도 payload 안의 summary를 가져, 삭제 알림이 의도 노드가 사라지기(진짜 삭제) 전에 기록으로 남기게 한다.
func intentSummaryOf(n *db.Node) string {
	if n == nil {
		return ""
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok {
			return s
		}
	}
	return ""
}

func (s *Server) applyIntentControl(ctx context.Context, t *Task, iid int64, action, reason, mode string) (intentControlResult, error) {
	out := intentControlResult{ID: iid}
	node, err := t.Store.GetNode(iid)
	if err != nil {
		return out, err
	}
	if node == nil {
		if inherited, sourceErr := t.Store.GetNodeWithSources(iid); sourceErr == nil && inherited != nil && inherited.Inherited {
			return out, fmt.Errorf("상속된 의도는 읽기 전용이며 제어할 수 없습니다")
		}
		return out, fmt.Errorf("의도를 찾을 수 없습니다")
	}
	if node.Kind != db.KindIntent {
		return out, fmt.Errorf("이 노드는 의도가 아닙니다")
	}
	switch action {
	case "pause":
		if node.State != "running" {
			return out, fmt.Errorf("실행 중인 의도만 일시 중지할 수 있습니다")
		}
		if err := s.engine.ControlWork(ctx, iid, "pause"); err != nil {
			return out, err
		}
		out.State = "paused"
	case "resume":
		if node.State != "paused" {
			return out, fmt.Errorf("일시 중지된 의도만 재개할 수 있습니다")
		}
		changed, err := t.Store.CompareAndSetIntentState(iid, "paused", "open")
		if err != nil {
			return out, err
		}
		if !changed {
			return out, fmt.Errorf("%w: 의도가 더 이상 paused 상태가 아닙니다", db.ErrIntentStateConflict)
		}
		t.Notify()
		out.State = "open"
	case "cancel":
		// 삭제는 두 가지 모드를 지원한다:
		//   soft(기본, 가짜 삭제): 의도를 state='deleted'에 멈추고, 삭제 이유는 delete_reason 필드에 적으며,
		//     의도 노드와 모든 산출/계보를 남기고, 그래프에 fact를 따로 달지 않는다.
		//   hard(진짜 삭제): 해당 의도와 "오직 그것만 지탱하는" 독점 자손 노드를 물리적으로 삭제하고(리프까지 연쇄), 남기지 않도록
		//     고립 데이터; 공유 노드, goal, 작업 루트 사실은 보존한다.
		// 두 모드 모두 cancelled로 플래너(의도만 생성)에 알린다(의도 내용 + 삭제 이유). 플래너는 이를 바탕으로 다시 계획한다.
		if node.State != "running" && node.State != "paused" && node.State != "open" {
			return out, fmt.Errorf("수령 대기/실행 중/일시 중지된 의도만 삭제할 수 있습니다")
		}
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return out, fmt.Errorf("삭제 사유를 입력하세요")
		}
		if node.State == "running" {
			if err := s.engine.ControlWork(ctx, iid, "cancel"); err != nil {
				return out, err
			}
		}
		summary := intentSummaryOf(node)
		if mode == "hard" {
			cleanup, err := t.Store.CancelIntent(iid)
			if err != nil {
				return out, err
			}
			s.cancelWorkerSide(t.ID, t.ExpID, iid)
			t.NotifyCancelled(iid, summary, reason)
			out.Deleted = &cleanup
			out.State = "" // 노드가 이미 삭제되면, 프론트엔드는 Deleted를 보고 목록에서 뺀다
		} else {
			if _, err := t.Store.SoftDeleteIntent(iid, reason); err != nil {
				return out, err
			}
			s.cancelWorkerSide(t.ID, t.ExpID, iid)
			t.NotifyCancelled(iid, summary, reason)
			out.State = db.StateIntentDeleted
		}
	default:
		return out, fmt.Errorf("action 은 pause, resume, cancel 중 하나여야 합니다")
	}
	return out, nil
}

func (s *Server) controlTasksBatch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var req struct {
		TaskIDs []string `json:"task_ids"`
		Action  string   `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "JSON 이 올바르지 않습니다: "+err.Error())
		return
	}
	if req.Action != "pause" && req.Action != "resume" {
		writeErr(w, 400, "action 은 pause 또는 resume 이어야 합니다")
		return
	}
	taskIDs := normalizeBatchTaskIDs(req.TaskIDs)
	if len(taskIDs) == 0 || len(taskIDs) > maxBatchControlIDs {
		writeErr(w, 400, fmt.Sprintf("task_ids 개수는 1-%d여야 합니다", maxBatchControlIDs))
		return
	}
	items := make([]batchControlItem, 0, len(taskIDs))
	for _, parsed := range taskIDs {
		item := batchControlItem{ID: parsed.id}
		if !parsed.valid {
			item.Error = "작업 id가 올바르지 않습니다"
			items = append(items, item)
			continue
		}
		t, ok := s.m.Task(parsed.id)
		if !ok {
			item.Error = "작업을 찾을 수 없습니다"
			items = append(items, item)
			continue
		}
		result, err := s.applyTaskControl(t, req.Action)
		if err != nil {
			item.Error = err.Error()
		} else {
			item.OK, item.Status, item.Queued = true, result.Status, result.Queued
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
