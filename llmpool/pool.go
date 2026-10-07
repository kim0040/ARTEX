package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Autumn-27/norma/llm"
)

// Member 는 사슬 안의 LLM 설정 하나입니다. 호출자가 이미 provider 로 만들어
// 기록기까지 감쌌습니다. 그래서 실패한 시도도 그 설정 이름으로 기록에 남습니다.
type Member struct {
	ID       int64  // llm_profiles.id. 설정 행
	Name     string // 설정 이름. 로그와 화면용
	Model    string
	Format   string // "anthropic" | "openai". 호출 형식
	Priority int    // 설정에 적힌 우선순위. 화면 표시용
	Active   bool   // is_default. 화면 표시용
	// Rank 는 호출자가 매긴 순서 키입니다. 클수록 앞이고, 같은 Rank 는 돌아가며
	// 앞장을 섭니다(같은 키를 여러 설정에 나눠 부하를 흩뿌립니다). 호출자는
	// "활성 설정이 사슬의 머리"를, 사용자가 넣을 수 있는 어떤 우선순위보다 높은
	// Rank 로 표현합니다. 그래서 이 타입 자체에는 정책이 없습니다.
	Rank int
	// WindowTokens 는 설정의 컨텍스트 창(토큰)입니다. 이번 요청이 창에 안 들어가면
	// 실패를 시켜 보지 않고 건너뜁니다.
	WindowTokens int
	Prov         llm.Provider
}

// RankActive 는 호출자가 사슬 머리(활성 설정, 또는 명시적으로 묶인 설정)에
// 주는 Rank 입니다. 설정된 어떤 우선순위보다 항상 앞섭니다.
const RankActive = int(^uint(0)>>1) - 1

// ErrExhausted 는 사슬의 모든 설정이 실패했을 때 돌아옵니다.
var ErrExhausted = errors.New("LLM 순회: 모든 설정을 쓸 수 없습니다")

// Pool 은 정렬된 설정 사슬로 장애 전환하는 llm.Provider 입니다.
// 동시에 써도 안전합니다. 멤버는 만든 뒤 바뀌지 않고, 바뀌는 상태는
// 공유 Registry 에만 있습니다.
type Pool struct {
	members []*Member // 사슬 순서(활성 설정이 먼저, 그다음 우선순위 내림차순)
	health  *Registry
	rr      atomic.Uint64 // 같은 우선순위 묶음 안에서 시작점을 돌립니다
}

// New 는 이미 사슬 순서로 정렬된 멤버로 Pool 을 만듭니다. 사슬이 비면 nil 입니다.
// 멤버가 하나여도 올바른 Pool 입니다. 그냥 맨 provider 와 똑같이 동작합니다.
func New(members []*Member, health *Registry) *Pool {
	if len(members) == 0 {
		return nil
	}
	if health == nil {
		health = NewRegistry(nil, nil)
	}
	return &Pool{members: members, health: health}
}

// Members 는 사슬을 순서대로 돌려줍니다. 읽기 전용으로 다루세요.
func (p *Pool) Members() []*Member { return p.members }

// Head 는 사슬의 첫 멤버를 돌려줍니다.
func (p *Pool) Head() *Member { return p.members[0] }

