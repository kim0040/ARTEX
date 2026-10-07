package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv는 스모크 테스트로 띄운 자식 프로세스가 Bootstrap을 바로 건너뛰게 합니다.
//
// 없어도 보통은 괜찮습니다. 자식의 os.Executable()이 artex.new라, 거기서 나온
// 경로는 모두 .new가 붙어 진짜 업그레이드 파일을 건드리지 않습니다. 다만 그 우연에
// 기대면 약합니다. 명시적으로 끊는 편이 분명하고, 자식의 쓸데없는 디스크 탐색도 줄어듭니다.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action은 Bootstrap이 main에 주는 지시입니다.
type Action int

const (
	// Continue: 서버를 평소처럼 시작합니다.
	Continue Action = iota
	// Restart: 곧바로 ExitRestart로 빠져, 데몬 스크립트가 다시 띄우게 합니다.
	Restart
)

// State는 이번 기동의 업그레이드 상태입니다. /api/update/check가 프론트에
// "지난 업그레이드가 성공했는지, 되돌려졌는지"를 그대로 알립니다.
type State struct {
	Pending     bool   // 갈아 끼운 뒤 아직 안정이 확인되지 않음
	RolledBack  bool   // 이번 기동에서 방금 자동 되돌리기를 실행함
	FailedStage bool   // 임시 파일 검증/스모크가 실패해 버렸음
	Detail      string // 사용자에게 보여 주는 한 문장 설명
}

// Bootstrap은 main의 맨 앞에서 돕니다. 포트를 열거나 데이터베이스를 열기 전에 호출해야 합니다.
//
// 초보용: 화면의 한 번 업데이트가 받아 둔 새 바이너리를, 다음 기동 때 제자리로 바꿉니다.
// 실패하면 이전 버전으로 돌아갑니다. 플래너·워커와는 별개로, 프로그램 자신을 바꾸는 문입니다.
//
// 세 가지 상황:
//
//	① 임시 파일 artex.new가 있음 → 검증 + 스모크. 통과하면 갈아 끼우고 재시작을 요구. 실패하면 버리고 옛 버전을 계속 실행
//	② 표시 파일만 남음 → 방금 갈아 끼웠다는 뜻. 시도를 한 번 더함. 연속 실패가 충분하면 되돌림
//	③ 아무것도 없음 → 정상 기동
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 자기 기동을 건너뜁니다: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged는 "임시 파일이 있는" 상황을 처리합니다. 검증을 통과하면 갈아 끼우고, 실패하면 버립니다.
//
// 업그레이드 경로에서 실행 파일을 덮는 유일한 곳이고, 마지막 문입니다. 스모크 테스트가
// 다운로드 손상, 아키텍처 착오, 동적 링크 누락 같은 문제를 막습니다. 실행되지 않는
// 바이너리를 통과시키면 데몬 스크립트가 끝없이 다시 띄우고, Go 코드는 돌 기회가 없어
// 자동 되돌리기도 시작할 수 없습니다.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 임시로 둔 새 버전이 검증을 통과하지 못해 버렸습니다. 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전 검증에 실패해 버렸습니다: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 갈아 끼우기에 실패해 현재 버전을 계속 실행합니다: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "갈아 끼우기 실패: " + err.Error()}
	}

	// 갈아 끼우기 성공. 표시는 남겨, 다음 기동(그때는 새 버전)이 안정인지 확인하게 합니다.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표시를 쓰지 못했습니다(자동 되돌리기 능력을 잃음): %v", err)
	}
	log.Printf("[update] %s(으)로 갈아 끼웠습니다. 재시작을 위해 종료합니다(exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback은 "갈아 끼운 뒤의 기동"을 처리합니다. 시도 횟수를 더하고, 한도를 넘으면 옛 버전으로 되돌립니다.
//
// 횟수는 Go 코드가 뜬 뒤에만 늘어납니다. 그래서 "실행은 되지만 초기화에서 죽는"
// 고장(설정이 안 맞음, 포트가 점유됨, DB 마이그레이션이 실패)을 다룹니다.
// "아예 exec가 안 되는" 경우는 갈아 끼우기 전의 스모크 테스트가 막습니다. 둘을 합쳐야 완전합니다.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// 되돌리기까지 실패하면 다시 재시작하지 않습니다. 그렇지 않으면 무한 재시작에 빠집니다. 표시를 지우고
			// 프로세스를 현재 상태 그대로 띄웁니다. 뜨지 못하면 사용자는 적어도 로그에서 이유를 볼 수 있습니다.
			log.Printf("[update] 새 버전이 %d번 연속 기동에 실패했고, 되돌리기도 실패했습니다: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전 기동에 실패했고 되돌리기도 실패했습니다: " + err.Error()}
		}
		log.Printf("[update] 새 버전이 %d번 연속 기동에 실패해 %s(으)로 되돌렸습니다. 재시작을 위해 종료합니다(exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전 기동에 실패해 %s(으)로 되돌렸습니다", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업그레이드 표시 갱신 실패: %v", err)
	}
	log.Printf("[update] 새 버전 기동 중(%d/%d번째 시도). 안정적으로 돌면 업그레이드를 확정합니다",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle은 새 버전이 안정적으로 돈 것을 확인하고 업그레이드 표시를 지웁니다.
//
// main이 HTTP 수신을 연 뒤에 지연 호출합니다. 이 시간을 넘겨야 인정됩니다. 그렇지 않으면
// 표시가 그대로 남고, 다음 기동이 시도 횟수를 더해 되돌리기가 켜질 때까지 갑니다.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 업그레이드 뒤의 기동이 아니므로 할 일이 없음
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 업그레이드 표시를 지우지 못했습니다: %v", err)
		return
	}
	log.Printf("[update] 새 버전이 안정적으로 돕니다. 업그레이드 완료(이전 버전은 %s로 남김)", p.Old)
}

