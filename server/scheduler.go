package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Scheduler는 P3 트리거를 돌립니다. 틱마다 시간이 된 간격 트리거를 울리고
// 새 발견과 새로 달성된 목표(어느 작업이든)를 살펴 이벤트 트리거를 울립니다.
// 울릴 때마다 그 에이전트의 트리거 대기열에 새 대화를 넣습니다
// (StartTriggeredRun). 같은 에이전트의 실행은 FIFO로 한 번에 하나,
// 다른 에이전트는 동시에 돕니다. 상태는 저장됩니다(트리거별 last_fire,
// 발견 워터마크, 이미 울린 목표 집합). 재시작해도 두 번 울리지 않고 이어 갑니다.
// 트리거는 사용자 정의 에이전트에만 붙습니다.
// 초보용: 발견이 생기거나 목표가 달성되면, 사용자 정의 에이전트 대화를 엔진이 하나씩 깨웁니다.
type Scheduler struct {
	s    *Server
	pg   *db.DB
	tick time.Duration
}

const (
	schedKeyLastFinding    = "last_finding_id"    // 워터마크: 이미 울린 발견 노드 id의 최댓값
	schedKeyFiredGoals     = "fired_goals"        // 이미 울린 목표 노드 id의 JSON 배열
	schedKeyLastTimeout    = "last_timeout_id"    // 워터마크: 작업 시간 초과로 이미 울린 작업 id의 최댓값
	schedKeyLastToolCall   = "last_toolcall_id"   // 워터마크: 도구 호출로 이미 울린 활동 id의 최댓값
	schedKeyLastTaskCreate = "last_taskcreate_id" // 워터마크: 작업 생성으로 이미 울린 작업 id의 최댓값
)

func newScheduler(s *Server) *Scheduler {
	return &Scheduler{s: s, pg: s.m.pg, tick: 5 * time.Second}
}

// Run은 ctx가 끝날 때까지 스케줄러를 틱합니다. 서버 New에서 한 번 시작합니다.
func (sc *Scheduler) Run(ctx context.Context) {
	if sc.pg == nil {
		return
	}
	sc.init()
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.s.reconcileConcurrency() // 동시 실행 상한: 빈자리가 있으면 대기 중인 작업을 시작해 채운다.
			sc.step()
		}
	}
}

// init은 첫 실행 때 워터마크를 심어, 이미 있던 발견/목표가 한꺼번에
// 울리지 않게 합니다. 스케줄러가 처음 시작한 뒤에 생긴 이벤트만 셉니다.
func (sc *Scheduler) init() {
	if sc.mustState(schedKeyLastFinding) == "" {
		var maxID int64
		if evs, err := sc.pg.NewFindingsSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastFinding, strconv.FormatInt(maxID, 10))
	}
	if sc.mustState(schedKeyFiredGoals) == "" {
		set := map[int64]bool{}
		if evs, err := sc.pg.MetGoals(); err == nil {
			for _, e := range evs {
				set[e.NodeID] = true
			}
		}
		sc.saveFiredGoalSet(set)
	}
	if sc.mustState(schedKeyLastTimeout) == "" {
		var maxID int64
		if evs, err := sc.pg.TimedOutTasksSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastTimeout, strconv.FormatInt(maxID, 10))
	}
	if sc.mustState(schedKeyLastToolCall) == "" {
		var maxID int64
		if evs, err := sc.pg.NewToolCallsSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastToolCall, strconv.FormatInt(maxID, 10))
	}
	if sc.mustState(schedKeyLastTaskCreate) == "" {
		var maxID int64
		if evs, err := sc.pg.NewTasksSince(0); err == nil {
			for _, e := range evs {
				if e.NodeID > maxID {
					maxID = e.NodeID
				}
			}
		}
		_ = sc.pg.SetSchedState(schedKeyLastTaskCreate, strconv.FormatInt(maxID, 10))
	}
}

