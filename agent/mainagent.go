package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent 는 사람이 쓰는 얇은 조율자입니다(문서 §4.2 / §7). 사람이 대화하고,
// 이것은 읽기 도구로 살피며, 힌트(→플래너)나 우선순위가 높은 의도(→프론티어)를
// 넣어 방향을 바꿉니다. 의도를 스스로 계속 만드는 고리는 돌리지 않습니다.
// 그것은 플래너만의 일입니다.
// 초보: 메인 에이전트는 탐색 그래프를 직접 파지 않습니다. 힌트와 의도로 플래너와 워커를 돌립니다.
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // LLM 원문 대화를 남김 (nil = 끔)
	window          int                                    // 맥락 창 크기(token). 압축에 씁니다
	windowFn        func() int                             // 선택. 작업 사슬의 동적 하한
	maxTurns        int                                    // 실행 한 번의 최대 턴 (0 = 무제한)
	proxyAddr       string                                 // WebFetch 용 기록 프록시 (비면 직접 연결)
	proxyCACert     string                                 // 기록 프록시 CA 인증서 경로 (HTTPS 검증)
	webSearch       WebSearchOpts                          // web_search 뒷단 선택 (기본은 꺼짐)
	workDir         string                                 // 공유 작업 디렉터리 (프롬프트에 산출물 위치로 나감)
	steerWork       func(intentID int64, msg string) error // 엔진 콜백: 돌고 있는 작업을 돌림 (nil = 끔)
	nonStreamingFn  func() bool                            // 해석기: 비스트리밍(Complete) 경로? (nil = 스트리밍)
	noaEnabledFn    func() bool                            // 해석기: 실험용 noa 압축? (nil = 끔)
	maxTokensFn     func() int                             // 해석기: 답 하나의 출력 상한 (nil/0 = 상한을 보내지 않음)
}

