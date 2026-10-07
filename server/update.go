package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// 페이지 원클릭 업데이트의 HTTP 면이다. 실제 다운로드/검증/교체 로직은 전부 selfupdate 패키지에 있다. 이 면은 UI가 엔진에 업데이트를 맡기는 입구이며, 자산 그래프·탐색 그래프와는 별개다.
// 여기서는 인증 경계, 동시성 상호 배제, 진행 방송, 그리고 "이제 종료해야 한다"를 main에 알리는 일만 맡는다.
//
// 재시작은 이 프로세스가 하지 않는다. 새 버전을 임시 저장한 뒤 프로세스는 selfupdate.ExitRestart로 빠져나가고,
// 데몬 스크립트(start.sh / start.bat, Docker에서는 ENTRYPOINT)가 다시 띄운다.

// restartCh는 업그레이드가 준비되거나 롤백이 끝난 뒤 닫히고, main은 그것을 받은 뒤 ExitRestart로 종료한다.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested는 "종료하고 데몬 프로세스가 나를 다시 띄우게 해 달라"일 때 닫히는 channel을 반환한다.
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState는 이번 기동 시 selfupdate.Bootstrap의 결론이다(업그레이드 성공 / 방금 롤백 /
// 임시 저장분이 버려짐). main이 주입하며, /api/update/check가 프론트엔드에 지난 업그레이드 결과를 그대로 알리게 한다. 이 값은 UI 표시용이며 자산 그래프·탐색 그래프와는 별개다.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState는 main이 기동할 때 한 번 호출한다.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache는 GitHub 최신 버전 조회 결과를 캐시한다.
//
// 상단 바의 "새 버전 있음" 안내는 페이지 전체를 불러올 때마다 한 번 조회하는데, 인증하지 않은 GitHub API는
// IP마다 시간당 60회다. 캐시가 없으면 탭을 몇 개 열거나 페이지를 몇 번 새로고침하는 것만으로 할당량이 바닥나고,
// 그 뒤에 정말 업데이트하려 하면 오히려 조회가 안 된다. 사용자가 "업데이트 확인"을 직접 누르면 force로 캐시를 건너뛸 수 있다.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch는 데이터를 가져오는 함수이고, 테스트용으로만 남겨 둔 주입 지점이다. nil이면 실제 GitHub 조회로 간다.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 실패 결과도 잠깐 캐시한다. 그렇지 않으면 GitHub에 닿지 않을 때 페이지를 불러올 때마다 타임아웃을 한 번씩 끝까지 기다리게 되며,
	// TTL은 짧게 두어, 네트워크가 돌아오면 곧 스스로 회복되게 한다.
	releaseErrTTL = 2 * time.Minute
	// 조회에 쓰는 타임아웃이다. NewClient의 30분 타임아웃은 패키지 전체 다운로드용이라, 버전 조회를 그만큼 기다릴 수는 없다.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get은 최신 Release를 반환하고, 캐시에 있으면 네트워크에 가지 않는다.
//
// 데이터를 가져오는 동안 락을 계속 잡는다. 동시에 온 요청은 각자 GitHub를 치지 않고, 한 번 조회한 결과를 같이 기다리며
// (페이지가 막 열릴 때 여러 탭이 동시에 조회하는 때가 속도 제한에 가장 걸리기 쉽다).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// 요청이 취소된 것(사용자가 탭을 닫음)은 GitHub에 문제가 있다는 뜻이 아니니 캐시에 적지 않는다.
	// 그렇지 않으면 다음 방문자가 영문도 모를 "취소됨" 오류를 받는다.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress는 프론트엔드로 보내는 진행 한 건이다.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // 다운로드 단계에서만 의미가 있고, 나머지는 -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub는 한 번의 업그레이드 진행을 들고 SSE 구독자에게 방송한다. 이 허브는 UI에 진행을 보여 주며 자산 그래프·탐색 그래프와는 별개다.
//
// running은 상호 배제도 겸한다. 업그레이드 중에 POST /api/update/apply를 다시 하면 바로 409이고,
// 두 goroutine이 같은 artex.new에 동시에 쓰지 않게 한다.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin은 업그레이드 권한을 선점하고, 이미 진행 중이면 false를 반환한다.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "준비 중…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish는 한 번의 업그레이드를 마친다. err가 nil이면 임시 저장에 성공해 재시작을 기다린다.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "업데이트 실패", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "새 버전이 준비되었습니다. 재시작 중…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout은 h.mu를 잡은 상태에서 호출해야 한다. 구독자 channel에는 버퍼가 있고, 가득 차면 버린다.
// 진행은 버려도 되는 짧은 정보라, 멈춘 SSE 연결 하나가 업그레이드 자체를 막아서는 안 된다.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck는 GitHub의 최신 정식판을 조회해 현재 버전과 비교한다.
//
// 프론트엔드도 api.github.com에 직접 연결한다(GitHub의 CORS는 *). 그러나 **이 인터페이스를 기준**으로 한다:
// 다운로드는 백엔드가 하므로, 백엔드가 GitHub에 닿아야 업데이트가 된다. 브라우저는 연결되고 서버는 연결되지 않는
// 경우가 흔하다(서버가 내부망이거나, 프록시가 브라우저에만 있는 경우). 그때 업데이트를 누르면 반드시 실패하니,
// 확인 단계에서 사실대로 오류를 내는 편이 낫다.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// 상단 바 안내는 캐시를 쓴다(기본). 사용자가 "업데이트 확인"을 누르면 force=1로 원본을 다시 조회한다.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 개발 빌드(dev / git describe에 접미사가 붙음)에는 비교할 버전 번호가 없다. 풀어 주면
		// 정식판이 로컬에서 디버그 중인 바이너리를 덮어쓰므로, 업데이트를 아예 주지 않는다.
		out["reason"] = fmt.Sprintf("현재 버전 %q은(는) 정식 릴리스가 아니어서 원클릭 업데이트가 비활성화되었습니다", current)
	}
	writeJSON(w, 200, out)
}

