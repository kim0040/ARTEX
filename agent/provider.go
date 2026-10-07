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

// Config 는 환경에서 읽은 LLM 연결 설정입니다.
// 초보: 플래너, 워커, 메인 에이전트가 이 연결 하나를 나눠 씁니다. 기록 프록시와는 다른 길입니다.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy 는 모든 LLM 요청을 이 프록시 URL 로 보냅니다(http/https/socks5,
	// user:pass@ 자격 증명도 가능). 비어 있으면 직접 연결입니다. 표준 *_PROXY
	// 환경 변수로 돌아가지 않습니다.
	Proxy string
	// RatePerSecond / RatePerMinute 는 이 provider 를 쓰는 모든 에이전트의
	// 요청 속도를 함께 제한합니다(0 = 그 창은 무제한).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK 는 모델 컨텍스트 창 크기입니다(K token, 사용자가 설정).
	// 압축 임계값을 정하는 데 씁니다. 0 = 기본값. CompactionWindow 를 보세요.
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

// 압축 창을 고르는 범위입니다(K token). 바닥보다 작으면
// 임계값 계산(창 − 요약 여유 − 버퍼)이 0 이하가 되어
// 매 턴 압축이 돕니다. 상한보다 크면 압축이 거의 안 돕니다.
const (
	defaultWindowK = 200  // 비어 있으면 200K 창으로 봅니다(Claude 기본)
	minWindowK     = 32   // 바닥. 유효 창이 넉넉히 양수가 되게 합니다
	maxWindowK     = 1000 // 상한 1M token(사용자 요청)
)

// CompactionWindow 는 압축 임계값에 쓸 모델 컨텍스트 창을 token 단위로 돌려줍니다.
// 사용자가 넣은 크기(ContextWindowK)에서 정합니다. 0 이거나 비어 있으면
// 200K 기본입니다. 그 외에는 [32K, 1M] 으로 잘라 압축이 계속 의미 있게 돕니다.
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

// compactionConfig 는 token 단위 컨텍스트 창으로 agent-core 압축 설정을 만듭니다.
// 이 값을 Options.Compaction 에 두면 agentcore.NewSession 이 같은 provider 로 요약기를 잇습니다.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv 는 환경 변수에서 LLM provider 설정을 읽습니다.
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (기본: 키에서 추론)
//	ARTEX_LLM_MODEL    = 모델 id          (기본: provider 마다)
//	ARTEX_LLM_BASE_URL = 끝점             (선택)
//	ARTEX_LLM_PROXY    = 프록시 URL       (선택. http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY    = 자격 증명
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

// ConfigFrom 은 화면에서 받은 문자열로 Config 를 만듭니다(provider 기본은
// anthropic, 모델 기본은 provider 마다). 입력은 앞뒤 공백을 자르고, base URL 은
// provider 가 기대하는 API 기반으로 맞춥니다(provider 가 경로를 스스로 붙임).
// 그래서 전체 끝점 URL 도 받아 줍니다.
// 초보: 화면의 LLM 설정이 여기로 들어와, 플래너와 워커가 같은 연결을 탑니다.
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
		// provider 가 "/chat/completions" 를 붙입니다. 전체 끝점 URL 도 받아 줍니다.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider 가 "/responses" 를 붙입니다. 전체 끝점 URL 도 받아 줍니다.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider 가 "/v1/messages" 를 붙입니다.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy 는 환경 변수 문자열이 명시적으로 "끔"을 요청하는지 봅니다. 비어 있거나
// 알아듣지 못하면 false 입니다(설정 안 한 변수는 스트리밍 기본을 유지).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider 는 짧은 provider 이름을 돌려줍니다("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider 는 설정으로 llm.Provider 를 만듭니다. 속도 제한이 있으면
// 제한기는 provider 인스턴스 하나에 붙습니다. 그래서 이 provider 를 나눠 쓰는
// 플래너, 모든 워커, 메인 에이전트가 같은 속도 제한을 탑니다.
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

