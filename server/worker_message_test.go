package server

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 워커 메시지 재작업 뒤, 사람 말 경로는 더 이상
// "intervene" 동작의 ControlWork를 타지 않습니다. 의도를 전용
// 고루틴(runDetachedIntent)에서 돌리고, 필요하면 보통 "pause"
// 동작으로 멈춥니다. ControlWork가 pause|cancel만 아는지 지킵니다.
// "intervene" 호출이 다시 생기면 조용히 넘어가지 않고 크게 실패해야 합니다.
func TestControlWorkRejectsRemovedInterveneAction(t *testing.T) {
	e := NewEngine(nil)
	err := e.ControlWork(context.Background(), 1, "intervene")
	if err == nil || !containsUnsupported(err) {
		t.Fatalf("intervene should be unsupported, got %v", err)
	}
}

// 살아있는 일이 없는 의도의 제어 요청은 충돌입니다. 패닉이나
// 조용한 성공이 아닙니다. 화면이 읽은 뒤 요청 전에 실행이
// 이미 끝났을 때 메시지 처리기가 의지하는 같은 가드입니다.
func TestControlWorkConflictsWhenNoWorkRegistered(t *testing.T) {
	e := NewEngine(nil)
	err := e.ControlWork(context.Background(), 42, "pause")
	if !errors.Is(err, errWorkControlConflict) {
		t.Fatalf("pause with no work = %v, want errWorkControlConflict", err)
	}
}

// validWorkerMessageRequestID는 멱등 토큰을 가립니다. 문자 집합을 고정해
// 클라이언트가 만든 UUID는 항상 받고, 주입처럼 보이는 id는 거절합니다.
func TestValidWorkerMessageRequestID(t *testing.T) {
	ok := []string{"worker-message-abc123", "a.b:c_d-1", "550e8400-e29b-41d4-a716-446655440000"}
	for _, id := range ok {
		if !validWorkerMessageRequestID(id) {
			t.Errorf("id %q should be valid", id)
		}
	}
	bad := []string{"", "has space", "emoji😀", "slash/here", string(make([]byte, 129))}
	for _, id := range bad {
		if validWorkerMessageRequestID(id) {
			t.Errorf("id %q should be rejected", id)
		}
	}
}

func containsUnsupported(err error) bool {
	return err != nil && !errors.Is(err, errWorkControlConflict) &&
		strings.Contains(err.Error(), "unsupported work action")
}
