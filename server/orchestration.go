package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult는 v를 JSON 도구 결과로 바꿉니다.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 이 파일은 P2「작업 간 오케스트레이션 도구 모음」(docs/채점 오케스트레이션 §2 P2)을 구현한다. 초보용: 이 모음은 엔진이 자산 그래프와 탐색 그래프를 오가며 작업을 잇게 하는 host 쪽 입구이다. 이들은 host 도구라서 다음이 필요하다
// Manager(아무 작업의 Store), Engine(일시정지), 그리고 작업 생성 흐름에 접근해야 하므로 server 층에 있다.
// 읽기 도구는 「기존 per-task 도구」를 대상 작업의 store로 돌려 실행한다(임시 ToolSet을 만들고
// 해당 도구를 Call한다). 그래서 완전히 같은 로직을 재사용한다. 제어류(spawn/pause)는 Manager/Engine을 직접 호출한다.
// 트래픽 도구처럼 tools 테이블에 seed하고 agent에 바인딩한다(오케스트레이션 agent에만 묶어야 보인다).

// hostTools는 ToolAugment에 넣는 실행 중 host 도구 제공자입니다. 트래픽 도구
// (캡처가 켤 때만)와 작업 사이 오케스트레이션 도구, 사용자가 만든 도구입니다.
// 두 번째 반환은 `deferred`로 표시된 사용자 정의 도구 이름입니다(스키마는
// 감추고 SearchExtraTools/ExecuteExtraTool로 갑니다). 에이전트별 바인딩이
// 누가 실제로 보는지를 여전히 정합니다.
// 초보용: 캡처·작업 사이 도구·사용자 정의 도구를 에이전트 실행에 넣는 입구입니다.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // 플랫폼 조작 도구(skill/도구/MCP를 만들고 고친다. Auto가 쓴다). 초보용: 이 도구는 엔진과 UI가 자산 그래프·탐색 그래프에 남기는 플랫폼 설정을 고친다.
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 로드 실패: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred 사용자 정의 도구 → 이름에서 묶인 에이전트 키로. ToolAugment는
	// 실제로 묶인 에이전트에게만 deferred 항목으로 바꿉니다(바인딩이 호출 집합에서
	// 뺄 도구를 광고하지 않으려고요).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools는 작업을 가로지르는 도구 묶음입니다. tools 표로 에이전트마다 묶습니다
// (기본은 바인딩 없음. 오케스트레이션 에이전트만 골라 켬).
// 초보용: 엔진이 다른 작업의 자산 그래프와 탐색 그래프를 읽게 하는 host 도구입니다.
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- 스키마 도우미 ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID는 도구 인자에서 LLM 설정 id를 읽습니다. JSON
// 숫자(5)나 숫자 문자열("5")일 수 있습니다. 없거나 못 읽으면 0입니다.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask는 입력의 task_id를 찾고, 그 작업의 저장소에 묶인 ToolSet을 만든 뒤
// task_id를 빼고 고른 작업별 도구를 부릅니다. 그래서
// 작업 사이 읽기가 다른 작업에 대해 작업 안 로직을 그대로 재사용합니다.
// 초보용: 다른 작업의 탐색 그래프를, 그 작업 안에서 쓰는 것과 같은 도구로 읽습니다.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id는 필수입니다"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("작업이 없습니다: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 범용 깨우기(전용 콜백이 없는 쓰기 작업이 이곳으로 간다. 읽기 도구는 no-op이다)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → 「사람이 전략 힌트 N개를 추가했다: …」를 한 줄 기록해 트리거하고 planner를 깨운다
	return pick(tsx).Call(ctx, inner, nil)
}

