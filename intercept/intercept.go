// Package intercept 는 도구 호출을 실행 전에 가로채는 규칙 층입니다.
//
// 초보: 규칙은 PostgreSQL 에 있고, 메모리에 올려 우선순위가 높은 것부터 봅니다.
// allow 는 통과, deny 는 막고 이유를 돌려주며, ask 는 사람이 승인할 때까지 멈춥니다.
// 승인은 /api/intercept/pending/{id}/decide 로 받습니다. 가드가 이 판단을 호출합니다.
// 시간 제한은 SetTimeoutConfig 로 바꿉니다.
package intercept

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ctxKey 는 키가 서로 섞이지 않게, 이 패키지 밖으로 나가지 않는 컨텍스트 키 타입입니다.
type ctxKey int

// ConvIDKey 는 지금 대화의 ID 를 context.Context 에 넣어 둡니다.
// 가로채기가 ask 대기 기록을 그 대화에 연결하게 합니다.
const ConvIDKey ctxKey = 0

// WithConvID 는 convID 를 담은 자식 컨텍스트를 돌려줍니다.
func WithConvID(ctx context.Context, convID int64) context.Context {
	return context.WithValue(ctx, ConvIDKey, convID)
}

// ConvIDFromContext 는 대화 ID 를 꺼냅니다. 없으면 0 입니다.
func ConvIDFromContext(ctx context.Context) int64 {
	v, _ := ctx.Value(ConvIDKey).(int64)
	return v
}

// taskCtxKey 는 작업 값만을 위한, 따로 둔 비공개 컨텍스트 키 타입입니다.
type taskCtxKey int

const (
	taskInfoCtxKey taskCtxKey = 1
	taskEmitCtxKey taskCtxKey = 2
)

type taskCtxInfo struct{ taskID, agentName string }

// WithTaskContext 는 작업 정보와 기록 함수를 ctx 에 넣습니다.
// HandleAsk 가 대기 기록에 표시를 달고, 탐색 활동 흐름에 intercept_request 를 씁니다.
// 그러면 세션 기록 안에 승인 카드가 바로 보입니다.
// 초보: 가드 판정이 아닙니다. 승인 대기를 탐색 그래프 기록에 올리는 연결입니다.
func WithTaskContext(ctx context.Context, taskID, agentName string, emit func(db.Activity)) context.Context {
	ctx = context.WithValue(ctx, taskInfoCtxKey, taskCtxInfo{taskID, agentName})
	if emit != nil {
		ctx = context.WithValue(ctx, taskEmitCtxKey, emit)
	}
	return ctx
}

func taskInfoFromCtx(ctx context.Context) (taskID, agentName string) {
	if v, ok := ctx.Value(taskInfoCtxKey).(taskCtxInfo); ok {
		return v.taskID, v.agentName
	}
	return "", ""
}

func taskEmitFromCtx(ctx context.Context) func(db.Activity) {
	f, _ := ctx.Value(taskEmitCtxKey).(func(db.Activity))
	return f
}

// compiledRule 은 정규식을 미리 컴파일해 둔 InterceptRule 입니다. 문자열 규칙은 nil 입니다.
type compiledRule struct {
	db.InterceptRule
	re *regexp.Regexp
}

// pendingManager 는 진행 중인 ask 요청을, 요청마다 채널 하나로 추적합니다.
type pendingManager struct {
	mu sync.Mutex
	ch map[int64]chan bool
}

func newPendingManager() *pendingManager { return &pendingManager{ch: map[int64]chan bool{}} }