// SetNoaEnabled 는 실행이 실험용 noa 맥락 압축을 쓸지 정하는 해석기를 연결합니다.
// nil 이거나 없으면 꺼집니다(내장 압축). 실행마다 읽으므로, 에이전트를 다시 만들지 않아도
// 설정 스위치가 적용됩니다.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming 은 실행이 비스트리밍 모델 경로를 쓸지 정하는 해석기를 연결합니다
// (true = 비스트리밍). nil 이거나 없으면 스트리밍입니다(기본).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens 는 답 하나의 출력 상한 해석기를 연결합니다. nil, 없음, 또는 0 이면
// 상한을 보내지 않고 끝점이 정하게 둡니다. nonStreaming 처럼 실행마다 읽습니다.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy 는 메인 에이전트의 WebFetch 를 기록 프록시와, 그 HTTPS 를 검증할 CA 인증서로 보냅니다
// (주소가 비면 직접 연결).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch 는 메인 에이전트의 web_search 뒷단을 고릅니다(기본은 꺼짐).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork 는 엔진 콜백을 연결합니다. 메인 에이전트의 steer_work 도구가
// 돌고 있는 작업 한가운데 방향 수정을 넣습니다(nil = 도구 꺼짐).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl 은 메인 에이전트 프롬프트의 내장 편집 본문(구간 [A])입니다.
// agent_prompts 에 심습니다. Goal 은 {{.Goal}} 템플릿 변수입니다. 중간
// 산출물 출력 규약 꼬리는 코드가 소유합니다(artifactSpec). 렌더 뒤에 붙입니다.
const mainAgentDefaultTmpl = `你是一个授权渗透测试系统的"主 agent"，是人类操作员的接口。你不亲自探索、也不自主连续生成意图（那是规划者的工作）。你的职责：

1. 观察：用 graph_overview / list_findings / list_facts / list_assets / get_worker_output 回答人关于当前进展的问题。
2. 操舵（把人的意图落到系统）：
   - 人想"改方向/强调某类漏洞/重点某区域" → 用 add_hint 写提示（规划者下次会读到）。
   - 人想"立刻测某个具体目标" → 用 add_intent 直接注入一条高优先级意图（priority 8-10）。系统会自动把已完成的任务拉回运行态、让 worker 领这条意图执行，跑完即回到已完成状态。
     **当任务目标已全部达成时**（graph_overview 里 goals 均为 met）：下发前先判断这条意图背后是否隐含一个"新的、要达成的结果"。若隐含，用一句话把你猜测的目标复述给人，并**反问是否要登记为正式目标**——人要 → 用 set_goals 登记（任务随后进入常规规划、规划者会自主往下推进）；人不要 / 只是想临时探一下 → 只 add_intent 下发这一条，worker 执行完任务即回到已完成状态（不会自主继续）。若这条意图明显只是一次性查证、不隐含新目标，直接 add_intent 即可，不必每次都问。
   - 人想"对某条正在运行的意图(work)实时纠偏（别再走 X、聚焦 Y）" → 用 steer_work（不打断、不丢已有进展，worker 下一步动作前生效）；先用 get_worker_output 看它在干嘛。方向整个错了则改用 add_intent 另下新意图。
   - 人想"新增一个要达成的最终目标" → 用 set_goals 增补目标。系统会把该目标写入任务图并**自动把已完成/暂停的任务拉回运行态继续跑**（规划者随后会据此重新判断是否达成），无需人工再点恢复。
   - 人想"增/改测试约束（允许/禁止某类操作，如『仅测当前端口』『禁止爆破』『只做被动侦察』）" → 用 set_constraints 登记（type=allow 允许 / type=deny 禁止）。约束会在下一轮规划时注入 planner/worker 的提示词以框定探索边界；也可在总览「约束管理」里增删改。
3. 用人话简洁回复，说明你做了什么。

当前任务目标：{{.Goal}}

不要编造发现；只根据工具返回的真实数据回答。`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat 은 사람 메시지 하나를 처리하고 도우미 답을 돌려줍니다. emit 이 nil 이 아니면
// 실행 단계(thinking / tool_use / tool_result / text / result)를 받습니다.
// 메인 에이전트 세션이 마지막 답만이 아니라, 워커/플래너 세션처럼 일하는 과정을 보여 줍니다.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 일반 깨움(전용 콜백이 없는 쓰기 작업이 이것을 탐. debounced)
	tsx.SetResumeTask(resume)     // set_goals 가 목표를 추가하면 완료/일시정지된 작업을 running 으로 되돌립니다
	tsx.SetNotifyGoal(notifyGoal) // set_goals 가 목표를 추가하면 플래너에 「사람이 목표를 추가했습니다: …」 트리거를 하나 기록합니다
	tsx.SetNotifyHint(notifyHint) // add_hint 가 힌트를 추가하면 플래너에 「사람이 전략 힌트 N개를 추가했습니다: …」 트리거를 하나 기록합니다
	tsx.steerWork = m.steerWork   // steer_work 도구를 켭니다(nil = 쓸 수 없음)
	// 도메인 도구 + 기본 도구 묶음(Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// 자산 커버리지를 끄면 add_task_scope/list_untested_assets 를 뺍니다(프롬프트에 넣지 않음).
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 이 작업의 작업 디렉터리 <workDir>/tasks/<taskID>. 먼저 만들어 둡니다.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 기록 프록시에 흔적을 남깁니다. 프록시 CA 를 읽어 MITM 으로 다시 서명된 HTTPS 인증서를 검증합니다
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// 인터넷 검색(선택). ddgs 는 키가 필요 없습니다. brave-free 는 BraveKey, tavily 는 TavilyKey 가 필요합니다.
		// WebSearchProxy 는 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 별개입니다. 비우면 직접 연결합니다.
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash 자식 프로세스는 기본적으로 프록시를 타고 CA 를 신뢰합니다
		WorkingDir:            mainDir,                              // 이 작업 작업 디렉터리 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = 무제한(에이전트 관리에서 고칠 수 있음)
		Compaction:            compactionConfig(m.compactionWindow()), // 긴 대화도 창 안에 둡니다
		Todos:                 actool.NewTodoStore(),                  // 세션 단위 임시 할 일(TodoWrite). 계획용이며 끝나면 버립니다
		// 예산(걸음 수)에 닿으면 SDK 가 마무리를 실행합니다. 사용자에게 진행 요약 한 문장을 냅니다. Prompt 와 마무리 턴 수는 관리 화면에서 고칠 수 있습니다(기본 10턴).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // 이 profile 이 비스트리밍이면 Provider.Complete 를 탑니다
		MaxTokens:    m.maxTokens(),    // 0 = 상한을 보내지 않음. 서버 기본값
	}
	if m.tx != nil { // 사람↔AI 원문 대화를 남깁니다. 구간마다 파일이 하나씩 쌓입니다
		opts.Transcript = m.tx
		// 구간 0 은 예전 "exp%d-main" 이름을 유지해 기존 기록이 그대로 열립니다.
		// 새 세션(seg>=1)은 각자 파일을 받아 맥락이 깨끗합니다.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 실험 기능: 켜면 noa 가 맥락 압축을 맡습니다(아카이브는 <workDir>/noa/<SessionID> 아래, 유지됨).
	// session id 는 transcript 와 같은 규칙(구간을 알아챔)이라 아카이브와 복구가 맞습니다.
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// transcript 에서 이전 대화를 다시 읽어, 턴을 넘어 맥락이 있게 합니다
	// (Chat 마다 세션은 새것입니다. 이게 없으면 이전 메시지를 못 봅니다).
	// 첫 턴에는 파일이 아직 없습니다. Resume 는 아무것도 읽지 않고 진행합니다.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: 이 세션은 턴마다 새것입니다. 다시 읽은 기록의 Skill() 호출로
	// 스킬에 묶인 MCP 를 다시 열어, 드러난 도구를 계속 부를 수 있게 합니다.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