// updateApply는 새 버전을 받아 잠시 저장하고, 끝나면 프로세스를 종료해 감시 스크립트가 다시 시작하게 한다.
//
// 즉시 202를 반환한다. 실제 작업은 백그라운드 goroutine에서 돈다. 패키지 전체 다운로드는 몇 분이 걸릴 수 있어,
// 요청에 묶어 두면 리버스 프록시 타임아웃에 끊긴다. 진행 상황은 /api/update/stream으로 간다.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// 캐시를 탄다. 설치되는 것이 사용자가 화면에서 보고 확인한 바로 그 버전이어야 한다.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("현재 버전 %q은(는) 정식 릴리스가 아니어서 원클릭 업데이트가 비활성화되었습니다", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("현재 최신 버전 %s입니다", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "이미 업데이트가 진행 중입니다")
		return
	}

	go func() {
		// 요청의 ctx가 아니라 s.ctx를 일부러 쓴다. HTTP 응답이 반환되면 요청은 끝나고,
		// 거기에 걸어 두면 다운로드가 바로 취소된다.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 업데이트 실패: %v", err)
			return
		}
		log.Printf("[update] %s → %s이(가) 임시 저장되었습니다. 교체를 마치기 위해 곧 종료합니다", current, rel.TagName)
		// 마지막 진행 상황을 프론트엔드로 보낼 시간을 조금 둔 뒤 종료를 일으킨다.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback은 이전 버전(교체 전에 백업한 artex.old)으로 직접 되돌린다.
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "업데이트가 진행 중이라 롤백할 수 없습니다")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 이전 버전으로 수동 롤백했습니다. 전환을 마치기 위해 곧 종료합니다")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream은 SSE로 업데이트 진행 상황을 보낸다.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "스트리밍을 지원하지 않습니다")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 먼저 현재 상태를 한 줄 보탠다. 페이지를 새로고침해도 진행 중인 업그레이드를 바로 볼 수 있다.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
