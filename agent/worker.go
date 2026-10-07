package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker 는 LLM 작업 에이전트입니다(문서 §4.4). 의도 하나를 집어, 실제 도구로
// 끝내고(Bash 는 기록 프록시를 탑니다), 찾은 사실을 그래프에 쓴 뒤 멈춥니다.
// 새 방향을 만들지 않습니다(그것은 플래너의 일입니다). 목표를 향해 스스로
// 계속 탐색하지도 않습니다. 워커 여러 개가 고루틴으로 동시에 돕니다.
// 초보: 워커는 탐색 그래프의 의도 정확히 하나를 받아 실행하고, 사실과 발견을 남긴 뒤 멈춥니다.
// WebSearchOpts 는 서버가 각 에이전트(플래너/워커/메인)에 넣는 웹 검색 뒷단 선택입니다.
// Enabled=false 이면 web_search 도구가 꺼집니다.
// Backend 는 "ddgs"(키 없음), "brave-free"(BraveKey 필요), "tavily"(TavilyKey 필요),
// 또는 "deepseek"(DeepSeek* 필요, 활성 LLM profile 에서 채움)입니다.
// agentcore.Options 에 그대로 대응합니다.
// Proxy 는 검색 요청만의 출구 프록시입니다(http/https/socks5).
// 트래픽을 기록하는 MITM 프록시와는 별개입니다. 검색 끝점이 VPN/SOCKS 로만
// 닿을 때 넣습니다. 비면 직접 연결입니다.
//
// 주의: deepseek 백엔드는 다른 셋과 성격이 다릅니다. DeepSeek 에는 직접 호출할 검색 인터페이스가 없고,
// 검색은 Anthropic 호환 messages 인터페이스 안의 server tool(web_search_20250305)로만 있습니다.
// 그래서 검색마다 모델 호출을 한 번 쓰고, 검색 요청은 DeepSeek 서버가 보냅니다.
// 이 컴퓨터의 Proxy 를 타지 않고, 트래픽 기록에도 남지 않습니다.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* 는 지금 활성화된 LLM 설정에서 옵니다(anthropic 형식의 DeepSeek 공식 엔드포인트만).
	// 따로 설정하지 않으며, LLM 설정이 바뀌면 함께 바뀝니다.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // 기록 프록시 CA 인증서 경로(WebFetch HTTPS 검증)
	webSearch       WebSearchOpts     // web_search 뒷단 선택(기본은 꺼짐)
	tx              *transcript.Store // LLM 원문 대화를 남김(nil = 끔)
	window          int               // 맥락 창 크기(token). 압축에 씁니다
	windowFn        func() int        // 선택. 작업 사슬의 동적 하한
	maxTurns        int               // 실행 한 번의 최대 턴(0 = 무제한)
	// runTimeout 은 의도 하나의 본 탐색에 주는 벽시계 예산입니다
	// (0 = 무제한). 닿으면 실행을 자르고 마무리 라운드를 강제로 돌려,
	// 이미 알아낸 사실이 사라지지 않고 쓰이게 합니다.
	runTimeout time.Duration
	// extraTools 는 호스트가 넣는 도구입니다(예: 트래픽 조회). 워커가
	// 그래프에 다시 쓰는 도구 뒤에 붙습니다.
	extraTools []actool.CoreTool
	// injectConstraints 는 이 작업의 조작 제약을 워커 시스템 프롬프트에
	// 넣을지 해석합니다. 실행마다 읽으므로, 에이전트를 다시 만들지 않아도
	// 설정 스위치가 적용됩니다. nil 이면 넣습니다(기본).
	injectConstraints func() bool
	// nonStreamingFn 은 이번 실행이 비스트리밍(Complete) 경로를 쓸지 해석합니다.
	// 실행마다 읽으므로, profile 이나 작업 사슬 스위치가 재생성 없이 적용됩니다.
	// nil 이면 스트리밍입니다(기본).
	nonStreamingFn func() bool
	// noaEnabledFn 은 이번 실행이 실험용 noa 맥락 압축을 쓸지 해석합니다.
	// nonStreaming 처럼 실행마다 읽습니다. nil 이면 꺼집니다(내장 압축).
	noaEnabledFn func() bool
	// maxTokensFn 은 답 하나의 출력 상한(token)을 같은 실행 단위로 해석합니다.
	// nil 또는 0 이면 상한을 보내지 않고 끝점이 정하게 둡니다.
	maxTokensFn func() int
}

