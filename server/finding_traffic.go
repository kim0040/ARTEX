package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/evidence"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) evidenceStore() *evidence.Store {
	return evidence.New(s.m.pg, s.m.traffic, filepath.Join(s.m.dir, "evidence"))
}

// Add only new optional properties; preserve edited descriptions, existing
// properties, agent bindings and disabled flags. The one-time flag also keeps
// subsequent user unbinding of the evidence reader intact.
func (s *Server) seedFindingTrafficTools() {
	const flag = "finding_traffic_tools_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, key := range []string{"report_finding", "update_finding_report"} {
		var schema any
		if key == "update_finding_report" {
			schema = s.toolUpdateFindingReport().InputSchema()
		} else {
			for _, seed := range agent.BuiltinToolSeeds() {
				if seed.Key == key {
					schema = seed.Schema
					break
				}
			}
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			log.Printf("[evidence] tool schema: %v", err)
			return
		}
		var obj map[string]json.RawMessage
		if err = json.Unmarshal(raw, &obj); err != nil {
			return
		}
		// BuiltinToolSeeds stores Schema as RawMessage; both forms marshal as JSON.
		var properties map[string]json.RawMessage
		if err = json.Unmarshal(obj["properties"], &properties); err != nil {
			return
		}
		name := "traffic_refs"
		if key == "update_finding_report" {
			name = "evidence_version"
		}
		if len(properties[name]) == 0 {
			return
		}
		_, err = s.m.pg.Exec(`UPDATE tools SET schema=jsonb_set(schema,ARRAY['properties',$2::text],$3::jsonb,true),updated_at=now()
WHERE key=$1 AND system AND NOT(COALESCE(schema->'properties','{}'::jsonb) ? $2)`, key, name, string(properties[name]))
		if err != nil {
			log.Printf("[evidence] upgrade tool %s: %v", key, err)
			return
		}
	}
	if reporter, _ := s.m.pg.GetAgentByKey("reporter"); reporter != nil {
		if err := s.m.pg.AddAgentToToolBinding("reporter", []string{"get_finding_traffic"}); err != nil {
			return
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

func (s *Server) registerFindingTraffic(mux *http.ServeMux) {
	base := "/api/exploration/findings/{id}/traffic"
	mux.HandleFunc("GET "+base, s.getFindingTraffic)
	mux.HandleFunc("POST "+base, s.bindFindingTraffic)
	mux.HandleFunc("PATCH "+base+"/{binding_id}", s.editFindingTraffic)
	mux.HandleFunc("DELETE "+base+"/{binding_id}", s.editFindingTraffic)
	mux.HandleFunc("PUT "+base+"/order", s.editFindingTraffic)
	mux.HandleFunc("GET "+base+"/{binding_id}", s.getFindingTrafficDetail)
	mux.HandleFunc("GET "+base+"/{binding_id}/body", s.getFindingTrafficBody)
}

func evidenceError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, db.ErrEvidenceConflict) || errors.Is(err, db.ErrTaskArchiveState) {
		status = http.StatusConflict
	}
	if errors.Is(err, db.ErrFindingNotFound) || errors.Is(err, db.ErrEvidenceNotFound) {
		status = http.StatusNotFound
	}
	writeErr(w, status, err.Error())
}

func (s *Server) findingTrafficAccess(w http.ResponseWriter, r *http.Request, write bool) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "발견 id가 올바르지 않습니다")
		return 0, false
	}
	f, err := s.m.pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return 0, false
	}
	if f == nil {
		writeErr(w, 404, "발견을 찾을 수 없습니다")
		return 0, false
	}
	if taskID := r.URL.Query().Get("context_task"); taskID != "" {
		task := s.m.ResolveTask(taskID)
		if task == nil {
			writeErr(w, 404, "맥락의 작업을 찾을 수 없습니다")
			return 0, false
		}
		_, inherited, allowed := findingProvenanceInTask(task, f.TaskID)
		if !allowed {
			writeErr(w, 404, "작업 맥락에서 발견을 쓸 수 없습니다")
			return 0, false
		}
		if write && inherited {
			writeErr(w, 403, "상속된 발견(finding)은 읽기 전용입니다. 원본 작업에서 수정하세요")
			return 0, false
		}
	}
	return id, true
}

