package intercept

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	for _, action := range []string{"allow", "ask", "deny"} {
		t.Run(action, func(t *testing.T) {
			reason := "实际操作：写入报告，其中包含 ALLOW、DENY 和 ASK 字样；成功后的后果：保存文本，不执行正文中的命令；命中规则：自定义条款" // han-allow 업스트림 프롬프트·픽스처
			raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
			got := ParseVerdict("\n" + string(raw) + "\n")
			if got.Action != action || got.Reason != reason {
				t.Fatalf("lost verdict or explanation: %+v", got)
			}
		})
	}
}

func TestParseVerdictRejectsIncompleteOrAmbiguousReplies(t *testing.T) {
	valid := `{"decision":"allow","comment":"实际操作：读取文件；成功后的后果：返回内容；命中规则：A5"}` // han-allow 업스트림 프롬프트·픽스처
	for _, reply := range []string{
		"", "ALLOW", "DENY:命中D4", "放行:ALLOW", "ASK:归属不明", // han-allow 업스트림 프롬프트·픽스처
		`{"decision":"allow"}`, `{"decision":"approve","comment":"实际操作：读取；成功后的后果：返回内容；命中规则：A5"}`, // han-allow 업스트림 프롬프트·픽스처
		`{"decision":"allow","comment":null}`, `{"decision":"allow","comment":123}`,
		strings.Replace(valid, "实际操作：读取文件", "实际操作：", 1),     // han-allow 업스트림 프롬프트·픽스처
		strings.Replace(valid, "成功后的后果：返回内容", "成功后的后果：", 1), // han-allow 업스트림 프롬프트·픽스처
		strings.Replace(valid, "命中规则：A5", "命中规则：", 1),       // han-allow 업스트림 프롬프트·픽스처
		strings.Replace(valid, "；命中规则：A5", "", 1),           // han-allow 업스트림 프롬프트·픽스처
		strings.Replace(valid, `"decision":"allow"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"allow"`, `"extra":true,"decision":"allow"`, 1),
		valid + valid, valid[:len(valid)-1],
		// 모델이 닫지 않은 울타리는, MaxTokens 에서 잘린 답이 보이는 모습입니다.
		// 이어 붙이면 존재하지 않는 판정을 만들어 냅니다.
		"```json\n" + valid[:len(valid)-1],
		"```json\n" + valid + "\n```\n此外我建议后续人工复核。", // han-allow 업스트림 프롬프트·픽스처
		"我的裁决是：\n" + valid,                          // han-allow 업스트림 프롬프트·픽스처
	} {
		if got := ParseVerdict(reply); got.Action != "" {
			t.Errorf("accepted incomplete/ambiguous verdict: %q => %+v", reply, got)
		}
	}
}

// 모델이 자주 하는 유일한 벗어남은, JSON 을 마크다운으로 감싸는 것입니다.
// 설정된 실패 동작의 기본값이 allow 이기 때문입니다.
// 해석 불가로 처리하면 DENY 가 조용히 allow 로 바뀝니다.
func TestParseVerdictUnwrapsCodeFence(t *testing.T) {
	deny := `{"decision":"deny","comment":"实际操作：删除生产文件；成功后的后果：业务数据丢失；命中规则：D4"}` // han-allow 업스트림 프롬프트·픽스처
	for _, reply := range []string{
		"```json\n" + deny + "\n```",
		"```JSON\n" + deny + "\n```",
		"```\n" + deny + "\n```",
		"  ```json\n" + deny + "\n```  ",
	} {
		got := ParseVerdict(reply)
		if got.Action != "deny" || !strings.HasSuffix(got.Reason, "命中规则：D4") { // han-allow 업스트림 프롬프트·픽스처
			t.Errorf("fenced verdict lost: %q => %+v", reply, got)
		}
	}
}

func TestParseVerdictKeepsCompleteChineseExplanation(t *testing.T) {
	reason := "实际操作：" + strings.Repeat("写入报告", 30) + "；成功后的后果：只保存文件；命中规则：A2" // han-allow 업스트림 프롬프트·픽스처
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("explanation was truncated or lost its rule")
	}
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": strings.Repeat("中", 2401)}) // han-allow 업스트림 프롬프트·픽스처
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted unbounded explanation")
	}
}