// WorkerSessionID 는 워커 의도가 쓰는 안정된 transcript 키를 돌려줍니다.
// 워커 자리는 재사용되므로, work#N 이 아니라 의도 id 가 세션 정체입니다.
// 이 도우미를 공개로 두어, 워커 메시지 API 와 UI 가 이어 갈 대화를 정확히 가리키게 합니다.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming 은 실행이 비스트리밍 모델 경로를 쓸지 정하는 해석기를 연결합니다
// (true = 비스트리밍). nil 이거나 없으면 스트리밍입니다(기본). 실행마다 읽으므로
// profile 이나 작업 사슬 스위치가 재생성 없이 적용됩니다.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled 는 실행이 실험용 noa 맥락 압축을 쓸지 정하는 해석기를 연결합니다.
// nil 이거나 없으면 꺼집니다(내장 압축). 실행마다 읽으므로, 에이전트를 다시 만들지 않아도
// 설정 스위치가 적용됩니다.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens 는 답 하나의 출력 상한 해석기를 연결합니다. nil, 없음, 또는 0 이면
// 상한을 보내지 않고 끝점이 정하게 둡니다. nonStreaming 처럼 실행마다 읽습니다.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject 는 이 작업의 조작 제약을 워커 시스템 프롬프트에 넣을지 정하는
// 해석기를 연결합니다. nil 이면 넣습니다(기본).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints 는 제약 주입이 켜졌는지 봅니다(기본은 켬).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout 은 의도 하나의 본 탐색에 벽시계 예산을 줍니다(0 = 무제한).
// 닿아도 SDK 마무리 단계는 돌아, 사실이 시간 초과로 사라지지 않습니다.
// Execute 전에 부르면 안전합니다.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt 는 워커가 턴/시간 예산에 닿을 때 SDK 마무리 단계가 넣습니다.
// 더 찌르지 말고, 찾은 것을 쓴 뒤, 한 줄 평문으로 끝내라고 합니다
// (그 한 줄이 이번 실행의 표시 결과가 됩니다).
const settleWrapUpPrompt = "你即将因预算耗尽被终止。不要再运行任何命令/探测。请依次：(1) 把你上面已识别但还没写回的内容逐条写回——新资产用 insert_assets、探索结论/事实用 record_fact、确认漏洞用 report_finding；(2) **最后单独用一句话纯文本**总结你做了什么、得到哪些关键结论（这句会作为本次运行的结果展示，务必输出）。" // han-allow 업스트림 프롬프트·픽스처

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept 는 actool.DefaultTools() 에서 이름 있는 도구를 뺍니다
// (CoreTool.Name() 기준). 에이전트가 가지면 안 되는 SDK 기본 도구를 자를 때 씁니다.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy 는 워커가 대상 트래픽을 보낼 기록 프록시 주소와, WebFetch 가
// 그 MITM 프록시의 HTTPS 를 검증할 CA 인증서 경로를 정합니다. 주소가 비면 안내를 끕니다.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch 는 이 워커의 web_search 뒷단을 고릅니다(기본은 꺼짐).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv 는 Bash 자식 프로세스의 환경을 만듭니다. 자식 명령의 HTTP 를
// 출구 프록시로 보냅니다(캡처가 켜지면 기록 MITM, 꺼지면 전역 프록시로 직접).
// MITM CA 가 있을 때만 흔한 도구 사슬이 그것을 믿게 합니다. 도구마다 -x/--proxy/-k 를
// 손으로 넣지 않아도 됩니다. 생태계마다 CA 변수가 다릅니다(실측): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests(SSL_CERT_FILE 을 무시함), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node. NODE_USE_ENV_PROXY 는 Node 24+ 가
// 프록시 변수를 따르게 합니다. ALL_PROXY 도 넣습니다. socks5 출구 프록시는 curl 이
// HTTP(S)_PROXY 가 아니라 ALL_PROXY 만 읽기 때문입니다. 캡처가 꺼진 경로에서 동작합니다.
// proxyAddr 이 비면 nil 입니다(직접 연결, 환경은 그대로).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 출구: curl 은 이것만 읽습니다
		"NODE_USE_ENV_PROXY=1", // Node 24+: 내장 fetch/http 가 HTTP(S)_PROXY 를 따릅니다
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl 은 워커 시스템 프롬프트의 내장 편집 본문(구간 [A])입니다.
// agent_prompts 에 심습니다. trafficTool 블록과 중간 산출물 출력 규약은
// 여기 없습니다. 코드가 소유하고, workerSystem 이 렌더 뒤에 붙입니다
// (구간 [B]/[C]). DB 본문을 고쳐도 그것들은 지워지지 않습니다.
const workerDefaultTmpl = `你是一个网络安全平台授权渗透测试系统的"执行者"(work agent)。你领到【一条意图】(一句话探索方向)，唯一职责：**完成这一条意图、把发现写回知识图谱、然后停止返回。**

**边界（红线）**：
1. **只做你领到的这一条意图**。**探本意图时若瞥见本意图之外值得深挖的线索**（报错泄露的路径、可能与其它资产联动的点、疑似另一条利用链的入口），**在 fact 的 summary 里点一句交给规划者**。
2. 初次受阻（payload 被过滤 / 404 / 注入无回显）不代表已探透——把本意图的所有绕过手段走完再输出结论；
3. 只在授权范围内操作。系统提示顶部若附【操作约束】，那是最高优先级红线：每条命令/探测执行前先自检，违反即不做（哪怕它落在你领到的意图里）。

**边发现边写回**（写进图才算数，脑子/文字里的不算；每得一个结果立刻写，别攒到最后被步数耗尽丢掉）。三种写回，别串图：
- **新资产/资源 → insert_assets（资产图）**：子域 / service / endpoint / 指纹 / 凭据 等一切资产【本身】。**这里只登记资产；探索结论/判断不写这里，用 record_fact。**
- **探索结论/事实 → record_fact（探索图，传 intent_id）**：都用它。**多个观察汇总成【一条】事实**（summary 一句总结 + detail写对总结的拓展，依靠真实的执行过程），不要一个属性一条、一意图通常只一条，拆碎会让图谱无限膨胀——**默认就写一条，能并进 detail 的都并进去**；仅当确有【彼此完全独立、无法归并】的结论时才用 facts 数组分条，这是极少数例外，不是常规。**只写增量**：只记这次【新得到】的，别把已有事实换措辞重记（只印证已有、无新增就不必记）。**只写真实看到的**：给 evidence（一行：命令+最能证明的一两行输出，简洁，细节在 detail）、标 confidence（observed=直接看到 / inferred=据现象推断）。
- **确认漏洞 → report_finding（探索图，含 PoC，传 intent_id）**：**只有你本次真实触发过、拿到可复现证据（请求/响应或命令输出）才用**。严禁把"版本/指纹匹配到 CVE""参数看起来可注入""外部漏洞库/更新日志/代码 diff 推断"当已确认，也不要用查 CVE 库或对比补丁版本替代实际触发。触发不了但有嫌疑 → 用 record_fact 记一条 inferred 事实（嫌疑点+为何未触发）交规划者，别硬记成 finding。


完成本意图后用一句话总结你做了什么、写回了哪些事实。`

