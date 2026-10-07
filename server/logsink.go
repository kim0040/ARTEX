package server

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// LogLine은 /api/logs가 보여주는, 잡은 백엔드 로그 한 줄입니다.
type LogLine struct {
	Seq   int64  `json:"seq"`
	DBID  int64  `json:"db_id,omitempty"` // server_logs.id. 저장 전 항목은 0
	TS    string `json:"ts"`
	Level string `json:"level"` // info=정보 | warn=경고 | error=오류
	Tag   string `json:"tag"`   // 앞의 [tag]입니다(pg / planner / activity / …). 없으면 비움
	Text  string `json:"text"`
}

// dbWriteReq는 로그 한 줄을 비동기 DB 기록기에 넘깁니다.
type dbWriteReq struct {
	seq int64
	ll  LogLine
}

// logSinkT는 표준 로거를 메모리 링 버퍼로 보내고, 새
// 줄을 SSE 구독자에게 퍼뜨립니다. 화면이 백엔드 로그를 실시간으로 보게 합니다.
// 그래도 전부 stderr로도 보냅니다(터미널은 그대로 됩니다).
// 초보용: 엔진이 찍은 로그를 화면의 실시간 로그에 보냅니다.
type logSinkT struct {
	mu   sync.Mutex
	ring []LogLine
	cap  int
	seq  int64
	subs map[chan LogLine]struct{}
	out  io.Writer // 통과(stderr)

	dbOnce sync.Once
	dbCh   chan dbWriteReq // 버퍼 있는 비동기 채널. SetDB 전에는 nil
}

var logSink = &logSinkT{cap: 3000, subs: map[chan LogLine]struct{}{}, out: os.Stderr}

// StartLogCapture는 표준 log 패키지를 메모리 싱크로 돌립니다
// (stderr에도 계속 씀). 시작 때 가능한 한 이르게, 한 번만 부릅니다.
func StartLogCapture() { log.SetOutput(logSink) }

// SetDB는 postgres DB를 싱크에 한 번 연결합니다(여러 번 해도 한 번).
//  1. DB의 최근 로그 100줄을 링에 복구해, /logs 페이지가
//     재시작 직후에도 이전 기록을 바로 보여 주게 합니다.
//  2. 그 뒤의 로그 줄을 DB에 비동기로 저장하는 고루틴을 시작합니다.
func (s *logSinkT) SetDB(ctx context.Context, pg *db.DB) {
	if pg == nil {
		return
	}
	s.dbOnce.Do(func() {
		ch := make(chan dbWriteReq, 2000)
		s.mu.Lock()
		s.dbCh = ch
		s.mu.Unlock()

		// DB의 최근 100줄을 링 앞에 이전 맥락으로 붙입니다.
		if logs, err := pg.RecentLogs(100); err == nil && len(logs) > 0 {
			s.mu.Lock()
			restored := make([]LogLine, 0, len(logs))
			for _, l := range logs {
				s.seq++
				restored = append(restored, LogLine{
					Seq:   s.seq,
					DBID:  l.ID,
					TS:    l.CreatedAt.Format(time.RFC3339),
					Level: l.Level,
					Tag:   l.Tag,
					Text:  l.Text,
				})
			}
			// 링에 이미 있는 시작 로그보다 앞에 이전 기록을 붙입니다.
			s.ring = append(restored, s.ring...)
			if len(s.ring) > s.cap {
				s.ring = s.ring[len(s.ring)-s.cap:]
			}
			s.mu.Unlock()
		}

		// 비동기 기록기: 채널에서 집어 server_logs에 넣습니다.
		// 오류는 os.Stderr에 직접 써서, 로그가 다시 호출되지 않게 합니다.
		go func() {
			for {
				select {
				case req, ok := <-ch:
					if !ok {
						return
					}
					id, err := pg.InsertLog(req.ll.Level, req.ll.Tag, req.ll.Text)
					if err != nil {
						_, _ = os.Stderr.Write([]byte("[logsink] db write: " + err.Error() + "\n"))
						continue
					}
					// DBID를 링 항목에 다시 찍어, 이전 기록 페이지가 되게 합니다.
					s.mu.Lock()
					for i := range s.ring {
						if s.ring[i].Seq == req.seq {
							s.ring[i].DBID = id
							break
						}
					}
					s.mu.Unlock()
				case <-ctx.Done():
					return
				}
			}
		}()
	})
}

// Write는 log 패키지용 io.Writer입니다. log.Printf 한 줄마다 한 번 불립니다.
func (s *logSinkT) Write(p []byte) (int, error) {
	_, _ = s.out.Write(p) // 터미널 출력은 유지합니다.
	line := strings.TrimRight(string(p), "\n")
	if strings.TrimSpace(line) != "" {
		s.add(parseLog(line))
	}
	return len(p), nil
}

func (s *logSinkT) add(l LogLine) {
	s.mu.Lock()
	s.seq++
	l.Seq = s.seq
	s.ring = append(s.ring, l)
	if len(s.ring) > s.cap {
		s.ring = s.ring[len(s.ring)-s.cap:]
	}
	ch := s.dbCh
	subs := make([]chan LogLine, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	// 비동기 DB 쓰기(막지 않음. 극단적 부하로 채널이 가득 차면 버림).
	if ch != nil {
		select {
		case ch <- dbWriteReq{seq: l.Seq, ll: l}:
		default:
		}
	}

	for _, sub := range subs {
		select {
		case sub <- l:
		default:
		}
	}
}

// recent는 Seq가 since보다 큰 링 줄을 돌려줍니다(limit까지). 최신 seq도 함께.
func (s *logSinkT) recent(since int64, limit int) (lines []LogLine, cursor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor = s.seq
	if limit <= 0 || limit > s.cap {
		limit = s.cap
	}
	for _, l := range s.ring {
		if l.Seq > since {
			lines = append(lines, l)
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, cursor
}

func (s *logSinkT) subscribe() (<-chan LogLine, func()) {
	ch := make(chan LogLine, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// parseLog는 표준 로거 줄에서 수준과 [tag]를 뽑습니다. 시각은
// 잡은 시각입니다(RFC3339).
func parseLog(line string) LogLine {
	msg := line
	// 있으면 "2006/01/02 15:04:05" LstdFlags 접두사를 떼어 냅니다.
	if len(msg) >= 20 && msg[4] == '/' && msg[7] == '/' && msg[10] == ' ' {
		msg = strings.TrimSpace(msg[19:])
	}
	tag := ""
	if strings.HasPrefix(msg, "[") {
		if i := strings.IndexByte(msg, ']'); i > 1 {
			tag = msg[1:i]
		}
	}
	return LogLine{TS: time.Now().Format(time.RFC3339), Level: levelOf(msg), Tag: tag, Text: msg}
}

func levelOf(msg string) string {
	low := strings.ToLower(msg)
	// 예전 중국어 로그와 지금 한국어 로그를 같은 단계로 분류한다. 화면에 직접 보여 주는 문구는 아니다.
	for _, k := range []string{"fatal", "panic", "error", "err:", "失败", "丢弃", "拒绝", "✕", "不可达", "실패", "폐기", "거부", "도달 불가"} { // han-allow 옛 로그 분류 키워드
		if strings.Contains(low, k) {
			return "error"
		}
	}
	for _, k := range []string{"warn", "disabled", "禁用", "skip", "stopped", "⚠", "重试", "비활성", "재시도"} { // han-allow 옛 로그 분류 키워드
		if strings.Contains(low, k) {
			return "warn"
		}
	}
	return "info"
}
