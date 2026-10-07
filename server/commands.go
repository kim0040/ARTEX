package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// pgListCommands는 activity 테이블에서 도구 실행(어느 도구든)을 돌려줍니다.
func (s *Server) pgListCommands(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 0)
	size := atoiDefault(q.Get("size"), 50)
	keyword := q.Get("q")

	records, total, err := s.m.PG().ListCommands(commandTaskFilter(q.Get("task")), keyword, page, size)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"commands": records, "total": total})
}

// pgToolStats는 도구 실행 기록의 도구별 호출 수를 돌려줍니다. 목록과
// 같은 작업/검색어 필터를 씁니다. 요약은 화면에 나온 쪽이 아니라
// 필터에 걸린 전체를 말합니다.
func (s *Server) pgToolStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pg := s.m.PG()
	if pg == nil {
		writeJSON(w, 200, map[string]any{"stats": []db.ToolStat{}})
		return
	}
	stats, err := pg.ToolStats(commandTaskFilter(q.Get("task")), q.Get("q"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"stats": stats})
}

// commandTaskFilter는 ?task= 탐색 id를 읽습니다. 없거나 숫자로 못 읽으면 nil입니다
// (필터 없음). 목록 엔드포인트가 오래 써 온, 조금 느슨한 동작과 같습니다.
func commandTaskFilter(v string) *int64 {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// pgListLLMRecords는 페이지로 나눈 LLM 호출 기록을 돌려줍니다.
func (s *Server) pgListLLMRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiDefault(q.Get("page"), 0)
	size := atoiDefault(q.Get("size"), 50)
	model := q.Get("model")
	session := q.Get("session")
	task := q.Get("task")

	records, total, err := s.m.PG().ListLLMRecords(model, session, task, page, size)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"records": records, "total": total})
}

// pgTokenByModel은 한 작업의 LLM 토큰 사용량을 모델별로 묶어 돌려줍니다.
// 항상 켜 둔 llm_usage 장부에서 읽습니다. 에이전트마다 모델이 달라도,
// 풀 교체나 장애 조치가 있어도, 중간에 끊긴 실행이어도 맞습니다(성공이든
// 오류든 호출마다 기록합니다). 기록 스위치를 따로 켤 필요가 없습니다.
func (s *Server) pgTokenByModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("task"))
	if id == "" {
		writeErr(w, 400, "작업이 지정되지 않았습니다")
		return
	}
	pg := s.m.PG()
	if pg == nil {
		writeJSON(w, 200, map[string]any{"models": []db.ModelTokenStat{}})
		return
	}
	models, err := pg.TokenByModel(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// pgUsageStats는 대시보드의 "새" 토큰 화면에 쓸 llm_usage 전체 합계를 돌려줍니다.
// 설정별 누적(전체 기간)과, 일별 차트용 (설정, 날짜) 묶음입니다.
// 항상 켜 둔 계량 장부에서 오므로, 에이전트별 바인딩,
// 풀 교체, 중간에 끊긴 실행까지 포함해 맞습니다.
func (s *Server) pgUsageStats(w http.ResponseWriter, r *http.Request) {
	pg := s.m.PG()
	if pg == nil {
		writeJSON(w, 200, map[string]any{"by_profile": []db.ProfileUsage{}, "daily": []db.ProfileDayUsage{}})
		return
	}
	days := atoiDefault(r.URL.Query().Get("days"), 365)
	byProfile, err := pg.UsageByProfile()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	daily, err := pg.UsageDaily(days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"by_profile": byProfile, "daily": daily})
}

// pgLLMTasks는 기록이 있는 작업을 개수와 함께, 중복 없이 돌려줍니다. 화면의 작업
// 고르기용입니다(작업을 고르면 목록을 거르고, 그 대화 기록을 지울 수 있습니다).
func (s *Server) pgLLMTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.m.PG().LLMTasks()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"tasks": tasks})
}

// pgDeleteLLMRecords는 작업 id가 정확히 같은 LLM 기록을 모두 지웁니다(작업
// 고르기와 같은 기준). 작업 id가 비어 있으면 400입니다. 지운 행 수를 돌려줍니다.
func (s *Server) pgDeleteLLMRecords(w http.ResponseWriter, r *http.Request) {
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	if task == "" {
		writeErr(w, 400, "작업이 지정되지 않았습니다")
		return
	}
	n, err := s.m.PG().DeleteLLMRecords(task)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// pgGetLLMRecord는 LLM 기록 하나의 요청/응답 본문 전체를 돌려줍니다.
func (s *Server) pgGetLLMRecord(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "id가 올바르지 않습니다")
		return
	}
	rec, err := s.m.PG().GetLLMRecord(id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, rec)
}
