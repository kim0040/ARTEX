// Package learncheck는 학습용 포크에 일부러 남기지 않은 한자가 있는지 검사한다.
//
// 이 패키지는 제품을 실행하지 않습니다. 학습용 포크에서 문서, 주석, 사람에게
// 보이는 문자열이 한국어로 바뀌었는지 확인합니다. 모델에게 넘기는 절차 원문,
// skills 디렉터리, AGPL LICENSE 는 허용 목록이라 원문을 유지해도 실패하지 않습니다.
package learncheck

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Han은 r이 CJK 통합 한자 블록 U+4E00–U+9FFF 안에 있는지 보고한다.
func Han(r rune) bool { return r >= 0x4E00 && r <= 0x9FFF }

// Hit는 허용 목록 밖에 한자가 남은 한 줄이다.
type Hit struct {
	Path string
	Line int
	Text string
	Kind string // 종류: doc, comment, string, ui
}

func (h Hit) String() string {
	return h.Path + ":" + itoa(h.Line) + ": " + h.Kind + ": " + oneLine(strings.TrimSpace(h.Text))
}

// promptConsts는 문자열 본문이 업스트림 LLM 프롬프트인 Go 상수 이름이다.
// 이 바이트는 업스트림 원문 그대로 둔다. 번역하면 에이전트 동작이 바뀌고,
// 절차 본문이 한국어 실행 안내가 된다.
var promptConsts = map[string]bool{
	"autoDefaultTmpl":         true,
	"pentestDefaultTmpl":      true,
	"DefaultAssistantPrompt":  true,
	"ReporterDefaultPrompt":   true,
	"goalsDefaultTmpl":        true,
	"goalsScopeTail":          true,
	"plannerDefaultTmpl":      true,
	"workerDefaultTmpl":       true,
	"mainAgentDefaultTmpl":    true,
	"RetesterDefaultPrompt":   true,
	"compressionSystemPrompt": true,
	"JudgeContextBoundary":    true,
	"JudgeOutputContract":     true,
	"DefaultJudgePrompt":      true,
	"findingTrafficGuidance":  true,
}

// promptFuncs는 문자열 리터럴이 시스템 프롬프트에 붙는 함수다.
// 함수 안의 주석은 그대로 검사한다.
var promptFuncs = map[string]bool{
	"workerTrafficBlock": true,
	"artifactSpec":       true,
	"workerArtifactSpec": true,
	"chatWorkDirSpec":    true,
	"constraintBlock":    true,
}

// Scan은 root를 걷고 허용 목록에 없는 한자 적중을 돌려준다.
func Scan(root string) ([]Hit, error) {
	var hits []Hit
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "skills", "screenshots":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "LICENSE" || strings.HasPrefix(rel, "web/LICENSE") || rel == "learncheck/scan_test.go" {
			// scan_test.go는 허용 목록 단위 테스트를 위해 한자를 일부러 넣는다.
			return nil
		}
		var h []Hit
		switch {
		case rel == "README.md" || rel == "CHANGELOG.md" || (strings.HasPrefix(rel, "docs/") && strings.HasSuffix(rel, ".md")):
			h, err = scanPlain(rel, path, "doc")
		case strings.HasSuffix(rel, ".go"):
			h, err = scanGoFile(rel, path)
		case strings.HasPrefix(rel, "web/src/") && isWebSrc(rel):
			h, err = scanPlain(rel, path, "ui")
		case isUserFacingScript(rel):
			h, err = scanPlain(rel, path, "ui")
		}
		if err != nil {
			return err
		}
		hits = append(hits, h...)
		return nil
	})
	return hits, err
}

func isWebSrc(rel string) bool {
	return strings.HasSuffix(rel, ".ts") || strings.HasSuffix(rel, ".tsx") || strings.HasSuffix(rel, ".mjs") || strings.HasSuffix(rel, ".css")
}