// workerTrafficBlock 은 구간 [B]입니다. 트래픽 도구 안내이며, 트래픽 캡처(기록)가
// 켜져 있을 때만 코드가 넣습니다. 즉 traffic_* 도구가 실제로 있을 때입니다.
// 기록 여부로 여닫고, 출구 프록시로는 아닙니다. 캡처가 꺼진 전역 프록시는
// 트래픽을 보내지만 기록하지 않으므로, 그 도구는 없습니다. 저장하지 않고, 고칠 수도 없습니다.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**流量工具**：\n- traffic_search / traffic_get / traffic_blob：回看响应、找已访问过的资源，**先查流量、不要重复 curl 同一 URL**。traffic_search **必须指定 host**、默认只回 3 条极轻量索引(id/method/url/status/resp_len，无响应内容)，需要更多显式调大 limit；可用 body_contains 在请求/响应正文里做全文搜索(至少 3 字符，支持子串和中文，如找密码/密钥/报错/内网地址)；要看某条原文用 traffic_get(id)，其中超大正文显示为 @blob sha256:<hash>，用 traffic_blob(hash) 分段取全文。"
}

// artifactSpec 은 구간 [C]입니다. 코드가 소유하고 고칠 수 없는 꼬리로, 침투 에이전트
// 프롬프트마다 붙습니다. 중간 산출물은 공유 작업 디렉터리에 두고 /tmp 에는 두지 않습니다.
// DB 본문을 어떻게 고쳐도 이 꼬리는 남습니다.
func artifactSpec(dir string) string {
	return "\n\n**中间产物输出规约**：脚本、payload、抓到的响应体、临时数据等一切中间产物，**一律写到本任务工作目录 " + dir + "**（相对路径即写在这里，也可用该绝对路径）——**不要写 /tmp、不要用其它绝对路径**。"
}

