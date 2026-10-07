package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// emptyBindingRecorder 는 저장소 없이 발견 한 건이 저장된 것처럼 보이게 한다.
// 트래픽 연결은 비어 있어, 화면과 도구 결과에 남는 안내 문장을 검사할 수 있다.
type emptyBindingRecorder struct{}

func (emptyBindingRecorder) Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error) {
	return &db.RecordedFinding{FindingID: 7, NodeID: 9, Traffic: &db.FindingTraffic{}}, nil
}

// TestWriteCountsKorean은 엔진 로그에 찍히는 기록 요약을 검사한다.
// 초보: 워커가 한 번 돌고 나서 사실·자산·발견을 몇 개 썼는지 이 한 줄로 남긴다.
func TestWriteCountsKorean(t *testing.T) {
	got := (WriteCounts{Facts: 1, Assets: 25, Findings: 0}).String()
	if got != "사실1 자산25 발견0" {
		t.Fatalf("write summary = %q", got)
	}
}

// TestToolErrorsKorean은 Postgres 없이, 실제로 도구가 돌려주는 오류 문장을 검사한다.
func TestToolErrorsKorean(t *testing.T) {
	ts := NewToolSet(nil, "fixture")
	if _, err := ts.addOneIntent(intentItem{}); err == nil || err.Error() != "summary는 비울 수 없습니다" {
		t.Fatalf("empty intent: %v", err)
	}
	if _, err := ts.addOneGoal(goalItem{}); err == nil || err.Error() != "text는 비울 수 없습니다" {
		t.Fatalf("empty goal: %v", err)
	}
	if _, err := ts.addOneConstraint(constraintItem{Text: "범위", Type: "nope"}); err == nil || err.Error() != "type 은 allow 또는 deny 여야 합니다" {
		t.Fatalf("bad constraint: %v", err)
	}
	res, err := ts.listGoals().Call(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !strings.Contains(res.Flatten(), "작업 맥락(탐색 그래프)이 필요합니다") {
		t.Fatalf("nil exploration: %v %s", err, res.Flatten())
	}
	res, err = ts.insertAssets().Call(context.Background(), json.RawMessage(`{"assets":[]}`), nil)
	if err != nil || !strings.Contains(res.Flatten(), "AssetStore 가 초기화되지 않았습니다") {
		t.Fatalf("nil asset store: %v %s", err, res.Flatten())
	}
}

// TestReportFindingNotesKorean은 발견 도구가 사람에게 남기는 안내를 검사한다.
// 탐색 그래프를 열지 않는다. 저장 함수만 바꿔 끼우고, 문장은 addFinding 이 만든다.
func TestReportFindingNotesKorean(t *testing.T) {
	old := FindingTrafficBindingEnabled
	t.Cleanup(func() { FindingTrafficBindingEnabled = old })

	FindingTrafficBindingEnabled = func() bool { return true }
	ts := NewToolSet(&db.ExplorationStore{}, "fixture")
	res, err := ts.addFinding().Call(context.Background(), json.RawMessage(`{"vulnclass":"TEST","summary":"no storage","severity":"low","traffic_refs":[{"traffic_id":"x"}]}`), nil)
	if err != nil || !strings.Contains(res.Flatten(), "등록하지 않았습니다") {
		t.Fatalf("no store: %v %s", err, res.Flatten())
	}

	FindingTrafficBindingEnabled = func() bool { return false }
	ts.SetFindingRecorder(emptyBindingRecorder{})
	res, err = ts.addFinding().Call(context.Background(), json.RawMessage(`{"vulnclass":"TEST","summary":"off","severity":"low","traffic_refs":[{"traffic_id":"x"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var note struct {
		EvidenceNote string `json:"evidence_note"`
	}
	parts := strings.SplitN(res.Flatten(), "\n", 2)
	if len(parts) != 2 || json.Unmarshal([]byte(parts[1]), &note) != nil || !strings.Contains(note.EvidenceNote, "닫혀 있어") {
		t.Fatalf("disabled note: %s", res.Flatten())
	}
}
