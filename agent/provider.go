// Package agent 는 LLM 플래너·워커·메인 에이전트를 두 그래프에 연결합니다.
//
// 초보: 플래너만 탐색 그래프에 의도(intent)를 만들고, 워커는 그 의도 하나를
// 받아 도구를 실행한 뒤 멈춥니다. 메인 에이전트는 사람이 끼어드는 대화입니다.
// 이 파일은 그 셋이 같이 쓰는 LLM 연결입니다. 키는 환경 변수에서 읽고,
// Anthropic 형식과 OpenAI 형식 끝점을 받을 수 있습니다. 키가 없으면 엔진은 쉬고,
// 화면의 나머지(자산 그래프, 기록 프록시)는 그대로 떠 있습니다.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType 은 생각「스위치」필드(thinking.type)만 따로 정합니다.
	//   "" = 보내지 않음(기본, 이 필드를 모르는 모델과 호환); "disabled" = 명시적으로 끔;
	//   "enabled" = 켬. ReasoningEffort 와 완전히 분리됩니다. 어떤 엔드포인트는 thinking 필드가 없고
	//   강도 파라미터만으로 생각을 켜므로, 둘은 각자 설정할 수 있습니다.
	ThinkingType string
	// ReasoningEffort 는 생각「강도」필드만 따로 정합니다.
	//   "" = 보내지 않음(기본); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도.
	//   OpenAI 는 최상위 reasoning_effort, Anthropic 은 output_config.effort 로 매핑합니다.
	ReasoningEffort string
	// Stream 은 이 profile 이 스트리밍(SSE) 인터페이스를 쓸지 정합니다. true(기본)= 스트리밍; false = 진짜
	// 비스트리밍(stream:false 를 보내고, 완성된 JSON 을 한 번에 받으며 Provider.Complete 를 탑니다). 비스트리밍은
	// 빈 프레임이나 생각 필드 유실처럼 품질이 나쁜 SSE 게이트웨이를 피할 수 있지만, 실행 중 실시간 진행과
	// 실시간 token 집계를 잃습니다. agentcore.Options.NonStreaming = !Stream 으로 매핑합니다.
	Stream bool
	// MaxTokens 는 한 번 답변의 출력 상한(token)입니다. 0 = 이 필드를 보내지 않고 서버 기본값에 맡깁니다
	// (이전 동작). ContextWindowK 와 다릅니다. 후자는 모델 전체 용량이라 로컬에서 압축 임계값만 계산하고
	// 요청에는 안 나갑니다. 이 값은 매 요청에 실려 나갑니다. agentcore.Options.MaxTokens 로 매핑합니다.
	MaxTokens int
	// MaxTokensField 는 MaxTokens 를 어떤 요청 필드 이름으로 보낼지 고릅니다. format=openai 에서만 적용됩니다.
	//   "" = max_tokens(기본); "max_completion_tokens" = 새 필드.
	// OpenAI 추론 모델(o 계열/GPT-5)은 후자만 알아듣고, max_tokens 를 받으면
	// unsupported_parameter 를 바로 반환합니다. 대부분의 호환 게이트웨이는 전자만 알아듣습니다. 자동 추론은 하지 않고, 엔드포인트에 맞게 사용자가 고릅니다.
	MaxTokensField string
	// SessionHeaderKey 가 비어 있지 않으면, 매 LLM 요청에 사용자 지정 HTTP 헤더를 붙입니다. 헤더 이름은 이 값이고
	// 헤더 값은 【현재 세션의 session id】입니다(채팅 세션=conv-<id>, 워커=exp<x>-worker-i<intent>
	// 등, WorkerSessionID 참고). session-id 헤더로 프롬프트 캐시나 고정 라우팅을 하는 게이트웨이용입니다.
	// 빈 값 = 보내지 않음. 값은 transcript.WithSessionID 가 요청 context 에 걸고, RoundTripper 가
	// 읽어 채웁니다. 그래서 같은 공유 provider 도 세션마다 다른 헤더 값을 보낼 수 있습니다.
	SessionHeaderKey string
	// Retry 는 이 설정이 풀린 뒤의 재시도 파라미터입니다(profile 덮어쓰기 → 전역 정책 → 내장 기본,
	// server 쪽이 해석). 세 층의 의미는 RetryConfig 를 보세요. 영값 = 내장 기본을 그대로 씁니다.
	Retry RetryConfig
}

