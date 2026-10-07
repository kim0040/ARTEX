package server

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

type goalSpec struct {
	Text      string
	VulnClass string
}

// launchTask는 어느 경로로 만든 작업이든, 만든 직후 공통 절차를 돌립니다.
// path (HTTP createTask 또는 orchestration spawn_task). 두 곳을 복사해 붙여 넣지 않으려고 한곳으로 모은다. 두 입구 모두 작업을 자산 그래프에 심고 탐색 그래프의 엔진으로 넘긴다:
//  1. seed로 루트 자산을 심어, 이벤트 구동 loop에 넘긴다;
//  2. 선택적 시드 의도. 워커(의도 하나를 실행한 뒤 정지)는 첫 라운드 플래너(의도만 생성)를 기다리지 않고 바로 실행한다;
//  3. 백그라운드에서 비동기로 목표를 분해한다(「0라운드 목표 분해」round + LLM 분해 단계 + goal 한 건씩, 페이지에 보임),
//     분해가 끝난 뒤에 engine.Run 한다. 엔진은 goal 노드가 준비된 뒤에야 시작해서, 플래너(의도만 생성)가 goal보다 먼저 도는 경합을 피한다.
//
// 비동기(goroutine)라서 호출자는 바로 반환하고, 두 경로의 동작은 같다. 작업은 즉시 만들고, 목표 분해는 백그라운드에서 한다.
func (s *Server) launchTask(t *Task, seedText string, seedFirstIntent bool) {
	if !s.engine.beginTaskOperation(t.ID) {
		return
	}
	s.seed(t, seedText)
	if seedFirstIntent {
		s.seedFirstIntent(t)
	}
	s.engine.decInflight(t.ID)
	if _, err := s.admitTask(t, "bootstrap"); err != nil {
		log.Printf("[concurrency] task %s 시작 실패: %v", t.ID, err)
	}
}

func (s *Server) startTaskEngine(t *Task) {
	ctx := s.engine.execContextFor(s.ctx, t.ID)
	if ctx.Err() != nil || s.engine.IsDeleting(t.ID) {
		return
	}
	s.engine.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
		Summary: "0라운드 목표 분해"})
	goals := s.createGoals(ctx, t, func(r db.Activity) {
		s.engine.emitActivity(t, r)
	})
	if ctx.Err() != nil || s.engine.IsDeleting(t.ID) {
		return
	}
	for _, g := range goals {
		summary := g.Text
		if g.VulnClass != "" {
			summary = fmt.Sprintf("[%s] %s", g.VulnClass, g.Text)
		}
		s.engine.emitActivity(t, db.Activity{Worker: "planner", Kind: "text", Summary: summary})
	}
	if ctx.Err() != nil || s.engine.IsDeleting(t.ID) {
		return
	}
	s.engine.Run(s.ctx, t)
}

func (s *Server) occupiesConcurrencySlot(t *Task) bool {
	if t == nil {
		return false
	}
	lifecycle := t.lifecycleSnapshot()
	if lifecycle.Queued || lifecycle.Paused || isTerminalStatus(lifecycle.Status) {
		return false
	}
	// 삭제가 엔진을 잠깐 일시정지했지만, 아직 확정 전입니다.
	// PostgreSQL 삭제가 성공할 때까지 그 작업의 자리를 유지합니다. 그렇지 않으면
	// 빼는 동안 FIFO 승격이 너무 많이 입장할 수 있습니다. 삭제가 나중에 취소되고
	// 저장돼 있던 실행 중 작업이 되살아날 때요.
	if s.engine.IsDeleting(t.ID) {
		return true
	}
	return !s.engine.IsPaused(t.ID) && (s.engine.ReadyFor(t) || s.engine.ActiveLLMCalls(t.ID) > 0)
}

func (s *Server) runningTaskCount(excludeID string) int {
	count := 0
	for _, task := range s.m.List() {
		if task.ID != excludeID && s.occupiesConcurrencySlot(task) {
			count++
		}
	}
	return count
}

