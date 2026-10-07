package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

const maxTaskTemplateRequestBytes = 512 << 10

// taskTemplateRequest는 포인터를 써서, PATCH가 필드를 뺀 것과
// 빈 값을 명시한 것을 구분합니다. 빈 값도 DB 검사가 거절합니다.
// category_id / intercept_rules 가 있는지는 원본 키 집합으로 봅니다
// (decodeTaskTemplateRequest). 그래서 PATCH가 분류를 null로 지울 수 있습니다.
// 초보용: 화면에서 작업 템플릿을 고칠 때 보내는 본문입니다. intercept_rules는 그 템플릿의 가로채기 규칙입니다.
type taskTemplateRequest struct {
	Name           *string                `json:"name"`
	Description    *string                `json:"description"`
	Goal           *string                `json:"goal"`
	CategoryID     *int64                 `json:"category_id"`
	InterceptRules []taskInterceptRuleReq `json:"intercept_rules"`
}

// decodeTaskTemplateRequest는 본문을 req로 풀고, JSON에 실제로 있던
// 최상위 키 집합을 돌려줍니다(PATCH에서 키가 있었는지 볼 때).
func decodeTaskTemplateRequest(w http.ResponseWriter, r *http.Request, req *taskTemplateRequest) (map[string]struct{}, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskTemplateRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "요청 본문이 너무 큽니다")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return nil, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if err := json.Unmarshal(body, req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	present := make(map[string]struct{}, len(raw))
	for k := range raw {
		present[k] = struct{}{}
	}
	return present, true
}

func validateTaskTemplateRequest(req taskTemplateRequest) error {
	checks := []struct {
		name  string
		value *string
		limit int
	}{
		{name: "name", value: req.Name, limit: db.MaxTaskTemplateNameRunes},
		{name: "description", value: req.Description, limit: db.MaxTaskTemplateTextRunes},
		{name: "goal", value: req.Goal, limit: db.MaxTaskTemplateTextRunes},
	}
	for _, check := range checks {
		if check.value == nil {
			continue
		}
		value := strings.TrimSpace(*check.value)
		if check.name == "name" {
			value = strings.Join(strings.Fields(value), " ")
		}
		if utf8.RuneCountInString(value) > check.limit {
			return fmt.Errorf("%s은(는) 최대 %d자입니다", check.name, check.limit)
		}
	}
	return nil
}

func writeTaskTemplateErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrTaskTemplateInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, db.ErrTaskTemplateNameConflict):
		writeErr(w, http.StatusConflict, "템플릿 이름이 이미 있습니다")
	case errors.Is(err, db.ErrTaskTemplateNotFound):
		writeErr(w, http.StatusNotFound, "작업 템플릿을 찾을 수 없습니다")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) pgListTaskTemplates(w http.ResponseWriter, _ *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	templates, err := pg.ListTaskTemplates()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": templates})
}

func (s *Server) pgCreateTaskTemplate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req taskTemplateRequest
	if _, ok := decodeTaskTemplateRequest(w, r, &req); !ok {
		return
	}
	if err := validateTaskTemplateRequest(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rules, err := buildTaskInterceptRules(req.InterceptRules)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "가로채기/허용 규칙이 올바르지 않습니다: "+err.Error())
		return
	}
	template, err := pg.CreateTaskTemplate(db.TaskTemplateInput{
		Name:           stringValue(req.Name),
		Description:    stringValue(req.Description),
		Goal:           stringValue(req.Goal),
		CategoryID:     req.CategoryID,
		InterceptRules: rules,
	})
	if err != nil {
		writeTaskTemplateErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, template)
}

func (s *Server) pgUpdateTaskTemplate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "작업 템플릿 id가 올바르지 않습니다")
		return
	}
	var req taskTemplateRequest
	present, ok := decodeTaskTemplateRequest(w, r, &req)
	if !ok {
		return
	}
	if err := validateTaskTemplateRequest(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_, catPresent := present["category_id"]
	_, rulesPresent := present["intercept_rules"]
	if req.Name == nil && req.Description == nil && req.Goal == nil && !catPresent && !rulesPresent {
		writeErr(w, http.StatusBadRequest, "name, description, goal, category_id, intercept_rules 중 하나는 있어야 합니다")
		return
	}
	patch := db.TaskTemplatePatch{Name: req.Name, Description: req.Description, Goal: req.Goal}
	if catPresent {
		patch.SetCategoryID = true
		patch.CategoryID = req.CategoryID
	}
	if rulesPresent {
		rules, err := buildTaskInterceptRules(req.InterceptRules)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "가로채기/허용 규칙이 올바르지 않습니다: "+err.Error())
			return
		}
		patch.SetInterceptRules = true
		patch.InterceptRules = rules
	}
	template, err := pg.PatchTaskTemplate(id, patch)
	if err != nil {
		writeTaskTemplateErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, template)
}

func (s *Server) pgDeleteTaskTemplate(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, http.StatusBadRequest, "작업 템플릿 id가 올바르지 않습니다")
		return
	}
	deleted, err := pg.DeleteTaskTemplate(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, "작업 템플릿을 찾을 수 없습니다")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
