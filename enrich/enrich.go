// Package enrich 는 AI 가 아닌, 자산 장부를 채우는 층입니다.
//
// 초보: 도메인 DNS 와 웹 응답을 기록 프록시 경유로 보고, 공유 PostgreSQL 자산
// 그래프에 IP·포트와 연결을 되돌립니다. 탐색 그래프의 의도를 만들지는 않습니다.
// DNS 조회는 가로채기 없이 가고, HTTP 조회는 가드를 거칩니다.
package enrich

import (
	"crypto/tls"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"

	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
)

type jobKind int

const (
	jobDNS  jobKind = iota // 도메인을 해석
	jobHTTP                // 웹 자산(사이트)을 조회
)

type job struct {
	kind jobKind
	id   int64  // 자산 id. DNS 는 도메인, HTTP 는 사이트
	arg  string // DNS 는 host, HTTP 는 url
}

// Engine 은 해석기, 기록 프록시를 거치는 HTTP 클라이언트, 워커 풀을 가집니다.
// 초보: 여기 워커는 에이전트 워커가 아닙니다. DNS·HTTP 조회만 처리하는 고루틴입니다.
type Engine struct {
	as     *db.AssetStore
	resolv *dnsx.DNSX
	client *http.Client

	jobs   chan job
	cool   sync.Map // 중복·쉬는 시간. "kind:id" → 마지막 실행 시각
	once   sync.Once
	closed chan struct{}
}

const (
	cooldown    = 5 * time.Minute
	httpTimeout = 12 * time.Second
	queueSize   = 1024
)

// New 는 엔진을 만듭니다. proxy() 는 HTTP 조회가 거칠 기록 프록시 주소입니다.
// 트래픽 저장소에 남기 위해서이고, 요청마다 다시 봐서 실행 중 캡처 토글이
// 바로 적용됩니다. "" 이면 직접 연결입니다. 해석기 초기화가 실패해도 쓸 수 있는
// 엔진을 돌려줍니다. 그때 DNS 는 아무 일도 하지 않습니다.
func New(as *db.AssetStore, proxy func() string, workers int) *Engine {
	if workers <= 0 {
		workers = 4
	}
	resolv, err := dnsx.New(dnsx.Options{
		BaseResolvers: dnsx.DefaultResolvers,
		MaxRetries:    3,
		QuestionTypes: []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME},
		Timeout:       4 * time.Second,
	})
	if err != nil {
		log.Printf("[enrich] dnsx 초기화에 실패해 DNS 해석을 끕니다: %v", err)
		resolv = nil
	}
	e := &Engine{
		as:     as,
		resolv: resolv,
		client: buildClient(proxy),
		jobs:   make(chan job, queueSize),
		closed: make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		go e.worker()
	}
	return e
}