func (p *pendingManager) add(id int64) chan bool {
	ch := make(chan bool, 1)
	p.mu.Lock()
	p.ch[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *pendingManager) resolve(id int64, allowed bool) {
	p.mu.Lock()
	ch, ok := p.ch[id]
	delete(p.ch, id)
	p.mu.Unlock()
	if ok {
		ch <- allowed
	}
}

func (p *pendingManager) remove(id int64) {
	p.mu.Lock()
	delete(p.ch, id)
	p.mu.Unlock()
}

// Reviewer 는 도구 호출 하나의 LLM 예비 판정을 실행하고 Decision 으로 돌려줍니다.
// Action 은 allow, ask, deny 중 하나이고, 비어 있으면 답을 해석하지 못한 것입니다.
// 서버 층이 넣어 주므로 이 패키지는 llm 에 의존하지 않습니다.
// profileID 가 0 이면 지금 켜져 있는 기본 프로필을 씁니다.
type Reviewer func(ctx context.Context, profileID int64, prompt string, input ReviewInput) (Decision, error)

// Interceptor 는 DB 에서 가로채기 규칙을 읽어 도구 호출에 적용합니다.
// 여러 곳에서 동시에 써도 안전합니다.
type Interceptor struct {
	db           *db.DB
	mu           sync.RWMutex
	cached       []compiledRule  // 우선순위 내림차순으로 정렬됨. nil 이면 아직 읽지 않음
	enabledTools map[string]bool // nil 이면 아직 읽지 않음
	pending      *pendingManager
	reviewer     Reviewer // nil 이면 LLM 예비 판정이 연결되지 않음
}

// SetReviewer 는 LLM 예비 판정 콜백을 붙입니다. nil 을 넘기면 끕니다.
func (i *Interceptor) SetReviewer(r Reviewer) {
	i.mu.Lock()
	i.reviewer = r
	i.mu.Unlock()
}

// defaultEnabledTools 는 intercept_enabled_tools 설정이 한 번도 저장되지 않았을 때
// 가로채기 규칙으로 들어가는 도구의 고정 목록입니다.
var defaultEnabledTools = []string{
	"Bash", "WebFetch", "web_search",
	"shell_open", "shell_send",
	"Write", "Edit", "MultiEdit",
}

// New 는 d 에 연결된 Interceptor 를 만듭니다.
// 규칙 캐시는 처음 쓸 때 읽습니다.
func New(d *db.DB) *Interceptor {
	return &Interceptor{db: d, pending: newPendingManager()}
}

// Invalidate 는 메모리의 규칙 캐시와 사용 도구 캐시를 비웁니다.
// 다음 Match 또는 IsToolEnabled 가 DB 에서 다시 읽습니다.
// 규칙이나 도구 설정을 추가·수정·삭제한 뒤에 호출하세요.
func (i *Interceptor) Invalidate() {
	i.mu.Lock()
	i.cached = nil
	i.enabledTools = nil
	i.mu.Unlock()
}

func (i *Interceptor) loadLocked() error {
	rules, err := i.db.ListInterceptRules()
	if err != nil {
		return err
	}
	var out []compiledRule
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{InterceptRule: r}
		if r.MatchType == "regex" {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				continue // 잘못된 정규식 규칙은 건너뜁니다. 죽이지 않고 넘어갑니다
			}
			cr.re = re
		}
		out = append(out, cr)
	}
	i.cached = out

	// 설정에서 사용 도구 집합을 읽습니다. 없으면 고정 기본값을 씁니다.
	val, ok, _ := i.db.GetSetting("intercept_enabled_tools")
	if !ok {
		m := make(map[string]bool, len(defaultEnabledTools))
		for _, n := range defaultEnabledTools {
			m[n] = true
		}
		i.enabledTools = m
	} else {
		var names []string
		if json.Unmarshal([]byte(val), &names) != nil {
			i.enabledTools = map[string]bool{}
		} else {
			m := make(map[string]bool, len(names))
			for _, n := range names {
				m[n] = true
			}
			i.enabledTools = m
		}
	}
	return nil
}

