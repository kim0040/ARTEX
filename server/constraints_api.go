package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 개요 「제약 관리」의 수동 CRUD 인터페이스와 주입 범위 스위치 해석. 조작 제약(allow/deny)은 에이전트 쪽의. 초보: 이 제약은 UI에서 넣고, 엔진이 다음 계획 때 자산 그래프와 탐색 그래프를 읽는 맥락에만 실리며 두 그래프의 행을 직접 고치지는 않는다.
// set_constraints 도구가 같은 task_constraints 테이블에 쓴다; 여기는 사람이 UI에서 직접 추가·수정·삭제한다. 제약은 단지
// 프롬프트 맥락이다——추가·수정·삭제 뒤 플래너(의도만 생성)에【알리지 않고】, 다음 계획 라운드가 저절로 DB를 읽어 반영한다(제품 결정). 각 변경 handler는
// 모두 beginTaskOperation/decInflight를 타서, 작업 삭제와 경합하지 않게 한다(목표/의도 CRUD와 같다).

// 주입 범위 스위치의 settings key이며, 기본은 모두 켜짐(GetBool 두 번째 인자 = true).
const (
	settingConstraintsInjectPlanner = "constraints_inject_planner"
	settingConstraintsInjectWorker  = "constraints_inject_worker"
)

// constraintInjectPlanner / constraintInjectWorker는 조작 제약을 해당 에이전트의
// 시스템 프롬프트에 넣을지 보고한다(기본 켜짐). resolver로 planner/worker에 넘기고, 매 라운드 읽는다 → 스위치를 바꾸면 바로 적용된다.
func (s *Server) constraintInjectPlanner() bool {
	return s.m.pg.GetBool(settingConstraintsInjectPlanner, true)
}

func (s *Server) constraintInjectWorker() bool {
	return s.m.pg.GetBool(settingConstraintsInjectWorker, true)
}

// listConstraints는 이 작업의 조작 제약을 모두 돌려준다(allow가 앞, deny가 뒤).
func (s *Server) listConstraints(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	rows, err := t.Store.ListConstraints()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"constraints": constraintDTOs(rows)})
}

// addConstraint는 조작 제약 하나를 수동으로 추가한다(kind=allow|deny). 플래너(의도만 생성)에 알리지 않는다.
func (s *Server) addConstraint(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 제약을 추가할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "제약 내용은 비울 수 없음")
		return
	}
	kind := normalizeConstraintKind(body.Kind)
	if kind == "" {
		writeErr(w, 400, "kind는 allow 또는 deny여야 함")
		return
	}
	id, err := t.Store.AddConstraint(kind, text, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ConstraintDTO{ID: strconv.FormatInt(id, 10), Kind: kind, Text: text, Origin: "human"})
}

// editConstraint는 제약 하나를 수동으로 고친다(kind + text). 플래너(의도만 생성)에 알리지 않는다.
func (s *Server) editConstraint(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 제약을 수정할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || cid <= 0 {
		writeErr(w, 400, "제약 id가 올바르지 않습니다")
		return
	}
	var body struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "제약 내용은 비울 수 없음")
		return
	}
	kind := normalizeConstraintKind(body.Kind)
	if kind == "" {
		writeErr(w, 400, "kind는 allow 또는 deny여야 함")
		return
	}
	if err := t.Store.UpdateConstraint(cid, kind, text); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ConstraintDTO{ID: strconv.FormatInt(cid, 10), Kind: kind, Text: text})
}

// deleteConstraint는 제약 하나를 수동으로 지운다. 플래너(의도만 생성)에 알리지 않는다.
func (s *Server) deleteConstraint(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "작업을 찾을 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 제약을 삭제할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || cid <= 0 {
		writeErr(w, 400, "제약 id가 올바르지 않습니다")
		return
	}
	if err := t.Store.DeleteConstraint(cid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// normalizeConstraintKind는 kind를 소문자로 맞추고 검사합니다. 잘못되면 ""입니다.
func normalizeConstraintKind(k string) string {
	k = strings.TrimSpace(strings.ToLower(k))
	if k == "allow" || k == "deny" {
		return k
	}
	return ""
}

// constraintDTOs는 DB 행을 화면이 그리는 모양으로 바꿉니다.
func constraintDTOs(in []db.Constraint) []ConstraintDTO {
	out := make([]ConstraintDTO, 0, len(in))
	for _, c := range in {
		out = append(out, ConstraintDTO{
			ID:     strconv.FormatInt(c.ID, 10),
			Kind:   c.Kind,
			Text:   c.Text,
			Origin: c.Origin,
			TS:     rfc3339(c.CreatedAt),
		})
	}
	return out
}