// --- 도구 ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"모든 작업(id/설명/목표/상태/실행 시간/부모 작업/LLM 설정)를 나열합니다. 오케스트레이션 에이전트가 전체 상황을 파악하고, 너무 오래 멈춘 작업과 각 작업이 쓰는 LLM을 보는 데 씁니다. 실행 시간: 실행 중=생성→현재, 종료 상태=생성→마지막 활동(초). llm_profile: 작업의 플래너(의도만 생성)/워커(의도 하나를 실행한 뒤 정지)가 쓰는 설정 이름이며, (활성 설정)=전역 활성 설정을 따릅니다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id에서 이름으로, 각 작업이 고정한 LLM 설정을 풀 때 씁니다.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(활성 설정)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(삭제됨)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles는 쓸 수 있는 LLM 설정(이름/모델/활성)을 나열합니다. 오케스트레이션
// 에이전트가 spawn_task의 llm_profile을 고르게 합니다. 키는 절대 새지 않습니다.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"사용 가능한 LLM 설정(profile)을 나열합니다: id, 이름, 모델, 형식, 현재 활성 설정 여부. id로 spawn_task의 llm_profile_id 인자에 하위 작업 전용 LLM을 지정합니다(예: 정찰에는 저렴한 모델, 이용에는 강한 모델). API Key는 포함하지 않습니다.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"하위 작업을 새로 만들고 탐색 엔진을 시작한 뒤 task_id를 반환합니다. 한 건(예: 문제 하나/목표 하나)을 독립 작업으로 보내는 데 씁니다. parent_ref는 선택입니다. 현재 오케스트레이션에 연결된 부모 작업 id를 넣어 부모-자식으로 연결합니다.",
		objSchema(map[string]any{
			"description":            strParam("작업 설명(짧은 제목)"),
			"goal":                   strParam("작업 목표(무엇을 달성할지)"),
			"parent_ref":             strParam("선택: 부모 작업 id(부모-자식 연결)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("선택: 읽기 전용으로 상속할 출처 작업 id 목록(최대 %d개). 하위 작업은 이 작업들에서 이미 밝혀진 자산/결론을 읽기 전용으로 참조하여 시작점으로 삼을 수 있습니다. parent_ref의 순수 부모-자식 포인터와 다르며, 이것은 내용 상속입니다.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "선택: 이 하위 작업의 플래너(의도만 생성)/워커(의도 하나를 실행한 뒤 정지)가 쓸 LLM 설정 id(list_llm_profiles 참고). 비우면 부모 작업을 상속하고, 그다음 전역 활성 설정으로 돌아갑니다"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "선택: 작업 단위 타임아웃(초). 시각이 되면 우아한 마무리를 트리거하고 timeout 종료 상태로 들어갑니다. 비우거나 0이면 시간 제한 없음"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "선택: 플래너(의도만 생성) 하트비트 트리거 간격(초). 이전 계획 종료 또는 작업 시작 이후 이 값이 찼고 그 사이 트리거가 없으면 계획 한 라운드를 트리거합니다(교착 대비 + 진행 중인 워커(의도 하나를 실행한 뒤 정지)를 감독하도록 깨움). 비우거나 0이면 기본 600(10분);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "선택: 단순한 작업에서 켤 수 있습니다. 생성 시 시드 의도 하나(내용=설명+목표)를 바로 내려, 워커(의도 하나를 실행한 뒤 정지)가 첫 플래너(의도만 생성) 라운드를 기다리지 않고 테스트를 시작하게 합니다. 기본값 false(표준은 먼저 계획한 뒤 실행)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "이름 없는 작업"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal은 필수입니다"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 상속 원본 작업은 읽기 전용이다: 개수 상한 + 각 id의 유효/중복 제거/존재. 검증 규칙은 HTTP 작업 생성과 같다.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("연관 작업은 최대 %d개까지 선택할 수 있습니다", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("연관 작업 id가 유효하지 않거나 중복됨"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("연관 작업 #%d이(가) 없습니다", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM 설정 고르기: 명시적 id > 부모의 고정 상속 > 활성(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 설정 #%d이(가) 없거나 API Key가 설정되지 않음", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 공유하는 생성 후 흐름. HTTP 작업 생성(server.go createTask)과 같은 launchTask 구간을 재사용한다:
			// seed + 백그라운드에서 보이게 목표를 분해한다(0라운드/LLM 단계/goal 한 줄씩) + engine.Run.
			// seed_first_intent 기본값은 false이다(표준은 먼저 계획한 뒤 실행). 단순한 작업은 켜서 work 하나를 바로 내려 테스트할 수 있다.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "지정한 작업을 일시정지합니다(해당 플래너(의도만 생성)/워커(의도 하나를 실행한 뒤 정지) 루프를 멈춤).",
		objSchema(map[string]any{"task_id": strParam("일시정지할 작업 id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("작업이 없습니다: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "지정한 작업의 탐색 그래프 개요를 읽습니다(graph_overview와 같음: 자산 수/프론티어/발견(finding)/커버리지 등). task_id로 작업을 지정합니다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "지정한 작업의 확인된 발견(finding)을 읽습니다(flag/PoC 포함. 각 항목에 id/task_id/intent_id/vulnclass/severity/요약/상태). task_id로 작업을 지정합니다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "지정한 작업에 전략 힌트를 주입합니다(해당 작업의 플래너(의도만 생성)가 다음 라운드에서 의도를 생성할 때 읽습니다).\n"+
		"★일괄을 우선: 여러 힌트를 hints 배열에 넣어 한 번에 제출합니다(ids 배열을 반환하며 hints와 길이와 순서가 같고, 실패 항목은 id=0). 한 건이면 hints를 생략하고 최상위 text를 직접 줍니다.",
		objSchema(map[string]any{
			"task_id":      strParam("작업 id"),
			"hints":        map[string]any{"type": "array", "description": "【이것을 우선 사용】힌트 배열. 각 요소의 필드는 최상위와 같습니다(text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[단건] 힌트 내용"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "앵커로 묶은 자산 id(선택, 0/1/여러 개. 해당 작업 안의 자산 id)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"지정한 작업에서 어떤 work(의도)의 실행 과정을 봅니다. get_task_worker_trace(task_id, intent_id)로 단계 요약을 보고, step_ids=[...]를 함께 주면 그 단계들의 전체 내용을 가져옵니다(한 번에 최대 5개, 더 넘기면 앞의 5개만 반환).",
		objSchema(map[string]any{
			"task_id":   strParam("작업 id"),
			"intent_id": map[string]any{"type": "integer", "description": "의도 id(해당 작업의 work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "선택: 전체 내용을 가져올 단계 id(한 번에 최대 5개, 더 넘기면 앞의 5개만 반환하고 나머지는 omitted_step_ids에 나열)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "지정한 작업에서 어떤 work(의도)가 돌았는지와 각 단계 수를 나열합니다. 어떤 work를 살펴볼지 찾는 데 쓰며, 이어서 get_task_worker_trace를 사용합니다.",
		objSchema(map[string]any{"task_id": strParam("작업 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "지정한 작업에서 키워드로 모든 work의 실행 과정을 검색합니다(일치한 단계 요약 + intent_id를 반환).",
		objSchema(map[string]any{"task_id": strParam("작업 id"), "q": strParam("검색 키워드")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"지정한 작업에서 탐색 그래프 노드 하나의 전체 내용을 읽습니다(발견(finding)/사실/의도/목표: 요약 + 상세/증거/PoC). id는 탐색 노드 id입니다(예: report_finding이 반환한 값, 또는 list_task_findings의 id). 발견(finding) 보고서를 쓰기 전에 이것으로 해당 발견(finding)의 전체 증거를 가져옵니다.",
		objSchema(map[string]any{
			"task_id": strParam("작업 id"),
			"id":      map[string]any{"type": "integer", "description": "탐색 그래프 노드 id(자산 id가 아님)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport는 발견의 상세 Markdown 보고서를 쓰거나 덮어씁니다.
// finding_id는 report_finding이 돌려준 id입니다("finding recorded: <id>",
// 발견 노드 id). 쓰기(SetFindingReportByNodeID)는 node_id로 하고
// 작업과 무관해서, 이 host 도구는 task_id나 탐색 저장소가 필요 없습니다.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"이미 등록된 발견(finding)에 【상세 보고서】를 쓰거나 갱신합니다(Markdown 전문, 통째로 이전 내용을 덮어씀). finding_id에는 report_finding이 반환한 id를 넘깁니다(\"finding recorded: <id>\" 안의 숫자). 보고서에는 발견(finding) 개요, 영향과 피해, 재현 단계, 증거/PoC, 수정 제안을 포함하는 것을 권합니다.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "대상 발견(finding) id(report_finding이 반환한 id)"},
			"report":           strParam("상세 보고서 전문, Markdown 형식"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic이 반환한 증거 version. 보고서가 더 새로운 증거 변경을 덮어쓰지 못하게 합니다"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「숫자 또는 숫자 문자열」 파싱을 재사용한다
			if nodeID <= 0 {
				return actool.Errorf("finding_id가 유효하지 않습니다"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d에 해당하는 발견(finding) 기록을 찾지 못했습니다(먼저 report_finding으로 등록하세요)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus는 list_tasks 도구를 위해 listTasks의 상태 계산을 그대로 씁니다.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds는 작업 사이 도구를 tools 표에 심어,
// 에이전트마다 묶을 수 있게 합니다(기본은 아무에게도 안 묶임. 오케스트레이션
// 에이전트만 골라 켬). 트래픽 씨앗처럼 처음 한 번만 넣습니다.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (이 agent는 원래
	// 플랫폼을 조작하는 용도이다). SeedTool은 첫 삽입만 적용된다. 이미 seed된 옛 라이브러리 행은 seedAutoDefaultBindings가 바인딩을 보완한다.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces를 worker 기본 바인딩에서 푼다(한 번만)
	s.seedWorkerReadbackRebind()  // 옛 마이그레이션이 잘못 지운 것을 고친다: search_all_worker_traces/get_worker_trace/node_detail을 worker에 다시 묶는다(한 번만)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals 프롬프트에 「조작 제약 추출」 단계를 넣는다 → 옛 라이브러리에 새 기본 버전을 하나 추가한다(한 번만)
	s.reseedMainAgentPrompt()         // mainagent 프롬프트에 「목표 달성 후 add_intent로 목표를 만들지 되묻기」를 넣는다(한 번만)
	s.reseedPlannerPrompt()           // planner 프롬프트: 「의도 0개」의 정당한 이유를 다시 쓰고 정량 인수 대조를 더한다(한 번만)
	s.reseedWorkerPrompt()            // worker 프롬프트: 부정 결론의 증거 문턱을 더한다(한 번만)
	s.seedReporterAgent()             // 「보고서 작성」 agent + 도구 바인딩 + finding 트리거를 미리 넣는다(한 번만)
	s.upgradeReporterTriggerMessage() // 옛 라이브러리 보완 마이그레이션: reporter가 evidence_version을 돌려주게 한다(한 번만)
	s.seedFindingTrafficTools()       // 선택 증거 파라미터와 읽기 전용 증거 도구를 더하고, 사용자 설정은 유지한다
	s.seedFindingWorkflowTools()
	// 주: pentest의 기본 도구 바인딩은 마이그레이션이 필요 없다——BuiltinToolSeeds가 완전 새 초기화 때 이미
	// list_assets/insert_assets/report_finding/list_findings/list_companies를
	// pentest와 함께 seed해 두었다(프로젝트에 아직 옛 라이브러리가 없어 마이그레이션하지 않는다).
}

// refreshBuiltinToolSchemas는 코드의 스키마/설명 변경을
// 이미 심긴 오케스트레이션·플랫폼 도구 행에, 버전 플래그마다 한 번 반영합니다.
// SeedTool is first-insert-only, so a new param (e.g. spawn_task의 llm_profile) never
// 옛 DB에는 반영되지 않습니다. 각 도구의 에이전트 바인딩과 켜짐 표시는 그대로 둡니다.
// 코드에서 이 도구들의 스키마나 설명이 바뀌면 이 플래그를 올립니다.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 동시에 일부 내장 agent 도구를 코드 기본값으로 다시 쓴다:
	//   - goal_met: 옛 라이브러리에 seed된 설명에 「이번 라운드 계획 종료」라는 오해가 있어, planner가 이것을
	//     「빈 라운드를 끝내는」 수단으로 여기고, 막 시작하자마자 작업 전체가 끝났다고 잘못 판단하게 한다.
	//   - insert_assets: related 인자를 새로 넣는다(자산이 현재 작업과 관련 있는지 표시하고, 커버리지에 넣을지 정한다),
	//     SeedTool은 첫 삽입 only라서, 그렇지 않으면 옛 라이브러리에 이미 seed된 schema는 이 새 파라미터를 받지 못한다.
	//   - list_facts: 페이지로 바꾸고 limit/before/q 인자를 새로 넣는다. 그렇지 않으면 옛 라이브러리에 이미 seed된 빈 schema는
	//     도구 관리 페이지에 「파라미터 없음」으로 보이고, 모델도 이 파라미터 설명을 받지 못한다.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform 도구 schema를 코드 기본값으로 새로고침했습니다(일회성)")
}

// unbindGoalMetDefault는 goal_met의 기본 "planner" 바인딩을 한 번 뗍니다
// (설정 플래그로 지킴). 기존 DB도 새 기본(에이전트 없음)과 같게 합니다. goal_met는
// 목표별 prove_goal을 건너뛰고 작업 전체를 끝났다고 선언합니다. 세고 위험하며
// prove_goal에서 자동 완료로 가는 길과 겹칩니다. 그래서 안 묶인 채 나갑니다. 사용자는 에이전트마다
// 화면에서 다시 묶을 수 있습니다. 다른 에이전트에 사용자가 묶은 것은 안 건드립니다(planner만 뗍니다).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met의 플래너(의도만 생성) 바인딩 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt는 goals 목표 분해기의 프롬프트를 【현재 코드 기본】으로 다시 쓴다——기본 본문에 다음이 새로 들어갔기 때문이다
// 「먼저 조작 제약을 뽑고(set_constraints) 그다음 목표를 쪼갠다」는 단계. SeedPromptIfEmpty는 첫 삽입 only라서 옛 라이브러리의
// 기존 version 1은 이 단계를 받지 못한다. 여기서는 버전 관리로 【새 버전을 하나 추가】하고 그쪽으로 전환한다(ResetPromptToDefault),
// 옛 버전은 히스토리에 남는다. 사용자가 커스터마이즈했다면 버전 기록에서 되찾을 수 있다. settings flag가 지킨다 → 한 번만 한다;
// 나중에 기본이 다시 바뀌면 이 flag를 bump한다. 완전 새 라이브러리는 처리할 필요 없다(SeedPromptIfEmpty가 이미 최신 기본을 seed했다).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공이든 실패든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 완전 새 라이브러리에 아직 agent 행이 없으면 seedPrompts가 최신 기본을 바로 seed하므로 이 마이그레이션은 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 완전 새 라이브러리는 seedPrompts가 최신 기본을 이미 seed했다 → 현재 버전이 이미 코드 기본과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals 프롬프트를 새 기본값으로 다시 덮어쓰지 못했습니다: %v", err)
		return
	}
	log.Printf("[prompts] goals 프롬프트에 새 기본 버전을 추가했습니다(조작을 뽑아내는 제약 단계를 넣음, 일회성)")
}

// reseedMainAgentPrompt는 mainagent 프롬프트를 【현재 코드 기본】으로 다시 쓴다——기본 본문에 「목표가 모두
// 달성된 뒤 add_intent로 의도를 바로 넣을 때, 사람에게 정식 목표로 등록할지 되묻는다」는 안내가 새로 들어갔다. SeedPromptIfEmpty는 첫 삽입
// only라서 옛 라이브러리의 기존 버전은 받지 못한다. 버전 관리로 【새 버전을 하나 추가】하고 그쪽으로 전환한다(ResetPromptToDefault). 옛 버전은 여전히
// 히스토리에 남는다. 사용자가 커스터마이즈했다면 버전 기록에서 되찾을 수 있다. settings flag가 지킨다 → 한 번만 한다. 완전 새 라이브러리는 처리할 필요 없다
// (SeedPromptIfEmpty가 이미 최신 기본을 seed했다). reseedGoalsPrompt와 완전히 같은 구조이다.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공이든 실패든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 완전 새 라이브러리에 아직 agent 행이 없으면 seedPrompts가 최신 기본을 바로 seed하므로 이 마이그레이션은 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 완전 새 라이브러리는 seedPrompts가 최신 기본을 이미 seed했다 → 현재 버전이 이미 코드 기본과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent 프롬프트를 새 기본값으로 다시 덮어쓰지 못했습니다: %v", err)
		return
	}
	log.Printf("[prompts] mainagent 프롬프트에 새 기본 버전을 추가했습니다(목표 달성 후 되물어 목표를 세우는 내용을 넣음, 일회성)")
}

// reseedPlannerPrompt는 플래너(의도만 생성) 프롬프트를 【현재 코드 기본값】으로 다시 기록한다——기본 본문을 간결하게 재구성했고, 「절제」를 한 단계 낮춰
// 한 일은 중복 제거뿐이고, 「깊이가 커버리지보다 우선」「하드 하한선: 목표가 아직 달성되지 않았고 실행 중인 의도가 없으면 반드시 산출해야 한다」를 새로 넣었으며, 부정 결론 재검토에 상한을 두었다.
// 기본값에 실질 변경이 있을 때마다 아래 flag를 bump(현재 v2)해서 기존 옛 데이터베이스를 한 번 더 다시 기록한다. SeedPromptIfEmpty는 최초 삽입 only라, 옛 데이터베이스에 이미 버전이 있으면 받지 못하므로 버전 관리로
// 【새 버전을 하나 추가】하고 그쪽으로 전환한다(ResetPromptToDefault). 옛 버전은 이력에 남으며, 사용자가 커스터마이즈했다면 버전 기록에서
// 되찾을 수 있다. settings flag 가드(guard) → 한 번만 한다. 완전 새 데이터베이스는 처리할 필요 없다(SeedPromptIfEmpty가 이미 최신 기본값을 seed했다). 그리고
// reseedGoalsPrompt와 완전히 같은 구조다.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공이든 실패든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 완전 새 라이브러리에 아직 agent 행이 없으면 seedPrompts가 최신 기본을 바로 seed하므로 이 마이그레이션은 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 완전 새 라이브러리는 seedPrompts가 최신 기본을 이미 seed했다 → 현재 버전이 이미 코드 기본과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner 프롬프트를 새 기본값으로 다시 덮어쓰지 못했습니다: %v", err)
		return
	}
	log.Printf("[prompts] planner 프롬프트에 새 기본 버전을 추가했습니다(간결한 재구성+절제한 강등 중복 제거+깊이 우선+부정 재검토 상한, 일회성)")
}

// reseedWorkerPrompt는 워커(의도 하나를 실행한 뒤 정지) 프롬프트를 【현재 코드 기본값】으로 다시 기록한다——기본 본문의 record_fact 단락에서 「부정 계열 결론에
// 관찰을 쓰고 잠정적으로 읽는 법」 문장 전체를 뺐고, confidence(observed/inferred)를 「이 의도의 수단을 다 썼는지」와 떼어 놓았다(이것들은 플래너(의도만 생성)를 쉽게 오도한다),
// 동시에 facts 배열의 항목을 「서로 완전히 독립이고 합칠 수 없는」 극소수 예외로 조였다. flag를 v3까지 bump해서 기존 옛 데이터베이스를 한 번 더 다시 기록한다.
// SeedPromptIfEmpty는 최초 삽입 only라 옛 데이터베이스에 이미 버전이 있으면 받지 못한다. 그래서 버전 관리로 【새 버전을 하나 추가】하고 그쪽으로 전환하며, 옛 버전은 이력에 남겨 되찾을 수 있다.
// settings flag 가드(guard) → 한 번만 한다. 완전 새 데이터베이스는 처리할 필요 없다. reseedGoalsPrompt와 완전히 같은 구조다.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공이든 실패든 한 번만 시도한다
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 완전 새 라이브러리에 아직 agent 행이 없으면 seedPrompts가 최신 기본을 바로 seed하므로 이 마이그레이션은 필요 없다
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 완전 새 라이브러리는 seedPrompts가 최신 기본을 이미 seed했다 → 현재 버전이 이미 코드 기본과 같으므로 중복 버전을 또 추가하지 않는다.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker 프롬프트를 새 기본값으로 다시 덮어쓰지 못했습니다: %v", err)
		return
	}
	log.Printf("[prompts] worker 프롬프트에 새 기본 버전을 추가했습니다(컨텍스트 조회 구간을 list_assets/list_findings로 수렴하고 list_facts/node_detail/asset_neighbors를 제거, 일회성)")
}

// reporterToolCallMessage는 무조건 먼저 get_finding_traffic을 한 번 읽은 뒤에 보고서를 쓰라고 요구해야 한다.
// 이 도구는 읽기 전용이고 「캡처 스위치에 의존하지 않으며」, 자동 바인딩이 켜져 있든 꺼져 있든 사람이 묶은 증거를 읽을 수 있다. 여기서
// 「자동 바인딩을 켰을 때만 읽는다」라고 쓰면, 기본이 꺼진 설정에서는 reporter가 evidence_version을 넘기지 않고,
// SetFindingReportVersionByNodeID는 legacy 의미로 -1을 쓰게 되며, 발견(finding) 상세와 Markdown 내보내기는
// 그때부터 「증거가 변경됨, 보고서 업데이트 대기」가 계속 남고, UI에는 그것을 지울 입구가 없다.
const reporterToolCallMessage = "방금 위쪽에서 발견(finding) 하나가 report_finding으로 등록되었습니다. 반환 JSON의 finding_id(독립 발견(finding) 기록 ID)와 finding_node_id(탐색 노드 ID)를 읽으세요." +
	"먼저 get_finding_traffic(finding_id)로 현재 증거 목록과 그 version을 읽습니다(빈 목록은 정상입니다. 그대로 보고서를 작성하세요)." +
	"실행 안내에서 자동 연결이 켜져 있으면, 읽기 전에 이번 발견(finding)의 트래픽을 확인하고 연결합니다. 노드 상세는 finding_node_id를 사용합니다." +
	"마지막으로 update_finding_report(finding_id=finding_node_id, report, evidence_version=실제로 읽은 버전)를 호출해 저장하고," +
	"evidence_version은 반드시 넘겨야 합니다. 그렇지 않으면 보고서가 영구히 업데이트 대기로 표시됩니다. 두 종류의 번호를 섞지 마세요."

// 구버전 트리거 메시지(0.3.8 및 그 이전). 그것과 글자 그대로 같은 기록만 마이그레이션으로 덮어쓰고, 사용자가 고친 것은 그대로 둔다.
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" /* han-allow 프로토콜 원문 */ +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" /* han-allow 프로토콜 원문 */ +
	"最后调用 update_finding_report(finding_id, report) 保存。" // han-allow 프로토콜 원문

// upgradeReporterTriggerMessage는 옛 데이터베이스에서 아직 기본 문구인 reporter 트리거 메시지를 새 버전으로 다시 기록한다.
// seedReporterAgent는 reporter_agent_seed_v1 가드(guard)를 받고 새 agent를 만들 때만 트리거를 쓰므로,
// 업그레이드된 데이터베이스는 새 문구를 받지 못한다 —— 도구 schema는 seedFindingTrafficTools가
// evidence_version을 채워 넣었지만, reporter에게 그것을 쓰라고 알려 주는 것은 없다. 한 번만 하고, 바뀌지 않은 문구만 덮어쓴다.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 한 번만 시도한다
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] 트리거 읽기 실패: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // 사용자가 고쳤거나 finding 트리거가 아니면 건드리지 않는다.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 메시지 업그레이드 실패: %v", err)
			return
		}
		log.Printf("[reporter] 트리거 메시지를 읽어 evidence_version을 돌려주는 형태로 업그레이드했습니다")
	}
}

// seedReporterAgent는 「보고서 작성」 사용자 정의 agent를 미리 둔다(builtin=false, UI에서 편집/삭제 가능). 이 agent는 엔진이 기록한 발견(finding)을 UI 보고서로 풀어 준다:
// update_finding_report + 작업 조회 도구를 묶고, 「report_finding이 호출되면 바로 트리거」되는
// 트리거 —— 발견(finding)을 하나 등록할 때마다 그것을 깨워 상세 보고서를 쓰게 한다. 한 번만(settings flag 가드(guard)): 사용자가 지우면 다시 만들지 않는다.
// 의존: orchestration 도구는 이 함수 위쪽에서 이미 SeedTool로 들어가 있으므로 바인딩이 된다.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공이든 실패든 한 번만 시도한다

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key가 이미 쓰이고 있다(사용자가 직접 만듦)——덮어쓰지 않는다
	}
	a, err := s.m.pg.CreateAgent("reporter", "보고서 작성",
		"발견(finding) 상세 보고서 작성: 발견(finding)이 확인되면 자동으로 트리거되어, 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 작성하고 다시 기록합니다. 이 기록은 탐색 그래프의 발견(finding)을 엔진이 UI에 보여 주기 위한 결과입니다.")
	if err != nil {
		log.Printf("[reporter] 에이전트 생성 실패: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt 실패: %v", err)
	}
	// 트리거 실행 정책: parallel + none —— 발견(finding) 하나당 보고서 하나, 여러 finding이 동시에 각자 쓴다.
	// merge는 반드시 none이어야 한다: 그렇지 않으면(기본 all) 한 무더기 finding이 한 번의 실행으로 합쳐져 병렬이 의미가 없다.
	// maxParallel=5: 동시에 보고서 세션은 최대 5개라, 순간적으로 LLM 호출이 너무 많아지지 않게 한다.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 트리거 실행 정책 설정 실패: %v", err)
	}
	// 필요한 도구를 묶는다: 보고서 쓰기 + 증거/실행 과정/상황 읽기.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] 도구 바인딩 실패: %v", err)
	}
	// 트리거: report_finding이 호출되면 바로 트리거된다(도구는 "finding recorded: <id>"를 반환하며 finding_id를 싣고,
	// 작업 id도 트리거 메시지 안에 있다).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] 트리거 생성 실패: %v", err)
	}
	log.Printf("[reporter] 「보고서 작성」 에이전트 + finding 트리거를 미리 구성했습니다")
}

// seedAutoReportFindingBinding은 report_finding 바인딩에 "auto"를 한 번 더합니다.
// 대화 맥락 에이전트가 intent_id 없이도 부를 수 있게 합니다.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings는 report_finding 바인딩에 "planner"를 한 번 더합니다
// (설정 플래그로 지킴). 기존 DB의 report_finding 행이
// 워커 전용으로 심겼어도, 플래너가 발견을 기록하게 합니다. 새 DB는 이미
// PlannerTools()로 받습니다. 이것은 사용자가 푼 것을 덮지 않고 빈칸만 채웁니다.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding은 list_assets 바인딩에 "planner"를 한 번 더합니다
// (설정 플래그로 지킴). 기존 DB의 list_assets 행이
// auto/pentest 전용으로 심겼어도, 플래너가 DSL로 자산 저장소를 조회하게 합니다. 새
// DB는 이미 PlannerTools()로 받습니다. 이것은 사용자가 푼 것을 덮지 않고
// 빈칸만 채웁니다.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind는 기존 DB에서 add_company_scope의 기본 바인딩을 한 번 바꿉니다
// (설정 플래그로 지킴). 도구를 워커에서 떼어
// 플래너에 붙입니다. 기업의 자산 범위를 정하는 일은 계획/메인/auto의 일이고,
// 워커가 탐색 중에 할 일이 아닙니다. 새 DB는 이미 PlannerTools()로 플래너를 받고
// WorkerTools()에는 이 도구의 워커 바인딩이 없습니다. 이것은 옛 행만 채웁니다.
// 한 번만, 플래그로 지켜, 나중에 사용자가 워커를 다시 묶어도 덮지 않습니다.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // 워커(의도 하나를 실행한 뒤 정지)→플래너(의도만 생성) 기본 바인딩 전환
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope 기본 바인딩 실패: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope 바인딩 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind는 기존 DB에서 읽기 맥락 도구를 워커 기본
// 바인딩에서 한 번 뗍니다(설정 플래그로 지킴). 워커는 의도 하나를 실행하고
// 결과를 씁니다. 사실/기업을 읽고 모든 워커의 흔적을 나열하는 것은
// 계획/메인의 일이고, 실행자의 일이 아닙니다. 새 DB는 이미 WorkerTools()에 이것들이 없습니다.
// 이것은 옛 행만 채우고, 워커를 일부러 다시 묶은 사용자는
// 덮지 않습니다. RemoveAgentFromTool은 도구마다,
// 소속만 확인하므로, 같은 도구의 플래너/메인 에이전트 바인딩은 안 건드립니다.
//
// 참고: search_all_worker_traces / get_worker_trace / node_detail은 일부러 안
// 뗍니다. 워커가 작업 사이 되돌아보기와 노드 드릴다운에 씁니다(WorkerTools를 보세요).
// 예전에는 워커에게 없어서 이 목록에 있었습니다. seedWorkerReadbackRebind가
// 옛 실행이 벗겨 버린 DB를 고칩니다.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s을(를) 워커에서 바인딩 해제하지 못했습니다: %v", k, err)
			return // 오류가 나면 flag를 쓰지 않고, 다음 기동 때 다시 시도한다
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind는 작업 사이 되돌아보기/드릴다운 도구를
// 워커에 한 번 다시 묶습니다(설정 플래그로 지킴). 예전의 seedWorkerReadToolsUnbind가
// search_all_worker_traces / get_worker_trace / node_detail을, WorkerTools()에 넣은 뒤에
// 워커에서 잘못 떼었습니다. 그 이관을 탄 DB는 이것을 잃었습니다.
// 새 DB는 이미 WorkerTools()로 가지고 있어, 여기서는 해가 없는 무동작입니다.
// 한 번만, 플래그로 지켜, 나중에 사용자가 일부러 풀어도 덮지 않습니다.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: node_detail을 추가
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 되돌아보기/상세 도구 추가 바인딩 실패: %v", err)
		return // 오류가 나면 flag를 쓰지 않고, 다음 기동 때 다시 시도한다
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings는 작업 조작·플랫폼 도구 바인딩에 "auto"를
// 한 번 더합니다(설정 플래그로 지킴). Auto가 생기기 전에 심긴 도구 행이 있는 기존 DB도
// Auto의 기본 도구 묶음을 갖게 합니다. 사용자가 일부러 푼 뒤에
// 다시 넣지는 않습니다.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 옛 자산 도구 이름을 바꾸고, insert_assets/add_company_scope를 넣는다
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 자산 도구: Auto 조작 플랫폼은 자주 자산을 보거나 등록하고, 회사 범위를 관리한다. 등록된 자산은 PostgreSQL 자산 그래프에 들어가고, 어디를 더 볼지의 탐색 그래프와는 별개다.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
