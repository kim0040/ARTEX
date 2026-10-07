// Package llmrec 는 LLM 호출을 기록하는 장식자입니다.
//
// 초보: 플래너·워커·메인 에이전트가 모델에 보낸 요청과 받은 응답을 PostgreSQL 에
// 남겨, 화면의 LLM 기록에서 다시 볼 수 있게 합니다. 모델이 따르는 지시 본문은 바꾸지 않습니다.
package llmrec

import (
	"context"
	"encoding/json"
	"iter"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type taskIDContextKey struct{}
type workerContextKey struct{}

// WithTaskID는 LLM 호출에 소유 작업의 등록 번호를 붙입니다.
// 세션 번호는 탐색 그래프의 탐색 번호라서, 작업 등록 번호와 바꿔 쓸 수 없습니다.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ctx
	}
	return context.WithValue(ctx, taskIDContextKey{}, taskID)
}

// TaskIDFrom은 작업 런타임이 붙인 명시적 등록 번호를 돌려줍니다.
func TaskIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	taskID, _ := ctx.Value(taskIDContextKey{}).(string)
	return strings.TrimSpace(taskID)
}

// WithWorker는 LLM 호출의 에이전트 레인(worker) 라벨을 덮어씁니다. 레인은
// 보통 대화 세션 번호(exp<N>-<role>)에서 뽑습니다. 엔진의 워커·플래너
// 세션 밖에서 난 호출, 예를 들어 가로채기 폴백 판정은 그런 세션이 없어
// 여기서 레인을 직접 붙입니다. 사용량 장부가 그 지출(worker가 judge)을
// 설정 화면용으로 가를 수 있습니다.
func WithWorker(ctx context.Context, worker string) context.Context {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return ctx
	}
	return context.WithValue(ctx, workerContextKey{}, worker)
}

// workerFrom은 명시적 레인 덮어쓰기입니다. 없으면 빈 문자열입니다.
func workerFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	worker, _ := ctx.Value(workerContextKey{}).(string)
	return strings.TrimSpace(worker)
}

// Recorder는 llm.Provider를 감싸 완료 호출마다 기록합니다.
type Recorder struct {
	inner llm.Provider
	pg    *db.DB
	model string // 설정에서 온 모델 이름입니다. CompletionRequest에는 없습니다
	prof  string // llm_profiles의 LLM 프로필 이름입니다
	// thinkingType / reasoningEffort는 설정 수준의 생각 파라미터(생각 스위치 / 생각 강도)입니다.
	// norma의 buildBody()가 provider 설정에서 실제 HTTP body로 넣고, CompletionRequest에는
	// 없습니다. 그래서 Recorder가 따로 들고 있다가 기록에 직렬화합니다.
	// 초보용: LLM 호출 기록(llmrec)은 플래너·워커·메인 에이전트가 쓴 토큰을 화면에 보여 줍니다.
	thinkingType    string
	reasoningEffort string
	enabled         func() bool // 지금 기록이 켜져 있는지입니다. nil이면 항상 기록합니다
}

// Wrap은 enabled()가 참일 때 pg에 호출을 기록하는 Provider를 돌려줍니다.
// profName은 LLM 프로필 이름(예: default)이며 비어 있을 수 있습니다. thinkingType과
// reasoningEffort는 API로 실제로 보내는 설정 수준 생각 파라미터입니다
// (비면 보내지 않음). enabled가 nil이면 조건 없이 기록합니다.
func Wrap(inner llm.Provider, pg *db.DB, model, profName, thinkingType, reasoningEffort string, enabled func() bool) *Recorder {
	return &Recorder{
		inner: inner, pg: pg, model: model, prof: profName,
		thinkingType: thinkingType, reasoningEffort: reasoningEffort, enabled: enabled,
	}
}

// parseSession은 세션 문자열에서 작업 번호와 워커 역할을 뽑습니다.
// "exp1-worker-i3"이면 ("1", "worker"), "exp2-planner"이면 ("2", "planner")입니다.
func parseSession(s string) (taskID, worker string) {
	// 형식: exp<N>-<role>[-suffix] (번호-역할-접미사)
	if !strings.HasPrefix(s, "exp") {
		return "", ""
	}
	rest := s[3:] // "exp" 다음입니다
	// 작업 번호를 나눕니다
	i := strings.IndexByte(rest, '-')
	if i < 0 {
		return rest, ""
	}
	taskID = rest[:i]
	rest = rest[i+1:]
	// 워커 역할은 다음 '-'까지입니다(예: "worker-i3"에서 "worker")
	if j := strings.IndexByte(rest, '-'); j >= 0 {
		worker = rest[:j]
	} else {
		worker = rest
	}
	return taskID, worker
}