// admitTask는 새 작업, 재개, 다시 실행, 이어하기의 유일한 입장 경로입니다.
// 원자적으로 작업을 시작하거나, 영구 FIFO 대기열 끝에 붙입니다.
// mode는 방금 만든 작업이면 bootstrap, 아니면 resume입니다.
// 초보용: 작업이 엔진에 들어가거나 대기열에 서는 입구입니다.
func (s *Server) admitTask(t *Task, mode string) (queued bool, err error) {
	return s.admitTaskWhen(t, mode, false)
}

// admitPausedTask는 작업 제어의 재개 경로입니다. 일시정지였는지는
// 입장과 같은 스케줄러 잠금 아래에서 확인합니다. 그래서 동시에 일시정지,
// 대기열 빼기, FIFO 승격이 일어나도 DB와 엔진 상태가 어긋나지 않습니다.
func (s *Server) admitPausedTask(t *Task) (queued bool, err error) {
	return s.admitTaskWhen(t, "resume", true)
}

func (s *Server) admitTaskWhen(t *Task, mode string, requirePaused bool) (queued bool, err error) {
	if t == nil {
		return false, fmt.Errorf("작업을 찾을 수 없습니다")
	}
	if mode != "bootstrap" {
		mode = "resume"
	}
	s.concMu.Lock()
	defer s.concMu.Unlock()
	// 삭제도 concMu 아래에서 장벽을 세웁니다. 잠금을 얻은 뒤에 다시 찾습니다.
	// 삭제가 성공하기 전에 작업 포인터를 잡은 요청이,
	// StopTask가 엔진 맵을 지운 뒤 그 낡은 손잡이를 되살리지 못하게 합니다.
	current, exists := s.m.Task(t.ID)
	if !exists || current != t || s.engine.IsDeleting(t.ID) {
		return false, fmt.Errorf("작업을 삭제하는 중입니다")
	}
	if !s.engine.beginTaskOperation(t.ID) {
		return false, fmt.Errorf("작업을 삭제하는 중입니다")
	}
	defer s.engine.decInflight(t.ID)
	lifecycle := t.lifecycleSnapshot()
	if requirePaused {
		if isTerminalStatus(lifecycle.Status) {
			return false, fmt.Errorf("종료 상태 작업은 계속을 실행할 수 없습니다")
		}
		if !lifecycle.Paused {
			return false, fmt.Errorf("일시 중지된 작업만 계속할 수 있습니다")
		}
		mode = s.resumeAdmissionMode(t)
	}
	wasTerminal := isTerminalStatus(lifecycle.Status)
	wasPaused := lifecycle.Paused || s.engine.IsPaused(t.ID)
	wasQueued := lifecycle.Queued
	engineWasPaused := s.engine.IsPaused(t.ID)
	enabled, limit := s.m.ConcurrencyLimit()
	ready := s.engine.ReadyFor(t)

	// 이미 입장한 작업에 일을 더하면 깨우기만 하면 됩니다.
	// 설정된 상한을 지금 도는 수보다 낮춰도 그렇습니다.
	// 플래너/워커가 아직 살아있는 작업이 갑자기 자신을 대기라고 표시하면 안 됩니다.
	// 그래도 저장된 상태는 비교 후 확정합니다. 동시에 종료로 바뀌면
	// 그 종료가 이겨야 하고, 이어하기 입장이 성공한 것처럼 조용히
	// 보고되면 안 됩니다.
	if !wasTerminal && !wasPaused && !wasQueued && s.engine.Started(t.ID) && (!enabled || ready) {
		if err := s.m.ApplyTaskAdmission(t.ID, lifecycle.Status, lifecycle.Status, false, "resume", false); err != nil {
			return false, err
		}
		s.startAdmittedTask(t, "resume")
		return false, nil
	}

	// 여러 번 입장해도 처음 실행 모드는 유지합니다. 예전
	// 대기 행은 queue_mode가 비어 있어서, 그래프로 bootstrap인지 추정합니다.
	if wasQueued {
		switch lifecycle.QueueMode {
		case "bootstrap":
			mode = "bootstrap"
		case "":
			mode = s.resumeAdmissionMode(t)
		}
	}

	readyBacklog := enabled && s.hasReadyQueuedTask(t.ID)
	atCapacity := enabled && s.runningTaskCount(t.ID) >= limit
	shouldQueue := enabled && (!ready || readyBacklog || atCapacity)

	// 종료됐거나 일시정지된 작업을 되살리기 전에 실행 장벽을 세웁니다. 이 순서가 아니면
	// 이미 돌던 워커 루프가, status가 running이 된 뒤 queued가 true가 되기 전 틈에
	// 새로 열린 의도를 가져갈 수 있습니다.
	if shouldQueue || wasTerminal || wasPaused || wasQueued {
		s.engine.Pause(t.ID, agent.Causef("queued_for_admission", "작업이 실행 승인을 기다립니다",
			"작업이 동시성 큐 또는 승인 상태 제출을 기다리는 중이라 이번 실행은 중지되었습니다. 실행 슬롯을 얻은 뒤에만 의도를 다시 가져옵니다"))
	}

	status := lifecycle.Status
	if wasTerminal {
		status = "running"
	}
	if err := s.m.ApplyTaskAdmission(t.ID, lifecycle.Status, status, shouldQueue, mode, wasQueued); err != nil {
		if !engineWasPaused && !wasPaused && !wasQueued {
			s.engine.Resume(t)
		}
		return false, err
	}
	if lifecycle.Status == "timeout" && status == "running" {
		// ApplyTaskAdmission이 이번 시간 초과 실행의 저장된 시계를 지웠습니다.
		// FIFO에 세우거나 일을 시작하기 전에, 그에 맞는 엔진 문을 지웁니다.
		// 안 그러면 옛 settling 플래그 때문에 워커가 영원히 건너뜁니다.
		s.engine.resetTimeoutRevival(t.ID)
	}
	if shouldQueue {
		if !wasQueued {
			summary := fmt.Sprintf("대기열에 넣음: 동시성 상한 %d에 도달하여 빈 자리가 생기면 자동으로 시작합니다", limit)
			switch {
			case !ready:
				summary = "대기열에 넣음: 지금 실행 가능한 LLM 구성이 없어 구성이 복구되면 자동으로 시작합니다"
			case readyBacklog:
				summary = "대기열에 넣음: 더 일찍 실행을 기다리는 작업이 있어 FIFO 순서로 자동 시작합니다"
			}
			s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: summary})
		}
		return true, nil
	}
	s.startAdmittedTask(t, mode)
	return false, nil
}