func (i *Interceptor) rules() ([]compiledRule, error) {
	i.mu.RLock()
	if i.cached != nil {
		out := i.cached
		i.mu.RUnlock()
		return out, nil
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cached != nil {
		return i.cached, nil
	}
	if err := i.loadLocked(); err != nil {
		return nil, err
	}
	return i.cached, nil
}

// IsToolEnabled 는 그 이름의 도구가 가로채기 대상이면 true 를 돌려줍니다.
// 대상이면 규칙 맞추기 경로로 들어갑니다.
// rules() 와 같이, 잠금을 두 번 확인하는 방식을 씁니다.
func (i *Interceptor) IsToolEnabled(name string) bool {
	i.mu.RLock()
	if i.enabledTools != nil {
		v := i.enabledTools[name]
		i.mu.RUnlock()
		return v
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.enabledTools == nil {
		_ = i.loadLocked()
	}
	return i.enabledTools[name]
}

// GetEnabledTools 는 지금 가로채기 규칙으로 들어가게 설정된 도구 이름을, 순서대로 돌려줍니다.
// 설정을 저장한 적이 없으면 고정 기본 목록을 돌려줍니다.
func (i *Interceptor) GetEnabledTools() ([]string, error) {
	val, ok, err := i.db.GetSetting("intercept_enabled_tools")
	if err != nil {
		return nil, err
	}
	if !ok {
		out := make([]string, len(defaultEnabledTools))
		copy(out, defaultEnabledTools)
		return out, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(val), &names); err != nil {
		return []string{}, nil
	}
	return names, nil
}

// SetEnabledTools 는 가로채기 규칙으로 넣을 도구 이름 목록을 저장합니다.
// 이어서 캐시를 비워, 다음 호출이 새 목록을 쓰게 합니다.
func (i *Interceptor) SetEnabledTools(tools []string) error {
	b, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	if err := i.db.SetSetting("intercept_enabled_tools", string(b)); err != nil {
		return err
	}
	i.Invalidate()
	return nil
}

// Decision 은 규칙이 맞았을 때의 결과입니다.
type Decision struct {
	ModelInput       json.RawMessage
	ModelInputDigest string
	ModelFallback    bool
	RuleName         string
	ConfigDigest     string
	ProfileID        int64
	Action           string // "allow" | "deny" | "ask". 통과, 차단, 또는 사람 승인
	Message          string
	RuleID           int64
	TimeoutEnabled   bool
	TimeoutSeconds   int
	TimeoutAction    string // "deny" | "allow". 시간 초과 시 차단 또는 통과
}

// --- LLM 예비 판정 ---

// 판정 설정 키입니다. 설정용 키-값 테이블에 저장됩니다. 문서 3절을 보세요.
const (
	settingJudgeEnabled          = "llm_judge_enabled"
	settingJudgeProfileID        = "llm_judge_profile_id"
	settingJudgePrompt           = "llm_judge_prompt"
	settingJudgeTimeoutSecs      = "llm_judge_timeout_seconds"
	settingJudgeFailAction       = "llm_judge_fail_action"
	settingJudgeAskTimeoutSecs   = "llm_judge_ask_timeout_seconds"
	settingJudgeAskTimeoutAction = "llm_judge_ask_timeout_action"
)

// 판정 기본값입니다.
const (
	defaultJudgeTimeoutSecs      = 15
	defaultJudgeFailAction       = "allow"
	defaultJudgeAskTimeoutSecs   = 300
	defaultJudgeAskTimeoutAction = "deny"
)

// JudgeConfig 는 확정된 LLM 예비 판정 설정입니다.
// Prompt 는 항상 비어 있지 않습니다. 없으면 DefaultJudgePrompt 를 씁니다.
type JudgeConfig struct {
	Enabled           bool   `json:"enabled"`
	ProfileID         int64  `json:"profile_id"` // 0 이면 활성 기본 프로필을 따릅니다
	Prompt            string `json:"prompt"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	FailAction        string `json:"fail_action"` // allow|ask|deny. 통과, 질문, 또는 차단
	AskTimeoutSeconds int    `json:"ask_timeout_seconds"`
	AskTimeoutAction  string `json:"ask_timeout_action"` // allow|deny. 통과 또는 차단
}

// judgeConfig 는 설정에서 판정 구성을 읽습니다. 없거나 잘못된 키에는 기본값을 넣습니다.
// 예비 판정마다 새로 읽습니다. 이어지는 LLM 호출에 비하면 키를 몇 번 읽는 비용은 작습니다.
// 매번 읽으면 캐시를 무효로 만드는 별도의 경로가 필요 없습니다.
func (i *Interceptor) judgeConfig() JudgeConfig {
	c := JudgeConfig{
		Enabled:           i.db.GetBool(settingJudgeEnabled, false),
		ProfileID:         int64(i.getSettingInt(settingJudgeProfileID, 0)),
		TimeoutSeconds:    i.getSettingInt(settingJudgeTimeoutSecs, defaultJudgeTimeoutSecs),
		FailAction:        i.getSettingChoice(settingJudgeFailAction, defaultJudgeFailAction, "allow", "ask", "deny"),
		AskTimeoutSeconds: i.getSettingInt(settingJudgeAskTimeoutSecs, defaultJudgeAskTimeoutSecs),
		AskTimeoutAction:  i.getSettingChoice(settingJudgeAskTimeoutAction, defaultJudgeAskTimeoutAction, "allow", "deny"),
	}
	// Prompt: 저장된 값이 비어 있지 않으면 그 값, 아니면 기본 템플릿입니다.
	if v, ok, _ := i.db.GetSetting(settingJudgePrompt); ok && strings.TrimSpace(v) != "" {
		c.Prompt = v
	} else {
		c.Prompt = DefaultJudgePrompt
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = defaultJudgeTimeoutSecs
	}
	if c.AskTimeoutSeconds <= 0 {
		c.AskTimeoutSeconds = defaultJudgeAskTimeoutSecs
	}
	return c
}

func (i *Interceptor) getSettingInt(key string, def int) int {
	v, ok, err := i.db.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func (i *Interceptor) getSettingChoice(key, def string, allowed ...string) string {
	v, ok, err := i.db.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	v = strings.TrimSpace(v)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

// GetJudgeConfig 는 API 와 설정 화면에 줄, 확정된 판정 설정을 돌려줍니다.
// Prompt 는 실제로 쓰일 문구입니다. 설정이 없으면 기본 템플릿이라 화면이 미리 채울 수 있습니다.
func (i *Interceptor) GetJudgeConfig() JudgeConfig { return i.judgeConfig() }

// SetJudgeConfig 는 판정 설정을 저장합니다.
// Prompt 가 비어 있으면 덮어쓰기를 지우고, 기본 템플릿을 다시 씁니다.
func (i *Interceptor) SetJudgeConfig(c JudgeConfig) error {
	if err := i.db.SetBool(settingJudgeEnabled, c.Enabled); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeProfileID, strconv.FormatInt(c.ProfileID, 10)); err != nil {
		return err
	}
	// 기본 템플릿과 다를 때만 프롬프트를 저장합니다.
	// 그래서 한 번도 고치지 않은 사용자에게는 DefaultJudgePrompt 의 버전 갱신이 그대로 적용됩니다.
	promptToStore := ""
	if strings.TrimSpace(c.Prompt) != "" && strings.TrimSpace(c.Prompt) != strings.TrimSpace(DefaultJudgePrompt) {
		promptToStore = c.Prompt
	}
	if err := i.db.SetSetting(settingJudgePrompt, promptToStore); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeTimeoutSecs, strconv.Itoa(c.TimeoutSeconds)); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeFailAction, c.FailAction); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeAskTimeoutSecs, strconv.Itoa(c.AskTimeoutSeconds)); err != nil {
		return err
	}
	if err := i.db.SetSetting(settingJudgeAskTimeoutAction, c.AskTimeoutAction); err != nil {
		return err
	}
	return nil
}

// Judge 는 어떤 규칙에도 안 걸린 도구 호출에 LLM 예비 판정을 돌립니다.
// 판정이 끝나면 (Decision, true) 를 돌려줍니다.
// 예비가 꺼졌거나 연결되지 않았으면 (빈 Decision, false) 입니다. 호출자는 지금처럼 allow 로 둡니다.
// 모델 오류나 해석할 수 없는 답이면, 설정된 FailAction 으로 돌아갑니다.
// ask 판정에는 사람 승인 시간 제한이 들어 있어, 기존 HandleAsk 가 그대로 처리합니다.
// 초보: 가드가 규칙 다음에 부르는 둘째 관문입니다. ask 면 승인 화면으로 갑니다.
func (i *Interceptor) Judge(ctx context.Context, tool string, arguments json.RawMessage) (Decision, bool) {
	cfg := i.judgeConfig()
	i.mu.RLock()
	rv := i.reviewer
	i.mu.RUnlock()
	if !cfg.Enabled || rv == nil {
		return Decision{}, false
	}

	cctx := ctx
	if cfg.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	input, contextErr := BuildReviewInput(cctx, tool, arguments)
	cfg.Prompt = EffectiveJudgePrompt(cfg.Prompt)
	var out Decision
	var err error
	var modelInput []byte
	if contextErr != nil {
		// 지금 인자가 잘못되면 내용을 그대로 심사할 수 없습니다.
		// 모델 실패 때 어떻게 할지와 상관없이, 사람이 입력을 확인해야 합니다.
		out = Decision{Action: "ask", ModelFallback: true, Message: "심사 맥락이 불완전해서 사람 확인이 필요합니다: " + contextErr.Error()}
	} else {
		modelInput, _ = json.Marshal(input)
		out, err = rv(cctx, cfg.ProfileID, cfg.Prompt, input)
	}
	if err != nil {
		out = Decision{ProfileID: out.ProfileID, ModelFallback: true, Action: cfg.FailAction, Message: "모델 승인이 실패해 실패 정책대로 처리합니다: " + err.Error()}
	}
	switch out.Action {
	case "allow", "ask", "deny":
		// 해석에 성공한 판정입니다
	default:
		out = Decision{ProfileID: out.ProfileID, ModelFallback: true, Action: cfg.FailAction, Message: "모델 출력을 해석할 수 없어 실패 정책대로 처리합니다"}
	}
	// 모델 판정에는 규칙이 없습니다. 기록의 RuleID 는 0, 즉 NULL 로 둡니다.
	out.RuleID = 0
	if len(modelInput) > 0 {
		out.ModelInput = modelInput
		out.ModelInputDigest = digestInput(modelInput)
	}
	if out.ProfileID != 0 {
		cfg.ProfileID = out.ProfileID
	}
	out.ProfileID = cfg.ProfileID
	configJSON, _ := json.Marshal(cfg)
	out.ConfigDigest = digestInput(configJSON)
	if out.Message == "" {
		out.Message = "[모델] " + judgeActionLabel(out.Action)
	} else if !strings.HasPrefix(out.Message, "[모델]") {
		out.Message = "[모델] " + out.Message
	}
	if out.Action == "ask" {
		out.TimeoutEnabled = true
		out.TimeoutSeconds = cfg.AskTimeoutSeconds
		out.TimeoutAction = cfg.AskTimeoutAction
	}
	return out, true
}

func judgeActionLabel(action string) string {
	switch action {
	case "allow":
		return "허용"
	case "deny":
		return "차단"
	case "ask":
		return "사람 승인으로 넘김"
	default:
		return action
	}
}

// Match 는 도구 호출을 규칙 목록과 비교합니다. 우선순위가 높은 것부터 봅니다.
// 켜진 규칙 가운데 처음 맞는 것을 (Decision, true) 로 돌려줍니다.
// 맞는 규칙이 없으면 (빈 Decision, false) 입니다.
// 초보: 가드가 먼저 부르는 규칙 판정입니다. 여기 안 걸리면 Judge 로 갈 수 있습니다.
func (i *Interceptor) Match(toolName string, input []byte) (Decision, bool) {
	rules, err := i.rules()
	if err != nil || len(rules) == 0 {
		return Decision{}, false
	}
	for _, r := range rules {
		if ruleMatches(r, toolName, input) {
			msg := r.Message
			if msg == "" {
				msg = defaultMessage(r.Action, r.Name)
			}
			configJSON, _ := json.Marshal(r.InterceptRule)
			return Decision{
				RuleName: r.Name, ConfigDigest: digestInput(configJSON),
				Action:         r.Action,
				Message:        msg,
				RuleID:         r.ID,
				TimeoutEnabled: r.TimeoutEnabled,
				TimeoutSeconds: r.TimeoutSeconds,
				TimeoutAction:  r.TimeoutAction,
			}, true
		}
	}
	return Decision{}, false
}

func ruleMatches(r compiledRule, toolName string, input []byte) bool {
	var subject string
	switch r.MatchTarget {
	case "tool_name":
		subject = toolName
	case "tool_input":
		subject = string(input)
	default:
		return false
	}
	if r.MatchType == "regex" {
		return r.re != nil && r.re.MatchString(subject)
	}
	return strings.Contains(subject, r.Pattern)
}

func defaultMessage(action, name string) string {
	switch action {
	case "deny":
		return "가로채기 규칙 [" + name + "] 이 도구 실행을 금지합니다"
	case "ask":
		return "가로채기 규칙 [" + name + "] 은 사용자 승인을 요구합니다. 기다려 주세요"
	default:
		return ""
	}
}

// Log 는 allow 또는 deny 로 이미 끝난 규칙·모델 결정을 intercept_pending 에 넣습니다.
// 상태는 "allowed" 또는 "denied" 이고, 나중에 보려고 남기는 기록입니다.
// HandleAsk 와 달리 멈추지 않으며, 사용자가 할 일도 없습니다.
// 기록 페이지(GET /api/intercept/history)와 그 작업의 가로채기 목록에 보이게 합니다.
// DB 오류는 삼킵니다. 기록 실패가 도구 호출의 결과를 바꾸면 안 됩니다.
// 대기 목록(status 가 pending)은 건드리지 않아, 결정을 기다리는 ask 만 계속 보입니다.
// 초보: 승인 화면의 대기 칸이 아니라, 이미 끝난 결정을 이력에 남기는 함수입니다.
func (i *Interceptor) Log(ctx context.Context, convID int64, dec Decision, toolName string, input []byte, status string) {
	taskID, agentName := taskInfoFromCtx(ctx)
	audit := auditFor(ctx, dec, input, status)
	id, err := i.db.CreateDecidedIntercept(dec.RuleID, convID, taskID, agentName, toolName, input, status, dec.Message, audit)
	if err == nil {
		i.bindResult(ctx, id, audit)
	}
}

// HandleAsk 는 승인 대기 기록을 만들고, 사용자가 정할 때까지 멈춥니다.
// 결정은 /api/intercept/pending/{id}/decide 로 들어오거나, 규칙별 시간이 지나면 끝납니다.
// 사용자가 허용하면 true 를 돌려줍니다.
//
// convID 가 0 이면 진행 중인 대화가 없는 백그라운드 작업입니다.
// 그래도 대기 기록은 만듭니다. conversation_id 는 NULL 이라, 승인 페이지와 옆 배지에 나타납니다.
// 워커 스레드는 채팅 때와 같이 멈춥니다. 사용자가 승인 페이지에서 풀어 줘야 계속됩니다.
// 초보: 승인 화면과 워커를 잇는 대기입니다. 가드가 규칙을 맞추는 단계는 아닙니다.
func (i *Interceptor) HandleAsk(ctx context.Context, convID int64, dec Decision, toolName string, input []byte) bool {
	ruleID := dec.RuleID
	taskID, agentName := taskInfoFromCtx(ctx)
	taskEmit := taskEmitFromCtx(ctx)

	audit := auditFor(ctx, dec, input, "pending")
	pendingID, err := i.db.CreateInterceptPending(ruleID, convID, taskID, agentName, toolName, input, dec.Message, audit)
	if err != nil {
		return false
	}

	i.bindResult(ctx, pendingID, audit)
	ch := i.pending.add(pendingID)
	defer i.pending.remove(pendingID)
	// 주기적으로 확인하는 쪽은, INSERT 가 커밋된 뒤 채널 등록 전에 결정할 수 있습니다.
	// 등록한 뒤에 다시 읽어, 그 결정이 사라지지 않게 합니다.
	// 그 다음 결정은 ch 로 전달됩니다.
	if saved, err := i.db.GetInterceptDetail(pendingID); err == nil && saved != nil && saved.Status != "pending" {
		return saved.Status == "allowed" || (saved.Audit != nil && saved.Audit.EffectiveAction == "allow")
	}

	detail, _ := json.Marshal(map[string]any{
		"pending_id": pendingID,
		"tool":       toolName,
		"input":      json.RawMessage(input),
	})
	activity := db.Activity{
		Kind:    "intercept_request",
		Summary: fmt.Sprintf("도구 %s 승인 요청 (#%d)", toolName, pendingID),
		Detail:  string(detail),
	}

	if convID != 0 {
		// 채팅: 대화 흐름에 바로 보이는 카드를 씁니다.
		_, _ = i.db.AppendConvActivity(convID, activity)
	} else if taskEmit != nil {
		// 작업 워커: 탐색 활동 흐름으로 보내, 세션 기록 안에 바로 보이게 합니다.
		// 기록 함수가 NodeID 와 Worker 를 찍습니다.
		taskEmit(activity)
	}

	if !dec.TimeoutEnabled {
		// 시간 제한이 없으면, 사용자가 정하거나 워커가 멈출 때까지 기다립니다.
		select {
		case allowed := <-ch:
			return allowed
		case <-ctx.Done():
			_, _ = i.db.ResolveIntercept(pendingID, "denied", "deny", "작업이 취소되었습니다")
			_ = i.db.CompleteIntercept(pendingID, audit.RunID, audit.ToolUseID, "not_executed", "실행 전에 작업이 취소되었습니다", false)
			return false
		}
	}

	secs := dec.TimeoutSeconds
	if secs <= 0 {
		secs = 60
	}
	timer := time.NewTimer(time.Duration(secs) * time.Second)
	defer timer.Stop()
	select {
	case allowed := <-ch:
		return allowed
	case <-timer.C:
		allowed := dec.TimeoutAction == "allow"
		action := "deny"
		if allowed {
			action = "allow"
		}
		resolved, err := i.db.ResolveIntercept(pendingID, "timeout", action, "승인 시간이 지나 시간 초과 정책대로 처리합니다")
		if err != nil {
			return false
		}
		if !resolved {
			detail, err := i.db.GetInterceptDetail(pendingID)
			return err == nil && detail != nil && (detail.Status == "allowed" || (detail.Audit != nil && detail.Audit.EffectiveAction == "allow"))
		}
		return allowed
	case <-ctx.Done():
		_, _ = i.db.ResolveIntercept(pendingID, "denied", "deny", "작업이 취소되었습니다")
		_ = i.db.CompleteIntercept(pendingID, audit.RunID, audit.ToolUseID, "not_executed", "실행 전에 작업이 취소되었습니다", false)
		return false
	}
}

var ErrAlreadyDecided = errors.New("승인이 이미 처리되었거나 없습니다. 기록을 새로고침하세요")

// Decide 는 대기 중인 요청을 끝냅니다. HTTP 결정 엔드포인트가 호출합니다.
// 초보: 승인 화면의 허용·거부가 여기로 들어와, 멈춰 있던 워커를 깨웁니다.
func (i *Interceptor) Decide(pendingID int64, allowed bool) error {
	status := "denied"
	if allowed {
		status = "allowed"
	}
	action, reason := "deny", "사람이 실행을 거부했습니다"
	if allowed {
		action, reason = "allow", "사람이 실행을 허용했습니다"
	}
	resolved, err := i.db.ResolveIntercept(pendingID, status, action, reason)
	if err != nil {
		return err
	}
	if !resolved {
		return ErrAlreadyDecided
	}
	i.pending.resolve(pendingID, allowed)
	return nil
}
