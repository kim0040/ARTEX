package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

const maxFindingFollowUpRunes = 4000

func findingPaginationParam(raw string, fallback, upperBound int) int {
	value := atoiDefault(raw, fallback)
	if value <= 0 {
		value = fallback
	}
	if upperBound > 0 && value > upperBound {
		value = upperBound
	}
	return value
}

// findingFilterFromQuery는 요청의 쿼리로 발견 공통 필터를 만듭니다.
// query string. 목록 / 그룹 / 자산 트리 / 내보내기는 같은 파싱을 타며, 필터를 새로 넣을 때는 여기만 고친다. 이 파싱은 UI가 자산 그래프와 탐색 그래프를 같은 조건으로 거르는 입구다.
func findingFilterFromQuery(q url.Values) db.FindingFilter {
	return db.FindingFilter{
		Severity:  normFilter(q.Get("severity")),
		Status:    normFilter(q.Get("status")),
		VulnClass: normFilter(q.Get("vulnclass")),
		// task_id(「작업 노드별」 분기로 가는 task 파라미터와는 별개): 전역 표를 작업으로 거른다.
		TaskID:     normFilter(q.Get("task_id")),
		Query:      q.Get("q"),
		Sort:       q.Get("sort"),
		AssetScope: strings.TrimSpace(q.Get("asset_scope")),
	}
}

// findingAssetTree serves the「자산별」view's left-hand tree: every asset that 이 왼쪽 트리는 UI가 자산 그래프의 자산을 펼쳐 보여 준다.
// 조건에 맞는 발견이 하나 이상 있는 자산과, 그 자산을 놓기 위한 조상을 담습니다.
func (s *Server) findingAssetTree(w http.ResponseWriter, r *http.Request) {
	tree, err := s.m.pg.BuildFindingAssetTree(findingFilterFromQuery(r.URL.Query()))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, tree)
}

// findingGroups는 전역 발견 보기가 쓰는 작업 묶음 페이지를 줍니다.
// 묶음 안의 발견은 기존 평면 발견 엔드포인트를 그대로 씁니다.
// 대시보드/내보내기 클라이언트를 유지하고, 펼친 묶음마다
// 페이지 커서를 따로 갖게 합니다.
func (s *Server) findingGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := findingPaginationParam(q.Get("page"), 1, 0)
	limit := findingPaginationParam(q.Get("limit"), 10, 100)
	groups, total, findingTotal, err := s.m.pg.ListFindingGroups(findingFilterFromQuery(q), page, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 일시정지/대기열 저장은 PostgreSQL이 맡습니다. 살아있는 엔진이 작업 DTO와 같은
	// 실행 중/쉼 보기를 더합니다. 그래서 에이전트가 도는 동안 발견 묶음이
	// 작업 목록보다 뒤처지지 않습니다.
	if s.engine != nil {
		for i := range groups {
			if groups[i].TaskID == nil {
				continue
			}
			if task, ok := s.m.Task(i64s(*groups[i].TaskID)); ok {
				groups[i].TaskStatus = s.resolvedTaskStatus(task)
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"items":         groups,
		"total":         total,
		"finding_total": findingTotal,
		"page":          page,
		"page_size":     limit,
	})
}

// deepenFinding은 그 발견의 원래 작업 아래에, 사람이 넣은 높은 우선순위 워커 의도를 만듭니다.
// DB 쓰기가 살아있는 발견 노드를 검사하고 잇습니다. 그다음 작업
// 입장이 공유 동시성 경로로 작업을 되살리거나 대기열에 넣습니다.
// 초보용: 화면에서 발견을 더 파라고 하면, 탐색 그래프에 워커 의도를 넣고 엔진이 다시 돌게 합니다.
func (s *Server) deepenFinding(w http.ResponseWriter, r *http.Request) {
	id := int64(atoiDefault(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "발견 id가 올바르지 않습니다")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var req struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "요청 본문이 너무 큽니다")
		} else {
			writeErr(w, http.StatusBadRequest, "JSON 이 올바르지 않습니다: "+err.Error())
		}
		return
	}
	description := strings.TrimSpace(req.Description)
	switch {
	case description == "":
		writeErr(w, 400, "설명은 필수입니다")
		return
	case utf8.RuneCountInString(description) > maxFindingFollowUpRunes:
		writeErr(w, 400, fmt.Sprintf("description은 최대 %d자까지 입력할 수 있습니다", maxFindingFollowUpRunes))
		return
	}

	finding, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if finding == nil {
		writeErr(w, 404, "발견을 찾을 수 없습니다")
		return
	}
	if finding.TaskID == nil || finding.NodeID == nil {
		writeErr(w, 409, "발견의 원래 작업이나 노드를 더 이상 쓸 수 없습니다")
		return
	}
	t, ok := s.m.Task(i64s(*finding.TaskID))
	if !ok || t == nil {
		writeErr(w, 409, "발견의 원래 작업을 더 이상 쓸 수 없습니다")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중입니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	audit := db.Activity{
		Worker:  "system",
		Kind:    "text",
		Summary: "사람이 제출한 발견을 더 깊게 보는 의도",
		Detail:  description,
	}
	intentID, audit, err := t.Store.AddFindingFollowUpIntent(id, *finding.NodeID, description, audit)
	if errors.Is(err, db.ErrFindingOriginUnavailable) {
		writeErr(w, 409, "발견의 원래 작업이나 노드를 더 이상 쓸 수 없습니다")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	queued, err := s.admitTask(t, "resume")
	if err != nil {
		if rollbackErr := t.Store.DiscardOpenIntent(intentID); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("discard follow-up intent %d: %w", intentID, rollbackErr))
		}
		writeErr(w, 500, err.Error())
		return
	}
	// 감사 행은 의도와 같은 트랜잭션으로 확정됐습니다. 그 행을 그대로 발행합니다.
	// emitActivity는 두 번째 사본을 넣어 DB와 SSE가 어긋납니다.
	s.engine.Broadcaster().Publish(t.ID, audit)
	s.engine.touch(t.ID)
	// 입장은 워커를 바로 깨울 수 있습니다. 응답 시각에 저장된 의도 상태를
	// 알려 주고, 항상 아직 열려 있다고 하지는 않습니다.
	// 작업 단위 대기 플래그는 별개입니다. 대기 중인 작업은 일부러
	// 동시성 자리를 받을 때까지 워커 의도를 열어 둡니다.
	state := "open"
	if current, stateErr := t.Store.GetNode(intentID); stateErr == nil && current != nil {
		state = current.State
	}
	writeJSON(w, 200, map[string]any{
		"task_id":   t.ID,
		"intent_id": i64s(intentID),
		"state":     state,
		"queued":    queued,
	})
}