// workerArtifactSpec 은 워커의 구간 [C]입니다. 의도마다의 실행 디렉터리는
// 엔진이 미리 만듭니다(ensureRunDir). 그래서 상대 경로로 쓰기만 하면 됩니다.
// 손으로 mkdir 하지 않고, 워커끼리 이름이 부딪히지 않습니다.
func workerArtifactSpec(runDir string) string {
	return "\n\n**中间产物输出规约**：脚本、payload、抓到的响应体、临时数据等一切中间产物，**一律写到本次意图的专属工作目录 " + runDir + "**（已自动建好，直接用相对路径写在这里即可，无需再手动建目录）——**不要写 /tmp、不要用其它绝对路径**。"
}

// ensureRunDir 는 base 아래에 에이전트 작업 디렉터리를 만들어 돌려줍니다.
// 플래너/메인은 <base>/tasks/<taskID>, 워커는 <base>/tasks/<taskID>/i<intentID>
// (intentID<=0 이면 작업 디렉터리만). "tasks/" 구간은 대화 에이전트의
// "sessions/<sessionID>" 와 같이, 작업마다 디렉터리를 모읍니다. mkdir 은 최선을 다합니다.
// 실패하면, 쓸 수 없는 CWD 처럼 쓰기가 실패합니다.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir 는 에이전트 실행 디렉터리 아래, SDK 가 큰 도구 출력을 넘기는 디렉터리입니다.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert 는 기록 MITM 이 켜져 있을 때만 있습니다. 그때 traffic_* 도구가 등록됩니다.
	// 그래서 이 값이 트래픽 도구 안내를 여닫습니다.
	// 발견 안내는 도구를 해석한 뒤 모든 역할에 선택으로 붙습니다.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask 는 집어 든 의도를 워커의 시작 USER 메시지 모양으로 만듭니다.
// 그 의도가 워커의 일 전부입니다. 예전에는 시스템 프롬프트에 있었고, 지금은
// 첫 사용자 턴에(상황 개요와 함께) 탑니다. 시스템 프롬프트는 고정된 역할만 남습니다.
// 플래너의 상황 블록과 같은 이동입니다.
// intentAssetIDs 는 의도 payload 에서 대상 자산 id 를 꺼냅니다
// (플래너의 add_intent 가 숫자 asset_ids 배열로 저장합니다). 없거나 payload 가
// 깨지면 nil 입니다.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【你领到的意图（本次唯一任务：只做这一条、只产生事实、做完即停）】：\n%s\n意图 id: %d（写回 record_fact / report_finding 时传它）", string(intent.Payload), intent.ID) // han-allow 업스트림 프롬프트·픽스처
}

