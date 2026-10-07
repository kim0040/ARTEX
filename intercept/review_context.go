package intercept

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const reviewTextLimit = 4000

const BackgroundUserMessage = "user_message"

// ReviewBackground 는 지금 사람의 메시지에서 명시적으로 묶은 배경입니다.
// 만들어진 워커 요약은 받지 않습니다. 배경이 심사 정책을 덮어쓸 수 없습니다.
// 초보: 가드 정책은 그대로입니다. 워커 의도는 심사 배경이 아닙니다.
type ReviewBackground struct {
	Source    string `json:"source"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ReviewInput 은 이번 호출과, 명시적으로 고른 배경만 담습니다.
// 실행 이력과 호출 연결은 따로 둔 감사 기록에 속합니다.
// 초보: 모델에게 주는 심사 입력입니다. 탐색 그래프나 승인 이력은 넣지 않습니다.
type ReviewInput struct {
	Version    int               `json:"version"`
	WorkingDir string            `json:"working_directory,omitempty"`
	Background *ReviewBackground `json:"background,omitempty"`
	Tool       string            `json:"tool_name"`
	Arguments  json.RawMessage   `json:"arguments"`
}

type reviewContextKey struct{}
type reviewEnvironment struct {
	workingDir string
	background ReviewBackground
}

// WithReviewContext 는 한 실행에 허용된 배경을 명시적으로 묶습니다.
// 원본 턴 기록으로 되돌아가지 않습니다. 그 안에 스케줄러 프롬프트 전체가 있을 수 있습니다.
// 이것은 앱이 연결하는 값이며, 모델이 호출할 수 있는 도구가 아닙니다.
// 초보: 가드가 모델에게 줄 배경을 여기서 고정합니다.
func WithReviewContext(ctx context.Context, workingDir string, background ReviewBackground) context.Context {
	return context.WithValue(ctx, reviewContextKey{}, reviewEnvironment{workingDir, background})
}

// WithReviewWorkingDirectory 는 명시적으로 고른 배경만 유지합니다.
// 채팅은 사람이 시작했을 수도, 예약으로 시작했을 수도 있습니다.
// 그래서 에이전트는 받은 글만으로 메시지 출처를 짐작하면 안 됩니다.
func WithReviewWorkingDirectory(ctx context.Context, workingDir string) context.Context {
	env, _ := ctx.Value(reviewContextKey{}).(reviewEnvironment)
	env.workingDir = workingDir
	return context.WithValue(ctx, reviewContextKey{}, env)
}

func BuildReviewInput(ctx context.Context, tool string, arguments json.RawMessage) (ReviewInput, error) {
	if !json.Valid(arguments) {
		return ReviewInput{}, fmt.Errorf("도구 인자가 올바른 JSON이 아닙니다")
	}
	in := ReviewInput{Version: 4, Tool: tool, Arguments: append(json.RawMessage(nil), arguments...)}
	if env, ok := ctx.Value(reviewContextKey{}).(reviewEnvironment); ok {
		in.WorkingDir = env.workingDir
		background := env.background
		if background.Source == BackgroundUserMessage && strings.TrimSpace(background.Text) != "" {
			var cut bool
			background.Text, cut = bounded(background.Text, reviewTextLimit)
			background.Truncated = background.Truncated || cut
			in.Background = &background
		}
	}
	return in, nil
}