// Stream은 llm.Provider입니다. 안쪽 프로바이더에 맡기고, 흘러온 이벤트를
// 모아 응답을 다시 만든 뒤, 주고받은 전체를 기록합니다.
//
// 세션 식별자(예: "exp1-worker-i3")는 norma 하네스가 transcript.WithSessionID로
// ctx에 실어 옵니다. 호출마다 읽으므로 플래너와 여러 워커가 Recorder 하나를
// 나눠 써도 경합이 없습니다.
func (r *Recorder) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	// 본문 기록(무거운 디버그 추적, 요청·응답 전문)은 llm_record 설정으로 엽니다.
	// 가벼운 사용량 계량은 항상 돕니다. 토큰 통계의 근거이고, 끊기거나 실패한
	// 실행까지 빠지면 안 되므로 이 스위치에 넣지 않습니다. 꺼져 있을 때 건너뛰는
	// 것은 본문 직렬화와 응답 누적뿐입니다.
	recordBodies := r.enabled == nil || r.enabled()
	start := time.Now()
	session := transcript.SessionIDFrom(ctx)
	parsedID, worker := parseSession(session)
	if ov := workerFrom(ctx); ov != "" {
		worker = ov
	}
	expID := db.ParseExpID(parsedID)
	taskID := TaskIDFrom(ctx)
	if taskID == "" {
		// 작업이 아닌 호출자의 하위 호환입니다. 작업 호출은 런타임이
		// 등록 번호를 항상 명시합니다.
		taskID = parsedID
	}

	// 본문을 저장할 때만 요청을 직렬화합니다(비싼 부분입니다).
	// 함께 Capture를 붙여, HTTP 전송이 손을 대지 않은 전송 원문을 돌려주게 합니다.
	// 아래 정규화 보기에서는 다시 만들 수 없습니다. 도구 스키마는 빠지고,
	// tool_use 블록은 이 층에 오지 않으며, SSE 프레임은 이미 풀려 있습니다.
	// capture.go를 보세요.
	reqBody := ""
	var capt *Capture
	if recordBodies {
		reqBody = r.serializeRequest(req)
		ctx, capt = NewCapture(ctx)
	}

	return func(yield func(llm.StreamEvent, error) bool) {
		var (
			textBuf     strings.Builder
			thinkingBuf strings.Builder
			usage       llm.Usage
			stopReason  string
			streamErr   error
		)

		finished := false
		finish := func(err error) {
			if finished {
				return
			}
			finished = true
			status := "ok"
			if err != nil {
				status = "error"
			}
			// 가벼운 계량 행은 항상 씁니다.
			r.recordUsage(taskID, expID, worker, usage, int(time.Since(start).Milliseconds()), status)
			// 무거운 추적 행은 본문 기록이 켜져 있을 때만 씁니다.
			if recordBodies {
				r.record(req, session, taskID, worker, reqBody, capt, start, textBuf.String(), thinkingBuf.String(), usage, stopReason, err)
			}
		}
		defer func() { finish(ctx.Err()) }()

		for ev, err := range r.inner.Stream(ctx, req) {
			if err != nil {
				streamErr = err
				finish(streamErr)
				if !yield(ev, err) {
					return
				}
				return
			}
			// 사용량은 항상 셉니다(가볍습니다). 글과 생각은 본문 기록일 때만 모읍니다.
			// Anthropic을 비롯한 프로바이더는 토큰 사용량을 이벤트에 나눠 보냅니다.
			// message_start는 입력 쪽(입력과 캐시)만, message_delta는 출력만 담습니다.
			// Add로 접어야 합니다. delta에서 덮어쓰면 시작 때 센 입력이 0이 됩니다
			// (SDK의 llm.Accumulator와 같고, norma/llm/accumulate.go를 보세요).
			switch ev.Type {
			case llm.SETextDelta:
				if recordBodies {
					textBuf.WriteString(ev.Text)
				}
			case llm.SEThinkingDelta:
				if recordBodies {
					thinkingBuf.WriteString(ev.Text)
				}
			case llm.SEMessageStart:
				usage.Add(ev.Usage)
			case llm.SEMessageDelta:
				if ev.StopReason != "" {
					stopReason = ev.StopReason
				}
				usage.Add(ev.Usage)
			}
			if !yield(ev, err) {
				return
			}
		}

		// 스트림이 정상으로 끝났습니다.
		finish(streamErr)
	}
}

// Complete는 한 번에 끝나는 완료 경로입니다. Stream과 같이 사용량 계량,
// 정규화 본문 기록, 전송 원문 캡처를 유지합니다.
func (r *Recorder) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	recordBodies := r.enabled == nil || r.enabled()
	start := time.Now()
	session := transcript.SessionIDFrom(ctx)
	parsedID, worker := parseSession(session)
	if ov := workerFrom(ctx); ov != "" {
		worker = ov
	}
	expID := db.ParseExpID(parsedID)
	taskID := TaskIDFrom(ctx)
	if taskID == "" {
		taskID = parsedID
	}

	reqBody := ""
	var capt *Capture
	if recordBodies {
		reqBody = r.serializeRequest(req)
		ctx, capt = NewCapture(ctx)
	}

	msg, stopReason, usage, err := r.inner.Complete(ctx, req)
	status := "ok"
	if err != nil {
		status = "error"
	}
	r.recordUsage(taskID, expID, worker, usage, int(time.Since(start).Milliseconds()), status)
	if recordBodies {
		r.record(req, session, taskID, worker, reqBody, capt, start, msg.Text(), thinkingText(msg), usage, stopReason, err)
	}
	return msg, stopReason, usage, err
}