// Stream 은 장애 전환이 있는 llm.Provider.Stream 입니다.
//
// 딱 하나의 규칙: 멤버는 이벤트를 하나도 내기 전에만 버릴 수 있습니다.
// 글이나 tool_use 가 호출자에게 도달한 뒤 같은 요청을 다른 모델에 다시 보내면
// 출력이 두 번 쌓이고 대화 기록이 망가집니다. 그래서 스트림 중간의 실패는
// 그대로 올리고, 에이전트 하네스의 이어하기에 맡깁니다. 이 장치가 겨냥하는
// 실패(402 잔액 없음, 401 나쁜 키, 429, 5xx)는 본문을 읽기 전, 요청을
// 맺는 동안에 나오므로 항상 안전한 구간에 떨어집니다.
func (p *Pool) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	order := p.order(req)
	return func(yield func(llm.StreamEvent, error) bool) {
		var lastErr error
		for i, m := range order {
			emitted := false
			var failed error
			for ev, err := range m.Prov.Stream(ctx, req) {
				if err != nil && !emitted && shouldFailover(ctx, err) {
					failed = err
					break // 안전 구간: 아직 호출자에게 아무것도 안 갔음
				}
				emitted = true
				if !yield(ev, err) {
					return // 호출자가 소비를 멈춤(취소 / 조기 종료)
				}
				if err != nil {
					return // 끝 오류는 이미 호출자에게 넘김
				}
			}
			if failed == nil {
				p.health.Pass(m.ID) // 끝났거나, 장애 전환 대상이 아닌 실패
				return
			}
			lastErr = failed
			hard := isHardFailure(failed)
			if p.health.Trip(m.ID, trimErr(failed), hard) {
				log.Printf("[llmpool] 설정 %q(%s) 회로가 열렸습니다: %s", m.Name, m.Model, trimErr(failed))
			}
			if i+1 < len(order) {
				n := order[i+1]
				log.Printf("[llmpool] LLM 장애 전환: %q(%s) → %q(%s), 이유: %s",
					m.Name, m.Model, n.Name, n.Model, trimErr(failed))
			}
		}
		if lastErr == nil {
			lastErr = ErrExhausted
		}
		log.Printf("[llmpool] 순회 사슬이 바닥났습니다(%d개 설정이 모두 실패). 마지막 오류: %s", len(order), trimErr(lastErr))
		yield(llm.StreamEvent{}, fmt.Errorf("%w：%v", ErrExhausted, lastErr))
	}
}

// Complete 는 스트림이 아닌 호출의 장애 전환입니다. 스트림이 아닌 요청은
// 원자적입니다. 부분 출력을 넘기지 않으므로, 장애 전환 대상인 실패는 모두
// 안전 구간에 떨어지고 출력이 두 번 쌓일 위험 없이 다음 멤버를 시도합니다.
// 건강 상태 트립과 사슬 소진은 Stream 과 같습니다.
func (p *Pool) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	order := p.order(req)
	var lastErr error
	for i, m := range order {
		msg, sr, usage, err := m.Prov.Complete(ctx, req)
		if err == nil {
			p.health.Pass(m.ID)
			return msg, sr, usage, nil
		}
		if !shouldFailover(ctx, err) {
			// 장애 전환 대상이 아닌 오류(예: ctx 취소, 원인이 분명한 4xx)는
			// 건강 상태를 트립하지 않고 그대로 올립니다. Stream 의 같은 경로와 맞춥니다.
			p.health.Pass(m.ID)
			return llm.Message{}, "", llm.Usage{}, err
		}
		lastErr = err
		hard := isHardFailure(err)
		if p.health.Trip(m.ID, trimErr(err), hard) {
			log.Printf("[llmpool] 설정 %q(%s) 회로가 열렸습니다: %s", m.Name, m.Model, trimErr(err))
		}
		if i+1 < len(order) {
			n := order[i+1]
			log.Printf("[llmpool] LLM 장애 전환: %q(%s) → %q(%s), 이유: %s",
				m.Name, m.Model, n.Name, n.Model, trimErr(err))
		}
	}
	if lastErr == nil {
		lastErr = ErrExhausted
	}
	log.Printf("[llmpool] 순회 사슬이 바닥났습니다(%d개 설정이 모두 실패). 마지막 오류: %s", len(order), trimErr(lastErr))
	return llm.Message{}, "", llm.Usage{}, fmt.Errorf("%w：%v", ErrExhausted, lastErr)
}

// order 는 시도할 멤버를 고릅니다. 쉬는 시간 중인 것과, 컨텍스트 창이 이번
// 요청을 담지 못하는 것은 건너뜁니다. 그다음 같은 우선순위 묶음 안에서는
// 돌아가며 앞에 세워, 같은 우선순위 설정이 부하를 나눕니다. 빈 조각은
// 돌려주지 않습니다. 전부 걸러지면 사슬 머리를 그래도 시도합니다. 엔진을
// 멈추는 것보다 실패한 요청 한 번이 낫기 때문입니다.
func (p *Pool) order(req llm.CompletionRequest) []*Member {
	est := estimateTokens(req)
	var open []*Member
	for _, m := range p.members {
		if p.health.IsOpen(m.ID) {
			continue
		}
		if m.WindowTokens > 0 && est > m.WindowTokens {
			continue // 길이 때문에 400 이 날 대상 — 장애 전환으로 쓸모 없음
		}
		open = append(open, m)
	}
	if len(open) == 0 {
		return p.members[:1] // 마지막 수단: 멈추지 말고 머리를 한 번 두드림
	}
	return rotateGroups(open, p.rr.Add(1)-1)
}

