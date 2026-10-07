package sidequestion

import (
	"context"
	"errors"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// SideQuestionService에는 하네스, 도구 실행기, 대화 기록기, 모델 장애 조치
// 사슬이 없습니다. Answer는 완료 한 번이고, Respond는 그 완료 둘레에 한정된
// 준비와 맥락 넘침 복구를 최대 한 번 더합니다.
type SideQuestionService struct{ Provider llm.Provider }

type Answer struct {
	Text    string
	Usage   llm.Usage
	ToolUse bool
}

func (s SideQuestionService) Answer(ctx context.Context, req llm.CompletionRequest, streaming bool, update func(Answer)) (out Answer, err error) {
	if streaming {
		complete := false
		for ev, streamErr := range s.Provider.Stream(ctx, req) {
			if streamErr != nil {
				err = streamErr
				break
			}
			switch ev.Type {
			case llm.SETextDelta:
				out.Text += ev.Text
			case llm.SEToolUseStart:
				out.ToolUse = true
			case llm.SEMessageStart, llm.SEMessageDelta:
				out.Usage.Add(ev.Usage)
			case llm.SEMessageStop:
				complete = true
			}
			if update != nil {
				update(out)
			}
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
		}
		if err == nil && !complete {
			err = errors.New("모델 응답이 중간에 끊겼습니다. 다시 질문하세요")
		}
	} else {
		var msg llm.Message
		msg, _, out.Usage, err = s.Provider.Complete(ctx, req)
		out.Text, out.ToolUse = msg.Text(), len(msg.ToolUses()) > 0
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && strings.TrimSpace(out.Text) == "" {
		if out.ToolUse {
			out.Text = "이번 곁길 질문은 도구를 실행할 수 없습니다. 메인 세션에서 조작을 요청하세요."
		} else {
			err = errors.New("모델이 답을 반환하지 않았습니다")
		}
	}
	return out, err
}
