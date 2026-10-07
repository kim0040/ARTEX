package server

import (
	"sync"

	"github.com/Autumn-27/artex/db"
)

// Broadcaster는 작업마다, 프로세스 안에서 실시간 활동 이벤트를 주고받는 발행/구독입니다.
// 엔진은 활동을 붙이는 한 지점에서 발행하고, SSE 처리기는 작업마다 구독합니다.
// 저장(activity 테이블)과 실시간 줄이 같은 Publish 호출에서 나오므로
// 서로 어긋나지 않습니다.
// 초보용: 엔진이 남긴 활동을 작업 화면의 실시간 줄로 보냅니다.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan db.Activity]struct{} // 작업 id → 구독자 채널 집합
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[string]map[chan db.Activity]struct{}{}}
}

// Subscribe는 한 작업의 활동을 받는 버퍼 채널과,
// 호출자가 defer로 반드시 불러 놓을 구독 해제 함수를 돌려줍니다.
func (b *Broadcaster) Subscribe(task string) (<-chan db.Activity, func()) {
	ch := make(chan db.Activity, 256)
	b.mu.Lock()
	if b.subs[task] == nil {
		b.subs[task] = map[chan db.Activity]struct{}{}
	}
	b.subs[task][ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if m := b.subs[task]; m != nil {
				delete(m, ch)
				if len(m) == 0 {
					delete(b.subs, task)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish는 활동 하나를 그 작업의 모든 구독자에게 퍼뜨립니다. 막지 않습니다.
// 구독자 버퍼가 가득 차면 그 이벤트는 버립니다. 클라이언트는 마지막 seq 커서로 다시 붙고
// DB에서 빈 구간을 따라잡습니다. 그래서 실시간성이
// 엔진을 멈추게 하지 않습니다.
func (b *Broadcaster) Publish(task string, a db.Activity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[task] {
		select {
		case ch <- a:
		default:
		}
	}
}