func thinkingText(msg llm.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == llm.BlockThinking && block.Thinking != "" {
			b.WriteString(block.Thinking)
		}
	}
	return b.String()
}

// recordUsage는 llm_usage에 가벼운 계량 행 하나를 붙입니다(본문 없음).
// 모델도 없고 토큰도 0인 호출은 잴 내용이 없어 건너뜁니다.
func (r *Recorder) recordUsage(taskID string, expID int64, worker string, usage llm.Usage, latencyMs int, status string) {
	if r.pg == nil {
		return
	}
	if r.model == "" && usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
		return
	}
	err := r.pg.InsertLLMUsage(&db.LLMUsage{
		TaskID:        taskID,
		ExplorationID: expID,
		Worker:        worker,
		Model:         r.model,
		ProfileName:   r.prof,
		LatencyMs:     latencyMs,
		InputTokens:   usage.InputTokens,
		OutputTokens:  usage.OutputTokens,
		CacheRead:     usage.CacheReadTokens,
		CacheWrite:    usage.CacheWriteTokens,
		Status:        status,
	})
	if err != nil {
		log.Printf("[llmusage] insert: %v", err)
	}
}

// record는 프로바이더 스트림이 돌아가기 전에 LLM 호출 하나를 PostgreSQL에 남깁니다.
// 쓰기를 소유 작업 연산 안에 두면, 작업을 지울 때 호출을 비운 뒤 기록을 지워도
// 늦은 비동기 insert가 행을 다시 만들지 않습니다.
func (r *Recorder) record(req llm.CompletionRequest, session, taskID, worker, reqBody string, capt *Capture, start time.Time, text, thinking string, usage llm.Usage, stopReason string, streamErr error) {
	latency := int(time.Since(start).Milliseconds())
	status := "ok"
	errMsg := ""
	if streamErr != nil {
		status = "error"
		errMsg = streamErr.Error()
	}

	// 응답 본문 JSON을 만듭니다.
	resp := map[string]any{
		"text":        text,
		"stop_reason": stopReason,
		"usage": map[string]int{
			"input_tokens":       usage.InputTokens,
			"output_tokens":      usage.OutputTokens,
			"cache_read_tokens":  usage.CacheReadTokens,
			"cache_write_tokens": usage.CacheWriteTokens,
		},
	}
	if thinking != "" {
		resp["thinking"] = thinking
	}
	respBody, _ := json.Marshal(resp)

	model := ""
	if len(req.System) > 0 {
		// 모델은 CompletionRequest에 없습니다. 설정된 모델 이름을 씁니다
	}
	model = r.model

	rec := &db.LLMRecord{
		SessionID:    session,
		TaskID:       taskID,
		Worker:       worker,
		Model:        model,
		ProfileName:  r.prof,
		LatencyMs:    latency,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CacheRead:    usage.CacheReadTokens,
		CacheWrite:   usage.CacheWriteTokens,
		Status:       status,
		Error:        errMsg,
		RequestBody:  reqBody,
		ResponseBody: string(respBody),
		// 전송 원문입니다. 전송이 Capture를 채우지 않으면 비어 있습니다
		// (캡처 갈고리 없는 클라이언트로 접속한 프로바이더, 또는
		// HTTP 요청이 나가기 전에 실패한 호출).
		RawRequest:  capt.RawRequest(),
		RawResponse: capt.RawResponse(),
	}

	if err := r.pg.InsertLLMRecord(rec); err != nil {
		log.Printf("[llmrec] insert: %v", err)
	}
}

// serializeRequest는 완료 요청의 JSON 표현을 만듭니다.
func (r *Recorder) serializeRequest(req llm.CompletionRequest) string {
	m := map[string]any{
		"system":     req.System,
		"messages":   req.Messages,
		"max_tokens": req.MaxTokens,
	}
	// 이번 호출이 실제로 보낸 생각 파라미터를 기록합니다. type은 「유효값」을 씁니다.
	// 요청마다 덮는 req.Thinking이 설정 수준 thinkingType보다 우선합니다(norma buildBody와
	// 같고, 예를 들어 compaction 요약은 disabled를 강제합니다). effort는 요청별 덮어쓰기가
	// 없어 설정값을 그대로 씁니다. 둘 다 비면 thinking 필드를 쓰지 않습니다.
	effType := r.thinkingType
	if req.Thinking != "" {
		effType = req.Thinking
	}
	if effType != "" || r.reasoningEffort != "" {
		m["thinking"] = map[string]string{"type": effType, "effort": r.reasoningEffort}
	}
	if len(req.Tools) > 0 {
		// 도구 이름만 저장합니다(스키마 전문은 큽니다).
		names := make([]string, len(req.Tools))
		for i, t := range req.Tools {
			names[i] = t.Name
		}
		m["tools"] = names
		m["tools_count"] = len(req.Tools)
	}
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		m["stop"] = req.Stop
	}
	b, _ := json.Marshal(m)
	return string(b)
}