func (s *Server) hasReadyQueuedTask(excludeID string) bool {
	for _, task := range s.m.List() {
		lifecycle := task.lifecycleSnapshot()
		if task.ID != excludeID && lifecycle.Queued && !lifecycle.Paused && !isTerminalStatus(lifecycle.Status) && s.engine.ReadyFor(task) {
			return true
		}
	}
	return false
}

func (s *Server) startAdmittedTask(t *Task, mode string) {
	if mode == "bootstrap" {
		if !s.engine.beginTaskOperation(t.ID) {
			return
		}
		// 처음 실행 작업은 동시성 대기열에서 기다리는 동안 엔진 일시정지 장벽을 유지했을 수 있습니다.
		// 작업 입장이 끝난 뒤에만 그 장벽을 풉니다. 그렇지 않으면
		// startTaskEngine의 첫 execContextFor가 취소된
		// 컨텍스트를 돌려주고, 대기열에서 뺀 작업이 조용히 멈춥니다.
		if s.engine.IsPaused(t.ID) {
			s.engine.Resume(t)
		}
		go func() {
			defer s.engine.decInflight(t.ID)
			s.startTaskEngine(t)
		}()
		return
	}
	s.engine.Run(s.ctx, t)
	s.engine.Resume(t)
	// 이미 시작한 작업이면 Run은 바로 돌아옵니다. 재개 뒤에는 시간 초과
	// 조정자가 있는지 명시적으로 확인합니다. resetTimeoutRevival이 끝난
	// 조정자의 표시를 지웠고, 다음 진짜 호출이 마감을 새로 찍습니다.
	s.engine.startDeadlineCoordinator(s.ctx, t)
}