func (sc *Scheduler) step() {
	triggers, err := sc.pg.ListEnabledTriggers()
	if err != nil {
		return
	}
	if len(triggers) == 0 {
		return
	}
	sc.fireIntervals(triggers)
	sc.fireFindings(triggers)
	sc.fireGoals(triggers)
	sc.fireTaskTimeouts(triggers)
	sc.fireToolCalls(triggers)
	sc.fireTaskCreates(triggers)
}

// fireIntervals는 last_fire 이후 간격이 지난 트리거를 울립니다.
func (sc *Scheduler) fireIntervals(triggers []*db.AgentTrigger) {
	now := time.Now()
	for _, tr := range triggers {
		if tr.IntervalSec <= 0 {
			continue
		}
		due := tr.LastFire == nil || now.Sub(*tr.LastFire) >= time.Duration(tr.IntervalSec)*time.Second
		if !due {
			continue
		}
		_ = sc.pg.TouchTriggerFire(tr.ID)
		ctx := "\n\n【이번 실행은 예약 시각에 의한 트리거】" + now.Format(" 2006-01-02 15:04:05 MST")
		sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("예약 트리거 · %s", now.Format("15:04")), tr.IntervalMessage+ctx, 0, false, "", "")
	}
}

// fireFindings는 저장된 워터마크보다 위의 발견에 on_finding 트리거를 울립니다
// (노드 id가 단조 증가 → 재시작해도 두 번 울리지 않음).
func (sc *Scheduler) fireFindings(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnFinding {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastFinding), 10, 64)
	events, err := sc.pg.NewFindingsSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// on_finding 트리거가 켜져 있든 없든 워터마크는 올립니다. 발견은
	// 나타나는 순간에 살아있는 트리거에만 울립니다. 그렇지 않으면
	// 나중에 켠 트리거가 옛 발견을 한 번에 전부 다시 울립니다.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		msgCtx := fmt.Sprintf("\n\n【이번 실행은 작업의 발견(finding)에 의한 트리거】\n발견: [%s/%s] %s",
			e.VulnClass, e.Severity, e.Summary)
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("finding 트리거 · task#%d", e.TaskID), tr.FindingMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastFinding, strconv.FormatInt(maxID, 10))
}

// fireGoals는 아직 울린 집합에 없는, 달성된 목표에 on_goal_met 트리거를 울립니다.
func (sc *Scheduler) fireGoals(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnGoalMet {
			want = append(want, tr)
		}
	}
	events, err := sc.pg.MetGoals()
	if err != nil || len(events) == 0 {
		return
	}
	// 트리거가 켜져 있든 없든 목표는 소비했다고 표시합니다. 나중에
	// on_goal_met를 켜도 이미 달성된 목표를 전부 다시 울리지 않게 합니다.
	fired := sc.firedGoalSet()
	changed := false
	for _, e := range events {
		if fired[e.NodeID] {
			continue
		}
		fired[e.NodeID] = true
		changed = true
		if len(want) == 0 {
			continue
		}
		msgCtx := fmt.Sprintf("\n\n【이번 실행은 작업 목표 달성에 의한 트리거】\n달성한 목표: %s", e.Summary)
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("목표 트리거 · task#%d", e.TaskID), tr.GoalMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	if changed {
		sc.saveFiredGoalSet(fired)
	}
}

// fireTaskTimeouts는 새로 status가 timeout이 된 작업에 on_task_timeout을 울립니다.
// 저장된 워터마크보다 위만(작업 id → 두 번 울리지 않음).
func (sc *Scheduler) fireTaskTimeouts(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnTaskTimeout {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastTimeout), 10, 64)
	events, err := sc.pg.TimedOutTasksSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// 활성 트리거가 없어도 워터마크는 올립니다. fireFindings를 보세요.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		msgCtx := "\n\n【이번 실행은 작업 시간 초과에 의한 트리거】"
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("시간 초과 트리거 · task#%d", e.TaskID), tr.TaskTimeoutMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastTimeout, strconv.FormatInt(maxID, 10))
}