// renderWorkerGraphOverview 는 전체 상황 스냅샷을 워커의 시작 USER 메시지에 접어 넣습니다.
// 알게 하려는 것뿐입니다. 문장을 일부러 세게 둡니다. 개요가 워커의 일을 넓히면 안 됩니다.
// 여전히 받은 의도만 합니다. 목적은 워커가 맥락(이미 있는 사실/자산/힌트)을 읽어
// 같은 일을 반복하지 않고, 남이 이미 찾은 것을 다시 끌어내지 않게 하는 것입니다.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage 는 플래너가 「어느 유형을 덜 봤는지 / 범위를 넓힐지」 판단하는 신호입니다. 워커는 「받은
	// 그 의도만 하고, 아직 안 덮인 점을 쫓지 말라」는 경계와 어긋납니다. 그래서 워커 뷰에서 뺍니다. data 는 이번 워커
	// 전용 새 map 이라 키를 지워도 플래너에는 영향이 없습니다.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // 조용히 폴백합니다. 워커는 전체 맥락 없이 진행합니다
	}
	return "\n\n【全局探索态势（只读，帮你把自己这条意图放进大局看）】：\n" + // han-allow 업스트림 프롬프트·픽스처
		"下面是整个任务当前的探索概况。用途有两个：一是知道别人已发现什么，别重复；二是让你探自己这条意图时，能联想到它和全局的关系。\n" + // han-allow 업스트림 프롬프트·픽스처
		"**发散是好事**：探本意图时尽管深想、多联想。唯一的界线是——别真的动手去执行别的意图（那是别的 worker 的事，由规划者调度）。但凡你联想到有价值的线索（跨资产的联动、疑似另一条利用链的入口、全局层面的可疑点），**务必写进 fact 交规划者**——这是你重要的产出，不是可有可无。宁可多报一条让规划者判断，也别自己咽下去。\n" + // han-allow 업스트림 프롬프트·픽스처
		string(b)
}