func (s *Server) reconcileConcurrency() {
	s.concMu.Lock()
	defer s.concMu.Unlock()
	enabled, limit := s.m.ConcurrencyLimit()

	// 프로바이더 체인을 쓸 수 없게 된 작업은 쓸모 있는 일을 못 하고,
	// 한정된 실행 자리를 영원히 잡으면 안 됩니다. 영구히 세워 두고,
	// 나중에 설정을 고치거나 복구되면 같은 FIFO 경로로 다시 들어오게 합니다.
	if enabled {
		for _, task := range s.m.List() {
			lifecycle := task.lifecycleSnapshot()
			if lifecycle.Queued || lifecycle.Paused || s.engine.IsPaused(task.ID) || s.engine.IsDeleting(task.ID) ||
				isTerminalStatus(lifecycle.Status) || s.engine.ReadyFor(task) || s.engine.ActiveLLMCalls(task.ID) > 0 {
				continue
			}
			mode := s.resumeAdmissionMode(task)
			s.engine.Pause(task.ID, agent.Causef("llm_unavailable_queued", "LLM을 사용할 수 없어 작업이 대기 큐에 들어갑니다",
				"작업이 지금 실행 가능한 플래너/워커 LLM을 해석할 수 없어 동시성 슬롯을 해제했습니다. 구성이 복구되면 큐 순서대로 계속합니다"))
			if err := s.m.EnqueueTask(task.ID, mode); err != nil {
				s.engine.Resume(task)
				log.Printf("[concurrency] task %s LLM을 사용할 수 없어 대기열 추가 실패: %v", task.ID, err)
				continue
			}
			s.engine.emitActivity(task, db.Activity{Worker: "system", Kind: "text",
				Summary: "대기열에 넣음: 지금 실행 가능한 LLM 구성이 없어 구성이 복구되면 자동으로 시작합니다"})
		}
	}

	type queuedTask struct {
		task      *Task
		lifecycle taskLifecycleState
	}
	queued := []queuedTask{}
	for _, task := range s.m.List() {
		lifecycle := task.lifecycleSnapshot()
		if lifecycle.Queued && !isTerminalStatus(lifecycle.Status) {
			queued = append(queued, queuedTask{task: task, lifecycle: lifecycle})
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		left, right := queued[i].lifecycle.QueuedAt, queued[j].lifecycle.QueuedAt
		if left == 0 {
			left = queued[i].task.CreatedAt * int64(1e9)
		}
		if right == 0 {
			right = queued[j].task.CreatedAt * int64(1e9)
		}
		if left != right {
			return left < right
		}
		// 옛 행에는 queued_at이 없을 수 있고, 작업 생성 시각은
		// 메모리에서 초 단위로만 남습니다. 작업 id는 단조 증가하므로,
		// 같은 시각이면 더 작은 id가 먼저인 결정적 순서로 씁니다.
		leftID, leftErr := strconv.ParseInt(queued[i].task.ID, 10, 64)
		rightID, rightErr := strconv.ParseInt(queued[j].task.ID, 10, 64)
		if leftErr == nil && rightErr == nil {
			return leftID < rightID
		}
		return queued[i].task.ID < queued[j].task.ID
	})
	slots := len(queued)
	if enabled {
		slots = limit - s.runningTaskCount("")
	}
	for _, entry := range queued {
		if slots <= 0 {
			break
		}
		task := entry.task
		lifecycle := task.lifecycleSnapshot()
		// FIFO 순서는 지키되, 명시한 체인이나 전역 프로바이더를 쓸 수 없는 작업에는
		// 동시성 자리를 주지 않습니다. 사용자가 LLM 체인을
		// 설정하거나 초기화한 뒤에 다시 시도합니다.
		if lifecycle.Paused || (enabled && !s.engine.ReadyFor(task)) {
			continue
		}
		mode := lifecycle.QueueMode
		if mode != "bootstrap" && mode != "resume" {
			mode = s.resumeAdmissionMode(task)
		}
		if mode != "bootstrap" {
			mode = "resume"
		}
		if !s.engine.beginTaskOperation(task.ID) {
			continue
		}
		if err := s.m.ApplyTaskAdmission(task.ID, lifecycle.Status, lifecycle.Status, false, mode, false); err != nil {
			s.engine.decInflight(task.ID)
			continue
		}
		s.startAdmittedTask(task, mode)
		s.engine.decInflight(task.ID)
		slots--
	}
}

