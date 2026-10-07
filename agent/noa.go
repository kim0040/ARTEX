package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa는 norma v0.4.0이 가져온 「모델이 이끄는 컨텍스트 압축」입니다. 플랫폼 실험 기능이라
// 사용자가 시스템 설정에서 켜고 끕니다. 내장 compaction과 동시에 쓰지 않습니다. noaadapter.Enable이
// 유일한 입구입니다. 한 번에 컨텍스트 인수(Compactor), Compress 도구, 상주 프롬프트 세 조각을 겁니다.
// Enable을 호출하지 않으면 꺼진 것이고, 내장 compaction은 그대로 돕니다. 스위치는 각 에이전트가
// 넣는 noaEnabledFn이 해석하고, run마다 한 번 읽습니다. 그래서 전환은 이후 시작하는 run에만 영향을 주고
// 에이전트를 다시 만들 필요가 없습니다.

// enableNoa는 해석기가 켜짐이라고 할 때 noa를 opts에 붙입니다. archiveRoot는 압축 원문의 저장 기준 디렉터리입니다
// (전역 workDir을 쓰고, 각 에이전트는 <workDir>/noa 아래에 모이며 작업/인텐트 디렉터리로 흩어지지 않습니다).
// sessionID가 그 아래 보관 하위 디렉터리 이름입니다(전역에서 유일하므로 같은 기준 디렉터리 안에서는 충돌하지 않습니다).
//
// noa는 실험 기능입니다. 붙이기에 실패해도 실제 작업을 끊으면 안 됩니다. 오류가 나면 onWarn으로 알리고 내장 압축으로 돌아갑니다.
// 켜기에 성공하면 opts.Compaction을 지웁니다. agentcore가 「컨텍스트 관리자가 둘 동시에 설정됨」이라고 경고하지 않게 합니다.
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 압축을 켜지 못해 내장 압축으로 돌아갑니다: " + err.Error())
		}
		return
	}
	// Compactor가 Compaction을 덮지만, 둘을 같이 두면 agentcore가 매번 경고합니다. 분명히 지웁니다.
	opts.Compaction = nil
}
