package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock 은 이 작업의 조작 제약(task_constraints)을 우선순위가 높은 블록으로 만들어
// 플래너와 워커의 시스템 프롬프트 뒤에 붙입니다. allow 와 deny 로 나누고, 제약이 없거나
// ts 가 nil 이면 빈 문자열입니다. 이 문장을 일부러 탐색 휴리스틱보다 위에 두어,
// 적힌 경계가 "다른 입구를 더 쫓자"는 유혹을 이기게 합니다.
// 초보: 탐색 그래프에 저장된 조작 제약이 플래너와 워커가 읽는 프롬프트에 여기서 들어갑니다.
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【操作约束（最高优先级，凌驾于下方一切探索/拓面启发式；每生成一条意图、每执行一个动作前都必须先自检是否违反，违反即不得进行）】：")
	if len(allow) > 0 {
		b.WriteString("\n允许的操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止的操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n（发现约束之外的新目标/新端口/新主机，不等于获得授权：除非它落在上述允许范围内，否则记为 out-of-scope 事实并跳过，不得为其派生意图或执行动作。）")
	return b.String()
}
