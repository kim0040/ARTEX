package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestValidSkillName(t *testing.T) {
	ok := []string{"web-recon", "a", "nuclei2", "중국어스킬", "포트스캔-x", "일본어스킬"}
	bad := []string{
		"", "Web-Recon", "-lead", "trail-", "dou--ble", "has space", "중국어 스킬",
		"dot.name", "a/b", `a\b`, "..", ".", "중국어/스킬", "sk\x00ill", "중‮어",
		strings.Repeat("a", 65), "1abc", "중국어스킬!",
	}
	for _, n := range ok {
		if !validSkillName(n) {
			t.Errorf("validSkillName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if validSkillName(n) {
			t.Errorf("validSkillName(%q) = true, want false", n)
		}
	}
}

func TestSkillRelPath(t *testing.T) {
	ok := map[string]string{
		"SKILL.md":           "SKILL.md",
		"scripts/run.py":     "scripts/run.py",
		"참고/중국어 설명.md":       "참고/중국어 설명.md",
		"references/a b.txt": "references/a b.txt",
		"./SKILL.md":         "SKILL.md",
		"assets/그림-1_v2.png": "assets/그림-1_v2.png",
	}
	for in, want := range ok {
		got, msg := skillRelPath(in)
		if msg != "" || got != want {
			t.Errorf("skillRelPath(%q) = (%q, %q), want (%q, \"\")", in, got, msg, want)
		}
	}
	bad := []string{
		"", "../etc/passwd", "a/../../b", "/abs/path", "a//b", `..\..\x`,
		"%2e%2e/x", "a\x00b", "중국어\u00a0이름.md", "중\u202e어.md", "a#b.md", "a?b.md",
		"a:b.md", string([]byte{0xd6, 0xd0}) + ".md", // 날것의 GBK 바이트: 잘못된 UTF-8
		strings.Repeat("a", maxSkillPathLen+1),
	}
	for _, in := range bad {
		if got, msg := skillRelPath(in); msg == "" {
			t.Errorf("skillRelPath(%q) = (%q, \"\"), want rejection", in, got)
		}
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// zipFile은 buildZip용 항목 하나를 적습니다.
type zipFile struct {
	name    string
	body    string
	method  uint16
	nonUTF8 bool // write the name bytes as-is (GBK 패키지)
}

func buildZip(t *testing.T, files ...zipFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zipMethodZstd, zstd.ZipCompressor())
	// Deflate64에는 순수 Go 인코더가 없다. 여기서는 그대로 기록하며, 항목에 method 9 표시만 붙이려는 것이다 ——
	// 단언하는 것은 「메서드를 지원하지 않을 때 어떤 안내를 주는지」이며, 실제로 압축을 풀지는 않는다.
	zw.RegisterCompressor(zipMethodDeflate64, func(w io.Writer) (io.WriteCloser, error) {
		return nopWriteCloser{w}, nil
	})
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: f.method, NonUTF8: f.nonUTF8}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", f.name, err)
		}
		if _, err := io.WriteString(w, f.body); err != nil {
			t.Fatalf("write %q: %v", f.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// uploadZip은 zip 바이트를 fsUploadSkill에 보내고 응답을 돌려줍니다.
func uploadZip(t *testing.T, skillDir string, filename string, data []byte) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/skills/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	(&Server{skillDir: skillDir}).fsUploadSkill(rr, req)

	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

const zhSkillMD = "---\nname: 중국어스킬\ndescription: 테스트\n---\n본문\n"

// A zstd-compressed archive (WinZip의 선택적 압축 방식) used to blow up with
// "zip: unsupported compression"으로 터지던 것이, 이제 Deflate 압축처럼 설치됩니다.
func TestUploadSkillZstdAndChineseNames(t *testing.T) {
	dir := t.TempDir()
	data := buildZip(t,
		zipFile{name: "중국어스킬/SKILL.md", body: zhSkillMD, method: zipMethodZstd},
		zipFile{name: "중국어스킬/참고/설명 문서.md", body: "참고", method: zipMethodZstd},
		zipFile{name: "중국어스킬/scripts/run.py", body: "print(1)", method: zip.Deflate},
	)
	rr, out := uploadZip(t, dir, "중국어스킬.zip", data)
	if rr.Code != 201 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if out["name"] != "중국어스킬" {
		t.Fatalf("name = %v, want 중국어스킬", out["name"])
	}
	for _, rel := range []string{"SKILL.md", "참고/설명 문서.md", "scripts/run.py"} {
		if _, err := os.Stat(filepath.Join(dir, "중국어스킬", rel)); err != nil {
			t.Errorf("missing extracted file %q: %v", rel, err)
		}
	}
}

// GBK 파일 이름(7-Zip / 중국어 Windows 탐색기)은 UTF-8이 아니라고 거절하지 말고 디코딩해야 한다.
// 이 샘플 바이트는 GBK로만 표현되는 중국어 파일 이름이라 한글으로 바꾸지 않는다.
func TestUploadSkillGBKNames(t *testing.T) {
	const gbkSkillMD = "---\nname: 中文技能\ndescription: 测试\n---\n正文\n" // han-allow 프로토콜 원문
	gbk := func(s string) string {
		b, err := simplifiedchinese.GBK.NewEncoder().String(s)
		if err != nil {
			t.Fatalf("gbk encode %q: %v", s, err)
		}
		return b
	}
	dir := t.TempDir()
	data := buildZip(t,
		zipFile{name: gbk("中文技能/SKILL.md"), body: gbkSkillMD, method: zip.Deflate, nonUTF8: true}, // han-allow 프로토콜 원문
		zipFile{name: gbk("中文技能/参考资料.md"), body: "内容", method: zip.Deflate, nonUTF8: true},        // han-allow 프로토콜 원문
	)
	rr, out := uploadZip(t, dir, "skill.zip", data)
	if rr.Code != 201 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if out["name"] != "中文技能" { // han-allow 프로토콜 원문
		t.Fatalf("name = %v, want 中文技能", out["name"]) // han-allow 프로토콜 원문
	}
	if _, err := os.Stat(filepath.Join(dir, "中文技能", "参考资料.md")); err != nil { // han-allow 프로토콜 원문
		t.Errorf("GBK-named entry not extracted: %v", err)
	}
}

// 정말 풀 수 없는 압축은 방식 이름을 중국어로 알려야 하고,
// "zip: unsupported compression algorithm"을 그대로 보이면 안 됩니다.
func TestUploadSkillUnsupportedMethod(t *testing.T) {
	data := buildZip(t,
		zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zipMethodDeflate64},
	)
	rr, out := uploadZip(t, t.TempDir(), "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "Deflate64") || !strings.Contains(msg, "지원하지 않는 압축 방식") {
		t.Fatalf("error = %q, want a Chinese message naming Deflate64", msg)
	}
}

func TestUploadSkillEncrypted(t *testing.T) {
	data := buildZip(t, zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zip.Deflate})
	// 로컬 파일 헤더의 "암호화됨" 범용 플래그 비트를 뒤집습니다
	// (오프셋 6)와 중앙 디렉터리 사본(오프셋 8)에서도.
	local := bytes.Index(data, []byte("PK\x03\x04"))
	central := bytes.Index(data, []byte("PK\x01\x02"))
	if local < 0 || central < 0 {
		t.Fatal("could not locate zip headers")
	}
	data[local+6] |= 1
	data[central+8] |= 1

	rr, out := uploadZip(t, t.TempDir(), "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "암호화") {
		t.Fatalf("error = %q, want 암호화 hint", msg)
	}
}

// 경로 검사가 유니코드를 받아도, Zip-slip은 여전히 거절돼야 합니다.
func TestUploadSkillRejectsTraversal(t *testing.T) {
	data := buildZip(t,
		zipFile{name: "demo/SKILL.md", body: "---\nname: demo\n---\n", method: zip.Deflate},
		zipFile{name: "demo/../../evil.sh", body: "rm -rf /", method: zip.Deflate},
	)
	dir := t.TempDir()
	rr, out := uploadZip(t, dir, "demo.zip", data)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "잘못된 경로") {
		t.Fatalf("error = %q, want 잘못된 경로", msg)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("upload left files behind: %v", entries)
	}
}

func TestSkillNameFromFrontmatterQuoted(t *testing.T) {
	got := skillNameFromFrontmatter([]byte("---\nname: \"중국어스킬\"\ndescription: x\n---\n"))
	if got != "중국어스킬" {
		t.Fatalf("name = %q, want 중국어스킬", got)
	}
}
