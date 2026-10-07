// Command artex 는 ARTEX 백엔드의 진입점입니다.
//
// 초보: 기본 수신 주소는 :8787 이고, 기록 프록시는 127.0.0.1:8788 입니다.
// PostgreSQL 이 자산 그래프(작업들이 공유하는 장부)와 탐색 그래프(작업마다 있는
// 목표·의도·사실·발견)를 담습니다. 트래픽 색인과 본문은 data/ 아래 SQLite 와 파일입니다.
// 화면은 이 프로세스의 JSON API 를 읽습니다. 그래프가 바뀌면 플래너가 깨어나 의도를 만들고,
// 워커는 그 의도 하나를 실행합니다.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/selfupdate"
	"github.com/Autumn-27/artex/server"
)

// version 은 빌드 버전입니다. 릴리스 때 -ldflags "-X main.version=<tag>" 로 넣습니다.
// 로컬 빌드의 기본값은 "dev" 입니다.
var version = "dev"

const banner = `
    _    ____ _____ _______  __
   / \  |  _ \_   _| ____\ \/ /
  / _ \ | |_) || | |  _|  \  /
 / ___ \|  _ < | | | |___ /  \
/_/   \_\_| \_\|_| |_____/_/\_\
`

// printBanner 는 시작 배너와 버전·런타임 정보를 표준 출력에 찍습니다.
func printBanner(addr string) {
	fmt.Print(banner)
	fmt.Println("  AI 자율 모의침투 시스템")
	fmt.Printf("  버전 %s  ·  %s/%s  ·  %s  ·  수신 %s\n\n",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), addr)
}

// main 은 run 의 결과를 프로세스 종료 코드로만 바꿉니다. 종료 코드는 업데이트
// 약속의 일부입니다. start 스크립트가 그 값을 보고 다시 띄울지 정합니다
// (selfupdate.ExitRestart). 그래서 본문은 os.Exit 로 defer 를 건너뛰지 않고
// return 할 수 있는 함수에 둡니다.
func main() {
	os.Exit(run())
}

func run() int {
	var (
		addr    = flag.String("addr", ":8787", "화면과 API를 제공할 주소")
		dataDir = flag.String("data", filepath.Join(config.BaseDir(), "data"), "SQLite 기록과 증거 디렉터리(기본: 실행 파일 옆 data/)")
		proxy   = flag.String("proxy", "127.0.0.1:8788", "트래픽 기록 프록시 주소(빈 문자열이면 끔)")
	)
	flag.Parse()

	// 빌드 버전을 server 패키지에 넘겨, GET /api/health 가 화면 위쪽에 보여 주게 합니다.
	server.BuildVersion = version

	printBanner(*addr)

	// 백엔드 로그를 메모리에도 담습니다(표준 에러에는 그대로 나갑니다). /logs 화면이
	// 실시간 로그를 보게 하려는 것입니다. 시작 로그도 잡으려면 가장 먼저 켭니다.
	server.StartLogCapture()

	// 자체 업데이트: 준비된 바이너리로 바꾸거나, 바꾼 뒤 부팅 횟수를 세어 새 빌드가
	// 계속 죽으면 되돌립니다. 저장소를 열거나 포트를 잡기 전에 해야 합니다. 여기서
	// 끝나고 start 스크립트가 다시 띄울 수 있어서, 그 전에 비용을 들일 이유가 없습니다.
	action, upState := selfupdate.Bootstrap()
	server.SetBootUpdateState(upState)
	if action == selfupdate.Restart {
		return selfupdate.ExitRestart
	}

	// 바이너리가 읽는 설정 파일을 보여 줍니다. 절대 경로라서 `go run` 의 상대
	// "config.json"(현재 작업 디렉터리 기준)도 헷갈리지 않습니다.
	cfgPath := config.Path()
	if abs, e := filepath.Abs(cfgPath); e == nil {
		cfgPath = abs
	}
	if _, e := os.Stat(cfgPath); e == nil {
		log.Printf("[config] 설정 파일: %s", cfgPath)
	} else {
		log.Printf("[config] 설정 파일: %s (없음 — 환경 변수 ARTEX_PG_DSN만 시도합니다)", cfgPath)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, shutdown := shutdownContext(sigCtx)
	defer shutdown(agent.AbortShutdown)

	mgr, err := server.NewManager(*dataDir, *proxy)
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	defer mgr.Close()

	// 여기까지 살아 있으면 방금 바꾼 빌드가 실제로 동작하는 것입니다. 업그레이드
	// 표시를 지우고 시도 횟수를 멈추게 합니다. 그 전까지는 부팅마다 횟수가 늘고,
	// 계속 죽는 빌드는 되돌려집니다.
	settle := time.AfterFunc(selfupdate.SettleDelay, selfupdate.Settle)
	defer settle.Stop()

	skillDir := config.SkillDir()
	if abs, err := filepath.Abs(skillDir); err == nil {
		skillDir = abs
	}
	log.Printf("[config] skill 디렉터리: %s", skillDir)
	srv := server.New(ctx, mgr, skillDir, *dataDir, config.BaseDir())
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ARTEX %s backend listening on %s (data=%s, workers=%d)", version, *addr, *dataDir, mgr.Workers())
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// 나가는 길은 두 가지입니다. 신호면 정상 종료(코드 0, start 스크립트가 루프를 멈춤)이고,
	// 준비된 업데이트나 되돌리기면 코드 75 입니다. 스크립트가 다시 띄우고, 위의
	// bootstrap 이 새 빌드를 설치합니다.
	code := 0
	select {
	case <-ctx.Done():
	case <-server.RestartRequested():
		code = selfupdate.ExitRestart
		shutdown(agent.AbortShutdown)
	}

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return code
}

// shutdownContext 는 일부러 signalCtx 에서 파생하지 않습니다. 그렇게 하면
// 부모의 평범한 context.Canceled 가 AbortShutdown 보다 먼저 자식에 붙어,
// 돌고 있는 에이전트마다 종료 이유가 사라집니다.
func shutdownContext(signalCtx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, shutdown := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-signalCtx.Done():
			shutdown(agent.AbortShutdown)
		case <-ctx.Done():
		}
	}()
	return ctx, shutdown
}
