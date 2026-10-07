// Package traffic 는 기록 프록시다.
//
// 초보: 도구와 브라우저가 보낸 HTTP 를 기본 127.0.0.1:8788 에서 받아 둡니다.
// 본문은 파일 트리로, 목록 검색은 SQLite 색인으로 남습니다. 이 기록은 지워도 되는
// 작업 일지이고, PostgreSQL 의 자산 그래프(작업들이 공유하는 장부)나 탐색 그래프
// (작업마다 있는 목표·의도·사실·발견)와는 별도입니다. 발견 보고서가 증거를 고정할 때
// 이 기록을 참조합니다. 대상 HTTP 만 평문으로 남기며, 내용을 가리지 않습니다.
package traffic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
	_ "modernc.org/sqlite"
)

const indexSchema = `
CREATE TABLE IF NOT EXISTS exchanges (
  id           TEXT PRIMARY KEY,
  ts           INTEGER,
  host         TEXT,
  method       TEXT,
  url_template TEXT,
  url          TEXT,
  status       INTEGER,
  content_type TEXT,
  req_len      INTEGER,
  resp_len     INTEGER,
  path         TEXT
);
CREATE INDEX IF NOT EXISTS idx_ex_host ON exchanges(host);
CREATE INDEX IF NOT EXISTS idx_ex_tmpl ON exchanges(host, url_template);
CREATE INDEX IF NOT EXISTS idx_ex_ts   ON exchanges(ts);
CREATE INDEX IF NOT EXISTS idx_ex_status ON exchanges(status);
CREATE INDEX IF NOT EXISTS idx_ex_resp   ON exchanges(resp_len);

CREATE TABLE IF NOT EXISTS exchange_bodies (
  id        TEXT PRIMARY KEY,
  req_head  TEXT NOT NULL DEFAULT '',
  req_body  BLOB,
  req_blob  TEXT,
  resp_head TEXT NOT NULL DEFAULT '',
  resp_body BLOB,
  resp_blob TEXT
);

CREATE TABLE IF NOT EXISTS blob_refs (
  hash        TEXT NOT NULL,
  exchange_id TEXT NOT NULL,
  PRIMARY KEY (hash, exchange_id)
);
CREATE INDEX IF NOT EXISTS idx_blob_refs_ex ON blob_refs(exchange_id);
`

// ftsSchema 는 indexSchema 와 따로 적용합니다. FTS5 가 없는 드라이버는
// 기록기 전체를 내리지 않고 "전문 검색 없음"으로 내려앉습니다.
// 기본 unicode61 이 아니라 trigram 인 이유는 둘입니다. 아무 부분 문자열이나
// 맞추고("ssw0r" 가 "P@ssw0rd" 를 찾음), unicode61 이 토큰으로 쪼개지 못하는
// 한중일 글자도 다룹니다. content 가 비어 있으면 색인만 남습니다. 본문은
// exchange_bodies 에 있기 때문입니다. contentless_delete 는 원래 글을 다시
// 넣지 않고도 행을 지울 수 있게 합니다.
const ftsSchema = `CREATE VIRTUAL TABLE IF NOT EXISTS ex_fts USING fts5(
  content, tokenize='trigram', content='', contentless_delete=1
);`

const (
	// maxInlineBody 는 본문을 SQLite 칸에 둘지, 내용 주소 blob 저장소로
	// 넘길지 가르는 크기입니다.
	maxInlineBody = 256 * 1024
	// blobPreview 는 넘긴 본문 중 칸에 남겨 두는 양입니다. 파일을 다시 받지
	// 않고도 JSON 모양, SQL 덤프 머리, ZIP 매직을 알아볼 수 있습니다.
	blobPreview = 8 * 1024
	// maxIndexBody 는 본문 하나가 전문 색인에 넣는 상한입니다. 이보다 작은
	// 글은 전부 색인하고, 이진 데이터는 색인에 넣지 않습니다.
	maxIndexBody = 4 * 1024 * 1024
	// minTrigram 은 trigram 토크나이저가 맞출 수 있는 가장 짧은 길이입니다.
	// 더 짧으면 메타데이터 LIKE 로 돌아갑니다.
	minTrigram = 3
	// maxBlobRead 는 traffic_blob 한 번이 읽는 상한입니다. 큰 본문을
	// 넘기며 에이전트 컨텍스트를 채우지 않게 합니다.
	maxBlobRead = 8 * 1024
	// autoVacuumIncremental 은 auto_vacuum=incremental 의 SQLite 숫자 값입니다.
	autoVacuumIncremental = 2
	// reclaimChunkPages 는 잠금을 잡은 한 단계가 파일 시스템으로 되돌리는
	// 색인 공간의 상한입니다(페이지 4KiB, 약 32MB). 회수는 쓰기 잠금을 잡고,
	// record() 는 go-mitmproxy 가 클라이언트에 답하기 전에 돕니다. 한도를
	// 없애면 기록 중인 요청이 멈춥니다.
	reclaimChunkPages = 8192
	// reclaimMergePages 는 조각마다 같이 하는 전문 병합의 상한입니다.
	// contentless_delete 색인에서 지우면 묘비 표시만 씁니다. 병합이 그것을
	// 버리고, 안 하면 지울 때마다 색인이 커집니다.
	reclaimMergePages = 256
	// reclaimMergeSteps 는 회수 한 번이 하는 병합 횟수입니다. fts5 는 색인이
	// 안정됐는지 묻는 방법이 없습니다. 특수 INSERT 가 스스로 행 변경을
	// 기록해서, 변경 횟수로는 진짜 병합과 빈 동작을 가를 수 없습니다.
	// 그래서 예산만 쓰고, 남은 것은 다음 삭제에 넘깁니다.
	reclaimMergeSteps = 16
	// reclaimMaxSteps 는 루프의 안전 장치입니다. 병합과 incremental_vacuum 은
	// 결국 진행이 멈춘다고 문서에 있습니다. 수렴하지 않는 회수는 쓰기 잠금을
	// 쥔 채 돌지 말고 포기해야 합니다.
	reclaimMaxSteps = 512
	// reclaimBudget 은 배경 회수 한 번의 시간 상한입니다. 바쁜 호스트를
	// 지우면 기가바이트가 나올 수 있고, 다음 삭제는 여기서 멈춘 곳부터 이어갑니다.
	reclaimBudget = 5 * time.Minute
)

// TrafficSearchDescription 은 새 설치와 업그레이드 설치의 도구 목록에 남습니다.
// 실행 중 도구와 목록 이전이 어긋나지 않게 traffic 패키지에 둡니다.
// 초보: 아래 문자열은 모델에게 가는 도구 설명이라 원문 그대로입니다.
const TrafficSearchDescription = "查询记录代理已抓取的目标流量（必须指定 host；支持裸主机、主机:端口或完整 URL，可再按 URL 子串或正文关键词过滤）。指定端口时只返回该服务的流量，避免同一 IP 的不同端口串包。body_contains 会在已抓取的请求/响应头与正文中做全文搜索，支持任意子串和中文（至少 3 个字符）。仅返回极轻量索引(id/method/url/status/resp_len)，不含响应内容；结果非空后必须用 traffic_get 逐条核实请求/响应，再把确实支持当前漏洞的 ID 交给 bind_finding_traffic。默认只返回 3 条、每页最多 10 条；结果多时用 page 翻页。" // han-allow 업스트림 프롬프트·픽스처

// Traffic 는 기록 프록시를 돌리고 파일 트리와 색인을 가집니다.
// 초보: 여기 데이터는 PostgreSQL 그래프가 아닙니다. 지워도 되는 HTTP 일지입니다.
type Traffic struct {
	dir   string
	addr  string
	db    *sql.DB
	wmu   sync.Mutex // record() 와 DeleteHost(blob 수거 포함)를 직렬화
	seq   atomic.Int64
	proxy *mproxy.Proxy
	// fts 는 전문 색인이 있는지입니다. FTS5 없는 빌드에서는 거짓입니다.
	// 기록과 메타데이터 검색은 되고, 본문 검색만 미지원으로 내려앉으며 오류는 아닙니다.
	fts bool
	// reaping 은 옛 트리와 색인 공간을 배경에서 회수 중인지 셉니다.
	// 종료와 테스트가 그것과 경합하지 않고 기다리게 합니다.
	reaping sync.WaitGroup
	// incrementalVacuum 는 색인이 빈 페이지를 스스로 파일 시스템에 되돌릴 수
	// 있는지입니다. 이 기본값이 생기기 전에 만든 DB 에서는 거짓입니다.
	// 거기서는 PRAGMA incremental_vacuum 이 조용히 아무 일도 안 해서,
	// 파일 전체를 압축하기 전에는 지워도 공간이 안 돌아옵니다.
	incrementalVacuum bool
	// reclaiming 은 배경 회수를 한 번에 하나만 돌립니다. 동시에 여러 개면
	// wmu 만 다투고 더 빨리 끝나지 않습니다.
	reclaiming atomic.Bool
	// closed 는 Close 가 닫습니다. 회수는 남은 예산을 버리고, 종료를
	// 몇 분씩 붙잡지 않습니다. 영값이면 nil 이고, stopping() 은 그것을
	// "닫는 중이 아님"으로 봅니다.
	closed    chan struct{}
	closeOnce sync.Once
	// pass 는 MITM 이 프록시·프로토콜 문제로 깨진 호스트 집합입니다.
	// 그 연결은 투명하게 터널(실패 시 열림)해서, 기록을 못 해도 요청은
	// 대상에 도달하고 죽이지 않습니다.
	pass sync.Map // 호스트 이름(string) → struct{}
	// upstream 은 잡은 요청을 모두 넘기는 전역 출구 프록시입니다.
	// nil 이면 대상에 직접 접속합니다. 가로챈 경로와 투명 통과 경로가
	// 모두 이것을 따릅니다(go-mitmproxy 의 getUpstreamConn). 그래서
	// 어떤 호스트도 빠져나가지 않습니다. SetUpstreamProxy 로 실행 중에 바꿉니다.
	upstream atomic.Pointer[url.URL]
}