// rotateGroups 는 같은 Rank 가 이어진 구간을 n 만큼 돌립니다. 우선순위를
// 공유하는 설정이 돌아가며 앞장을 섭니다(같은 키에 부하를 공짜로 나눕니다).
// 사슬 머리는 자기 Rank 가 있어 항상 맨 앞에 남습니다.
func rotateGroups(in []*Member, n uint64) []*Member {
	out := make([]*Member, 0, len(in))
	for i := 0; i < len(in); {
		j := i + 1
		for j < len(in) && in[j].Rank == in[i].Rank {
			j++
		}
		g := in[i:j]
		if len(g) > 1 {
			off := int(n % uint64(len(g)))
			for k := range g {
				out = append(out, g[(k+off)%len(g)])
			}
		} else {
			out = append(out, g...)
		}
		i = j
	}
	return out
}

// statusRe 는 SDK 오류 글에서 HTTP 상태를 뽑습니다. 형식은
// "<prefix>: status <code>: <body>" 입니다(norma/llm/retry.go). SDK 가
// 타입 있는 오류를 주지 않아서, 문자열이 유일한 단서입니다.
var statusRe = regexp.MustCompile(`status (\d{3})`)

// statusOf 는 err 가 가진 HTTP 상태를 돌려줍니다. 아니면 0 입니다.
func statusOf(err error) int {
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	code, _ := strconv.Atoi(m[1])
	return code
}

// shouldFailover 는 err 때문에 다음 설정을 시도해도 되는지 보고합니다.
//
// context 취소에는 넘어가지 않습니다. 그건 사용자가 작업을 멈추거나 작업
// 제한 시간이 된 것이고, 예비 키를 태우면 잔액도 낭비되고 종료 진단도
// 더러워집니다. 400 도 넘어가지 않습니다. 형식이 나쁘거나 너무 긴 요청은
// 어디서나 똑같이 실패합니다.
func shouldFailover(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch code := statusOf(err); {
	case code == 0:
		return true // 상태 없음 → 전송 계층 실패(reset / DNS / timeout)
	case code == 400:
		return false // 나쁘거나 너무 긴 요청: 어디서나 같음
	case code == 401, code == 402, code == 403, code == 404, code == 408, code == 429:
		return true
	case code >= 500:
		return true
	}
	return false
}

// isHardFailure 는 실패가 결정적인지 보고합니다. 사람이 고치기 전까지 같은
// 설정은 계속 실패합니다. 일시적 실패와 다릅니다. 결정적 실패는 한 번에
// 차단기를 엽니다.
func isHardFailure(err error) bool {
	switch statusOf(err) {
	case 401, 402, 403, 404:
		return true
	}
	return false
}

// trimErr 는 로그와 화면용으로 오류를 줄입니다. provider 본문은 길 수 있습니다.
func trimErr(err error) string {
	s := strings.TrimSpace(strings.ReplaceAll(err.Error(), "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// estimateTokens 는 요청 크기를 대충 잽니다. 컨텍스트 창이 명백히 못 담는
// 멤버를 건너뛰기 위해서입니다. 일부러 거칠고(~3.5자/토큰) 조금 크게 잡습니다.
// "들어간다"와 "한참 모자라다"만 가르면 됩니다.
func estimateTokens(req llm.CompletionRequest) int {
	n := 0
	for _, s := range req.System {
		n += len(s)
	}
	for _, m := range req.Messages {
		n += blocksLen(m.Content)
	}
	for _, t := range req.Tools {
		// InputSchema 는 이미 풀린 map 이라 직렬화 크기를 싸게 알 수 없습니다.
		// 최상위 속성 하나당 약 120자를 대신 셉니다.
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)*120
	}
	return n * 2 / 7 // ≈ len/3.5
}

func blocksLen(bs []llm.ContentBlock) int {
	n := 0
	for _, b := range bs {
		n += len(b.Text) + len(b.Thinking) + len(b.Input) + len(b.Name)
		if len(b.Content) > 0 {
			n += blocksLen(b.Content)
		}
	}
	return n
}