func isUserFacingScript(rel string) bool {
	switch rel {
	case "install.sh", "update.sh", "start.sh", "start.bat", "dev.sh", "build.sh", "reset-password.sh",
		"docker-compose.yml", "docker-compose.bench.yml", "config.example.json":
		return true
	default:
		return strings.HasPrefix(rel, ".github/") && (strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml"))
	}
}

func scanPlain(rel, path, kind string) ([]Hit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var hits []Hit
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if strings.Contains(line, "han-allow") {
			continue
		}
		if containsHan(line) {
			hits = append(hits, Hit{Path: rel, Line: n, Text: line, Kind: kind})
		}
	}
	return hits, sc.Err()
}

func scanGoFile(rel, path string) ([]Hit, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return scanGoSource(rel, string(b)), nil
}

type scope struct {
	depth int
	name  string
}

// scanGoSource는 주석과 문자열 리터럴을 검사한다. 허용된 프롬프트 상수의
// 본문이거나, 허용된 프롬프트 함수 안에 있는 문자열은 건너뛴다.
// 줄에 han-allow 표시가 있으면 그 줄도 건너뛴다.
func scanGoSource(rel, src string) []Hit {
	var hits []Hit
	line := 1
	i := 0
	depth := 0
	var scopes []scope
	expectFuncBrace := false
	pendingFunc := ""
	constName := ""

	inPrompt := func() bool {
		if promptConsts[constName] {
			return true
		}
		for _, s := range scopes {
			if promptFuncs[s.name] {
				return true
			}
		}
		return false
	}

	for i < len(src) {
		switch src[i] {
		case '\n':
			line++
			i++
			continue
		case ' ', '\t', '\r':
			i++
			continue
		}

		if strings.HasPrefix(src[i:], "//") {
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			text := src[i : i+end]
			if containsHan(text) && !strings.Contains(text, "han-allow") {
				hits = append(hits, Hit{Path: rel, Line: line, Text: text, Kind: "comment"})
			}
			i += end
			continue
		}
		if strings.HasPrefix(src[i:], "/*") {
			end := strings.Index(src[i+2:], "*/")
			text := src[i:]
			next := len(src)
			if end >= 0 {
				text = src[i : i+4+end]
				next = i + 4 + end
			}
			if containsHan(text) && !strings.Contains(text, "han-allow") {
				hits = append(hits, Hit{Path: rel, Line: line, Text: oneLine(text), Kind: "comment"})
			}
			line += strings.Count(text, "\n")
			i = next
			continue
		}
		if src[i] == '`' || src[i] == '"' {
			text, next, nl := readString(src, i)
			// 문자열 자체나, 그 문자열이 시작되는 소스 줄에 han-allow 가 있으면 건너뛴다.
			// 로그 분류 키워드와 연결 테스트 프롬프트처럼 동작 때문에 원문을 남기는 줄이다.
			if containsHan(text) && !inPrompt() && !strings.Contains(text, "han-allow") && !strings.Contains(lineTextAt(src, i), "han-allow") {
				hits = append(hits, Hit{Path: rel, Line: line, Text: oneLine(text), Kind: "string"})
			}
			line += nl
			constName = ""
			i = next
			continue
		}
		if src[i] == '\'' {
			_, next, nl := readRune(src, i)
			line += nl
			i = next
			continue
		}
		if src[i] == '{' {
			depth++
			if expectFuncBrace {
				scopes = append(scopes, scope{depth: depth, name: pendingFunc})
				expectFuncBrace = false
				pendingFunc = ""
			}
			i++
			continue
		}
		if src[i] == '}' {
			if len(scopes) > 0 && scopes[len(scopes)-1].depth == depth {
				scopes = scopes[:len(scopes)-1]
			}
			if depth > 0 {
				depth--
			}
			i++
			continue
		}
		if isIdentStart(src[i]) {
			j := i + 1
			for j < len(src) && isIdentCont(src[j]) {
				j++
			}
			word := src[i:j]
			rest := trimLeftSpace(src[j:])
			switch word {
			case "func":
				if name, ok := declarationFuncName(rest); ok {
					pendingFunc = name
					expectFuncBrace = true
				}
			case "const":
				if name, ok := leadingIdent(rest); ok && promptConsts[name] {
					constName = name
				}
			default:
				if promptConsts[word] && strings.HasPrefix(rest, "=") {
					constName = word
				}
			}
			i = j
			continue
		}
		if src[i] == ';' {
			constName = ""
		}
		i++
	}
	return hits
}

