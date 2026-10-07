package agent

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// ChatAgent 는 대화 화면 뒤의, 작업에 묶이지 않은 범용 대화 실행기입니다.
// MainAgent.Chat 을 일반화합니다. 내장이든 사용자 정의든 key 로 아무 에이전트와
// 대화할 수 있고, 여러 턴의 기록은 transcript 에서 이어 갑니다. 순수한 도우미입니다.
// 기본 도구는 SDK DefaultTools(Bash/Read/Write/Edit/LS/Glob/Grep)와, 그 key 에
// 보이게 한 스킬/MCP 입니다. 침투용 그래프나 작업 맥락은 넣지 않습니다.
// 그것은 메인 에이전트만의 일입니다.
// 초보: 작업의 탐색 그래프와 자산 그래프를 건드리지 않는 일반 대화입니다.
type ChatAgent struct {
	prov           llm.Provider
	model          string
	workDir        string
	tx             *transcript.Store
	window         int
	proxyAddr      string
	proxyCACert    string
	webSearch      WebSearchOpts
	guard          *guard.Guard // 선택. nil 이면 이 대화의 가로채기 훅을 끕니다
	nonStreamingFn func() bool  // 해석기: 비스트리밍(Complete) 경로를 쓸까 (nil = 스트리밍)
	noaEnabledFn   func() bool  // 해석기: 실험용 noa 압축을 쓸까 (nil = 끔)
	maxTokensFn    func() int   // 해석기: 답 하나의 출력 상한 (nil/0 = 상한을 보내지 않음)
}

func NewChatAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window int) *ChatAgent {
	return &ChatAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window}
}

// SetNonStreaming 은 대화 실행이 비스트리밍 모델 경로를 쓸지 정하는 해석기를 연결합니다
// (true = 비스트리밍). nil 이거나 없으면 스트리밍입니다.
func (c *ChatAgent) SetNonStreaming(fn func() bool) { c.nonStreamingFn = fn }

func (c *ChatAgent) nonStreaming() bool { return c.nonStreamingFn != nil && c.nonStreamingFn() }

// SetNoaEnabled 는 대화 실행이 실험용 noa 맥락 압축을 쓸지 정하는 해석기를 연결합니다.
// nil 이거나 없으면 꺼집니다(내장 압축). 실행마다 읽으므로, 에이전트를 다시 만들지 않아도
// 설정 스위치가 적용됩니다.
func (c *ChatAgent) SetNoaEnabled(fn func() bool) { c.noaEnabledFn = fn }

// SetMaxTokens 는 답 하나의 출력 상한 해석기를 연결합니다. nil, 없음, 또는 0 이면
// 상한을 보내지 않고 끝점이 정하게 둡니다. nonStreaming 처럼 실행마다 읽습니다.
func (c *ChatAgent) SetMaxTokens(fn func() int) { c.maxTokensFn = fn }

func (c *ChatAgent) maxTokens() int {
	if c.maxTokensFn == nil {
		return 0
	}
	return c.maxTokensFn()
}

// SetProxy 는 대화 에이전트의 WebFetch/Bash 를 기록 프록시와, 그것이 믿는 CA 인증서로 보냅니다
// (주소가 비면 직접 연결). 다른 에이전트와 맞추려고 둡니다.
func (c *ChatAgent) SetProxy(addr, caCert string) { c.proxyAddr, c.proxyCACert = addr, caCert }

// SetWebSearch 는 대화 에이전트의 web_search 뒷단을 고릅니다(기본은 꺼짐).
func (c *ChatAgent) SetWebSearch(o WebSearchOpts) { c.webSearch = o }

// SetGuard 는 사용자가 정한 가로채기 규칙이 있는 가드를 이 대화 에이전트에 붙입니다.
// Chat 보다 먼저 불러야 합니다. 여러 번 불러도 안전합니다.
func (c *ChatAgent) SetGuard(g *guard.Guard) { c.guard = g }

// chatWorkDirSpec 은 모든 대화 에이전트 시스템 프롬프트 뒤에 붙는 작업 디렉터리 안내입니다.
// artifactSpec 과 비슷하지만, 일반 도우미에게 어색한 침투 용어("payload", "응답 본문 수집")는 없습니다.
func chatWorkDirSpec(workDir string) string {
	return "\n\n**文件输出规约**：需要写文件时，一律写到工作目录 " + workDir + "（这是默认 CWD，相对路径即落在这里，也可用该绝对路径）——不要写 /tmp 或其他绝对路径。"
}

// chatSystem 은 agentKey 의 DB 관리 프롬프트 본문을 렌더합니다. 사용자 에이전트에는
// 키마다의 코드 기본값이 없으므로, 렌더가 실패하면 DefaultAssistantPrompt 로 돌아갑니다.
func chatSystem(agentKey, dataDir, workDir string) string {
	return renderSystem(agentKey, DefaultAssistantPrompt, chatVars{DataDir: dataDir, Now: nowStr()}) + chatWorkDirSpec(workDir)
}

// chatVars 는 사용자 에이전트 프롬프트가 가리킬 수 있는 실행 중 변수입니다.
// DataDir(서버 데이터 뿌리)과 Now(서버 시각, 턴마다 새로 고침)가 공통입니다.
// 그 밖의 {{.X}} 는 렌더가 실패해 DefaultAssistantPrompt 로 돌아갑니다.
type chatVars struct{ DataDir, Now string }