// fireTaskCreates는 워터마크보다 새로 생긴 작업에 on_task_create를 울립니다
// (작업 id → 재시작해도 두 번 울리지 않음).
func (sc *Scheduler) fireTaskCreates(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnTaskCreate {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastTaskCreate), 10, 64)
	events, err := sc.pg.NewTasksSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// 활성 트리거가 없어도 워터마크는 올립니다. fireFindings를 보세요.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		msgCtx := "\n\n【이번 실행은 작업 생성에 의한 트리거】"
		for _, tr := range want {
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("작업 생성 트리거 · task#%d", e.TaskID), tr.TaskCreateMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastTaskCreate, strconv.FormatInt(maxID, 10))
}

// fireToolCalls는 워터마크보다 위의 도구 호출(tool_result 행) 중
// 트리거가 고른 도구 이름인 것에 on_tool_call을 울립니다. 울림
// 메시지에는 작업 id/설명/목표, 도구 이름, 자른 입력과 출력이 들어갑니다.
func (sc *Scheduler) fireToolCalls(triggers []*db.AgentTrigger) {
	var want []*db.AgentTrigger
	for _, tr := range triggers {
		if tr.OnToolCall && len(tr.ToolNames) > 0 {
			want = append(want, tr)
		}
	}
	last, _ := strconv.ParseInt(sc.mustState(schedKeyLastToolCall), 10, 64)
	events, err := sc.pg.NewToolCallsSince(last)
	if err != nil || len(events) == 0 {
		return
	}
	// 활성 트리거가 없어도 워터마크는 올립니다. fireFindings를 보세요.
	maxID := last
	for _, e := range events {
		if e.NodeID > maxID {
			maxID = e.NodeID
		}
		if len(want) == 0 {
			continue
		}
		// 발견 쓰기가 실패하면 확정된 발견이 없어 보고할 것이 없습니다. 다른
		// 도구 오류 트리거는 사용자가 정의한 자동화를 위해 남겨 둡니다.
		if e.ToolIsErr && e.Tool == "report_finding" {
			continue
		}
		errTag := ""
		if e.ToolIsErr {
			errTag = "[error] "
		}
		msgCtx := fmt.Sprintf("\n\n【이번 실행은 도구 호출에 의한 트리거】\n도구: %s\n입력: %s\n반환: %s%s",
			e.Tool, trunc(e.ToolInput, 1500), errTag, trunc(e.ToolOutput, 1500))
		for _, tr := range want {
			if !containsFold(tr.ToolNames, e.Tool) {
				continue
			}
			sc.s.StartTriggeredRun(tr.AgentKey, fmt.Sprintf("도구 트리거 · %s · task#%d", e.Tool, e.TaskID), tr.ToolCallMessage+msgCtx, e.TaskID, true, e.TaskDesc, e.TaskGoal)
		}
	}
	_ = sc.pg.SetSchedState(schedKeyLastToolCall, strconv.FormatInt(maxID, 10))
}

// containsFold는 name이 set에 있는지 알려 줍니다(대소문자 무시).
func containsFold(set []string, name string) bool {
	for _, s := range set {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

// trunc는 s를 max 글자까지 자릅니다. 잘리면 말줄임과 원래 길이를 붙입니다.
func trunc(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("…(잘림, 총 %d자)", len(r))
}

func (sc *Scheduler) mustState(key string) string {
	v, _ := sc.pg.GetSchedState(key)
	return v
}

func (sc *Scheduler) firedGoalSet() map[int64]bool {
	out := map[int64]bool{}
	raw := sc.mustState(schedKeyFiredGoals)
	if raw == "" {
		return out
	}
	var ids []int64
	if json.Unmarshal([]byte(raw), &ids) == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

func (sc *Scheduler) saveFiredGoalSet(set map[int64]bool) {
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	b, _ := json.Marshal(ids)
	if err := sc.pg.SetSchedState(schedKeyFiredGoals, string(b)); err != nil {
		log.Printf("[scheduler] save fired goals failed: %v", err)
	}
}
