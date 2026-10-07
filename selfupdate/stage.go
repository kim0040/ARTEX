package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset은 release.yml이 만드는 체크섬 목록입니다. Release의 zip을 모두 덮습니다.
const sumsAsset = "SHA256SUMS"

// maxBinarySize는 풀어낸 바이너리 크기를 제한합니다. 기형 zip이 디스크를 채우지 못하게 합니다.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase는 업그레이드 과정의 단계입니다. SSE 이벤트의 phase 필드로 그대로 씁니다.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress는 호출자가 줍니다. 진행을 프론트로 밀어 줍니다. pct는 다운로드 단계에서만 의미 있습니다(0-100).
// 다른 단계는 -1을 넘깁니다.
type Progress func(ph Phase, pct int, msg string)

// Stage는 지정한 Release에서 현재 플랫폼 배포 패키지를 받아, 검증한 뒤 새 바이너리를 artex.new로 임시 저장합니다.
//
// 맨 바이너리가 아니라 완전한 zip을 받는 이유는 둘입니다. 기존 Release의 SHA256SUMS는
// 원래 zip만 덮으므로 zip을 쓰면 CI를 바꿀 필요가 없고, 이미 나간 과거 버전과도 맞습니다.
// zip에는 skills/도 들어 있어, 나중에 내장 skill을 맞출 여지가 있습니다. 대가는 skill 수백 KB를 더 받는 것뿐입니다.
//
// 함수가 돌아오면 임시 저장이 끝난 것입니다. 호출자는 이어서 부드럽게 닫고 ExitRestart로 종료합니다.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("이 버전은 %s/%s 배포 패키지를 제공하지 않습니다(%s 없음)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "체크섬 목록을 가져오는 중…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s에 %s가 없어, 검증되지 않은 바이너리 설치를 거부합니다", sumsAsset, name)
	}

	// 임시 파일은 모두 대상 디렉터리에 둡니다. 마지막 rename이 같은 파일 시스템 안의 원자 동작이 되게 합니다
	// (장치를 넘는 rename은 실패하고, /tmp는 종종 별도 마운트입니다).
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s 다운로드 중(%s)…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 검증 중…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256이 일치하지 않습니다. 기대 %s, 실제 %s(다운로드가 손상되었거나 바뀌었습니다)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "압축을 풀고 스모크 테스트 중…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("새 버전을 현재 시스템에서 실행할 수 없습니다: %w", err)
	}

	// 임시 파일 자신의 sha256을 따로 저장합니다. 다음 기동에서 갈아 끼우기 전에 한 번 더 검증합니다.
	// 임시 저장 후 재시작 전에 파일이 바뀌거나 깨지는 것을 막습니다.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("새 바이너리 체크섬 계산: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("체크섬 쓰기: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("새 버전 임시 저장: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// 표시는 자동 되돌리기 능력에만 영향을 줍니다. 임시 파일 자체는 이미 준비됐으므로 이 때문에 업그레이드를 끊지 않습니다.
		prog(PhaseStaged, -1, "경고: 업그레이드 표시를 쓰지 못했습니다. 이번 업그레이드에는 자동 되돌리기 보호가 없습니다")
	}

	prog(PhaseStaged, 100, "새 버전이 준비되었습니다. 재시작 중…")
	return nil
}

// fetchSums는 SHA256SUMS를 받아 해석하고, 파일 이름 → 16진 요약을 돌려줍니다.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("이 Release에 %s가 없어 무결성을 검증할 수 없습니다. 업그레이드를 거부합니다", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s 다운로드: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s 읽기: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 내용이 비었거나 형식을 알 수 없습니다", sumsAsset)
	}
	return out, nil
}

// parseSums는 sha256sum 스타일 목록을 해석해 파일 이름 → 16진 요약을 돌려줍니다.
//
// 첫 필드가 64자리 16진수일 때만 받습니다. "필드가 정확히 둘"만으로는 부족합니다.
// 단어 두 개짜리 설명 줄이 합법 항목으로 들어가 쓰레기 값이 요약 표에 쌓이고,
// 진짜 자산이 잘못된 요약과 맞을 수 있습니다.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 형식은 "<sha256>  <filename>"입니다(sha256sum은 공백 두 칸, shasum의 바이너리
		// 모드는 파일 이름 앞에 *를 붙입니다).
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download는 자산을 dst에 쓰면서 SHA256을 계산하고, Content-Length로 진행을 알립니다.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s 다운로드: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("임시 파일 만들기: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("다운로드 중단: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("디스크에 쓰지 못했습니다: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("다운로드가 불완전합니다. 기대 %d바이트, 실제 %d바이트", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get은 허용 목록에 묶인 GET을 보내고 응답 본문을 돌려줍니다.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary는 배포 패키지에서 artex 실행 파일을 꺼냅니다.
//
// 패키지 안 구조는 artex-<버전>-<os>-<arch>/artex입니다. 여기서는 전체 경로를 잇지 않고 **베이스 이름**으로 맞춥니다.
// 버전 번호는 패키지 이름에 한 번 나옵니다. 글자 하나를 잘못 붙이면 업그레이드 전체가 실패하므로, 베이스 이름이 더 잘 견딥니다.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("배포 패키지 열기: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s 읽기: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("새 바이너리 쓰기: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s 압축 풀기: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("배포 패키지 안의 실행 파일이 %s를 넘습니다. 압축 풀기를 거부합니다", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("배포 패키지 안의 %s는 빈 파일입니다", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("배포 패키지에서 %s를 찾지 못했습니다", want)
}

// checkWritable는 디렉터리가 쓰기 가능한지 미리 확인합니다. 이 단계가 없으면 root가 아닌 실행이거나
// 바이너리가 시스템 디렉터리에 있을 때, 수십 MB를 받은 뒤에야 갈아 끼우는 순간에 실패합니다.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("프로그램 디렉터리 %s에 쓸 수 없어 자동 업데이트가 안 됩니다(권한을 확인하거나 수동 업그레이드를 쓰세요): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter는 쓴 바이트를 세고 보고 빈도를 제한합니다. 32KiB 조각마다 SSE를 보내지 않습니다.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("다운로드 중 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
