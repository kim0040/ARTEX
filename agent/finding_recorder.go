package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder 는 호스트가 넣어 줍니다. 에이전트는 증거 본문을 직접 만들거나 복사하지 않습니다.
// 구현체가 한 번에 쓰는 일을 맡습니다.
// 초보: 워커가 올린 발견을 기록 프록시의 트래픽 증거와 함께 탐색 그래프에 남기는 입구입니다.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// 도구 사용 안내는 사용자가 고칠 수 있는 프롬프트를 바꾸지 않고 뒤에 붙입니다.
// 캡처를 요구하지 않고, 없는 트래픽 도구가 있다고 말하지도 않습니다.
const findingTrafficGuidance = "\n\n**漏洞流量证据（可选）**：调用 report_finding 上报漏洞时，如有已查看并确认支持漏洞结论的 HTTP 请求/响应，可用 traffic_refs 按复现顺序绑定真实 ID；域名和时间只作候选筛选，不推定关联。TCP 等非 HTTP 漏洞、未采集或无确切匹配时省略或传 []，在 evidence 保留命令输出、日志等其他可验证证据，建议说明未绑定原因。不要猜测 ID，也不要仅为补包重复探测。"

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