// SettleDelay는 "새 버전이 살아남았다"고 볼 실행 시간입니다.
const SettleDelay = 30 * time.Second

// verifyStaged는 임시 파일을 검증합니다. SHA256을 먼저 맞춘 뒤, 실제로 한 번 띄웁니다.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256이 일치하지 않습니다(다운로드가 손상되었거나 바뀌었습니다)")
	}
	return smokeTest(p.New)
}

// smokeTest는 -h로 새 바이너리를 띄워, 이 시스템에서 정말 실행되는지 확인합니다.
// 다운로드가 잘림, 아키텍처가 틀림(exec format error), 의존성이 없음 같은 문제를 막습니다.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리가 응답하지 않음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap은 현재 바이너리를 임시로 둔 새 버전으로 바꿉니다.
//
// Unix와 Windows 모두 실행 중인 실행 파일의 rename을 허용합니다(Windows가 막는 것은
// 삭제와 덮어쓰기이고, rename은 아닙니다). 그래서 플랫폼을 나누거나 먼저 자신을 멈출 필요가 없습니다.
func swap(p Paths) error {
	// Windows의 rename은 이미 있는 대상을 덮지 않습니다. 지난 업그레이드가 남긴 .old를 먼저 지워야 합니다.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("옛 백업 정리 %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 갈아 끼우기는 실패했는데 현재 버전은 이미 옮겨졌습니다. 그대로 되돌려야 합니다. 그렇지 않으면 다음 기동에 실행 파일이 없습니다.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전을 넣지 못했고(%v) 현재 버전 복구도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 넣기: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback은 swap이 백업한 옛 버전으로 되돌립니다.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("되돌릴 백업이 없습니다 %s: %w", p.Old, err)
	}
	// 뜨지 못하는 새 버전은 바로 지우지 않고 .failed로 옮겨 조사할 수 있게 남깁니다.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 옮기기: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("옛 버전 복구: %w", err)
	}
	return nil
}

// Rollback은 /api/update/rollback의 구현입니다. 사용자가 이전 버전으로 되돌립니다.
// 갈아 끼우기만 하고, 재시작은 데몬 스크립트에 맡깁니다(호출자가 이어서 ExitRestart로 종료).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("되돌릴 이전 버전이 없습니다(" + p.Old + " 이 없습니다)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 되돌리기를 거부합니다: %w", err)
	}
	// 현재와 백업을 바꿉니다. 되돌린 뒤에도 다시 되돌릴 수 있습니다.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 옮기기: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 넣기: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] 되돌린 뒤 백업 정리 실패(실행에는 영향 없음): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup은 되돌릴 이전 버전이 있는지 알립니다. 프론트가 되돌리기 버튼을 보일지 정합니다.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
