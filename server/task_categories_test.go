package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTaskCategoryBatchRoute는 일괄 이동 엔드포인트 계약을 고정합니다.
// /api/tasks/{id}/... 패턴보다 앞에 등록되고, DB를 건드리지 전에
// 요청을 검사하며, 모르는 id는 배치 전체를 실패시키지 않고
// 작업마다 보고합니다.
func TestTaskCategoryBatchRoute(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer m.Close()

	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	h := s.Handler()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/category/batch", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// category_id는 필수입니다. 빼는 것을 "분류 해제"로 읽으면 안 됩니다.
	if rec := post(`{"task_ids":["1"]}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "category_id 는 필수입니다") {
		t.Fatalf("missing category_id: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 빈 선택은 DB 작업 전에 거절됩니다.
	if rec := post(`{"task_ids":[],"category_id":null}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "task_ids 개수는") {
		t.Fatalf("empty task_ids: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 모르거나 형식이 나쁜 id는 200 안에서 작업별 실패로 돌아옵니다.
	// 호출자가 어느 선택이 낡았는지 정확히 알게 합니다.
	rec := post(`{"task_ids":["999999999","abc","999999999"],"category_id":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Items []struct {
			ID    string `json:"id"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"items"`
		Category *struct {
			ID int64 `json:"id"`
		} `json:"category"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	// 중복 id는 합쳐져, 항목은 두 개 남습니다.
	if len(response.Items) != 2 {
		t.Fatalf("items=%+v, want 2 after de-duplication", response.Items)
	}
	if response.Items[0].ID != "999999999" || response.Items[0].OK || response.Items[0].Error != "작업을 찾을 수 없습니다" {
		t.Fatalf("unknown id entry=%+v", response.Items[0])
	}
	if response.Items[1].ID != "abc" || response.Items[1].OK || response.Items[1].Error != "작업 id가 올바르지 않습니다" {
		t.Fatalf("malformed id entry=%+v", response.Items[1])
	}
	if response.Category != nil {
		t.Fatalf("category=%+v, want null when clearing", response.Category)
	}
}