// Execute 는 의도 하나를 실행합니다. hooks(작업마다의 가드)가 도구 호출마다 문을 봅니다.
// nil 일 수 있습니다. emit 이 nil 이 아니면 실행 단계마다 활동 기록을 받습니다.
// notifyFinding 이 nil 이 아니면, 이 워커가 발견을 쓸 때(report_finding)
// (intentID, summary)로 부릅니다. 작업의 플래너가 워커가 끝나기를 기다리지 않고
// 도중에 깨어납니다. 어느 의도가 무엇을 찾았는지가 같이 갑니다.
// 종료 이유를 돌려줍니다(엔진이 completed 와 max_turns 를 구분합니다). 그리고
// 무엇을 썼는지 종류별 개수를 돌려줍니다. 탐색만 하고 아무것도 안 남긴 의도를
// 끝난 것으로 착각하지 않게 하고, 엔진이 사실/자산/발견을 "사실"로 뭉개지 않고
// 따로 기록하게 합니다.
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage 는 같은 의도 대화의 다음 턴을 사람이 쓴 메시지로 돌립니다.
// HTTP 핸들러는 transcript 를 고치지 않습니다. 이 워커가 시작할 때 agentcore 가
// 그 메시지를 평범한 사용자 턴으로 기록합니다. 워커를 이어 가는 흐름이
// 일반 에이전트 대화와 같게 유지됩니다.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // 이 워커가 찾은 자산은 그 의도에 앵커됩니다 → 작업에 보입니다
	tsx.SetEnrich(enr)                  // 이 워커가 쓴 자산의 DNS/HTTP 를 비동기로 채웁니다
	tsx.SetNotifyFinding(notifyFinding) // report_finding 이 기록되는 즉시 플래너를 깨웁니다. 「어느 의도+finding」을 함께 넘깁니다
	// base = 내장 워커 도구 ∪ 호스트 도구(기록 프록시 traffic) ∪ 기본 도구(Bash 포함).
	// 그다음 에이전트에 보이는 스킬/MCP 를 얹습니다. SDK 마무리 단계에서는
	// Settlement.DisabledTools 로 Bash 를 숨깁니다(여기서 따로 잠글 필요 없음).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// 워커에게 MultiEdit/Glob/Grep 을 일부러 주지 않습니다. 파일 수정은 Edit, 검색은 Bash(grep/find)로 하고,
	// 도구 면을 줄여 가치 낮은 호출을 줄입니다. 나머지 SDK 기본 도구(Read/Write/Edit/LS/Bash/Sleep)는 그대로입니다.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 의도는 워커의 【유일한 책임이자 run 전체를 관통하는 불변량】입니다. 시작 지시, 의도가 앵커한 대상 자산의
	// 원본 데이터와 함께 system prompt 에 넣습니다. system 은 run 마다 다시 조립되고 compaction 에 눌리지 않아,
	// 긴 run 에서도 의도가 항상 있고, 이어서 실행할 때 transcript 가 첫 메시지를 남겼는지에 기대지 않습니다. 대가는 system 이
	// 의도마다 바뀌는 데이터를 섞어 의도 사이 캐시를 못 쓴다는 점입니다. 일부러 그렇게 골랐습니다(의도를 잃는 편이 토큰을 아끼는 것보다 훨씬 심각합니다).
	// 플래너는 「상황 블록을 user turn 에 둔다」와 일부러 갈립니다. 플래너는 의도를 만드는 쪽이라 단일한 mandate 가 없고,
	// 워커는 있습니다. 【전체 상황 overview】만 시작 user 메시지에 둡니다. 그것은 낮춰도 되고 오래되어도 되며, 눌려도 괜찮습니다.
	// 이번 의도 전용 작업 디렉터리 <workDir>/tasks/<taskID>/i<intentID>. 엔진이 먼저 만들어 둡니다.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// 실행 전체의 의도는 지금 도구 동작이 아닙니다. 동작 검토자에게 넘기거나
	// 부모 실행의 배경을 물려주지 않습니다.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 조작 제약(있으면)을 시스템 프롬프트에 넣습니다. 워커는 실행할 때 그것을 지킵니다
	}
	// 의도 블록 → 의도가 앵커한 자산 블록 → 시작 지시 순으로 system 끝에 붙입니다(constraintBlock 과 같은 덧붙이기).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\n本意图 asset_ids 对应的目标资产：\n" + string(b) // han-allow 업스트림 프롬프트·픽스처
				}
				// 의도가 분명히 겨냥한 이 자산들은 작업 테스트 범위에 자동으로 넣습니다(insertAssets 와 같은
				// 보수적 단위). upsertTaskScope 의 ON CONFLICT DO NOTHING + uq_task_scope
				// 유니크 인덱스가 중복 추가를 막습니다. 다시 실행하거나 재시도해도 멱등 no-op 입니다.
				// 자산 커버리지를 끄면 테스트 범위(분모)를 더 이상 쌓지 않습니다.
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n开始执行上面这条意图：只做它、只产生事实、assets、finding、做完即停。" // han-allow 업스트림 프롬프트·픽스처
	system, boundary := deferredSystem(sysBody, def)
	// 작업 deadline(ctx 로 주입)이 이번 run 의 벽시계 예산을 누르고 마무리 문구를 정합니다(taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch 는 기록 프록시를 탑니다. HTTP 는 curl 과 같이 기록됩니다. 프록시 CA 를 읽어 MITM 으로
		// 다시 서명된 HTTPS 인증서가 【정상적으로 검증되게】 합니다(검증을 끄는 것이 아님). proxy 가 비면 직접 연결합니다.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// 인터넷 검색(선택). ddgs 는 키가 필요 없습니다. brave-free 는 BraveKey, tavily 는 TavilyKey 가 필요합니다.
		// WebSearchProxy 는 별도의 출구 프록시(http/https/socks5)로, 트래픽을 기록하는 MITM 프록시와 별개입니다. 비우면 직접 연결합니다.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 자식 프로세스의 HTTP 는 기본적으로 기록 프록시를 타고 그 CA 를 신뢰합니다(도구에 -x/-k 가 필요 없음).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = 무제한(에이전트 관리에서 고칠 수 있음)
		// 벽시계 예산. 턴 경계에서 판단하고 한중간은 끊지 않습니다. 0 = 제한 없음. 작업 deadline 이 있으면 min(자체 예산,
		// deadline 까지 남은 시간)으로 누릅니다. 이번 run 이 작업 시각에 자연스럽게 마무리를 타게 합니다(taskclock.go).
		MaxDuration: maxDur,
		// 예산(턴 수 또는 시간)에 닿으면 SDK 가 마무리 한 번을 실행합니다(Bash 는 숨김). 알아낸 것을 기록해 끝이 흐려지지 않게 합니다.
		// clamped(작업 deadline 에 눌림)이면 PromptByReason 을 씁니다. 시간 초과=작업 시각 도달→작업 시간 초과 문구,
		// 걸음 수=눌린 창에서 걸음이 먼저 소진→per-run 문구로 돌아감. clamped 가 아니면 순수 per-run 을 유지합니다.
		Settlement: settle,
		// 큰 도구 출력은 머리와 포인터와 함께 cmd-output/ 으로 넘깁니다(SDK tool.Capture).
		// 전문은 디스크에 남습니다. 자르는 상한은 SDK 기본(30000 문자)입니다.
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // 도구가 많은 긴 실행도 창 안에 둡니다
		Todos:         actool.NewTodoStore(),                  // 세션 단위 임시 할 일(TodoWrite). 계획용이며 끝나면 버립니다
		NonStreaming:  w.nonStreaming(),                       // 이 profile 이 비스트리밍이면 Provider.Complete 를 탑니다
		MaxTokens:     w.maxTokens(),                          // 0 = 상한을 보내지 않음. 서버 기본값
	}
	if hooks != nil { // 타입드 nil 검사: 실제 값이 있을 때만 넣습니다(하네스 패닉을 피함)
		opts.Hooks = hooks
	}
	if w.tx != nil { // LLM 원문 대화를 남깁니다. 집어 든 의도마다 파일이 하나입니다
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 의도 / 시작 지시 / 의도가 앵커한 자산은 이미 system prompt 로 내려갔습니다(위의 sysBody 조립).
	// 이 시작 user 메시지는 【전체 상황 overview】만 담습니다. 낮춰도 되는 큰 그림이라 눌려도 괜찮습니다.
	// overview 가 드물게 marshal 에 실패해 비면, 시작 문구 한 줄로 돌아갑니다. 첫 턴에 빈 user 메시지가 없게 합니다.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "开始执行 system 里领到的意图：只做它、只产生事实、assets、finding、做完即停。" // han-allow 업스트림 프롬프트·픽스처
	}

	// 실험 기능: 켜면 noa 가 맥락 압축을 맡습니다(아카이브는 <workDir>/noa/<SessionID> 아래, 유지됨).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // 세션의 백그라운드 작업 관리자(임시 디렉터리와 프로세스)를 놓습니다

	// 이 의도가 일시정지/막힘/소진 뒤 다시 돌면 이전 대화를 이어 갑니다.
	// transcript ID 는 의도마다 정해져 있습니다. 이전 세션이 있으면 워커는
	// 처음부터 다시 하지 않고 멈춘 곳에서 이어 갑니다.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "继续执行。" // han-allow 업스트림 프롬프트·픽스처
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "继续执行上一次人工对话输入的新意图。不要重复已经完成的动作。" // han-allow 업스트림 프롬프트·픽스처
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【人工对话输入的新意图】\n" + message + // han-allow 업스트림 프롬프트·픽스처
				"\n\n请立即按这条人工输入执行，完成后再根据上下文决定原任务是否需要继续。" // han-allow 업스트림 프롬프트·픽스처
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【人工对话输入的新意图】\n" + message + // han-allow 업스트림 프롬프트·픽스처
				"\n\n请优先执行这条人工输入。" // han-allow 업스트림 프롬프트·픽스처
		}
	}

	// 예산과 마무리는 SDK 가 소유합니다(MaxTurns/MaxDuration + Settlement).
	// 닿으면 마무리 턴을 돌리고 ReasonMaxTurns/ReasonTimeout 으로 끝냅니다.
	// MaxDuration 은 이제 벽시계 마감에 돌고 있던 도구를 끊고, 살아 있는 ctx 위에서
	// 마무리로 들어갑니다. 도구가 예산을 넘겨도 마무리가 됩니다(바깥 하드 타임아웃이 필요 없음).
	// ctx 자체는 일시정지 / 플래너의 종료 / 프로세스 종료만 실어 나릅니다.
	// 엔진이 그것을 구분해 다시 넣거나 멈춥니다.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