// buildClient 는 기록 프록시로 접속하는 HTTP 클라이언트를 만듭니다. proxy() 로
// 요청마다 주소를 다시 봐서 캡처 토글이 바로 적용됩니다. TLS 검증은 건너뜁니다.
// 프록시가 자기 CA 로 다시 서명하고, 대상 인증서도 자체 서명인 경우가 많습니다.
func buildClient(proxy func() string) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: httpTimeout,
	}
	if proxy != nil {
		tr.Proxy = func(*http.Request) (*url.URL, error) {
			p := proxy()
			if p == "" {
				return nil, nil // 직접 연결
			}
			return url.Parse(p)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   httpTimeout,
		// 리다이렉트는 여기서 끊습니다. 범위 안인지는 조회 시점에 다시 확인합니다.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ResolveDomain 은 도메인 자산의 DNS 해석을 큐에 넣습니다. 가로채기를 거치지 않습니다.
// host 는 FQDN 입니다. 엔진이 비어 있으면 아무 일도 하지 않습니다.
func (e *Engine) ResolveDomain(id int64, host string) { e.enqueue(job{jobDNS, id, host}) }

// ProbeSite 는 웹 자산(사이트)의 HTTP 조회를 큐에 넣습니다. rawURL 은 사이트 주소입니다.
func (e *Engine) ProbeSite(id int64, rawURL string) { e.enqueue(job{jobHTTP, id, rawURL}) }

func (e *Engine) enqueue(j job) {
	if e == nil || id0(j.id) || j.arg == "" {
		return
	}
	select {
	case e.jobs <- j:
	default: // 큐가 가득 차면 버림. 채우기는 최선을 다할 뿐
		log.Printf("[enrich] 큐가 가득 차 작업을 버립니다 kind=%d id=%d", j.kind, j.id)
	}
}

func id0(id int64) bool { return id <= 0 }

// Close 는 워커를 멈춥니다. 여러 번 호출해도 한 번만 동작합니다.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	e.once.Do(func() { close(e.closed) })
}

func (e *Engine) worker() {
	for {
		select {
		case <-e.closed:
			return
		case j := <-e.jobs:
			if e.onCooldown(j) {
				continue
			}
			switch j.kind {
			case jobDNS:
				e.doDNS(j.id, j.arg)
			case jobHTTP:
				e.doHTTP(j.id, j.arg)
			}
		}
	}
}

// onCooldown 은 이 (kind, id) 가 쉬는 시간 안에 이미 돌았으면 true(건너뜀)입니다.
func (e *Engine) onCooldown(j job) bool {
	key := string(rune(j.kind)) + ":" + itoa(j.id)
	if v, ok := e.cool.Load(key); ok {
		if t, ok := v.(time.Time); ok && time.Since(t) < cooldown {
			return true
		}
	}
	e.cool.Store(key, time.Now())
	return false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ---- DNS 해석 ----

func (e *Engine) doDNS(id int64, host string) {
	if e.resolv == nil {
		return
	}
	data, err := e.resolv.QueryMultiple(host)
	if err != nil || data == nil {
		return
	}
	ips := uniq(append(append([]string{}, data.A...), data.AAAA...))
	// 해석된 IP 를 자산 그래프에 넣거나 갱신합니다.
	for _, ip := range ips {
		_, _ = e.as.UpsertIP(db.UpsertIPReq{
			IP:           ip,
			BoundDomains: []string{host},
		})
	}
	// A 레코드를 서브도메인으로 자산 그래프에 남깁니다. host 가 서브도메인처럼 보일 때입니다.
	for _, a := range data.A {
		_, _ = e.as.UpsertSubdomain(db.UpsertSubdomainReq{
			Domain:      host,
			RecordType:  "A",
			RecordValue: []string{a},
		})
	}
	for _, aaaa := range data.AAAA {
		_, _ = e.as.UpsertSubdomain(db.UpsertSubdomainReq{
			Domain:      host,
			RecordType:  "AAAA",
			RecordValue: []string{aaaa},
		})
	}
	for _, cname := range data.CNAME {
		_, _ = e.as.UpsertSubdomain(db.UpsertSubdomainReq{
			Domain:      host,
			RecordType:  "CNAME",
			RecordValue: []string{cname},
		})
	}
}

// ---- HTTP 조회 ----

var reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func (e *Engine) doHTTP(id int64, rawURL string) {
	host := hostOf(rawURL)
	if host == "" {
		return
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "artex-enrich/1.0")
	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 본문은 1 MiB 에서 자름
	statusCode := resp.StatusCode
	bodyLen := int64(len(body))
	title := extractTitle(body)
	_, _ = e.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
		URL:           rawURL,
		StatusCode:    &statusCode,
		ContentLength: &bodyLen,
		PageTitle:     title,
	})
}

func extractTitle(body []byte) string {
	m := reTitle.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1])))
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func isBlank(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case float64:
		return x == 0
	case int:
		return x == 0
	}
	return false
}

func uniq(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