// IsQuotaExhaustedMessage 는 잔액, 결제, 크레딧, 할당량 소진처럼
// 분명한 신호만 알아봅니다. 평범한 429/속도 제한 문구, 인증 실패,
// 네트워크 오류, 서버 실패는 제외합니다.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// 인증/권한 실패와 provider 쪽 5xx 는 갈아타지 않습니다.
	// 게이트웨이가 본문에 할당량처럼 보이는 문구를 섞어도 같습니다.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// provider API 는 평범한 속도 제한을 "quota exceeded" 라고 적는 일이 많습니다.
	// 특히 Google 식 응답은 quota metric 을 포함합니다.
	// 이런 제한은 시간이 지나면 풀리므로 현재 provider 에 남깁니다.
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
	// gRPC RESOURCE_EXHAUSTED 는 계정 할당량과 평범한 요청 속도 제한에
	// 둘 다 쓰입니다. 같은 오류가 일시적 속도 제한을 가리키지 않을 때만
	// 명시적 소진 신호로 둡니다.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport 는 Norma 의 보통 재시도를 그대로 둡니다. 예외는 본문이
// 계정 할당량이나 잔액 소진을 분명히 말하는 429 입니다. Norma 재시도 루프는
// 모든 429 를 일시적이라고 봅니다. 그 응답만 402 로 바꾸면 작업 라우터가
// 바로 다른 provider 로 넘어가고, 원래 응답 본문은 분류와 감사 기록에 남습니다.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey 가 비어 있지 않으면, 매 요청이 이 HTTP 헤더 이름을 답니다.
	// 값은 요청 context 에서 읽은 session id 입니다. 비어 있으면 끕니다.
	// Config.SessionHeaderKey 를 보세요.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 사용자 지정 session-id 헤더입니다. 이름은 사용자가 정하고, 값은 이번 실행의
	// session id 입니다(norma 가 transcript.WithSessionID 로 context 에 넣음).
	// 한 세션의 턴 동안 같고, 세션끼리는 다릅니다. session 단위 프롬프트 캐시가
	// 원하는 형태입니다. session id 가 없으면 건너뜁니다.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// LLM 기록이 켜지면 Recorder 가 context 에 Capture 를 넣어, 원본 wire 본문을
	// 남길 수 있게 합니다. 이 층만 그 본문을 아직 봅니다. norma 는 요청 본문을
	// 안에서 만들고, SSE 응답을 푼 뒤에야 recorder 에 도달합니다.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// 읽지 않고 Tee 합니다. 200 은 계속 흘러야 하는 SSE 스트림입니다.
	// 아래 429 분기는 이 래퍼를 통해 읽으므로, 본문이 갈리기 전에 capture 에 남습니다.
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

// requestBodySnapshot 은 나가는 요청 본문을 소비하지 않고 복사합니다.
// norma 는 모델 요청을 *bytes.Reader 로 만듭니다. 그래서 net/http 가
// GetBody 를 채우고, 이 복사는 실제로 나가는 내용에 영향을 주지 않습니다.
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

// logTestConnection 은 연결 테스트의 HTTP 상태 코드와 응답 본문을 서버 로그에 찍습니다.
// 그래서 "연결 테스트"가 게이트웨이가 실제로 돌려준 것(401 본문, 할당량 문구, 빈 프레임)을
// 남깁니다. 화면이 보여주는 접힌 ok/err 만으로는 부족합니다. 본문은 잘라, 수다스러운
// SSE 스트림이 로그를 채우지 않게 합니다.
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

// clipBody 는 로그용으로 wire 본문을 자릅니다. 4K 면 오류 JSON 이나
// SSE 스트림의 앞부분을 보여 주기에 충분하고, 폭주하는 응답은 막습니다.
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

// TestConnection 은 아주 짧은 실제 완성을 보내, provider/모델/끝점/키가
// 정말 동작하는지 확인합니다. 왕복 지연과 모델 답 글을 돌려줍니다.
// 초보: 자산 그래프나 탐색 그래프를 건드리지 않습니다. 화면의 연결 테스트가 이 함수를 탑니다.
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