// reviveTask 는 이미 멈춘 작업을 다시 돌린다: 종료 상태(done/failed/timeout)를 running으로 되돌리고,
// 일시 정지를 해제하고 엔진 루프를 (재)시작하며 깨운다. 이미 running이고 일시 정지되지 않은 작업은 Run 안의 한 번뿐인
// Notify만 남고, 부작용은 거의 없다. 「메인 에이전트 set_goals로 목표 추가」와 「blocked 의도 재실행」 두 곳에서 쓴다.
//
// 왜 반드시 명시적으로 되살려야 하는가: 플래너/워커 루프의 종료 상태 문(engine.go)이 일반 notify를 삼킨다—그저
// 그래프를 고치고 Notify하는 것만으로는 이미 완료로 판정된 작업을 깨울 수 없다. 재시작 후 종료 상태 작업의 goroutine도 이미 없을 수 있어 Run도 필요하다. 이 함수는 엔진이 자산 그래프와 탐색 그래프를 다시 따라가게 작업을 살린다.
func (s *Server) reviveTask(t *Task) {
	if t == nil {
		return
	}
	if _, err := s.admitTask(t, "resume"); err != nil {
		log.Printf("[revive] task %s 복구 실패: %v", t.ID, err)
	}
}

// createGoals는 작업 루트 아래에 목표 노드를 만듭니다(관계 objective).
// 분해는 전부 LLM이 합니다(이 프로젝트는 LLM이 필요합니다).
// 규칙으로 자르는 대체 경로는 없습니다. 그건 쓰레기만 만들었습니다(URL을 찢고,
// 의미 없는 2분할). LLM이 아무것도 안 주면(오류), 작업 목표 원문을
// 목표 하나로 그대로 써서, 판단할 기준은 남깁니다.
// 심은 명세를 돌려주어, 호출자가 활동 기록을 낼 수 있게 합니다.
// emit이 nil이 아니면 DecomposeGoals로 넘깁니다. LLM 단계가 화면에 보이게 하려고요.
// 초보용: 작업 목표를 탐색 그래프의 목표 노드로 만들고, 분해 과정은 화면에 보입니다.
func (s *Server) createGoals(ctx context.Context, t *Task, emit func(db.Activity)) []goalSpec {
	if t == nil {
		return nil
	}
	// 작업이 도는 것과 같은 LLM을 씁니다(고정한 설정, 없으면 활성 설정).
	// agent.FromEnv()가 아닙니다. LLM은 화면의 DB 설정으로 고르고, 환경 변수가 아니라서
	// FromEnv는 빈 값을 줬고, 모든 작업이 조용히 거친 규칙
	// 분할로 떨어졌습니다(URL을 찢거나 의미 없는 2분할).
	var specs []goalSpec
	var as *db.AssetStore
	if s.m != nil {
		as = s.m.Assets()
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	s.engine.BeginLLMCall(t.ID)
	goalRuntime := s.agentsForTask(t).runtime
	decomposed := agent.DecomposeGoalsWithProvider(ctx, goalRuntime, s.m.dir, t.Goal, t.Description, as, t.Store, taskID, goalRuntime.nonStreaming(), goalRuntime.maxTokens(), emit)
	s.engine.EndLLMCall(t.ID)
	for _, g := range decomposed {
		if strings.TrimSpace(g.Text) != "" {
			specs = append(specs, goalSpec{Text: g.Text, VulnClass: g.VulnClass})
		}
	}
	if len(specs) == 0 {
		// 분해된 목표가 없으면(LLM 오류 / 프로바이더 없음) 작업 목표 원문을
		// 목표 하나로 씁니다. 판단할 기준은 남기려고요. 여기 쓰는 경로는
		// 이것뿐입니다. 분해된 목표는 도구가 이미 저장합니다.
		if g := strings.TrimSpace(t.Goal); g != "" {
			log.Printf("[goals] task %s: LLM 목표 분해 산출이 없어 「원래 목표를 단일 목표로」 되돌립니다", t.ID)
			origin, _ := t.Store.OriginFactID()
			id, _ := t.Store.AddNode(db.KindGoal, map[string]any{"text": g}, 0, "open", "system", nil)
			if origin > 0 && id > 0 {
				_ = t.Store.Link(origin, db.RelSpawns, id) // 목표는 작업 루트(출발 사실) 아래에 매달립니다.
			}
			specs = []goalSpec{{Text: g}}
		}
	}
	return specs
}
