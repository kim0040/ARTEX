package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths는 격리된 업그레이드 디렉터리를 만듭니다. ResolvePaths()를 바로 쓰면 안 됩니다.
// 테스트 바이너리 자신을 가리켜, 실행하면 go test 실행 파일 이름이 바뀝니다.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin은 실행 가능한 껍데기 스크립트로 artex인 척합니다. smokeTest는 -h로 띄워 종료 코드만 봅니다.
// 스크립트로 충분하고, 진짜 바이너리를 컴파일하는 것보다 훨씬 빠릅니다.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("가짜 바이너리 쓰기 %s: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage는 bin을 "임시 저장되어 갈아 끼우기를 기다림" 상태로 둡니다. artex.new와 체크섬을 씁니다.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("체크섬 계산: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("체크섬 쓰기: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s 읽기: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("가짜 바이너리는 sh 스크립트라 Windows에서 돌릴 수 없습니다")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh는 v를 떼고 tag는 v를 답니다. 양쪽 다 알아봐야 합니다
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 사전 순이 아니라 숫자로 비교
		{"1.0.0", "0.99.99", 1, true},
		// 개발 빌드는 비교 불가로 봐야 합니다. 그렇지 않으면 정식판이 커밋하지 않은 변경을 덮습니다.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, 기대 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, 기대 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 핵심 불변: 업그레이드 파일은 모두 실행 파일과 같은 디렉터리에 있습니다. CWD에 떨어지면
	// 서비스 실행(작업 디렉터리가 / 일 수 있음)의 갈아 끼우기가 완전히 무효가 됩니다.
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s가 실행 파일 디렉터리에 없습니다: %s (기대 %s)", name, path, p.Dir)
		}
	}
	// Windows에서는 .new/.old가 .exe를 유지해야 합니다. 그렇지 않으면 스모크 테스트와 갈아 끼운 뒤 실행이 실패합니다.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows에서 .new/.old는 .exe로 끝나야 합니다: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// 체크섬을 쓴 뒤 파일을 바꿔, 다운로드 손상 / 바꿔치기를 흉내 냅니다.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 불일치로 거부되기를 기대했는데 통과했습니다")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 실행은 되지만 종료 코드가 0이 아님

	if err := verifyStaged(p); err == nil {
		t.Fatal("스모크 테스트 실패로 거부되기를 기대했는데 통과했습니다")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("표시 쓰기: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart를 기대했는데 %v", action)
	}
	if !st.Pending {
		t.Error("갈아 끼운 뒤 상태는 Pending이어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex는 새 버전으로 바뀌어 있어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("옛 버전은 artex.old로 백업되어야 합니다")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("갈아 끼운 뒤 artex.new는 사라져야 합니다")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("갈아 끼운 뒤 체크섬 파일은 지워져야 합니다")
	}
	// 표시는 남아 있어야 합니다. 다음 기동(새 버전이 돔)이 이것으로 횟수를 세고, 필요하면 되돌립니다.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("갈아 끼운 뒤 업그레이드 표시는 남아 있어야 합니다")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // 체크섬을 망가뜨림

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("검증 실패 때는 Continue를 기대했는데 %v", action)
	}
	if !st.FailedStage {
		t.Error("상태는 FailedStage로 표시되어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("검증 실패 때 현재 버전을 건드리면 안 됩니다")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("검증에 실패한 임시 파일은 지워야 합니다. 그렇지 않으면 다음 기동이 다시 시도합니다")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 지난 업그레이드가 남긴 백업
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3로 갈아 끼워야 합니다")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("백업은 방금 내려온 v2로 갱신되어야 합니다")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 처음 maxAttempts번 기동은 횟수만 더합니다. 새 버전이 스스로 설 기회를 줍니다.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d번째 시도는 Continue를 기대했는데 %v", i, action)
		}
		if !st.Pending {
			t.Errorf("%d번째 시도 상태는 Pending이어야 합니다", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d번째 시도 후 attempts=%d(ok=%v), 기대 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// 한 번 더 죽으면 한도를 넘어, 옛 버전으로 자동으로 되돌립니다.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("시도 한도를 넘으면 Restart를 기대했는데 %v", action)
	}
	if !st.RolledBack {
		t.Error("상태는 RolledBack으로 표시되어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("옛 버전으로 되돌려져 있어야 합니다")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("되돌린 뒤 표시는 지워야 합니다. 그렇지 않으면 무한 되돌리기가 됩니다")
	}
	// 뜨지 못한 버전은 바로 지우지 않고 조사할 수 있게 남깁니다.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("실패한 버전은 조사할 수 있게 .failed로 남아야 합니다")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback()은 ResolvePaths()를 탑니다. 여기서는 아래층의 교환 의미만 직접 시험합니다.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("되돌린 뒤 현재 버전은 v1이어야 합니다")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("되돌린 뒤 백업은 v2가 되어야 다시 되돌릴 수 있습니다")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum 출력은 공백 두 칸으로 나눕니다. shasum -a 256의 바이너리 모드는 파일 이름 앞에 *를 붙입니다.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // 필드가 정확히 둘이지만 첫 칸이 요약이 아님
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // 요약 길이가 틀림

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux 항목 해석 오류: %v", out)
	}
	// 요약은 소문자로 통일합니다. 대조할 때 대소문자 때문에 불일치로 오판하지 않습니다.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows 항목 오류(* 접두사는 떼고, 요약은 소문자여야 함): %q", got)
	}
	if len(out) != 2 {
		t.Errorf("빈 줄, 요약이 아닌 줄, 길이가 틀린 줄은 무시해야 하는데 %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("패키지 안 베이스 이름은 Windows에서 artex.exe입니다. 이 케이스는 Unix 이름으로 만듭니다")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 실제 배포 패키지 구조: artex-<버전>-<os>-<arch>/artex, 그리고 방해 파일 몇 개.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("풀어낸 것이 artex 실행 파일이 아닙니다: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("풀어낸 바이너리에는 실행 비트가 있어야 합니다")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("패키지 안에 실행 파일이 없으면 오류여야 합니다")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS가 아님
		"https://evil.com/artex.zip",    // 도메인이 허용 목록에 없음
		"https://github.com.evil.com/x", // 접미사 위장
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) 는 거부해야 합니다", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // 도메인 대소문자를 가리지 않음
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) 는 허용해야 하는데 오류: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh의 package_binary는 artex-<버전>-<os>-<arch>.zip을 쓰고, 버전 번호에서
	// v 접두사를 뗍니다. 여기서 글자 하나가 틀리면 모든 플랫폼의 한 번 업데이트가 자산을 못 찾습니다.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q 해석: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("안정 확인 뒤 업그레이드 표시는 지워야 합니다")
	}
	// 표시가 없으면 이후 정상 재시작은 횟수를 더하지 않고, 되돌리기를 잘못 켜지도 않습니다.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("표시 읽기는 실패해야 합니다")
	}
	// 백업은 남겨 둡니다. 사용자가 아직 수동으로 되돌릴 수 있습니다.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("안정 확인 뒤에도 이전 버전 백업은 남아야 합니다")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 보통 기동 경로. panic도 없고 어떤 파일도 건드리면 안 됩니다
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("표시가 없으면 settle은 어떤 파일에도 영향을 주면 안 됩니다")
	}
}