// Open 은 dir 아래에 트래픽 트리, blob 저장소, SQLite 색인을 준비합니다.
func Open(dir, addr string) (*Traffic, error) {
	for _, d := range []string{dir, filepath.Join(dir, "_index"), filepath.Join(dir, "_blobs"), filepath.Join(dir, "_ca")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// busy_timeout 은 연결마다의 설정이라, 한 번의 Exec 가 아니라 DSN 에 둡니다.
	// 풀은 필요할 때 연결을 열고, Exec 는 그 요청을 처리한 연결만 바꿉니다.
	// 그러면 다른 연결은 쓰는 쪽이 데이터베이스를 잡는 순간 바로 실패합니다.
	// 드라이버는 DSN 을 첫 '?' 에서 자르므로, 경로에 '?' 가 있으면 다른 파일을
	// 조용히 가리킵니다. 그 경로는 맨 DSN 으로 돌아가고, initIndex 가
	// 자기 연결에 pragma 를 여전히 적용합니다.
	index := filepath.Join(dir, "_index", "index.sqlite")
	dsn := index
	if !strings.ContainsRune(index, '?') {
		dsn = "file:" + index + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	t := &Traffic{dir: dir, addr: addr, db: db, closed: make(chan struct{})}
	if err := t.initIndex(); err != nil {
		db.Close()
		return nil, err
	}

	p, err := mproxy.NewProxy(&mproxy.Options{
		Addr:        addr,
		SslInsecure: true,
		CaRootPath:  filepath.Join(dir, "_ca"),
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	// 출구 선택. 전역 출구 프록시가 없으면 대상에 직접 접속합니다.
	// go-mitmproxy 기본 출구는 http.ProxyFromEnvironment 라서, 환경의
	// HTTP_PROXY/HTTPS_PROXY(VPN·시스템 프록시)가 있으면 대상 요청을 그
	// 바깥 프록시로 넘깁니다. 그 프록시가 대상에 못 가면 502 가 됩니다.
	// nil 을 돌려주면 직접 접속입니다. 전역 출구가 설정되면(SetUpstreamProxy)
	// 잡은 요청은 가로챈 것이든 투명 터널이든 그쪽으로 넘깁니다.
	// 그래서 어떤 호스트도 진짜 출발 IP 를 흘리지 않습니다.
	p.SetUpstreamProxy(func(*http.Request) (*url.URL, error) { return t.upstream.Load(), nil })
	// 실패 시 열림: 기본은 모든 호스트를 MITM 합니다. 다만 이전 요청이
	// 깨지지 않고는 가로챌 수 없다고 보여 준 호스트는 예외입니다(maybePassthrough).
	// 그 호스트는 투명 터널이라, 요청을 죽이지 않고 대상까지 보냅니다.
	p.SetShouldInterceptRule(func(req *http.Request) bool {
		_, tunnel := t.pass.Load(hostOnly(req.Host))
		return !tunnel
	})
	p.AddAddon(&sink{t: t})
	t.proxy = p
	return t, nil
}

// initIndex 는 하나의 고정 연결에 schema 를 적용합니다. 고정이 auto_vacuum 을
// 믿게 합니다. 이 값은 테이블이 아직 없을 때만 정할 수 있고, 이어지는 VACUUM 이
// 파일 헤더에 씁니다. 풀링된 *sql.DB 는 두 단계를 다른 연결로 보낼 수 있어,
// 설정이 조용히 사라질 수 있습니다.
//
// auto_vacuum=incremental 이 있어야 삭제가 빈 페이지를 파일 시스템으로
// 되돌립니다. 없으면 SQLite 는 프리리스트에만 매달아서, 트래픽을 아무리
// 지워도 색인 파일은 줄지 않습니다. maxInlineBody 보다 작은 본문이 그 파일에
// 있고, trigram 색인은 글의 약 두 배라, 캡처가 많은 설치는 이미 없는
// 트래픽을 위해 기가바이트를 붙잡습니다. 테이블이 이미 있으면 이 pragma 는
// 문서상 아무 일도 안 합니다. 그 전에 만든 설치는 전체 압축으로 바꾸기 전까지
// auto_vacuum=0 입니다. incrementalVacuum 가 그 사실을 기억해서, 회수가
// 공간을 되돌리는 척하지 않게 합니다.
func (t *Traffic) initIndex() error {
	ctx := context.Background()
	conn, err := t.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// DSN 에만 맡기지 않고 다시 적용합니다. Open 의 맨 DSN 우회로도 맞으려면
	// 그렇습니다. journal_mode 는 파일 헤더에 남고, 이후 연결은 그것을 읽습니다.
	for _, p := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	var tables int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		for _, p := range []string{`PRAGMA auto_vacuum=incremental`, `VACUUM`} {
			if _, err := conn.ExecContext(ctx, p); err != nil {
				return err
			}
		}
	}
	var mode int
	if err := conn.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	t.incrementalVacuum = mode == autoVacuumIncremental
	if !t.incrementalVacuum {
		log.Printf("[traffic] 인덱스 저장소가 증분 회수를 켜지 않았습니다(auto_vacuum=%d). 트래픽을 지워도 index.sqlite 는 줄지 않습니다. 저장소 압축을 한 번 실행해 전환해야 합니다", mode)
	}
	if _, err := conn.ExecContext(ctx, indexSchema); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, ftsSchema); err != nil {
		log.Printf("[traffic] 전문 인덱스를 쓸 수 없어 본문 검색을 끕니다(메타데이터 검색은 영향 없음): %v", err)
		return nil
	}
	t.fts = true
	return nil
}

// hostOnly 는 선택적인 :포트를 뺍니다. 통과 키가 "example.com:443"(CONNECT) 이든
// "example.com"(요청 URL) 이든 같게 맞춥니다.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// ProxyAddr 는 워커가 HTTP(S)_PROXY 로 넣을 주소입니다. 맨 ":포트" 는
// "모든 인터페이스에 바인드"(예전 기본값)라, 루프백으로 바꿉니다.
func (t *Traffic) ProxyAddr() string {
	if strings.HasPrefix(t.addr, ":") {
		return "http://127.0.0.1" + t.addr
	}
	return "http://" + t.addr
}

// SetUpstreamProxy 는 잡은 요청을 모두 전역 출구 프록시로 보냅니다
// (http/https/socks5, URL 에 user:pass 가능). 빈 문자열은 지우고 직접 접속으로
// 돌아갑니다. 바뀜은 원자적이고 다음 연결부터 적용됩니다. 재시작이나 프록시
// 재조립은 없습니다. go-mitmproxy 가 세 스킴을 직접 다이얼하므로, 대상 도구와
// 상관없이 socks5 가 여기서 같이 동작합니다.
func (t *Traffic) SetUpstreamProxy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		t.upstream.Store(nil)
		return nil
	}
	u, err := ValidateProxyURL(raw)
	if err != nil {
		return err
	}
	t.upstream.Store(u)
	return nil
}

// ValidateProxyURL 은 프록시 URL 을 해석하고 검사합니다(http/https/socks5,
// user:pass 가능). 해석된 URL 을 돌려줍니다. 기록 프록시가 꺼져 있어도
// 저장하기 전에 전역 프록시를 검사할 수 있게 열어 둡니다.
func ValidateProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("프록시 주소 %q 해석: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	case "":
		return nil, fmt.Errorf("프록시 %q 에 프로토콜이 없습니다(http://, https:// 또는 socks5://)", raw)
	default:
		return nil, fmt.Errorf("지원하지 않는 프록시 프로토콜 %q(http, https 또는 socks5)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("프록시 %q 에 호스트 주소가 없습니다", raw)
	}
	return u, nil
}

// CACertPath 는 클라이언트가 MITM 프록시의 HTTPS 를 믿으려면 신뢰할 PEM CA
// 인증서 경로입니다. go-mitmproxy 가 첫 기동 때 여기에 씁니다.
func (t *Traffic) CACertPath() string {
	return filepath.Join(t.dir, "_ca", "mitmproxy-ca-cert.pem")
}

// Start 는 프록시를 돌립니다. 호출을 막으므로 고루틴에서 실행하세요.
func (t *Traffic) Start() error { return t.proxy.Start() }

// Close 는 배경의 트리 회수가 끝난 뒤에 색인을 닫습니다. 종료가 데이터
// 디렉터리를 지운 뒤에도 파일을 푸는 고루틴을 남기지 않게 합니다. 색인 공간
// 회수는 먼저 멈추라고 알립니다. 그 예산은 몇 분이고, 그것을 끝내려고 종료를
// 미룰 가치는 없습니다. 다음 삭제가 이어서 합니다.
func (t *Traffic) Close() error {
	if t.closed != nil {
		t.closeOnce.Do(func() { close(t.closed) })
	}
	t.reaping.Wait()
	return t.db.Close()
}

// stopping 은 Close 가 호출됐는지 보고합니다. nil 채널(영값)은 준비되지
// 않으므로, 별도 가드 없이 "닫는 중이 아님"으로 읽힙니다.
func (t *Traffic) stopping() bool {
	select {
	case <-t.closed:
		return true
	default:
		return false
	}
}
func (t *Traffic) DB() *sql.DB { return t.db }

// sink 는 끝난 교환을 기록하는 go-mitmproxy 애드온입니다.
type sink struct {
	mproxy.BaseAddon
	t *Traffic
}

func (s *sink) Response(f *mproxy.Flow) {
	if f.Request == nil || f.Response == nil {
		return
	}
	s.t.record(f)
}

// RequestError 는 이미 맺은 MITM 터널의 요청이 실패할 때 돕니다. 실패가
// 프록시·프로토콜 때문이면(h2 quirk, 본문 있는 HEAD, 프로토콜 오류) —
// 대상에 닿지 않는 평범한 오류가 아니면 — 그 호스트를 투명 통과로 표시합니다.
// 다음 요청은 죽지 않고 성공합니다.
func (s *sink) RequestError(f *mproxy.Flow, err error) { s.t.maybePassthrough(f, err) }

// maybePassthrough 는 다음 연결에서 그 호스트를 투명 터널로 표시합니다.
// 프록시 자신이 일으킨 오류만 해당합니다. 대상이 그냥 죽거나 걸러진 것은
// 우리가 없어도 실패하므로, MITM 과 기록을 유지해야 합니다.
func (t *Traffic) maybePassthrough(f *mproxy.Flow, err error) {
	if err == nil || f == nil || f.Request == nil || f.Request.URL == nil || !proxyCausedErr(err) {
		return
	}
	host := f.Request.URL.Hostname()
	if host == "" {
		return
	}
	if _, loaded := t.pass.LoadOrStore(host, struct{}{}); !loaded {
		log.Printf("[traffic] %s 와 MITM 이 실패해 그대로 통과시킵니다(이 host 는 이후 대상에 직접 연결하고 기록하지 않지만, 요청은 그대로 갑니다): %v", host, err)
	}
}