// Chat 은 agentKey 에이전트와 대화 한 턴을 돌리고, sessionID 로 이전 기록을 이어 갑니다.
// maxTurns 는 턴마다의 에이전트 걸음 예산입니다(0 = 무제한). maxDuration 은 턴마다의
// 벽시계 예산입니다(0 = 무제한). Chat 을 부를 때마다 시계가 다시 시작하므로,
// 새 사용자 메시지는 항상 새 카운트다운입니다. webSearch 는 이 에이전트의 인터넷 검색을
// 여닫습니다(전역 뒷단과 키는 여전히 대화 에이전트 설정에서 오고, 켜고 끄기만 에이전트마다 정합니다).
// emit 은 실행 단계(thinking / tool_use / tool_result / text / result)를 받고,
// 에이전트 key 를 워커 칸 이름으로 답니다.
func (c *ChatAgent) Chat(ctx context.Context, agentKey, sessionID, message string, maxTurns int, maxDuration time.Duration, webSearch bool, emit func(db.Activity)) (string, error) {
	// 전역 웹 검색 설정을 이 에이전트 자신의 스위치로 여닫습니다.
	ws := c.webSearch
	if !webSearch {
		ws.Enabled = false
	}

	// 세션마다의 작업 디렉터리: <workDir>/sessions/<sessionID>/
	// 대화끼리 파일 쓰기를 나눕니다. 워커가 i<intentID>/ 를 쓰는 것과 같습니다.
	sessionWorkDir := filepath.Join(c.workDir, "sessions", sessionID)
	_ = os.MkdirAll(sessionWorkDir, 0o755)
	ctx = intercept.WithReviewWorkingDirectory(ctx, sessionWorkDir)

	// 순수한 도우미입니다. 기본은 DefaultTools 입니다. AugmentTools 가 그 key 에
	// 보이는 스킬/MCP 를 얹고, DB tools 표가 거르거나 덮어쓰게 합니다. DefaultTools 는
	// tools 표에 행이 없어 항상 통과합니다.
	base := actool.DefaultTools()
	ctx = WithRunInfo(ctx, RunInfo{SessionID: sessionID})
	tools, def, cleanup := AugmentTools(ctx, agentKey, base)
	defer cleanup()

	system, boundary := deferredSystem(chatSystem(agentKey, c.workDir, sessionWorkDir), def)
	opts := agentcore.Options{
		Provider:        c.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시에 흔적을 남깁니다. 프록시 CA 를 읽어 MITM 으로 다시 서명된 HTTPS 인증서를 검증합니다
		WebFetchProxy:   c.proxyAddr,
		WebFetchCACert:  c.proxyCACert,
		// 인터넷 검색(선택). ddgs 는 키가 필요 없습니다. brave-free 는 BraveKey, tavily 는 TavilyKey 가 필요합니다.
		// WebSearchProxy 는 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 별개입니다. 비우면 직접 연결합니다.
		EnableWebSearch:       ws.Enabled,
		WebSearchBackend:      ws.Backend,
		BraveSearchAPIKey:     ws.BraveKey,
		TavilySearchAPIKey:    ws.TavilyKey,
		DeepSeekSearchBaseURL: ws.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  ws.DeepSeekAPIKey,
		DeepSeekSearchModel:   ws.DeepSeekModel,
		WebSearchProxy:        ws.Proxy,
		BashEnv:               proxyEnv(c.proxyAddr, c.proxyCACert), // Bash 자식 프로세스는 기본적으로 프록시를 타고 CA 를 신뢰합니다
		WorkingDir:            sessionWorkDir,
		MaxTurns:              maxTurns,
		MaxDuration:           maxDuration,
		Compaction:            compactionConfig(c.window),
		Todos:                 actool.NewTodoStore(),
		// 큰 도구 출력은 세션 디렉터리 아래 cmd-output/ 으로 넘칩니다.
		// 자르는 상한은 SDK 기본(tool.Capture 의 30000 문자)입니다.
		ToolOutputDir: filepath.Join(sessionWorkDir, "cmd-output"),
		// 예산(걸음 수)에 닿으면 SDK 가 마무리를 실행합니다. 요약 한 문장을 냅니다. Prompt 와 마무리 턴 수는 이 에이전트 key 로 관리 화면에서 고칠 수 있습니다
		// (사용자 에이전트는 각자 한 벌. 비우거나 0 이면 공통 기본 10턴).
		Settlement:   wrapupSettlement(agentKey, nil),
		NonStreaming: c.nonStreaming(), // 이 profile 이 비스트리밍이면 Provider.Complete 를 탑니다
		MaxTokens:    c.maxTokens(),    // 0 = 상한을 보내지 않음. 서버 기본값
	}
	if c.guard != nil {
		opts.Hooks = c.guard.Hooks()
	}
	if c.tx != nil { // 사람↔AI 원문 대화를 남깁니다. 스레드마다 파일이 하나씩 쌓입니다
		opts.Transcript = c.tx
		opts.SessionID = sessionID
	}
	// 실험 기능: 켜면 noa 가 맥락 압축을 맡습니다(아카이브는 <workDir>/noa/<SessionID> 아래, 유지됨).
	enableNoa(&opts, c.noaEnabledFn, c.workDir, "chat-"+sessionID, noaWarn("chat-"+sessionID))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// 이전 대화를 다시 읽어, 턴을 넘어 맥락이 있게 합니다(Chat 마다 세션은 새것입니다).
	// 첫 턴에는 파일이 아직 없습니다. Resume 는 아무것도 읽지 않고 진행합니다.
	if c.tx != nil {
		_ = s.Resume(sessionID)
	}
	// 다시 읽은 기록의 Skill() 호출로, 스킬에 묶인 MCP 를 다시 엽니다.
	// 새로 만든 세션에서도 드러난 도구를 계속 부를 수 있습니다.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = agentKey
			emit(r)
		}
	})
	return text, err
}
