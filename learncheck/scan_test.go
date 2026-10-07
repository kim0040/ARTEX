package learncheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanGoAllowsPromptBodiesAndFlagsTheRest(t *testing.T) {
	src := `package p

// 자산 그래프는 회사 단위로 공유되는 장부다.
const workerDefaultTmpl = ` + "`" + `你是执行者。完成这一条意图。` + "`" + `

func constraintBlock() string {
	return "【操作约束】违反即不得进行"
}

func greet() string {
	return "你好"
}

type T struct {
	killWork func(intentID int64) error
}

func also() string {
	return "世界"
}
`
	hits := scanGoSource("p.go", src)
	var kinds []string
	for _, h := range hits {
		kinds = append(kinds, h.Kind+":"+strings.TrimSpace(h.Text))
	}
	got := strings.Join(kinds, "\n")
	if strings.Contains(got, "你是执行者") {
		t.Fatalf("prompt const was flagged:\n%s", got)
	}
	if strings.Contains(got, "操作约束") {
		t.Fatalf("prompt function string was flagged:\n%s", got)
	}
	if !strings.Contains(got, "你好") || !strings.Contains(got, "世界") {
		t.Fatalf("ordinary string was not flagged:\n%s", got)
	}
	if strings.Contains(got, "자산 그래프") {
		t.Fatalf("Korean comment was flagged:\n%s", got)
	}
}

func TestScanGoAllowsHanStringOnMarkedLine(t *testing.T) {
	src := "package p\nconst p = \"你是连接测试\" // han-allow\nconst q = \"你好\"\n"
	hits := scanGoSource("p.go", src)
	got := ""
	for _, h := range hits {
		got += h.Text
	}
	if strings.Contains(got, "你是连接测试") {
		t.Fatalf("marked line was flagged: %s", got)
	}
	if !strings.Contains(got, "你好") {
		t.Fatalf("unmarked string was not flagged: %v", hits)
	}
}

func TestScanGoFlagsHanComment(t *testing.T) {
	hits := scanGoSource("p.go", "package p\n// 这是注释\nvar x = 1\n")
	if len(hits) != 1 || hits[0].Kind != "comment" {
		t.Fatalf("hits=%v", hits)
	}
}

func TestREADMEStatesTheStructureABeginnerNeeds(t *testing.T) {
	root := filepath.Join("..")
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if containsHan(text) {
		t.Fatal("README.md still contains Han ideographs")
	}
	for _, needle := range []string{
		"cmd/artex",
		"자산 그래프",
		"탐색 그래프",
		"앵커",
		"플래너",
		"워커",
		"의도",
		"`server`",
		"`agent`",
		"`db`",
		"`traffic`",
		"`guard`",
		"`intercept`",
		"`enrich`",
		"`web`",
		"`skills`",
		"127.0.0.1:8788",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("README missing %q", needle)
		}
	}
}
