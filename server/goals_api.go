package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 개요의 「목표 관리」를 위한 사람용 CRUD 인터페이스다. 이 목표는 엔진이 작업에서 자산 그래프와 탐색 그래프를 어디까지 볼지 가늠하는 기준이다. agent 쪽 set_goals 도구와 같은 goal 노드 묶음을 쓰며,
// 다만 입구는 사람이 UI에서 직접 추가·삭제·수정하는 것이다. 추가/수정 뒤에는 「작업 되살리기」 로직을 재사용한다(admitTask resume:
// 종료 상태→running, 일시정지 해제, 필요하면 대기열), 삭제는 되살리지 않는다(제품 결정). 각 변경 handler는
// beginTaskOperation/decInflight를 타서, 작업 삭제와 경합하지 않게 한다(의도 CRUD와 같다).

// listGoals는 이 작업의 목표 전부(text/vulnclass/state를 나눠 둠)를 돌려주고, 목표 관리 카드가 그린다.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal은 사람이 목표를 하나 추가한다: 저장(작업 루트 spawns 아래에 건다) → 「목표를 추가했다」 한 줄을 적어 트리거로 깨운다
// 플래너(의도만 생성) → 작업을 되살려, 플래너가 새 목표로 달성 여부를 다시 판단하게 한다.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 목표를 추가할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "대상 내용은 비울 수 없습니다")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // 목표는 작업 루트(출발 사실) 아래에 매달립니다
	}
	t.NotifyGoal([]string{text}) // 「사람이 목표를 추가했다:…」를 적어 트리거하고 플래너(의도만 생성)를 깨운다
	s.reviveTask(t)              // 완료되었거나 일시정지된 작업을 실행 상태로 끌어와 계속 돌린다
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "대상을 기록한 뒤 읽기에 실패했습니다")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal은 사람이 목표 텍스트(및 vulnclass)를 고친다: 저장을 고친 뒤 → 「사용자가 목표를 old에서 new로 바꿨다」를 적는다
// 트리거하여 플래너(의도만 생성)를 깨운다 → 작업을 되살려, 새 목표에 따라 방향을 조정하게 한다.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 목표를 수정할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "목표 id가 올바르지 않습니다")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "대상 내용은 비울 수 없습니다")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "대상이 없습니다")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「사람이 목표를 old에서 new로 바꿨다」고 기록하면 트리거되어 플래너(의도만 생성)를 깨운다
	s.reviveTask(t)                   // 추가와 같음: 작업을 되살려 새 목표로 다시 판단한다
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "대상을 갱신한 뒤 읽기에 실패했습니다")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal은 사람이 목표 하나를 삭제한다(하드 삭제, 간선/앵커(exploration_anchors) 연쇄 삭제): 저장소 삭제 → 「사용자가 해당 목표 X를 삭제했다」고 기록
// 트리거하여 플래너(의도만 생성)를 깨우고 이에 따라 남은 목표를 다시 판단한다. 제품 결정상 삭제는 작업을 되살리지 【않는다】.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 목표를 삭제할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "목표 id가 올바르지 않습니다")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "대상이 없습니다")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「사람이 해당 목표를 삭제했다:…」고 기록하면 트리거되어 플래너(의도만 생성)를 깨운다(작업은 되살리지 않음)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
