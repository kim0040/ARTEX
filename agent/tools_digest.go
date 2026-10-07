package agent

// cold-digest §6: graph_overview 의 접기와 되돌리기 도구.
//
//	coldDigestsRecent — graph_overview 의 접힌 cold 구역을 만듭니다.
//	  cold_digests (납작한 {id, body, member_count}). 가장 최근 구성원부터, 상한이 있습니다.
//	expand_digest(id) — 1단계 되돌리기. digest 구성원의 짧은 목록입니다.
// 초보: 탐색 그래프에서 식어 접힌 사실과 의도를, 플래너가 보는 개요에 여기서 넣습니다.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// digestMemberEntry 는 expand_digest 가 돌려주는, 구성원마다의 짧은 모양을 만듭니다.
// recent_facts / recent_done_intents 와 같습니다(§6.1 중간 층). store 는
// 그 digest 를 소유한 저장소입니다(현재 작업, 또는 읽기 전용 원본 작업 §2).
func (t *ToolSet) digestMemberEntry(store *db.ExplorationStore, id int64) map[string]any {
	n, _ := store.GetNode(id)
	if n == nil {
		return map[string]any{"id": id, "missing": true}
	}
	m := compactNode(n)
	m["state"] = n.State
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if c, ok := p["confidence"].(string); ok && c != "" {
			m["confidence"] = c
		}
	}
	return m
}

// coldDigestsRecent 는 저장소의 활성 digest 를 graph_overview 용 납작한 본문으로 돌려줍니다.
// 가장 최근 구성원 순입니다(구성원 id 최대값 ≈ 가장 늦게 식은 노드.
// 살아 있는 프론티어에 가까운 digest 가 더 관련 있기 쉽습니다). `cap` 으로 자릅니다.
// 넘친 digest id 는 moreIDs 로 따로 돌려, 한 줄에 안 보여도 expand_digest 로 닿게 합니다.
// 접힌 cold 노드가 나가는 출구는 cold_digests 뿐입니다. 현재 작업 개요와
// 읽기 전용 연관 작업 개요(§2 작업 사이 재사용)가 같이 씁니다.
func coldDigestsRecent(store *db.ExplorationStore, cap int) (shown []map[string]any, moreIDs []int64) {
	ads, err := store.ActiveDigests()
	if err != nil || len(ads) == 0 {
		return nil, nil
	}
	type dg struct {
		id        int64
		entry     map[string]any
		freshness int64 // 구성원 id 최대값(id 는 단조 증가라 만든 시각에 가깝습니다)
	}
	items := make([]dg, 0, len(ads))
	for _, d := range ads {
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(d.Payload, &p)
		ms, _ := store.DigestMembers(d.ID) // 오름차순. 마지막이 가장 최근입니다
		var fresh int64
		if len(ms) > 0 {
			fresh = ms[len(ms)-1]
		}
		items = append(items, dg{
			id:        d.ID,
			entry:     map[string]any{"id": d.ID, "body": p.Body, "member_count": len(ms)},
			freshness: fresh,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].freshness > items[j].freshness })
	for i, it := range items {
		if i < cap {
			shown = append(shown, it.entry)
		} else {
			moreIDs = append(moreIDs, it.id)
		}
	}
	return shown, moreIDs
}

// hiddenMembersFor 는 그 저장소에서 구성원이 숨었는지(활성 digest 에 접혀 있고 아직 cold 인지)
// 알려 주는 판별 함수를 돌려줍니다. 원본 작업의 개요가 그 작업 자신과 같게 접히게 합니다
// (§2 작업 사이: "현재 작업이 어떤 표시 논리를 쓰면, 연결된 작업도 그 논리를 씁니다").
// 다시 살아난(지금 hot 인) 구성원은 숨지 않습니다(§6 그릴 때의 부활 검사).
// digest 가 없으면 절대 숨지 않는 판별 함수를 돌려줍니다.
func hiddenMembersFor(store *db.ExplorationStore) func(int64) bool {
	covered, err := store.CoveredMembers()
	if err != nil || len(covered) == 0 {
		return func(int64) bool { return false }
	}
	var hot map[int64]bool
	if cg, _, err := loadColdGraph(store); err == nil {
		hot = cg.hotSet()
	}
	return func(id int64) bool { _, c := covered[id]; return c && !hot[id] }
}

// resolveDigest 는 id 로 digest 노드를 찾습니다. 현재 작업에서 먼저, 없으면 직접 원본
// 작업에서 찾습니다(읽기 전용, §2). 노드, 그것을 소유한 저장소, 원본 작업 id 를
// 돌려줍니다(0 = 현재 작업).
func (t *ToolSet) resolveDigest(id int64) (*db.Node, *db.ExplorationStore, int64) {
	if n, _ := t.ts.GetNode(id); n != nil && n.Kind == db.KindDigest {
		return n, t.ts, 0
	}
	srcs, _ := t.ts.DirectSourceStores()
	for _, s := range srcs {
		if n, _ := s.Store.GetNode(id); n != nil && n.Kind == db.KindDigest {
			return n, s.Store, s.Task.TaskID
		}
	}
	return nil, nil, 0
}

// expandDigest 는 digest 가 덮은 구성원을 짧은 목록으로 돌려줍니다(§6.1).
// node_detail 과 다른 도구입니다. 노드 하나의 전체가 아니라 구성원 목록을 돌려주기 때문입니다.
func (t *ToolSet) expandDigest() actool.CoreTool {
	return t.writeExpTool("expand_digest",
		"展开一个 cold digest：返回它折叠的成员紧凑列表（id/summary/state/confidence），与概览 recent_facts/recent_done_intents 同形状。要某条完整细节/证据用 node_detail(member_id)。", // han-allow 업스트림 프롬프트·픽스처
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "digest 节点 id（来自概览 cold_digests）"}, // han-allow 업스트림 프롬프트·픽스처
			},
			"required": []any{"id"},
		},
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var in struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(raw, &in)
			n, store, srcTaskID := t.resolveDigest(in.ID)
			if n == nil {
				return jsonResult(map[string]any{"error": fmt.Sprintf("#%d 은(는) digest 노드가 아닙니다(이 작업과 직접 연관된 작업에서 찾지 못했습니다)", in.ID)})
			}
			var p struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(n.Payload, &p)
			members, _ := store.DigestMembers(in.ID)
			list := make([]map[string]any, 0, len(members))
			for _, m := range members {
				entry := t.digestMemberEntry(store, m)
				if srcTaskID > 0 { // 연결된 작업의 구성원: 읽기 전용, 상속 표시를 답니다(§2)
					entry["inherited"] = true
					entry["source_task_id"] = srcTaskID
				}
				list = append(list, entry)
			}
			out := map[string]any{
				"id":      in.ID,
				"state":   n.State, // active / superseded. 활성인지, 더 새 digest 에 밀렸는지
				"body":    p.Body,
				"members": list,
			}
			if srcTaskID > 0 {
				out["inherited"] = true
				out["source_task_id"] = srcTaskID
			}
			return jsonResult(out)
		})
}