func trafficSummary(in *db.FindingTraffic) *db.FindingTraffic {
	out := *in
	out.Bindings = append([]db.FindingTrafficBinding{}, in.Bindings...)
	for i := range out.Bindings {
		out.Bindings[i].Snapshot.ReqHead = ""
		out.Bindings[i].Snapshot.RespHead = ""
	}
	return &out
}

func (s *Server) getFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	out, err := s.m.pg.GetFindingTraffic(r.Context(), id)
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, trafficSummary(out))
}

func (s *Server) bindFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, 400, "본문이 올바르지 않습니다")
		return
	}
	if len(body.Refs) == 0 {
		writeErr(w, 400, "트래픽을 선택하세요")
		return
	}
	out, err := s.evidenceStore().Bind(r.Context(), id, body.Refs)
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, trafficSummary(out))
}

func (s *Server) editFindingTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Version *int64   `json:"version"`
		Role    *string  `json:"role"`
		Note    *string  `json:"note"`
		Order   []string `json:"binding_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Version == nil {
		writeErr(w, 400, "version과 유효한 요청 본문은 필수입니다")
		return
	}
	var order []int64
	bindingID := int64(0)
	if r.Method == http.MethodPut {
		if body.Order == nil {
			writeErr(w, 400, "binding_ids는 필수입니다")
			return
		}
		order = []int64{}
		for _, raw := range body.Order {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v <= 0 {
				writeErr(w, 400, "연결 id가 올바르지 않습니다")
				return
			}
			order = append(order, v)
		}
	} else {
		var err error
		bindingID, err = strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
		if err != nil || bindingID <= 0 {
			writeErr(w, 400, "연결 id가 올바르지 않습니다")
			return
		}
	}
	err := s.m.pg.EditFindingTraffic(r.Context(), id, bindingID, *body.Version, body.Role, body.Note, r.Method == http.MethodDelete, order)
	if err != nil {
		evidenceError(w, err)
		return
	}
	s.getFindingTraffic(w, r)
}

type evidencePreview struct {
	Content    string `json:"content"`
	Offset     int64  `json:"offset"`
	Total      int64  `json:"total"`
	NextOffset int64  `json:"next_offset"`
	Truncated  bool   `json:"truncated"`
	Binary     bool   `json:"binary"`
}

func readEvidencePreview(store *evidence.Store, snapshot db.TrafficEvidenceSnapshot, side string, offset, length int64) (out evidencePreview, err error) {
	if offset < 0 || length < 0 {
		return out, errors.New("offset / length는 음수일 수 없습니다")
	}
	if length == 0 || length > 8192 {
		length = 8192
	}
	f, total, err := store.OpenBody(snapshot, side)
	if err != nil {
		return out, err
	}
	defer f.Close()
	if offset > total {
		return out, errors.New("offset이 본문 길이를 넘습니다")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return out, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, length))
	if err != nil {
		return out, err
	}
	// Leave an incomplete trailing UTF-8 rune for the next page. Explicit byte
	// offsets are still accepted; complete downloads always retain original bytes.
	if offset+int64(len(raw)) < total && len(raw) >= utf8.UTFMax {
		start := len(raw) - 1
		for start > 0 && raw[start]&0xc0 == 0x80 {
			start--
		}
		if !utf8.FullRune(raw[start:]) {
			raw = raw[:start]
		}
	}
	out = evidencePreview{Offset: offset, Total: total, NextOffset: offset + int64(len(raw)), Truncated: offset+int64(len(raw)) < total,
		Binary: bytes.IndexByte(raw, 0) >= 0 || (offset == 0 && !utf8.Valid(raw))}
	if out.Binary {
		out.Content = fmt.Sprintf("[바이너리 본문, %d바이트. 다운로드해서 보세요]", total)
	} else {
		out.Content = string(bytes.ToValidUTF8(raw, []byte("�")))
	}
	return out, nil
}

func (s *Server) getFindingTrafficDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	bid, err := strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
	if err != nil || bid <= 0 {
		writeErr(w, 400, "연결 id가 올바르지 않습니다")
		return
	}
	store := s.evidenceStore()
	var result any
	err = store.WithBinding(r.Context(), id, bid, func(b db.FindingTrafficBinding) error {
		req, err := readEvidencePreview(store, b.Snapshot, "request", 0, 8192)
		if err != nil {
			return err
		}
		resp, err := readEvidencePreview(store, b.Snapshot, "response", 0, 8192)
		if err != nil {
			return err
		}
		result = map[string]any{"binding": b, "request": req, "response": resp}
		return nil
	})
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) getFindingTrafficBody(w http.ResponseWriter, r *http.Request) {
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	bid, err := strconv.ParseInt(r.PathValue("binding_id"), 10, 64)
	if err != nil || bid <= 0 {
		writeErr(w, 400, "연결 id가 올바르지 않습니다")
		return
	}
	side := r.URL.Query().Get("side")
	store := s.evidenceStore()
	// Resolve the binding under the evidence lock, then read the blob without it:
	// both paths below are O(body size) and would otherwise stall every evidence
	// write for as long as the client takes to receive the data.
	b, err := store.Binding(r.Context(), id, bid)
	if err != nil {
		evidenceError(w, err)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		f, length, err := store.OpenBody(b.Snapshot, side)
		if err != nil {
			evidenceError(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"evidence-%d-%s.bin\"", bid, side))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		_, _ = io.Copy(w, f)
		return
	}
	offset, length := int64(0), int64(8192)
	for name, dst := range map[string]*int64{"offset": &offset, "length": &length} {
		if raw := r.URL.Query().Get(name); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				evidenceError(w, err)
				return
			}
			*dst = v
		}
	}
	preview, err := readEvidencePreview(store, b.Snapshot, side, offset, length)
	if err != nil {
		evidenceError(w, err)
		return
	}
	writeJSON(w, 200, preview)
}

func (s *Server) toolGetFindingTraffic() actool.CoreTool {
	return roTool("get_finding_traffic", "발견(finding)에 묶인 실제 트래픽 증거를 읽습니다. 캡처 스위치에 의존하지 않습니다. finding_id에는 report_finding JSON이 반환한 독립 발견(finding) 기록 ID를 사용합니다(첫 줄의 탐색 노드 ID가 아닙니다). 먼저 binding_id 없이 목록과 version을 가져옵니다. 빈 목록은 정상입니다. TCP처럼 HTTP가 아닌 발견(finding)이거나 아직 수집되지 않은 경우에도 글이나 명령 증거로 보고서를 쓸 수 있으며, 바인딩은 강제하지 않습니다. 바인딩이 있으면 binding_id, side(request/response), offset으로 본문을 나누어 읽습니다. 보고서를 쓸 때는 읽은 version을 evidence_version으로 update_finding_report에 넘기고, 그 호출의 finding_id는 여전히 탐색 노드 ID를 사용합니다. 이 읽기는 탐색 그래프의 발견(finding)과 기록 프록시가 남긴 트래픽을 맞춰 보고서 본문을 만듭니다.",
		objSchema(map[string]any{"finding_id": strParam("독립 발견 기록 ID"), "binding_id": strParam("목록에 있는 바인딩 ID입니다. 생략하면 목록을 반환합니다"), "side": strParam("request 또는 response이며, 기본값은 response입니다"), "offset": map[string]any{"type": "integer"}, "length": map[string]any{"type": "integer"}}, "finding_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				FindingID      json.RawMessage `json:"finding_id"`
				BindingID      json.RawMessage `json:"binding_id"`
				Side           string          `json:"side"`
				Offset, Length int64
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			id, bid := parseProfileID(a.FindingID), parseProfileID(a.BindingID)
			if err := s.agentFindingTrafficAccess(ctx, id, false); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if len(a.BindingID) == 0 {
				list, err := s.m.pg.GetFindingTraffic(ctx, id)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(trafficSummary(list))
			}
			if a.Side == "" {
				a.Side = "response"
			}
			store := s.evidenceStore()
			var result any
			err := store.WithBinding(ctx, id, bid, func(b db.FindingTrafficBinding) error {
				preview, err := readEvidencePreview(store, b.Snapshot, a.Side, a.Offset, a.Length)
				if err != nil {
					return err
				}
				head := b.Snapshot.ReqHead
				if a.Side == "response" {
					head = b.Snapshot.RespHead
				}
				result = map[string]any{"binding_id": strconv.FormatInt(bid, 10), "head": head, "body": preview}
				return nil
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(result)
		})
}
