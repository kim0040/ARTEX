package server

import (
	"strings"
	"testing"
)

// 이벤트마다 반복된 긴 작업 목표가 부풀림의 대부분이었습니다. 이 테스트는 고친 내용을 고정합니다.
// 작업 맥락 머리(설명 + 목표)는 작업마다 한 번만 그립니다.
// 같은 작업의 울림을 몇 개나 합치든 그렇습니다.

const longGoal = "문제 f2-05의 보호된 flag를 얻어 submit_flag로 제출한다. 이 문제의 암호문은 이미 고도로 수렴되어 있으며, flag는 바이너리에 내장된 데이터에서만 파생할 수 있다……" // 그 수천 글자의 상속된 사실을 대표한다

func sameTaskFires(n int) []triggeredRun {
	items := make([]triggeredRun, n)
	for i := range items {
		items[i] = triggeredRun{
			agentKey: "tec_benchmark", taskID: 72, taskDesc: "f2-05 역분석", taskGoal: longGoal,
			message: "【이번은 도구 호출로 트리거됨】\n도구: submit_flag\n인자: {...}\n반환: {correct:false}", mergeable: true,
		}
	}
	return items
}

func TestMergeAllRunsWritesTaskGoalOnce(t *testing.T) {
	out := mergeAllRuns(sameTaskFires(39))
	if got := strings.Count(out.message, longGoal); got != 1 {
		t.Fatalf("same-task goal should appear exactly once in a merged-all run, got %d", got)
	}
	if strings.Count(out.message, "── 트리거 ") < 1 || !strings.Contains(out.message, "트리거 39") {
		t.Fatalf("all 39 event bodies should be present: %q", out.message)
	}
	// 합친 실행은 머리를 안에 넣으므로, finalTriggerMessage가 다시 넣으면 안 됩니다.
	if out.taskDesc != "" || out.taskGoal != "" {
		t.Fatalf("merged run must clear taskDesc/taskGoal to avoid a duplicate header")
	}
	if finalTriggerMessage(out) != out.message {
		t.Fatalf("finalTriggerMessage must not prepend another header for a merged run")
	}
}

func TestMergeAllRunsGroupsInterleavedTasks(t *testing.T) {
	// 두 작업의 울림이 섞여 와도(A,B,A,B) 각
	// 작업의 맥락은 정확히 한 번만 있어야 합니다. 묶는 것이지, 이벤트마다 반복이 아닙니다.
	mk := func(id int64, goal string) triggeredRun {
		return triggeredRun{agentKey: "a", taskID: id, taskDesc: "d", taskGoal: goal, message: "body", mergeable: true}
	}
	out := mergeAllRuns([]triggeredRun{mk(1, "GOAL_A"), mk(2, "GOAL_B"), mk(1, "GOAL_A"), mk(2, "GOAL_B")})
	if got := strings.Count(out.message, "GOAL_A"); got != 1 {
		t.Fatalf("task #1 goal should appear once despite interleaving, got %d", got)
	}
	if got := strings.Count(out.message, "GOAL_B"); got != 1 {
		t.Fatalf("task #2 goal should appear once despite interleaving, got %d", got)
	}
	if !strings.Contains(out.message, "작업 총 2개") {
		t.Fatalf("header should report 2 tasks: %q", out.message)
	}
	if got := strings.Count(out.message, "── 트리거 "); got != 4 {
		t.Fatalf("all 4 event bodies should be present, got %d", got)
	}
}

func TestMergeTriggeredRunsWritesTaskGoalOnce(t *testing.T) {
	out := mergeTriggeredRuns(sameTaskFires(5))
	if got := strings.Count(out.message, longGoal); got != 1 {
		t.Fatalf("same-task goal should appear exactly once in a by-task merge, got %d", got)
	}
}

func TestFinalTriggerMessageSingleFirePrependsHeaderOnce(t *testing.T) {
	item := sameTaskFires(1)[0]
	msg := finalTriggerMessage(item)
	if got := strings.Count(msg, longGoal); got != 1 {
		t.Fatalf("single fire should carry the task goal exactly once, got %d", got)
	}
	if !strings.HasPrefix(msg, "【작업 #72") {
		t.Fatalf("single fire should be prefixed with the task-context header: %q", msg)
	}
}

func TestTaskContextHeaderEmptyForIntervalFire(t *testing.T) {
	if h := taskContextHeader(0, "", ""); h != "" {
		t.Fatalf("interval/none trigger (no task) must produce no header, got %q", h)
	}
	// 간격 울림의 메시지는 그대로 통과해야 합니다.
	item := triggeredRun{message: "예약 트리거 본문"}
	if finalTriggerMessage(item) != "예약 트리거 본문" {
		t.Fatalf("interval fire message must pass through unchanged")
	}
}

func TestTaskContextHeaderTruncatesLongGoal(t *testing.T) {
	huge := strings.Repeat("가", 5000)
	h := taskContextHeader(72, "d", huge)
	if len([]rune(h)) > 800 { // 200 desc + 500 goal + 잘림 표시/장식이며, 5000보다 훨씬 작다
		t.Fatalf("header should be bounded even for a huge goal, got %d runes", len([]rune(h)))
	}
}