// proxyCausedErr 는 오류가 대상이 아니라 가로채기 층 때문인지 보고합니다.
// HTTP/2 처리, 본문 있는 HEAD, 프로토콜 위반입니다.
func proxyCausedErr(err error) bool {
	s := strings.ToLower(err.Error())
	for _, p := range []string{"head request", "http2", "http/2", "protocol error", "protocol_error", "malformed"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// record 는 교환 하나를 SQLite 에만 남깁니다. 메타데이터, 본문, 전문 색인입니다.
// 요청마다 디렉터리를 만들지 않습니다. maxInlineBody 를 넘는 본문만
// 내용 주소 blob 저장소로 넘깁니다.
func (t *Traffic) record(f *mproxy.Flow) {
	// 쓰기 잠금이 blob 과 색인 쓰기를 덮습니다. DeleteHost(와 blob 수거)가
	// 같은 잠금 아래에서, 동시에 도는 record 와 경합하지 않습니다.
	t.wmu.Lock()
	defer t.wmu.Unlock()
	host := f.Request.URL.Hostname()
	method := f.Request.Method
	tmpl := db.TemplatePath(f.Request.URL.EscapedPath())
	n := t.seq.Add(1)
	now := time.Now()
	id := fmt.Sprintf("%d-%04d", now.Unix(), n%10000)

	ct := f.Response.Header.Get("Content-Type")
	url := f.Request.URL.String()
	reqHead := fmt.Sprintf("%s %s %s\n%s", method, f.Request.URL.RequestURI(), f.Request.Proto, requestHeaderLines(f.Request))
	respHead := fmt.Sprintf("HTTP %d\n%s", f.Response.StatusCode, headerLines(f.Response.Header))
	// 본문은 트랜잭션을 열기 전에 넘깁니다. blob 쓰기는 파일 시스템 일이라
	// SQLite 쓰기 잠금 안에 두면 안 됩니다.
	reqB := t.spill(f.Request.Body, f.Request.Header.Get("Content-Type"))
	respB := t.spill(f.Response.Body, ct)

	tx, err := t.db.Begin()
	if err != nil {
		log.Printf("[traffic] %s 기록 실패(트랜잭션 시작): %v", url, err)
		return
	}
	defer tx.Rollback() //nolint:errcheck // 커밋된 뒤에는 아무 일도 없음

	// DB 에 있는 교환의 path 는 비웁니다. path 가 있으면 본문이 아직 옛
	// 디스크 트리에 있는 레거시 행입니다(Get 을 보세요).
	res, err := tx.Exec(`INSERT OR REPLACE INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,'')`,
		id, now.Unix(), host, method, tmpl, url, f.Response.StatusCode, ct,
		len(f.Request.Body), len(f.Response.Body))
	if err != nil {
		log.Printf("[traffic] %s 기록 실패(인덱스 쓰기): %v", url, err)
		return
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		log.Printf("[traffic] %s 기록 실패(rowid 읽기): %v", url, err)
		return
	}

	if _, err := tx.Exec(`INSERT OR REPLACE INTO exchange_bodies(id,req_head,req_body,req_blob,resp_head,resp_body,resp_blob)
VALUES(?,?,?,?,?,?,?)`,
		id, reqHead, reqB.inline, nullIfEmpty(reqB.hash), respHead, respB.inline, nullIfEmpty(respB.hash)); err != nil {
		log.Printf("[traffic] %s 기록 실패(본문 쓰기): %v", url, err)
		return
	}

	for _, h := range []string{reqB.hash, respB.hash} {
		if h == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO blob_refs(hash,exchange_id) VALUES(?,?)`, h, id); err != nil {
			log.Printf("[traffic] %s 기록 실패(blob 참조 등록): %v", url, err)
			return
		}
	}

	if t.fts {
		// 색인은 메모리에서 채웁니다. _blobs 로 넘긴 본문도, 칸에는 미리보기만
		// 있어도 전문 검색이 됩니다.
		idx := strings.Join([]string{url, reqHead, reqB.index, respHead, respB.index}, "\n")
		if _, err := tx.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, rowid, idx); err != nil {
			log.Printf("[traffic] %s 기록 실패(전문 인덱스 쓰기): %v", url, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[traffic] %s 기록 실패(커밋): %v", url, err)
	}
}

// storedBody 는 칸에 둘지 넘길지 정한 뒤의 본문입니다. inline 은 SQLite 칸에
// 넣는 값(본문 전체, 또는 넘겼을 때의 미리보기)입니다. hash 는 넘긴 blob 의
// 이름이고, index 는 FTS 에 넘기는 글입니다. 이진이면 비웁니다.
type storedBody struct {
	inline []byte
	hash   string
	index  string
}

// spill 은 본문을 어디에 둘지 정합니다. 작은 본문은 칸에 남습니다. 큰 본문은
// 내용 주소 저장소에 쓰고, 알아볼 수 있는 미리보기만 칸에 남겨 blob 을
// 다시 받지 않아도 되게 합니다. 어느 쪽이든 글 본문은 전문 색인에
// 전부 넘깁니다(maxIndexBody 까지). 색인은 바이트가 어디에 있는지와 별개입니다.
func (t *Traffic) spill(body []byte, contentType string) storedBody {
	if len(body) == 0 {
		return storedBody{}
	}
	text := !isBinaryBody(contentType, body)
	indexText := func() string {
		if !text {
			return ""
		}
		return string(clipBytes(body, maxIndexBody))
	}
	if len(body) <= maxInlineBody {
		return storedBody{inline: body, index: indexText()}
	}

	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	// 버킷 한 단계(256개)면 디렉터리 하나가 커지지 않습니다. 저장소는
	// maxInlineBody 를 넘는 본문만, 해시로 중복을 없앤 채 둡니다.
	blobDir := filepath.Join(t.dir, "_blobs", "sha256", h[:2])
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		log.Printf("[traffic] blob 디렉터리 만들기 실패: %v", err)
		return storedBody{inline: clipBytes(body, blobPreview), index: indexText()}
	}
	blobPath := filepath.Join(blobDir, h+".bin")
	if _, err := os.Stat(blobPath); os.IsNotExist(err) {
		if err := os.WriteFile(blobPath, body, 0o644); err != nil {
			log.Printf("[traffic] blob %s 쓰기 실패: %v", h, err)
			return storedBody{inline: clipBytes(body, blobPreview), index: indexText()}
		}
	}
	sb := storedBody{hash: h, index: indexText()}
	if text {
		sb.inline = []byte(truncateUTF8(body, blobPreview))
	} else {
		sb.inline = []byte(binaryTag(contentType, body))
	}
	return sb
}

// binaryTypes 는 글처럼 색인하거나 미리볼 가치가 없는 content-type 접두사입니다.
// 목록에 없으면 글으로 봅니다(NUL 바이트 검사가 마지막 안전장치). 그래서
// application/sql 이나 맨 text/* 처럼 드물지만 검색할 만한 타입이
// 색인에서 조용히 빠지지 않습니다.
var binaryTypes = []string{
	"image/", "audio/", "video/", "font/",
	"application/octet-stream", "application/zip", "application/gzip",
	"application/x-gzip", "application/x-tar", "application/x-7z-compressed",
	"application/x-rar", "application/pdf", "application/x-msdownload",
	"application/vnd.android.package-archive", "application/java-archive",
	"application/wasm", "application/x-shockwave-flash",
}

func isBinaryBody(contentType string, body []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	for _, p := range binaryTypes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	// 잘못 붙었거나 없는 content-type 의 안전장치입니다. 진짜 글에는 NUL 이
	// 없으므로, 본문 앞에 NUL 이 있으면 이진으로 봅니다.
	return bytes.IndexByte(clipBytes(body, 512), 0) >= 0
}

// binaryTag 는 넘긴 이진 본문을 한 줄로 적습니다. 앞의 매직 바이트를 넣어,
// 파일을 받지 않고도 형식을 알아볼 수 있게 합니다.
func binaryTag(contentType string, body []byte) string {
	ct := strings.TrimSpace(contentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	magic := hex.EncodeToString(clipBytes(body, 4))
	return fmt.Sprintf("[이진 %s, %d바이트, magic=%s]", ct, len(body), magic)
}

func clipBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// truncateUTF8 는 여러 바이트 문자를 쪼개지 않고 b 를 최대 n 바이트로 자릅니다.
// 미리보기가 한중일 글자 중간에서 끝나지 않게 합니다.
func truncateUTF8(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	b = b[:n]
	// 룬은 최대 4바이트라, 3바이트만 뒤로 물라도 경계를 찾습니다.
	// 입력이 이미 잘못된 UTF-8 이면 자른 그대로 둡니다.
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func headerLines(h map[string][]string) string {
	var b strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// requestHeaderLines 는 HTTP Host 헤더를 되돌립니다. net/http 는 그것을
// Header 가 아니라 Request.Host 에 둡니다. request.http 에 남겨야 원문
// 캡처가 완전하고 그대로 다시 보낼 수 있습니다.
func requestHeaderLines(req *mproxy.Request) string {
	headers := req.Header.Clone()
	headers.Del("Host")
	host := req.URL.Host
	if raw := req.Raw(); raw != nil && strings.TrimSpace(raw.Host) != "" {
		host = raw.Host
	}
	var b strings.Builder
	if host = strings.TrimSpace(host); host != "" {
		b.WriteString("Host: ")
		b.WriteString(host)
		b.WriteByte('\n')
	}
	b.WriteString(headerLines(headers))
	return b.String()
}

func sanitize(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "*", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	out := r.Replace(s)
	if out == "" || out == "_" {
		return "root"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

var blobHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blobPath 는 저장된 blob 위치를 찾습니다. 지금 쓰기는 버킷 한 단계이고,
// 그 전에 쓴 blob 은 두 단계였습니다. 옮기지 않고 두 배치를 모두 봅니다.
func (t *Traffic) blobPath(hash string) (string, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	// 파일 시스템을 건드리기 전에 순수 16진수인지 확인합니다. 조작된 hash 가
	// blob 디렉터리 밖으로 나가지 못하게 합니다.
	if !blobHashRe.MatchString(hash) {
		return "", fmt.Errorf("blob hash 가 올바르지 않습니다")
	}
	for _, p := range []string{
		filepath.Join(t.dir, "_blobs", "sha256", hash[:2], hash+".bin"),
		filepath.Join(t.dir, "_blobs", "sha256", hash[:2], hash[2:4], hash+".bin"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("blob %s 이 없습니다", hash)
}

// Blob 은 넘긴 본문을 스트림으로 엽니다. 호출자가 파일을 닫아야 합니다.
// 스트림이 필요한 이유: 드라이버에 조각 BLOB API 가 없어서, 큰 본문을
// 디스크에 둬야 통째로 올리지 않고 내줄 수 있습니다.
func (t *Traffic) Blob(hash string) (*os.File, int64, error) {
	p, err := t.blobPath(hash)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// BlobRange 는 offset 부터 최대 length 바이트를 읽고, blob 전체 크기를
// 함께 알립니다. 에이전트 도구가 큰 본문을 모델 컨텍스트에 전부 넣지 않고
// 페이지로 보게 합니다.
func (t *Traffic) BlobRange(hash string, offset, length int64) (data []byte, total int64, err error) {
	f, size, err := t.Blob(hash)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if offset < 0 {
		offset = 0
	}
	if offset >= size {
		return nil, size, nil
	}
	if length <= 0 || offset+length > size {
		length = size - offset
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, size, err
	}
	return buf[:n], size, nil
}

// ExchangeMeta 는 색인 한 행입니다. Search 가 돌려줍니다.
type ExchangeMeta struct {
	ID          string `json:"id"`
	TS          int64  `json:"ts"`
	Host        string `json:"host"`
	Method      string `json:"method"`
	URLTemplate string `json:"url_template"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	RespLen     int    `json:"resp_len"`
	Path        string `json:"path"`
}

// Search 는 페이지로 나눈 교환 메타데이터를 돌려줍니다. 본문은 없습니다.
func (t *Traffic) Search(host string, page, size int) ([]ExchangeMeta, error) {
	if size <= 0 || size > 500 {
		size = 100
	}
	q := `SELECT id,ts,host,method,url_template,url,status,content_type,resp_len,path FROM exchanges`
	args := []any{}
	if host != "" {
		q += ` WHERE host=?`
		args = append(args, host)
	}
	q += ` ORDER BY ts DESC LIMIT ? OFFSET ?`
	args = append(args, size, page*size)
	rows, err := t.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExchangeMeta
	for rows.Next() {
		var m ExchangeMeta
		if err := rows.Scan(&m.ID, &m.TS, &m.Host, &m.Method, &m.URLTemplate, &m.URL, &m.Status, &m.ContentType, &m.RespLen, &m.Path); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ftsFilter 는 색인된 글이 term 과 맞는 행만 남기는 SQL 조건을 만듭니다.
// ok 가 거짓이면 전문 검색이 그 단어를 처리할 수 없습니다. 이 빌드에 FTS
// 색인이 없거나, trigram 이 못 맞추는 세 글자 미만입니다. 호출자는
// 메타데이터 검색으로 돌아갑니다.
func (t *Traffic) ftsFilter(term string) (cond string, arg any, ok bool) {
	term = strings.TrimSpace(term)
	if !t.fts || utf8.RuneCountInString(term) < minTrigram {
		return "", nil, false
	}
	return `rowid IN (SELECT rowid FROM ex_fts WHERE ex_fts MATCH ?)`, ftsQuote(term), true
}

// ftsQuote 는 단어를 FTS5 문자열 리터럴로 감쌉니다. 안의 문장부호와 질의
// 연산자(따옴표, AND/OR/NEAR, *, ^)가 질의 문법이 아니라 글자 그대로 맞게 합니다.
func ftsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// PageQuery 는 Page 의 선택 필터와 정렬을 묶습니다. 필터는 모두 AND 입니다.
// 빈 문자열은 그 필터를 건너뛰고, RespMin/RespMax 가 -1 이면 응답 크기
// 한계를 건너뜁니다. Sort/Order 가 비면 시각 기준 최신이 먼저입니다.
type PageQuery struct {
	Host    string // 호스트 부분 문자열
	Method  string // 메서드 정확히. 대소문자 무시
	Query   string // 넓은 검색: 메타데이터 칸 또는 잡은 글
	Body    string // 본문 칸: 잡은 요청/응답 글만(전문)
	Path    string // url_template 부분 문자열. 경로 필터
	Status  string // 정확한 코드("404") 또는 묶음("4xx")
	RespMin int64  // resp_len 하한. 없으면 -1
	RespMax int64  // resp_len 상한. 없으면 -1
	Sort    string // ts | status | resp_len. 기본 ts
	Order   string // asc | desc. 기본 desc
}

// sortColumns 는 Page 가 정렬할 수 있는 칸의 허용 목록입니다. 호출자가 준
// Sort 가 이 고정 이름 외의 SQL 로 들어가지 못하게 합니다.
var sortColumns = map[string]string{"ts": "ts", "status": "status", "resp_len": "resp_len"}

// statusFilter 는 상태 토큰을 SQL 조건으로 바꿉니다. 정확한 코드("404")는
// 그 상태만, "Nxx" 묶음("4xx")은 그 백 단위 전체를 맞춥니다.
// 비었거나 알아보지 못하는 토큰이면 ok 는 거짓입니다.
func statusFilter(s string) (cond string, args []any, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil, false
	}
	if len(s) == 3 && s[0] >= '1' && s[0] <= '5' && s[1] == 'x' && s[2] == 'x' {
		base := int(s[0]-'0') * 100
		return "status>=? AND status<?", []any{base, base + 100}, true
	}
	if n, err := strconv.Atoi(s); err == nil {
		return "status=?", []any{n}, true
	}
	return "", nil, false
}

// Page 는 트래픽 목록의 교환 메타데이터 한 페이지를 돌려줍니다. f 의 조건으로
// 거르고(모두 선택, AND) 요청한 칸으로 정렬합니다. Query 는 넓은 검색칸입니다
// (메타데이터 칸, 또는 단어가 trigram 색인에 맞을 만큼 길면 잡은 요청/응답 글).
// Body 는 같은 색인으로, 잡은 글이 맞는 교환만 남깁니다. 필터에 맞는 전체
// 행 수도 돌려줍니다(화면의 페이지 번호). 행에 본문은 넣지 않습니다.
func (t *Traffic) Page(f PageQuery, page, size int) (rows []ExchangeMeta, total int, err error) {
	if size <= 0 || size > 500 {
		size = 100
	}
	if page < 0 {
		page = 0
	}
	where := ""
	var args []any
	add := func(cond string, vs ...any) {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += cond
		args = append(args, vs...)
	}
	if h := strings.TrimSpace(f.Host); h != "" {
		add("host LIKE ?", "%"+h+"%")
	}
	if m := strings.TrimSpace(f.Method); m != "" {
		add("method=?", strings.ToUpper(m))
	}
	if p := strings.TrimSpace(f.Path); p != "" {
		add("url_template LIKE ?", "%"+p+"%")
	}
	if cond, sargs, ok := statusFilter(f.Status); ok {
		add(cond, sargs...)
	}
	if f.RespMin >= 0 {
		add("resp_len>=?", f.RespMin)
	}
	if f.RespMax >= 0 {
		add("resp_len<=?", f.RespMax)
	}
	if s := strings.TrimSpace(f.Query); s != "" {
		like := "%" + s + "%"
		const meta = "host LIKE ? OR url LIKE ? OR url_template LIKE ? OR method LIKE ? OR content_type LIKE ? OR CAST(status AS TEXT) LIKE ?"
		// 메타데이터 일치 또는 전문 일치. 검색칸 하나, 가장 넓게 찾습니다.
		// trigram 에 짧은 단어는 조용히 메타데이터만 봅니다.
		if cond, arg, ok := t.ftsFilter(s); ok {
			add("(("+meta+") OR "+cond+")", like, like, like, like, like, like, arg)
		} else {
			add("("+meta+")", like, like, like, like, like, like)
		}
	}
	// Body 는 본문 전용 칸입니다. 잡은 요청/응답 글만 보므로 메타데이터
	// 우회 없이 전문 색인으로 갑니다. trigram 이 못 맞추는 짧은 단어는
	// 추측하지 않고 건너뜁니다. 화면이 세 글자 최소를 알려 줍니다.
	if b := strings.TrimSpace(f.Body); b != "" {
		if cond, arg, ok := t.ftsFilter(b); ok {
			add(cond, arg)
		}
	}
	if err = t.db.QueryRow(`SELECT COUNT(*) FROM exchanges`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	col := sortColumns[strings.ToLower(strings.TrimSpace(f.Sort))]
	if col == "" {
		col = "ts"
	}
	dir := "DESC"
	if strings.EqualFold(strings.TrimSpace(f.Order), "asc") {
		dir = "ASC"
	}
	// id 에 캡처 시각과 순번이 들어 있습니다. 뒤에 id DESC 를 붙이면
	// 정렬 칸에 동점이 많아도 순서와 페이지가 흔들리지 않습니다.
	sel := `SELECT id,ts,host,method,url_template,url,status,content_type,resp_len,path FROM exchanges` +
		where + ` ORDER BY ` + col + ` ` + dir + `, id DESC LIMIT ? OFFSET ?`
	qargs := append(append([]any{}, args...), size, page*size)
	rs, err := t.db.Query(sel, qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rs.Close()
	for rs.Next() {
		var m ExchangeMeta
		if err := rs.Scan(&m.ID, &m.TS, &m.Host, &m.Method, &m.URLTemplate, &m.URL, &m.Status, &m.ContentType, &m.RespLen, &m.Path); err != nil {
			return nil, 0, err
		}
		rows = append(rows, m)
	}
	return rows, total, rs.Err()
}

// Get 은 교환 하나의 요청/응답 글 전체를 돌려줍니다. 본문은 데이터베이스에서
// 옵니다. 본문이 SQLite 로 들어가기 전에 기록된 행은 path 가 비어 있지 않아
// 옛 디스크 트리에서 읽습니다. 옮기지 않아도 과거 기록을 볼 수 있습니다.
func (t *Traffic) Get(id string) (req, resp string, err error) {
	var reqHead, respHead string
	var reqBody, respBody []byte
	var reqBlob, respBlob sql.NullString
	var reqLen, respLen int
	err = t.db.QueryRow(`SELECT b.req_head,b.req_body,b.req_blob,b.resp_head,b.resp_body,b.resp_blob,e.req_len,e.resp_len
FROM exchange_bodies b JOIN exchanges e ON e.id=b.id WHERE b.id=?`, id).
		Scan(&reqHead, &reqBody, &reqBlob, &respHead, &respBody, &respBlob, &reqLen, &respLen)
	if err == nil {
		return assembleRaw(reqHead, reqBody, reqBlob, reqLen), assembleRaw(respHead, respBody, respBlob, respLen), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}

	var rel string
	if err = t.db.QueryRow(`SELECT path FROM exchanges WHERE id=?`, id).Scan(&rel); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(rel) == "" {
		return "", "", fmt.Errorf("exchange %s 에 본문 기록이 없습니다", id)
	}
	rb, _ := os.ReadFile(filepath.Join(t.dir, rel, "request.http"))
	pb, _ := os.ReadFile(filepath.Join(t.dir, rel, "response.http"))
	return string(rb), string(pb), nil
}

// assembleRaw 는 한쪽의 원문 HTTP 를 다시 조립합니다. 머리, 빈 줄, 본문입니다.
// blob 저장소로 넘긴 본문은 칸의 미리보기 뒤에 blob 포인터를 붙여,
// 무엇인지 보고 나머지를 hash 로 가져오게 합니다.
func assembleRaw(head string, body []byte, blob sql.NullString, total int) string {
	var b strings.Builder
	b.WriteString(head)
	b.WriteByte('\n')
	b.Write(body)
	if blob.Valid && blob.String != "" {
		fmt.Fprintf(&b, "\n…[truncated] @blob sha256:%s (len=%d)", blob.String, total)
	}
	return b.String()
}

// HostCount 는 기록된 호스트 하나와 그 교환 수입니다.
type HostCount struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// Hosts 는 기록된 호스트와 교환 수를, 최근 활동이 먼저 오게 돌려줍니다.
// 화면의 대상 고르기가 이것을 씁니다.
func (t *Traffic) Hosts() ([]HostCount, error) {
	rows, err := t.db.Query(`SELECT host, COUNT(*) AS n, MAX(ts) AS last FROM exchanges GROUP BY host ORDER BY last DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostCount
	for rows.Next() {
		var h HostCount
		var last int64
		if err := rows.Scan(&h.Host, &h.Count, &last); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Count 는 기록된 교환의 총수입니다.
func (t *Traffic) Count() (int, error) {
	var n int
	err := t.db.QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n)
	return n, err
}

// DeleteHost 는 호스트에 그 부분 문자열이 들어 있는 교환을 모두 지웁니다.
// 화면의 호스트 필터와 같아서, 거른 것이 지워집니다. 전부 SQLite 에 있으므로
// 삭제는 색인, 본문, 전문 색인, blob 참조를 한 트랜잭션으로 지웁니다.
// 내용 주소 blob 은 호스트끼리 공유하므로 호스트로 지우지 않고 나중에
// 쓰레기를 수거합니다. 지운 행 수를 돌려줍니다.
func (t *Traffic) DeleteHost(host string) (int64, error) {
	like := "%" + host + "%"
	t.wmu.Lock()
	defer t.wmu.Unlock()
	// 행이 사라지기 전에 디렉터리를 정합니다. 부분 문자열 일치에서는
	// 색인만이 필터가 실제로 맞은 호스트 디렉터리를 압니다.
	legacy, err := t.hostTrees(`host LIKE ?`, like)
	if err != nil {
		return 0, err
	}
	tx, err := t.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋된 뒤에는 아무 일도 없음
	n, err := t.deleteWhere(tx, `host LIKE ?`, like)
	if err != nil {
		return 0, err
	}
	// 커밋 전에 임시로 옮겨, 파일 시스템 실패가 삭제 전체를 되돌리게 합니다.
	// gcBlobs 보다 앞입니다. 참조 수거가 살아 있는 집합을 맞게 보게 합니다.
	// 임시로 옮긴 트리는 수거가 보지 못합니다.
	stageDir, moves, err := t.stageTrees(legacy)
	if err != nil {
		return 0, errors.Join(err, restoreTrees(stageDir, moves))
	}
	if err := tx.Commit(); err != nil {
		return 0, errors.Join(fmt.Errorf("트래픽 인덱스 삭제 커밋: %w", err), restoreTrees(stageDir, moves))
	}
	t.reapStage(stageDir)
	if n > 0 {
		t.reclaim()
		if err := t.gcBlobs(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// DeleteAll 은 기록된 교환을 모두 지웁니다. 화면의 전부 지우기입니다.
// 호스트 단위 삭제와 두 가지가 다릅니다. 색인을 보지 않고 호스트 디렉터리를
// sweep 합니다. 전부 지우기는 아무것도 남기면 안 되고, 행이 이미 없는
// 디렉터리가 살아남으면 안 되기 때문입니다. 그리고 전체 압축으로 끝냅니다.
// VACUUM 비용은 남기는 내용에 비례하므로, 색인이 빈 순간이 공짜에 가깝습니다.
// auto_vacuum 없이 만든 데이터베이스를 켜는 방법도 이것뿐입니다.
//
// 발견에 이미 묶인 증거는 건드리지 않습니다. 그 본문은 묶을 때 증거
// 저장소로 복사됐습니다. 지워도 되는 트래픽을 지워도 증거가 따라 사라지지
// 않게 하려는 것입니다.
//
// 지운 교환 수와, 압축이 되돌린 색인 바이트 수를 돌려줍니다.
func (t *Traffic) DeleteAll() (deleted int64, reclaimed int64, err error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	before := t.indexBytes()
	trees, err := t.allHostTrees()
	if err != nil {
		return 0, 0, err
	}
	tx, err := t.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋된 뒤에는 아무 일도 없음
	if deleted, err = t.deleteWhere(tx, `1=1`); err != nil {
		return 0, 0, err
	}
	// 커밋 전에 임시로 옮깁니다. 파일 시스템 실패가 삭제 전체를 되돌립니다.
	// DeleteHost 와 같습니다.
	stageDir, moves, err := t.stageTrees(trees)
	if err != nil {
		return 0, 0, errors.Join(err, restoreTrees(stageDir, moves))
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, errors.Join(fmt.Errorf("트래픽 인덱스 삭제 커밋: %w", err), restoreTrees(stageDir, moves))
	}
	t.reapStage(stageDir)
	if err := t.gcBlobs(); err != nil {
		return deleted, 0, err
	}
	if err := t.compactIndex(); err != nil {
		// 삭제는 이미 디스크에 남았습니다. 압축은 용량 문제이지 정확성이
		// 아니므로, 끝난 비우기를 실패로 바꾸면 안 됩니다.
		log.Printf("[traffic] 인덱스 압축 실패: %v", err)
		return deleted, 0, nil
	}
	return deleted, before - t.indexBytes(), nil
}

// allHostTrees 는 SQLite 이전 배치가 남긴 호스트 디렉터리를 모두 나열합니다.
// hostTrees 와 달리 색인을 거치지 않아서, 행이 이미 없는 디렉터리도 찾습니다.
// 밑줄로 시작하는 항목은 저장소 자신(_index, _blobs, _ca, _delete_staging)이라
// 호스트가 아닙니다.
func (t *Traffic) allHostTrees() ([]string, error) {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			dirs = append(dirs, filepath.Join(t.dir, e.Name()))
		}
	}
	return dirs, nil
}

// compactIndex 는 색인을 새 파일로 다시 씁니다. 그래야 페이지가 파일
// 시스템으로 돌아갑니다. VACUUM 의 시간과 임시 공간은 남기는 내용에
// 비례하므로, DeleteAll 이 색인을 비운 직후에만 도달합니다. 일상 단계는
// 아닙니다. auto_vacuum=incremental 이전에 만든 데이터베이스를 바꾸는
// 경로이기도 합니다. 그 pragma 는 뒤따르는 VACUUM 으로만 적용되고, 둘은
// 같은 연결에서 돌아야 합니다. 호출자는 wmu 를 잡고 있어야 합니다.
func (t *Traffic) compactIndex() error {
	ctx := context.Background()
	conn, err := t.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if t.fts {
		// 회수가 쓰는 제한 병합이 아니라 전체 병합입니다. 색인이 비었으므로
		// 합칠 내용이 없고, 묘비 표시만 버립니다.
		if _, err := conn.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts) VALUES('optimize')`); err != nil {
			return fmt.Errorf("전문 인덱스 병합: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum=incremental`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("인덱스 압축: %w", err)
	}
	var mode int
	if err := conn.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	// 옛 데이터베이스를 방금 바꿨습니다. 이후 삭제는 또 한 번의 비우기를
	// 기다리지 않고 스스로 공간을 되돌립니다.
	t.incrementalVacuum = mode == autoVacuumIncremental
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("WAL 자르기: %w", err)
	}
	return nil
}

// deleteWhere 는 조건에 맞는 교환의 흔적을 모두 지웁니다. 전문 색인 행,
// 본문, blob 참조, 마지막에 색인 행입니다. 순서가 중요합니다. 하위 SELECT 가
// exchanges 를 읽으므로 그 테이블을 마지막에 비웁니다.
func (t *Traffic) deleteWhere(tx *sql.Tx, where string, args ...any) (int64, error) {
	if t.fts {
		// ex_fts 는 내용이 없고 rowid 로 가리킵니다. 그래서 rowid 하위 SELECT 입니다.
		if _, err := tx.Exec(`DELETE FROM ex_fts WHERE rowid IN (SELECT rowid FROM exchanges WHERE `+where+`)`, args...); err != nil {
			return 0, fmt.Errorf("전문 인덱스 삭제: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM exchange_bodies WHERE id IN (SELECT id FROM exchanges WHERE `+where+`)`, args...); err != nil {
		return 0, fmt.Errorf("본문 삭제: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM blob_refs WHERE exchange_id IN (SELECT id FROM exchanges WHERE `+where+`)`, args...); err != nil {
		return 0, fmt.Errorf("blob 참조 삭제: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM exchanges WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("인덱스 행 삭제: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// hostTrees 는 조건에 맞는 호스트의 디스크 디렉터리를 돌려줍니다.
// 본문이 SQLite 로 들어간 뒤의 교환은 디렉터리가 없습니다. 그래서 이 경로
// 대부분은 없고, 임시 옮기기는 그것을 건너뜁니다. 일부러 path 가 있는 행만
// 보지 않습니다. 색인 행이 이미 없어도 고아 디렉터리가 남을 수 있고,
// 호스트를 지우면 그것도 함께 가야 합니다.
func (t *Traffic) hostTrees(where string, args ...any) ([]string, error) {
	rows, err := t.db.Query(`SELECT DISTINCT host FROM exchanges WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dirs []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		dirs = append(dirs, filepath.Join(t.dir, sanitize(h)))
	}
	return dirs, rows.Err()
}

type stagedTrafficPath struct {
	source string
	staged string
}

type hostDeleteStageJournal struct {
	Version   int                   `json:"version"`
	ArchiveID int64                 `json:"archive_id,omitempty"`
	TaskID    int64                 `json:"task_id,omitempty"`
	Hosts     []string              `json:"hosts,omitempty"`
	Moves     []hostDeleteStageMove `json:"moves"`
}

type hostDeleteStageMove struct {
	Source string `json:"source"`
	Staged string `json:"staged"`
}

const hostDeleteStageJournalName = "journal.json"

// stageTrees 는 호스트 디렉터리를 같은 파일 시스템 옆으로 옮깁니다. rename 은
// 원자적이고 즉시라서, unlink 가 못 하는 두 가지를 얻습니다. 트랜잭션이
// 커밋되기 전까지 삭제를 되돌릴 수 있고, 트리가 blob 참조 수거에 바로
// 안 보입니다(legacyBlobRefs 는 밑줄 디렉터리를 건너뛰므로 _delete_staging
// 아래는 이미 살아 있는 집합 밖입니다). 옮길 것이 없으면 stageDir 은 빈 문자열입니다.
func (t *Traffic) stageTrees(dirs []string) (stageDir string, moves []stagedTrafficPath, err error) {
	return t.stageTreesForArchive(dirs, nil, 0, 0)
}

func (t *Traffic) stageTreesForArchive(dirs, hosts []string, archiveID, taskID int64) (stageDir string, moves []stagedTrafficPath, err error) {
	seen := make(map[string]struct{}, len(dirs))
	planned := make([]stagedTrafficPath, 0, len(dirs))
	for _, source := range dirs {
		if _, dup := seen[source]; dup {
			continue
		}
		seen[source] = struct{}{}
		if _, err := os.Lstat(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return stageDir, moves, fmt.Errorf("과거 트래픽 디렉터리 %s 확인: %w", source, err)
		}
		planned = append(planned, stagedTrafficPath{source: source})
	}
	// 아카이브 경로에 과거 host 디렉터리가 하나도 없어도(새로 깐 머신의 트래픽은 SQLite 와 _blobs 에만 있음)
	// journal 은 남겨야 합니다. 크래시가 PostgreSQL 커밋과 SQLite 커밋 사이에 떨어지면, 재시작 후
	// SQLite 트랜잭션이 롤백되고, 이 journal 만이 복구 절차가 host 행 삭제를 마저 하게 합니다. 이것이 없으면
	// 이미 차가워진 작업의 독점 트래픽이 뜨거운 저장소에 영원히 남습니다.
	uniqueHosts := uniqueArchiveHosts(hosts)
	if len(planned) == 0 && len(uniqueHosts) == 0 {
		return "", nil, nil
	}
	parent := filepath.Join(t.dir, "_delete_staging")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", nil, fmt.Errorf("트래픽 임시 디렉터리 만들기: %w", err)
	}
	if stageDir, err = os.MkdirTemp(parent, "hosts-"); err != nil {
		return "", nil, fmt.Errorf("트래픽 임시 디렉터리 만들기: %w", err)
	}
	journal := hostDeleteStageJournal{Version: 1, ArchiveID: archiveID, TaskID: taskID, Hosts: uniqueHosts}
	for i := range planned {
		planned[i].staged = filepath.Join(stageDir, fmt.Sprintf("%d-%s", i, filepath.Base(planned[i].source)))
		journal.Moves = append(journal.Moves, hostDeleteStageMove{Source: planned[i].source, Staged: planned[i].staged})
	}
	if err := writeHostDeleteStageJournal(filepath.Join(stageDir, hostDeleteStageJournalName), journal); err != nil {
		_ = os.RemoveAll(stageDir)
		return "", nil, err
	}
	for _, move := range planned {
		source, staged := move.source, move.staged
		if err := os.Rename(source, staged); err != nil {
			return stageDir, moves, fmt.Errorf("과거 트래픽 디렉터리 %s 옮기기: %w", source, err)
		}
		moves = append(moves, stagedTrafficPath{source: source, staged: staged})
	}
	return stageDir, moves, nil
}

func writeHostDeleteStageJournal(path string, journal hostDeleteStageJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// restoreTrees 는 임시로 옮긴 디렉터리를 원래 자리로 되돌립니다. 최근 이동이
// 먼저이고, 모두 돌아오면 임시 디렉터리를 지웁니다.
func restoreTrees(stageDir string, moves []stagedTrafficPath) error {
	var errs []error
	for i := len(moves) - 1; i >= 0; i-- {
		move := moves[i]
		if _, err := os.Lstat(move.source); err == nil {
			errs = append(errs, fmt.Errorf("restore destination already exists: %s", move.source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("inspect restore destination %s: %w", move.source, err))
			continue
		}
		if err := os.Rename(move.staged, move.source); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", move.source, err))
		}
	}
	if len(errs) == 0 && stageDir != "" {
		if err := os.RemoveAll(stageDir); err != nil {
			errs = append(errs, fmt.Errorf("remove traffic stage: %w", err))
		}
	}
	return errors.Join(errs...)
}

// reapStage 는 커밋된 임시 디렉터리를 배경에서 지웁니다. 예전에는 이 단계가
// 기록기를 멈췄습니다. 옛 트리는 요청마다 URL 경로를 그대로 둬서 작은 파일이
// 수십만 개일 수 있고, unlink 전체가 쓰기 잠금을 잡은 채 돌았습니다. 행은
// 이미 없습니다. 종료 때문에 이것을 잃으면 _delete_staging 아래 쓰레기가
// 남을 뿐, 상태가 어긋나지는 않습니다.
func (t *Traffic) reapStage(stageDir string) {
	if stageDir == "" {
		return
	}
	t.reaping.Go(func() {
		if err := os.RemoveAll(stageDir); err != nil {
			log.Printf("[traffic] 과거 트래픽 디렉터리 %s 정리 실패: %v", stageDir, err)
		}
	})
}

// DeleteHostsExact 는 정확히 그 호스트들의 교환을 지웁니다. 화면의 여러 개
// 선택 삭제입니다. 색인 행과 호스트별 파일 트리, 그다음 blob 수거 한 번입니다.
// DeleteHost 의 부분 문자열과 달리 정확 일치라, "api.example.com" 을 골라도
// "api.example.com.cn" 은 쓸지 않습니다. 지운 행 수를 돌려줍니다.
// 중복 호스트는 해가 없습니다. 삭제는 여러 번 해도 같고, 트리는 한 번만 봅니다.
func (t *Traffic) DeleteHostsExact(hosts []string) (int64, error) {
	stage, err := t.StageDeleteHostsExact(hosts)
	if err != nil {
		return 0, err
	}
	deleted := stage.Deleted()
	if err := stage.Commit(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// HostDeleteStage 는 SQLite 삭제 트랜잭션을 열어 둡니다. 작업 삭제를
// PostgreSQL 과 맞추기 위해서입니다. 트래픽 쪽은 여기서 임시로 옮기고,
// 호출자 트랜잭션이 성공한 뒤에만 커밋합니다. Commit 이나 Rollback 까지
// Traffic 쓰기 잠금을 잡아서, 삭제 중인 호스트에 기록기가 행이나 blob
// 참조를 더하지 못합니다. 디스크에서 옮긴 것이 없으면 되돌리기는
// 트랜잭션 롤백만입니다.
type HostDeleteStage struct {
	traffic  *Traffic
	tx       *sql.Tx
	stageDir string
	moves    []stagedTrafficPath
	deleted  int64
	done     bool
}

// Deleted 는 이 단계에서 고른 교환 행 수입니다.
func (s *HostDeleteStage) Deleted() int64 {
	if s == nil {
		return 0
	}
	return s.deleted
}

// StageDeleteHostsExact 는 되돌릴 수 있는 정확 호스트 삭제를 준비합니다.
// 성공한 단계는 Commit 이나 Rollback 으로 끝내야 합니다.
func (t *Traffic) StageDeleteHostsExact(hosts []string) (*HostDeleteStage, error) {
	return t.stageDeleteHostsExact(hosts, 0, 0)
}

// StageDeleteHostsExactForArchive 는 되돌릴 수 있는 트래픽 삭제를 영구
// 작업 아카이브에 묶습니다. 시작 복구는 archiveCommitted 로, 끊긴 단계를
// 끝낼지 되돌릴지 정합니다.
func (t *Traffic) StageDeleteHostsExactForArchive(hosts []string, archiveID, taskID int64) (*HostDeleteStage, error) {
	if archiveID <= 0 || taskID <= 0 {
		return nil, errors.New("archive and task ids must be positive")
	}
	return t.stageDeleteHostsExact(hosts, archiveID, taskID)
}

func (t *Traffic) stageDeleteHostsExact(hosts []string, archiveID, taskID int64) (*HostDeleteStage, error) {
	t.wmu.Lock()
	stage := &HostDeleteStage{traffic: t}
	fail := func(cause error) (*HostDeleteStage, error) {
		if rollbackErr := stage.rollbackLocked(); rollbackErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("트래픽 삭제 되돌리기: %w", rollbackErr))
		}
		return nil, cause
	}
	abort := func(cause error) (*HostDeleteStage, error) {
		t.wmu.Unlock()
		stage.done = true
		return nil, cause
	}

	unique := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		unique = append(unique, host)
	}

	// 정확 호스트는 미리 알고 있으므로 디렉터리를 조회하지 않고 바로 만듭니다.
	// 색인 행이 이미 없어도 고아 디렉터리가 남을 수 있고, 이 삭제가 그것을
	// 함께 가져가야 합니다.
	legacy := make([]string, 0, len(unique))
	for _, h := range unique {
		legacy = append(legacy, filepath.Join(t.dir, sanitize(h)))
	}

	tx, err := t.db.Begin()
	if err != nil {
		return abort(err)
	}
	stage.tx = tx
	for _, h := range unique {
		n, err := t.deleteWhere(tx, `host=?`, h)
		if err != nil {
			return fail(err)
		}
		stage.deleted += n
	}

	stage.stageDir, stage.moves, err = t.stageTreesForArchive(legacy, unique, archiveID, taskID)
	if err != nil {
		return fail(err)
	}
	return stage, nil
}

// RecoverHostDeleteStages 는 프로세스가 죽으며 남긴 파일 이동을 정리합니다.
// SQLite 는 재시작 때 열린 트랜잭션을 되돌리고, 넘긴 콜백은 PostgreSQL
// 압축이 이미 커밋된 작업 아카이브를 가려 냅니다.
func (t *Traffic) RecoverHostDeleteStages(archiveCommitted func(int64, int64) (bool, error)) error {
	if t == nil {
		return nil
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	parent := filepath.Join(t.dir, "_delete_staging")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	needsGC := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		stageDir := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(stageDir, hostDeleteStageJournalName))
		if os.IsNotExist(err) {
			// 옛 버전은 손실 없이 복구할 만큼의 정보를 남기지 않았습니다.
			// 디렉터리는 사람이 보게 그대로 둡니다.
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal hostDeleteStageJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, fmt.Errorf("트래픽 임시 로그 %s 읽기: %w", stageDir, err))
			continue
		}
		if journal.Version != 1 {
			errs = append(errs, fmt.Errorf("트래픽 임시 로그 %s 의 버전 %d 는 지원하지 않습니다", stageDir, journal.Version))
			continue
		}
		moves := make([]stagedTrafficPath, 0, len(journal.Moves))
		for _, move := range journal.Moves {
			if !pathWithin(t.dir, move.Source) || !pathWithin(stageDir, move.Staged) {
				errs = append(errs, fmt.Errorf("트래픽 임시 로그에 범위를 벗어난 경로가 있습니다: %s", stageDir))
				moves = nil
				break
			}
			if _, err := os.Lstat(move.Staged); err == nil {
				moves = append(moves, stagedTrafficPath{source: move.Source, staged: move.Staged})
			} else if !os.IsNotExist(err) {
				errs = append(errs, err)
				moves = nil
				break
			}
		}
		if moves == nil {
			continue
		}
		committed := false
		if journal.ArchiveID > 0 {
			if archiveCommitted == nil {
				errs = append(errs, fmt.Errorf("트래픽 아카이브 %d 에 상태 해석기가 없습니다", journal.ArchiveID))
				continue
			}
			committed, err = archiveCommitted(journal.ArchiveID, journal.TaskID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
		} else if len(journal.Hosts) > 0 {
			committed, err = t.hostsHaveNoExchanges(journal.Hosts)
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if !committed {
			if err := restoreTrees(stageDir, moves); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := t.deleteArchivedHosts(journal.Hosts); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(stageDir); err != nil {
			errs = append(errs, err)
			continue
		}
		needsGC = true
	}
	if needsGC {
		t.reclaim()
		if err := t.gcBlobs(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *Traffic) hostsHaveNoExchanges(hosts []string) (bool, error) {
	for _, host := range hosts {
		var count int
		if err := t.db.QueryRow(`SELECT count(*) FROM exchanges WHERE host=?`, host).Scan(&count); err != nil {
			return false, err
		}
		if count > 0 {
			return false, nil
		}
	}
	return true, nil
}

func (t *Traffic) deleteArchivedHosts(hosts []string) error {
	tx, err := t.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, host := range hosts {
		if _, err := t.deleteWhere(tx, `host=?`, host); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func pathWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Rollback 은 임시 삭제를 버립니다. 트래픽 저장소는 그대로입니다.
func (s *HostDeleteStage) Rollback() error {
	if s == nil || s.done {
		return nil
	}
	return s.rollbackLocked()
}

func (s *HostDeleteStage) rollbackLocked() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	if s.tx != nil {
		if err := s.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			errs = append(errs, fmt.Errorf("트래픽 인덱스 되돌리기: %w", err))
		}
	}
	if err := restoreTrees(s.stageDir, s.moves); err != nil {
		errs = append(errs, err)
	}
	s.done = true
	s.traffic.wmu.Unlock()
	return errors.Join(errs...)
}

// Commit 은 임시 삭제를 확정합니다. SQLite 는 호출자가 PostgreSQL 작업
// 삭제를 커밋한 뒤에만 커밋됩니다.
func (s *HostDeleteStage) Commit() error {
	if s == nil || s.done {
		return nil
	}
	if err := s.tx.Commit(); err != nil {
		// SQLite 커밋이 실패하면 보통 트랜잭션은 되돌아갑니다. 트리를 되돌려
		// 트래픽 저장소가 안에서 맞고, 복구할 수 있게 합니다.
		restoreErr := restoreTrees(s.stageDir, s.moves)
		s.done = true
		s.traffic.wmu.Unlock()
		return errors.Join(fmt.Errorf("트래픽 인덱스 삭제 커밋: %w", err), restoreErr)
	}
	// 임시 트리를 지우는 일이 예전에는 쓰기 잠금을 몇 시간 잡았습니다.
	// 지금은 배경에서 돌고, 아래 수거는 그것들이 살아 있는 트리 밖에만
	// 있으면 됩니다. 임시 rename 이 이미 그것을 보장합니다.
	s.traffic.reapStage(s.stageDir)
	s.traffic.reclaim()
	var errs []error
	if err := s.traffic.gcBlobs(); err != nil {
		errs = append(errs, fmt.Errorf("트래픽 blob 회수: %w", err))
	}
	s.done = true
	s.traffic.wmu.Unlock()
	return errors.Join(errs...)
}

// indexBytes 는 색인이 디스크에서 실제로 차지하는 크기입니다. 데이터베이스와
// write-ahead 로그, 공유 메모리 파일입니다. 운영자가 데이터 디렉터리에서
// 보는 것이 이것들이기 때문입니다.
func (t *Traffic) indexBytes() int64 {
	base := filepath.Join(t.dir, "_index", "index.sqlite")
	var total int64
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if st, err := os.Stat(p); err == nil {
			total += st.Size()
		}
	}
	return total
}

// reclaim 은 삭제가 비운 공간을 파일 시스템으로 되돌립니다. 행을 지워도
// 안 보이게만 됩니다. SQLite 는 그 페이지를 프리리스트에 매달고,
// contentless_delete 전문 색인에서 지우면 숨긴 포스팅을 빼지 않고
// 묘비 표시만 씁니다. 둘 다 디스크 바이트를 줄이지 않습니다. maxInlineBody
// 보다 작은 본문이 같은 파일에 있으므로, 캡처가 많은 설치는 아직 있는
// 트래픽보다 훨씬 많이 붙잡습니다.
//
// 일은 배경에서, 단계마다 쓰기 잠금을 놓는 제한된 단계로 합니다. 지운
// 양에 비례하기 때문입니다. 잠금 하나로 몇 기가바이트를 비우면 record() 가
// 멈춥니다. go-mitmproxy 는 클라이언트에 답하기 전에 record() 를 부르므로
// 기록 중인 요청도 멈춥니다. wmu 를 잡은 채 호출해도 안전합니다.
// 배경 단계는 그 잠금을 기다릴 뿐입니다.
//
// 처음부터 최선을 다할 뿐입니다. 회수 실패는 디스크 공간만 비용이고
// 정확성은 아니므로, 오류는 로그에 남기고 다음 삭제가 일을 이어갑니다.
func (t *Traffic) reclaim() {
	if !t.reclaiming.CompareAndSwap(false, true) {
		return // 한 번에 한 단계. 둘째는 잠금만 다툼
	}
	t.reaping.Go(func() {
		defer t.reclaiming.Store(false)
		// 단계 전체에 고정 연결 하나입니다. 긴 회수는 문장을 수백 개 내고,
		// 풀이 매번 다른 연결을 주면 연결이 출렁이고, 언제 멈출지 정하는
		// 프리리스트 읽기가 재는 vacuum 과 갈라집니다.
		ctx := context.Background()
		conn, err := t.db.Conn(ctx)
		if err != nil {
			log.Printf("[traffic] 인덱스 공간 회수 실패(연결 얻기): %v", err)
			return
		}
		defer conn.Close()
		deadline := time.Now().Add(reclaimBudget)
		merges := reclaimMergeSteps
		for step := 0; ; step++ {
			t.wmu.Lock()
			progressed, err := t.reclaimChunk(ctx, conn, &merges)
			t.wmu.Unlock()
			if err != nil {
				log.Printf("[traffic] 인덱스 공간 회수 실패: %v", err)
				return
			}
			if !progressed {
				break
			}
			if t.stopping() {
				return // 종료가 남은 예산을 기다리면 안 됨
			}
			if step+1 >= reclaimMaxSteps {
				log.Printf("[traffic] 인덱스 공간 회수를 끝내지 못했습니다(%d걸음 상한을 다 씀). 다음 삭제 때 이어 합니다", reclaimMaxSteps)
				return
			}
			if time.Now().After(deadline) {
				log.Printf("[traffic] 인덱스 공간 회수를 끝내지 못했습니다(%s 예산을 다 씀). 다음 삭제 때 이어 합니다", reclaimBudget)
				return
			}
		}
		// 로그를 잘라야 회수가 디스크에 보입니다. WAL 모드에서는 빈 페이지가
		// 먼저 거기에 기록되고, PASSIVE 체크포인트는 로그를 최고 수위에
		// 그대로 둡니다.
		t.wmu.Lock()
		defer t.wmu.Unlock()
		if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			log.Printf("[traffic] WAL 자르기 실패: %v", err)
		}
	})
}

// reclaimChunk 는 제한된 단계 하나를 하고, 진행이 있었는지 알립니다.
// 호출자가 그 값으로 루프를 돕니다. merges 는 남은 전문 병합 예산이고
// 여기서 줄어듭니다. 호출자는 wmu 를 잡고 있어야 합니다.
func (t *Traffic) reclaimChunk(ctx context.Context, conn *sql.Conn, merges *int) (bool, error) {
	progressed := false
	if t.fts && *merges > 0 {
		// 음수 rank 는 증분 병합 한 번의 fts5 페이지 예산입니다.
		if _, err := conn.ExecContext(ctx, `INSERT INTO ex_fts(ex_fts, rank) VALUES('merge', ?)`, -reclaimMergePages); err != nil {
			return false, fmt.Errorf("전문 인덱스 병합: %w", err)
		}
		*merges--
		progressed = true
	}
	if !t.incrementalVacuum {
		// auto_vacuum=0 으로 만든 데이터베이스에서 incremental_vacuum 은
		// 조용히 아무 일도 안 합니다. 전체 압축만 바꿉니다. 위의 전문 색인
		// 병합은 그래도 이득이라, 더 앞에서 멈추지 않고 여기서 멈춥니다.
		return progressed, nil
	}
	var before, after int
	if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&before); err != nil {
		return false, err
	}
	if before == 0 {
		return progressed, nil
	}
	// 예산은 상수이고, PRAGMA 인자는 파라미터로 바인드할 수 없습니다.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, reclaimChunkPages)); err != nil {
		return false, fmt.Errorf("인덱스 빈 페이지 회수: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&after); err != nil {
		return false, err
	}
	// 멈출 조건은 빈 프리리스트가 아니라 진행입니다. incremental_vacuum 은
	// 파일 끝으로 옮긴 페이지만 놓을 수 있어서, 줄이지 못한 남은
	// 프리리스트는 정상입니다.
	return progressed || after < before, nil
}

// gcBlobs 는 남은 교환이 가리키지 않는 blob 을 지웁니다. 살아 있는 참조는
// blob_refs 에 있으므로, 수거는 질의 하나와 blob 디렉터리 순회입니다.
// 교환 본문은 읽지 않습니다. 빈 버킷 디렉터리도 지웁니다. 예전 파일 스캔
// 수거는 파일만 지우고 버킷을 영원히 남겼습니다. 최선을 다할 뿐입니다.
// 순회 오류는 그 항목만 건너뜁니다. 호출자는 wmu 를 잡아, 동시에 도는
// record() 가 새 blob 과 참조를 쓰는 것과 경합하지 않게 합니다.
func (t *Traffic) gcBlobs() error {
	refs := make(map[string]struct{})
	rows, err := t.db.Query(`SELECT DISTINCT hash FROM blob_refs`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		refs[h] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := t.legacyBlobRefs(refs); err != nil {
		return err
	}

	root := filepath.Join(t.dir, "_blobs", "sha256")
	var buckets []string
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			if p != root {
				buckets = append(buckets, p)
			}
			return nil
		}
		h := strings.TrimSuffix(d.Name(), ".bin")
		if _, ok := refs[h]; !ok {
			os.Remove(p)
		}
		return nil
	}); err != nil {
		return err
	}
	// 깊은 곳부터입니다. 옛 배치의 두 단계 버킷이 완전히 접히게 합니다.
	// 비어 있지 않은 디렉터리에서 Remove 는 그냥 실패합니다. 살아 있는
	// blob 이 있는 버킷을 지우지 않는 가드입니다.
	sort.Slice(buckets, func(i, j int) bool { return len(buckets[i]) > len(buckets[j]) })
	for _, b := range buckets {
		os.Remove(b)
	}
	return nil
}

// legacyBlobRefs 는 SQLite 이전 교환이 가리키는 hash 를 더합니다. 그 본문은
// 아직 디스크의 .http 파일이고 "@blob sha256:<hex>" 포인터를 담습니다.
// 이것이 없으면 업그레이드 뒤 첫 수거가 과거가 아직 가리키는 blob 을 지웁니다.
// 레거시 행이 없으면 전부 건너뜁니다. 그게 정상 상태라, 트리 순회는
// 영구 비용이 아니라 전환기 비용입니다.
func (t *Traffic) legacyBlobRefs(refs map[string]struct{}) error {
	var n int
	if err := t.db.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE path<>''`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	blobRe := regexp.MustCompile(`@blob sha256:([0-9a-f]{64})`)
	return filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil // 읽을 수 없는 항목은 건너뜀
		}
		if d.IsDir() {
			// _blobs/_index/_ca 에는 참조가 없습니다. 그 하위 트리는 건너뜁니다.
			if p != t.dir && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if nm := d.Name(); nm != "request.http" && nm != "response.http" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, m := range blobRe.FindAllSubmatch(b, -1) {
			refs[string(m[1])] = struct{}{}
		}
		return nil
	})
}

// query 는 호스트와 URL 부분 문자열로 거른 교환 메타데이터 한 페이지를
// 돌려줍니다. 기본 페이지 크기는 일부러 작게(3) 해서 도구 결과를 가볍게
// 두고, 상한은 10입니다. page 는 0부터입니다(page*limit 오프셋).
func (t *Traffic) query(host, contains, bodyContains string, page, limit int) ([]ExchangeMeta, error) {
	if limit <= 0 {
		limit = 3
	}
	if limit > 10 {
		limit = 10
	}
	if page < 0 {
		page = 0
	}
	q := `SELECT id,ts,host,method,url_template,url,status,content_type,resp_len,path FROM exchanges WHERE 1=1`
	args := []any{}
	if host != "" {
		hostName, port, err := normalizeSearchHost(host)
		if err != nil {
			return nil, err
		}
		q += ` AND host=?`
		args = append(args, hostName)
		if port != "" {
			// 기록된 행은 예전부터 URL.Hostname()(포트 없음)을 저장합니다.
			// 그래서 호환을 위해 원래 URL authority 도 조건에 넣습니다.
			// 새 캡처와 옛 캡처가 같은 검색 계약을 공유합니다.
			authority := net.JoinHostPort(hostName, port)
			q += ` AND (url LIKE ? OR url LIKE ? OR url LIKE ?)`
			args = append(args,
				"%://"+authority+"/%",
				"%://"+authority+"?%",
				"%://"+authority,
			)
		}
	}
	if contains != "" {
		q += ` AND (url LIKE ? OR url_template LIKE ?)`
		args = append(args, "%"+contains+"%", "%"+contains+"%")
	}
	if b := strings.TrimSpace(bodyContains); b != "" {
		cond, arg, ok := t.ftsFilter(b)
		if !ok {
			if !t.fts {
				return nil, fmt.Errorf("이 인스턴스는 전문 인덱스를 켜지 않아 본문으로 검색할 수 없습니다")
			}
			return nil, fmt.Errorf("본문 검색 키워드는 최소 %d자여야 합니다(현재 %d자)", minTrigram, utf8.RuneCountInString(b))
		}
		q += ` AND ` + cond
		args = append(args, arg)
	}
	q += ` ORDER BY ts DESC LIMIT ? OFFSET ?`
	args = append(args, limit, page*limit)
	rows, err := t.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExchangeMeta
	for rows.Next() {
		var m ExchangeMeta
		if err := rows.Scan(&m.ID, &m.TS, &m.Host, &m.Method, &m.URLTemplate, &m.URL, &m.Status, &m.ContentType, &m.RespLen, &m.Path); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// normalizeSearchHost 는 에이전트가 보통 가진 형태를 받습니다. 저장된
// 호스트(URL.Hostname())를 기준 키로 유지합니다. 포트가 있으면 URL
// authority 에 적용해서, 같은 IP 의 다른 서비스 캡처가 섞이지 않게 합니다.
func normalizeSearchHost(raw string) (host, port string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("host 는 필수입니다")
	}
	if strings.Contains(raw, "://") {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Host == "" {
			return "", "", fmt.Errorf("host 를 해석할 수 없습니다: %q", raw)
		}
		host, port = u.Hostname(), u.Port()
	} else if h, p, splitErr := net.SplitHostPort(raw); splitErr == nil {
		host, port = h, p
	} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
	} else {
		host = raw
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return "", "", fmt.Errorf("host 를 해석할 수 없습니다: %q", raw)
	}
	if port != "" {
		p, parseErr := strconv.Atoi(port)
		if parseErr != nil || p < 1 || p > 65535 {
			return "", "", fmt.Errorf("포트가 올바르지 않습니다: %q", port)
		}
		port = strconv.Itoa(p)
	}
	return strings.ToLower(host), port, nil
}

// Tools 는 작업 에이전트에게 트래픽 조회를 엽니다. 같은 자원을 다시
// 요청하지 않고 이미 잡은 트래픽을 보게 합니다. 토큰과 중복 제거에 이득입니다.
func (t *Traffic) Tools() []actool.CoreTool {
	allow := func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}
	ro := func(json.RawMessage) bool { return true }

	search := actool.Build(actool.Spec{
		Name:        "traffic_search",
		Description: TrafficSearchDescription,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":          map[string]any{"type": "string", "description": "按主机过滤（必填；如 '107.172.96.177'、'107.172.96.177:8082' 或 'http://107.172.96.177:8082/path'）"}, // han-allow 업스트림 프롬프트·픽스처
				"contains":      map[string]any{"type": "string", "description": "URL 子串过滤（可选，如 'api' / 'login'）"},                                                         // han-allow 업스트림 프롬프트·픽스처
				"body_contains": map[string]any{"type": "string", "description": "正文全文搜索（可选，至少 3 个字符），匹配请求/响应的头与正文，如 'password' / 'root:x:0' / '内网测试'"},                    // han-allow 업스트림 프롬프트·픽스처
				"limit":         map[string]any{"type": "integer", "description": "每页条数，默认 3，最大 10"},                                                                       // han-allow 업스트림 프롬프트·픽스처
				"page":          map[string]any{"type": "integer", "description": "页码，从 0 开始，默认 0（按 ts 倒序分页）"},                                                             // han-allow 업스트림 프롬프트·픽스처
			},
			"required": []any{"host"},
		},
		ReadOnly:    ro,
		Permissions: allow,
		Run: func(_ context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Host, Contains string
				BodyContains   string `json:"body_contains"`
				Limit          int
				Page           int
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Host) == "" {
				return actool.Errorf("host 는 필수입니다. 호스트만, 호스트:포트, 또는 전체 URL 을 지정하세요. 저장소 전체를 훑지 않기 위해서입니다."), nil
			}
			rows, err := t.query(a.Host, a.Contains, a.BodyContains, a.Page, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if len(rows) == 0 {
				return actool.Text("일치하는 트래픽이 없습니다."), nil
			}
			// 최소 인덱스로 줄입니다. 위치를 찾는 데 필요한 필드와 응답 코드/길이만 남기고, 응답 내용은 하나도 넣지 않습니다.
			type liteRow struct {
				ID      string `json:"id"`
				Method  string `json:"method"`
				URL     string `json:"url"`
				Status  int    `json:"status"`
				RespLen int    `json:"resp_len"`
			}
			lite := make([]liteRow, 0, len(rows))
			for _, r := range rows {
				lite = append(lite, liteRow{ID: r.ID, Method: r.Method, URL: r.URL, Status: r.Status, RespLen: r.RespLen})
			}
			b, _ := json.Marshal(lite)
			return actool.Text(string(b)), nil
		},
	})

	get := actool.Build(actool.Spec{
		Name:        "traffic_get",
		Description: "按 id 取一条已抓流量的请求/响应原文（过大会截断）。配合 traffic_search 用，避免重复 curl。", // han-allow 업스트림 프롬프트·픽스처
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "string", "description": "traffic_search 返回的 id"}}, // han-allow 업스트림 프롬프트·픽스처
			"required":   []any{"id"},
		},
		ReadOnly:    ro,
		Permissions: allow,
		Run: func(_ context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct{ ID string }
			_ = json.Unmarshal(in, &a)
			req, resp, err := t.Get(a.ID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("=== REQUEST ===\n" + clip(req, 2500) + "\n\n=== RESPONSE ===\n" + clip(resp, 4000)), nil
		},
	})

	blob := actool.Build(actool.Spec{
		Name:        "traffic_blob",
		Description: "分段读取超大请求/响应体的原文。traffic_get 里显示为 '…[truncated] @blob sha256:<hash>' 的部分即存放于此，把该 hash 传进来即可取完整内容。单次最多返回 8KB，用 offset 继续往后读（返回结果会给出总长度）。适合翻阅备份文件、源码泄露、大 JSON 导出等超过内联阈值的响应。", // han-allow 업스트림 프롬프트·픽스처
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"hash":   map[string]any{"type": "string", "description": "traffic_get 中 @blob sha256: 后面的 64 位十六进制值"}, // han-allow 업스트림 프롬프트·픽스처
				"offset": map[string]any{"type": "integer", "description": "起始字节偏移，默认 0"},                              // han-allow 업스트림 프롬프트·픽스처
				"length": map[string]any{"type": "integer", "description": "本次读取字节数，默认且最大 8192"},                       // han-allow 업스트림 프롬프트·픽스처
			},
			"required": []any{"hash"},
		},
		ReadOnly:    ro,
		Permissions: allow,
		Run: func(_ context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Hash   string
				Offset int64
				Length int64
			}
			_ = json.Unmarshal(in, &a)
			if a.Length <= 0 || a.Length > maxBlobRead {
				a.Length = maxBlobRead
			}
			data, total, err := t.BlobRange(a.Hash, a.Offset, a.Length)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if len(data) == 0 {
				return actool.Text(fmt.Sprintf("오프셋 %d 이(가) 내용 길이(총 %d 바이트)를 벗어났습니다.", a.Offset, total)), nil
			}
			head := fmt.Sprintf("[offset=%d 이번=%d 총길이=%d]\n", a.Offset, len(data), total)
			if isBinaryBody("", data) {
				return actool.Text(head + "이진 내용입니다. 앞 512 바이트를 16진수로 보여 줍니다:\n" + hex.EncodeToString(clipBytes(data, 512))), nil
			}
			return actool.Text(head + truncateUTF8(data, len(data))), nil
		},
	})

	return []actool.CoreTool{search, get, blob}
}

// SeedToolMetas 는 영값 리시버로 만든 트래픽 도구를 돌려줍니다. 도구
// 목록을 심을 때 씁니다(메타데이터만 — Name/Description/InputSchema).
// 핸들러는 nil 리시버를 닫아 잡지만 이 인스턴스에서는 호출되지 않으므로 안전합니다.
func SeedToolMetas() []actool.CoreTool { return (&Traffic{}).Tools() }

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n... [잘림, 총 %d 바이트. 전체는 트래픽 파일 트리에 있습니다] ...", len(s))
}
