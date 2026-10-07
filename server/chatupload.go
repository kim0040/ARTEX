package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// maxChatUpload는 채팅 첨부 업로드 요청 하나의 상한입니다(메모리와 넘침 포함).
const maxChatUpload = 128 << 20 // 128 MiB

// safeChatID는 경로의 {id} 조각이 디렉터리를 빠져나가지 못하게 막습니다. 작업 id는 숫자이고,
// 세션 id는 영숫자와 _/- 입니다. "/" 나 ".." 가 있으면 거절합니다.
var safeChatID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// chatAttachment는 화면과 에이전트가 보는 업로드 파일 하나입니다. Path는
// 그 대화의 작업 디렉터리 기준 상대 경로입니다(예: "uploads/report.txt"). 그 디렉터리가 에이전트의 현재 디렉터리라
// Read/Bash로 파일을 바로 열 수 있습니다. Name/Size는 화면 카드를 그릴 때 씁니다.
// 초보용: 화면에서 올린 파일을 그 대화의 작업 폴더에 두어, 엔진이 기존 읽기 도구로 열게 합니다.
type chatAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Abs는 디스크에 쓴 절대 경로다(m.dir은 이미 절대 경로). 작업을 만들기 전 임시 저장(scope=staging)일 때 프론트엔드는 이 값으로
	// 프롬프트를 설명에 적는다. task/session은 composeAgentMessage로 백엔드에서 경로를 붙이며, 이 필드에 의존하지 않는다.
	Abs string `json:"abs,omitempty"`
}

// chatUpload는 파일 지원의 첫 방식입니다. 파일 하나 이상을 대화의
// 작업 디렉터리 안 uploads/에 저장합니다. 에이전트는 기존 Read/Bash 도구로 열고,
// 보낸 메시지에 그 경로가 실립니다. LLM 층은 바꾸지 않고, 멀티모달도 아닙니다.
//
// POST /api/chat/upload?scope=task|session|staging&id=<id>, multipart 필드 "file"
// (여러 번 가능). {attachments:[{name,path,size,abs}]} 를 돌려줍니다. 대상 디렉터리는
// 에이전트 현재 디렉터리와 같은 배치입니다.
//
//	scope=task    → <workDir>/tasks/<id>/uploads/  (작업)
//	scope=session → <workDir>/sessions/<id>/uploads/  (세션)
//	scope=staging → <workDir>/drafts/<id>/uploads/   (작업을 만들기 전 임시 저장: 작업에 아직 ID가 없고,
//	                파일은 먼저 여기에 두고, 프론트엔드는 반환된 abs 절대 경로를 작업 설명에 적는다)
func (s *Server) chatUpload(w http.ResponseWriter, r *http.Request) {
	var sub string
	taskScoped := false
	switch r.URL.Query().Get("scope") {
	case "task":
		sub = "tasks"
		taskScoped = true
	case "session":
		sub = "sessions"
	case "staging":
		sub = "drafts"
	default:
		writeErr(w, 400, "scope는 task / session / staging이어야 합니다")
		return
	}
	id := r.URL.Query().Get("id")
	if !safeChatID.MatchString(id) {
		writeErr(w, 400, "잘못된 id")
		return
	}
	if taskScoped {
		if s.m.ResolveTask(id) == nil {
			writeErr(w, 404, "작업을 찾을 수 없습니다")
			return
		}
		if !s.engine.beginTaskOperation(id) {
			writeErr(w, http.StatusConflict, "작업을 삭제하는 중이라 첨부 파일을 올릴 수 없습니다")
			return
		}
		defer s.engine.decInflight(id)
	}
	dir := filepath.Join(s.m.dir, sub, id, "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, "디렉터리 생성 실패: "+err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "업로드 해석에 실패했거나 크기 제한을 초과했습니다: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "업로드 파일이 없습니다(폼 필드 file)")
		return
	}
	out := make([]chatAttachment, 0, len(files))
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // 경로 조각은 떼어 냅니다.
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		dest := uniqueUploadPath(dir, name)
		if err := saveUpload(hdr, dest); err != nil {
			writeErr(w, 500, "저장 실패: "+err.Error())
			return
		}
		base := filepath.Base(dest)
		out = append(out, chatAttachment{Name: base, Path: "uploads/" + base, Size: hdr.Size, Abs: dest})
	}
	writeJSON(w, 200, map[string]any{"attachments": out})
}

// uniqueUploadPath는 dir/name을 돌려줍니다. 이미 있으면 dir/name-1, dir/name-2… 를 써서
// 같은 이름을 다시 올려도 예전 첨부를 덮지 않습니다.
func uniqueUploadPath(dir, name string) string {
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return dest
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// composeAgentMessage는 사용자 메시지 뒤에 첨부 목록을 붙여, 에이전트가
// 어떤 파일이 어디 있는지 Read로 열게 합니다. baseDir는 에이전트의 작업
// 디렉터리(현재 디렉터리)입니다. 절대 경로(baseDir + 상대 경로)를 적어, 상대 경로를
// 어떻게 읽든 Read/Bash로 파일을 헷갈리지 않게 합니다.
func composeAgentMessage(msg string, atts []chatAttachment, baseDir string) string {
	if len(atts) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\n【사용자가 업로드한 첨부】(절대 경로, 필요할 때 Read/Bash로 확인):")
	for _, a := range atts {
		fmt.Fprintf(&b, "\n- %s（%s）", filepath.Join(baseDir, a.Path), humanBytes(a.Size))
	}
	return b.String()
}

// userActivityWithAttachments는 저장할 'user' 활동을 만듭니다. 첨부가 있으면
// Detail에 JSON {text, attachments}를 넣어, 대화 기록이 글과 첨부
// 카드를 그리게 합니다. Summary는 그냥 글입니다(목록은 Detail을 빼 두고, 나중에 읽습니다).
func userActivityWithAttachments(worker, text string, atts []chatAttachment) db.Activity {
	a := db.Activity{Worker: worker, Kind: "user", Summary: text}
	if len(atts) > 0 {
		blob, _ := json.Marshal(map[string]any{"text": text, "attachments": atts})
		a.Detail = string(blob)
	}
	return a
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
