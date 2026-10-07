package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// 작업 공간 파일 관리기. 공유 작업 디렉터리(s.m.dir)를 둘러보고, 보고, 고치고, 받고, 올리고, 지웁니다.
// 모든 에이전트가 산출물을 쓰는 곳입니다. 모든 경로는
// 작업 디렉터리 루트 안에 가둡니다(".."로 빠져나가는 것은 무력화). 모든 경로는
// requireAuth 뒤에 있습니다(Handler()를 보세요).
// 초보용: 화면의 파일 관리기입니다. 에이전트가 작업 폴더에 남긴 파일을 보고 고칩니다.

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: 이보다 큰 파일은 보기/편집에 본문을 넣지 않습니다(대신 받기).
	maxWorkspaceUpload = 512 << 20 // 업로드 요청 하나당 512 MiB
)

// wsResolve는 사용자가 준 상대 경로를 작업 디렉터리 안의 절대 경로로 바꿉니다.
// 루트 밖으로 나가면 ok=false입니다. 루트를 붙인 사본에 filepath.Clean을 하면
// ".."가 접혀, 루트 위로 올라갈 수 없습니다.
func (s *Server) wsResolve(rel string) (string, bool) {
	base := filepath.Clean(s.m.dir)
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean("/" + rel) // 예: "/a/../../etc" → "/etc" (그래도 "/"에 붙어 있음)
	abs := filepath.Clean(filepath.Join(base, clean))
	if abs != base && !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// wsRel은 절대 경로를 작업 공간 상대 경로로 되돌립니다(슬래시는 /).
func (s *Server) wsRel(abs string) string {
	base := filepath.Clean(s.m.dir)
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // 유닉스 밀리초
}

// GET /api/workspace/list?path=<rel> — 목록
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		writeErr(w, 404, "경로가 없습니다")
		return
	}
	if !fi.IsDir() {
		writeErr(w, 400, "디렉터리가 아닙니다")
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]wsEntry, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, wsEntry{
			Name:  e.Name(),
			Path:  s.wsRel(filepath.Join(abs, e.Name())),
			Dir:   e.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		})
	}
	// 디렉터리가 앞이고, 각각 이름순으로 정렬한다.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "entries": out})
}

// GET /api/workspace/read?path=<rel> — 보기/편집용으로 글을 넣습니다. 바이너리이거나 너무 크면
// 내용 없이 {binary:true}/{too_large:true}를 줍니다(대신 받기를 쓰세요).
func (s *Server) wsRead(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		writeErr(w, 404, "파일이 없습니다")
		return
	}
	if fi.IsDir() {
		writeErr(w, 400, "디렉터리라서 파일로 읽을 수 없습니다")
		return
	}
	if fi.Size() > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "size": fi.Size(), "too_large": true, "binary": true})
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "size": fi.Size(), "binary": true})
		return
	}
	writeJSON(w, 200, map[string]any{"path": s.wsRel(abs), "size": fi.Size(), "binary": false, "content": string(data)})
}

// POST /api/workspace/write {path, content} — 텍스트 파일을 만들거나 덮어씁니다.
func (s *Server) wsWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, ok := s.wsResolve(req.Path)
	if !ok || abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		writeErr(w, 400, "대상이 디렉터리입니다")
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(abs, []byte(req.Content), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(abs)})
}

// POST /api/workspace/mkdir {path} — 디렉터리 만들기
func (s *Server) wsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, ok := s.wsResolve(req.Path)
	if !ok || abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(abs)})
}

// DELETE /api/workspace/delete?path=<rel> — 파일이나 디렉터리 트리를 지웁니다
// (작업 디렉터리 안만. 루트 자체는 못 지움).
func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	if abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "작업 공간 루트는 삭제할 수 없습니다")
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeErr(w, 404, "경로가 없습니다")
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/workspace/download?path=<rel> — 파일을 첨부로 흘려 보냅니다.
func (s *Server) wsDownload(w http.ResponseWriter, r *http.Request) {
	abs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		writeErr(w, 404, "파일이 없습니다")
		return
	}
	name := filepath.Base(abs)
	// RFC 5987의 filename*가 비ASCII 이름을 지킵니다. 일반 filename은 폴백입니다.
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(name)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	http.ServeFile(w, r, abs)
}

// POST /api/workspace/upload?path=<dir> — multipart 필드 "file"(하나 이상).
func (s *Server) wsUpload(w http.ResponseWriter, r *http.Request) {
	dirAbs, ok := s.wsResolve(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "잘못된 경로")
		return
	}
	if fi, err := os.Stat(dirAbs); err != nil || !fi.IsDir() {
		writeErr(w, 400, "대상 디렉터리가 없습니다")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "업로드를 해석하지 못했거나 크기 제한을 넘었습니다: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "업로드 파일이 없습니다(폼 필드 file)")
		return
	}
	saved := 0
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // 경로 조각은 떼어 냅니다.
		if name == "" || name == "." || name == ".." {
			continue
		}
		destAbs, okd := s.wsResolve(filepath.Join(s.wsRel(dirAbs), name))
		if !okd {
			continue
		}
		if err := saveUpload(hdr, destAbs); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

func saveUpload(hdr *multipart.FileHeader, dest string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// sanitizeFilename은 Content-Disposition 파일 이름에 위험한 문자를 뺍니다.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}
