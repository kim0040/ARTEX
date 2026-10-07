package db

import (
	"testing"
	"time"
)

func TestTaskQueuePreservesBootstrapAndFIFOPosition(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()

	task, err := d.CreateTask("queue metadata", "keep first-run mode", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)

	if err := d.Enqueue(task.ID, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	first, err := d.GetTask(task.ID)
	if err != nil || first == nil || first.QueuedAt == nil {
		t.Fatalf("first enqueue: task=%+v err=%v", first, err)
	}

	// 후속/재실행이 이미 대기 중인 작업을 resume로 넣으려 할 수 있다.
	// 원래 부트스트랩 모드와 FIFO 시각이 기준이어야 한다.
	time.Sleep(time.Millisecond)
	if err := d.Enqueue(task.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	second, err := d.GetTask(task.ID)
	if err != nil || second == nil {
		t.Fatalf("second enqueue: task=%+v err=%v", second, err)
	}
	if second.QueueMode != "bootstrap" {
		t.Fatalf("queue mode=%q, want bootstrap", second.QueueMode)
	}
	if second.QueuedAt == nil || !second.QueuedAt.Equal(*first.QueuedAt) {
		t.Fatalf("repeated enqueue moved FIFO position: first=%v second=%v", first.QueuedAt, second.QueuedAt)
	}

	// 대기 중인 작업을 멈추면 대기열에서는 빠지지만 필요한
	// 시작 모드는 남는다. 나중에 다시 넣으면 꼬리의 새 자리를 받는다.
	if err := d.Dequeue(task.ID, false); err != nil {
		t.Fatal(err)
	}
	paused, err := d.GetTask(task.ID)
	if err != nil || paused == nil || paused.Queued || paused.QueueMode != "bootstrap" {
		t.Fatalf("paused queue metadata: task=%+v err=%v", paused, err)
	}
	time.Sleep(time.Millisecond)
	if err := d.Enqueue(task.ID, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	requeued, err := d.GetTask(task.ID)
	if err != nil || requeued == nil || requeued.QueuedAt == nil {
		t.Fatalf("requeue: task=%+v err=%v", requeued, err)
	}
	if !requeued.QueuedAt.After(*first.QueuedAt) {
		t.Fatalf("requeue did not move to FIFO tail: first=%v requeued=%v", first.QueuedAt, requeued.QueuedAt)
	}
}