// declarationFuncName은 함수 선언이나 메서드 선언의 이름을 돌려준다.
// 함수 타입(`fn func(int) int`)과 함수 리터럴(`go func() {`)은 false다.
// 그 중괄호는 평범한 블록으로 남긴다.
func declarationFuncName(rest string) (string, bool) {
	rest = trimLeftSpace(rest)
	if rest == "" {
		return "", false
	}
	if strings.HasPrefix(rest, "(") {
		// 메서드 리시버이거나, '('로 시작하는 함수 타입·리터럴이다.
		end := matchingParen(rest)
		if end < 0 {
			return "", false
		}
		after := trimLeftSpace(rest[end+1:])
		name, ok := leadingIdent(after)
		if !ok {
			return "", false
		}
		// 메서드 이름 뒤에는 매개변수 목록이 온다. 결과 타입
		// (`error`, `string`, `int`)은 그렇지 않다.
		afterName := trimLeftSpace(after[len(name):])
		if !strings.HasPrefix(afterName, "(") && !strings.HasPrefix(afterName, "[") {
			return "", false
		}
		return name, true
	}
	name, ok := leadingIdent(rest)
	if !ok {
		return "", false
	}
	afterName := trimLeftSpace(rest[len(name):])
	if !strings.HasPrefix(afterName, "(") && !strings.HasPrefix(afterName, "[") {
		return "", false
	}
	return name, true
}

// lineTextAt은 i가 속한 소스 한 줄을 돌려준다. 줄 끝 주석의 han-allow 를 보기 위해서다.
func lineTextAt(src string, i int) string {
	start := 0
	if i > 0 {
		if p := strings.LastIndexByte(src[:i], '\n'); p >= 0 {
			start = p + 1
		}
	}
	end := strings.IndexByte(src[i:], '\n')
	if end < 0 {
		return src[start:]
	}
	return src[start : i+end]
}

func matchingParen(s string) int {
	if s == "" || s[0] != '(' {
		return -1
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		case '"', '`', '\'':
			_, next, _ := readAnyString(s, i)
			if next <= i {
				return -1
			}
			i = next - 1
		}
	}
	return -1
}

func readString(src string, i int) (text string, next int, newlines int) {
	if src[i] == '`' {
		end := strings.IndexByte(src[i+1:], '`')
		if end < 0 {
			text = src[i:]
			return text, len(src), strings.Count(text, "\n")
		}
		text = src[i : i+end+2]
		return text, i + end + 2, strings.Count(text, "\n")
	}
	j := i + 1
	for j < len(src) {
		if src[j] == '\\' && j+1 < len(src) {
			j += 2
			continue
		}
		if src[j] == '"' {
			j++
			break
		}
		j++
	}
	text = src[i:j]
	return text, j, strings.Count(text, "\n")
}

func readRune(src string, i int) (text string, next int, newlines int) {
	return readStringLike(src, i, '\'')
}

func readAnyString(src string, i int) (text string, next int, newlines int) {
	switch src[i] {
	case '`', '"':
		return readString(src, i)
	case '\'':
		return readRune(src, i)
	default:
		return "", i, 0
	}
}

func readStringLike(src string, i int, quote byte) (string, int, int) {
	j := i + 1
	for j < len(src) {
		if src[j] == '\\' && j+1 < len(src) {
			j += 2
			continue
		}
		if src[j] == quote {
			j++
			break
		}
		j++
	}
	text := src[i:j]
	return text, j, strings.Count(text, "\n")
}

func containsHan(s string) bool {
	for _, r := range s {
		if Han(r) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 180 {
		return s[:180]
	}
	return s
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func isIdentCont(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func trimLeftSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return s[i:]
}

func leadingIdent(s string) (string, bool) {
	s = trimLeftSpace(s)
	if s == "" || !isIdentStart(s[0]) {
		return "", false
	}
	j := 1
	for j < len(s) && isIdentCont(s[j]) {
		j++
	}
	return s[:j], true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