// RetryConfig 는 LLM 설정 하나에 붙는 재시도 파라미터입니다. 각 층의 「횟수」 의미는 같습니다.
// 0 = 내장 기본 횟수; 음수 = 그 층 재시도를 끔; >0 = 그 값을 씀. 각 층의 「간격」:
// 0 = 그 층 원래의 지수 백오프; >0 = 이 고정 간격으로 바꿈.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK 연결 재시도(연결 리셋/시간 초과/429/5xx, 스트림 시작 전).
	// llm.Config.MaxRetries / RetryInterval 로 바로 매핑합니다. 기본 3회, 0.5s 부터 지수(상한 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK 빈 응답 재시도(끝났지만 content block 이 없음, openai
	// 형식만). llm.Config.EmptyResponseRetries / EmptyResponseInterval 로 매핑합니다.
	// 기본 2회, 같은 지수 간격.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: 같은 provider 의 안전 구간 재시도. 이 프로젝트가 SDK 위에 얹은
	// 한 층으로, 「호출자에게 아직 어떤 출력도 넘기지 않았을 때」만 끊긴 스트림/과부하/스트림 안 429 를 다시 보냅니다. SDK 는 이 층을 모릅니다.
	// server/task_llm.go 가 소비합니다. 기본 2회, 0.5s 부터 지수(상한 4s).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 기본은 스트리밍. ARTEX_LLM_STREAM=false/0/off 이면 비스트리밍으로 명시적으로 끕니다.
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 기본은 스트리밍. 호출자가 profile 로 덮어씁니다
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 생각 스위치와 강도 두 필드는 각자 그대로 전달합니다(빈 값 = 그 필드는 보내지 않음). 둘은 분리됩니다.
	// thinking.type 만, effort 만, 둘 다, 또는 둘 다 안 보낼 수 있습니다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 상한의 필드 이름 선택(빈 값 = max_tokens). 상한의 「값」은 여기 없습니다. 매 턴
	// agentcore.Options.MaxTokens 로 가고, provider 는 그 값을 어느 키에 넣을지만 정합니다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 파라미터는 SDK 와 같은 의미입니다(횟수 0=기본/음수=끔, 간격 0=지수 백오프/>0=고정). 그대로 전달합니다.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费", // han-allow 업스트림 프롬프트·픽스처
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 비우면 직접 연결. HTTP_PROXY/HTTPS_PROXY 환경 변수로 돌아가지 않습니다
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "연결 테스트" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청을 하나도 보내지 못했습니다(설정 해석 또는 연결에서 바로 실패)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\n응답 본문: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어 있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(잘림, 총 %d바이트)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 원본 wire 메시지를 잡습니다. 연결 테스트에서 가장 보고 싶은 것은 게이트웨이가 실제로 무엇을 돌려줬는지(상태 코드+응답 본문)이고,
	// norma 가 응답을 StreamEvent 로 푼 뒤에는 그것이 사라집니다. quotaAwareTransport 는
	// context 에서 이 Capture 를 찾아 매 HTTP 시도의 상태 코드와 body 를 채웁니다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 한 번만 보내는 경로라 agentcore 세션 루프를 타지 않습니다. 그래서 아무도 context 에
	// session id 를 걸지 않습니다. SessionHeaderKey 를 쓰는 엔드포인트(예: opencode zen 은
	// x-opencode-session 헤더가 필수이고, 없으면 바로 400 MissingSessionID)에서는 "대화는 되는데
	// 연결 테스트만 400"이 됩니다. 여기서 일회용 무작위 session id 를 걸어, 테스트와 실제 대화가 같은
	// 헤더 로직을 타게 합니다. SessionHeaderKey 가 없는 엔드포인트는 이 값을 읽지 않으므로 부작용이 없습니다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens 는 넉넉해야 합니다. 추론 모델(예: deepseek-v4-pro)은 답을 내기 전에 긴
	// 생각을 먼저 만듭니다(한 줄 "ping" 에도 약 2900 token 을 쓴 측정이 있습니다). 32 만 주면 모델은 "생각 단계"에 머물다
	// 출력 상한(finish=length)에 부딪혀 잘립니다. 연결 테스트는 통과(err=nil)로 보여도
	// "중단됨/length/resume" 처럼 어수선해집니다. 예산을 줘서 OK 를 깨끗이 끝내게 합니다(finish=stop).
	// EscalateMaxTokens 는 false 로 둡니다. 잘렸다고 한도를 올려 재시도하지 않아 resume 루프가 토큰을 비우지 않게 합니다.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"你是连接测试。直接输出两个字符 OK 即可，不要思考、不要解释、不要别的。"}, // han-allow 연결 테스트 프롬프트 원문
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 이 profile 의 실제 송수신 방식으로 연결 테스트
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil 만으로는 부족합니다. 요청은 통했는데 모델이 글자를 하나도 안 내는 경우가 실제로 있습니다(생각이 예산을 다 쓰거나,
	// 본문이 안전 정책에 삼켜지거나, 호환 층이 content 를 버리거나). 이런 설정은 세션에서 "답이 없는" 상태인데
	// 테스트는 성공으로 보고합니다. 이 항목이 없애려는 차이가 그것입니다. 보이는 본문이 없으면 실패로 판정합니다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델 응답 내용이 없습니다(요청은 통과했지만 텍스트가 전혀 돌아오지 않았습니다)")
	}
	return lat, reply, nil
}
