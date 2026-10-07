// Package selfupdate implements ARTEX의 화면 한 번 업데이트입니다. GitHub Release에서
// 새 바이너리를 받아 검증하고 임시로 둔 뒤, 다음 기동 때 한 번에 갈아 끼웁니다.
//
// 초보용: 화면의 업데이트 버튼이 이 패키지로 옵니다. 플래너·워커·메인 에이전트와는
// 별개로, 프로그램 자신을 바꿉니다. 실패하면 이전 실행 파일로 돌아갑니다.
//
// 역할 나눔(start.sh / start.bat 참고):
//
//	기동 스크립트 = 단순한 데몬 루프. 프로세스가 끝난 뒤 종료 코드로 다시 띄울지만 정함
//	이 패키지     = 틀리기 쉬운 전부(다운로드 / SHA256 검증 / 스모크 / 갈아 끼우기 / 실패 시 되돌리기)
//
// 갈아 끼우기를 스크립트가 아니라 Go에 둔 이유: sha256 검증과 스모크 테스트를 sh와 bat에
// 두 벌로 써야 하고(sha256sum / shasum / certutil), 그 부분이 가장 틀리면 안 됩니다.
// 실행되지 않는 바이너리를 올리면 데몬이 그대로 반복해서 띄우고, 사용자는 머신에 들어가 손으로 고쳐야 합니다.
//
// 한 번의 완전한 업그레이드는 프로세스 기동 세 번을 거칩니다.
//
//	① 옛 server가 /api/update/apply를 받음 → 다운로드 검증 → artex.new로 임시 저장 → exit 75
//	② 스크립트가 옛 버전을 다시 띄움 → Bootstrap이 artex.new를 발견 → 검증+스모크 → 갈아 끼우기 → exit 75
//	③ 스크립트가 다시 띄우면 이미 새 버전 → Bootstrap이 시도를 한 번 기록 → 기동 성공 후 표시를 지움
//
// 어느 단계든 실패하면 옛 버전으로 돌아갑니다. ② 검증 실패면 임시 파일을 지우고 옛 버전을 계속 실행합니다.
// ③ 표시를 지우기 전에 3번 연속 살아남지 못하면(뜨자마자 죽음) artex.old로 자동으로 되돌립니다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart는 "데몬이 나를 다시 띄워 달라"는 종료 코드입니다(EX_TEMPFAIL). 기동 스크립트는
// 이것을 보면 바로 다시 실행하고, 충돌 대기에 넣지 않습니다. 0은 사용자가 정상 중지(스크립트가 루프를 나감)이고, 나머지는 충돌로 봅니다.
const ExitRestart = 75

// maxAttempts는 갈아 끼운 뒤 허용하는 기동 시도 횟수입니다. 새 버전은 기동할 때마다 횟수를 +1하고,
// settleDelay를 넘기면 표시를 지웁니다. maxAttempts번 연속으로 죽으면 새 버전은 뜨지 못하는 것이므로 자동으로 되돌립니다.
const maxAttempts = 3

// Paths는 한 번 업그레이드에 쓰이는 모든 파일입니다. 모두 **실행 파일이 있는 디렉터리**에 둡니다.
// CWD를 일부러 쓰지 않습니다. 서비스로 돌 때 작업 디렉터리는 / 이거나 임의의 경로일 수 있어,
// CWD를 쓰면 임시 파일이 다른 곳에 떨어져 갈아 끼우기가 바로 무효가 됩니다.
type Paths struct {
	Dir     string // 실행 파일이 있는 디렉터리
	Current string // 지금 실행 중인 바이너리        artex      / artex.exe
	New     string // 임시로 둔 새 버전            artex.new  / artex.new.exe
	Sum     string // 새 버전의 sha256(hex)  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 갈아 끼우기 전에 백업한 옛 버전      artex.old  / artex.old.exe
	Marker  string // 업그레이드 상태 표시            artex.upgrade.json
}

// ResolvePaths는 현재 실행 파일에서 업그레이드 경로를 모두 이끌어 냅니다.
//
// Windows에서는 .new/.old에도 .exe 접미사가 있어야 합니다. 없으면 스모크 테스트와
// 갈아 끼운 뒤의 실행이 실패합니다. 그래서 접미사를 떼고 다시 붙입니다. 두 플랫폼의 이름이 대칭이 됩니다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 찾기: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows에서는 ".exe", Unix에서는 보통 비어 있음
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker는 한 번 갈아 끼운 진행을 기록합니다. 새 버전이 뜨지 못하면 자동 되돌리기를 켭니다.
type marker struct {
	From     string `json:"from"`     // 업그레이드 전의 버전
	To       string `json:"to"`       // 목표 버전
	Attempts int    `json:"attempts"` // 갈아 끼운 뒤 기동을 시도한 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged는 임시 파일을 지웁니다. 갈아 끼우기 성공, 검증 실패, 사용자 취소가 모두 여기를 탑니다.
// 남은 artex.new가 다음 기동 때 다시 시도되지 않게 합니다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions는 버전 번호 둘을 비교해 -1/0/1을 돌려줍니다(a<b / a==b / a>b).
// ok=false는 적어도 한쪽이 비교할 수 있는 버전 번호가 아니라는 뜻입니다(예: 로컬 개발 빌드의 "dev",
// git describe가 만든 "0.3.7-2-gabc1234-dirty"). 이때 호출자는 한 번 업데이트를 꺼야 합니다.
// 그렇지 않으면 개발 중인 빌드를 정식판으로 "업그레이드"해 커밋하지 않은 변경을 덮어씁니다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion은 "v0.3.7" / "0.3.7" 형태의 버전 번호를 [3]int로 해석합니다.
//
// 깨끗한 세 칸만 받습니다. build.sh는 tag가 아닌 빌드에서 git describe로
// "0.3.7-2-gabc1234"처럼 접미사가 있는 버전을 만듭니다. 그것은 비교 불가로 봐야 하고
// 0.3.7로 취급하면 안 됩니다. 그렇지 않으면 개발 빌드가 "이미 최신"으로 오판되거나 정식판에 덮입니다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker는 프로세스가 컨테이너 안에서 도는지 알립니다. Docker에서는 갈아 끼우기가 컨테이너의 쓰기 층에 쓰입니다.
// `docker compose up -d`로 컨테이너를 다시 만들면 이미지에 들어 있던 버전으로 돌아갑니다. 이것은 예상된 동작입니다
// (그때 사용자는 원래 새 이미지를 받는 중입니다). 다만 프론트가 이 사실을 말로 설명할 수 있어야 합니다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
