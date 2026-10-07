package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
)

// DTO/직렬화 층입니다. 각 처리기는 화면 규격 모양을 그대로 내보냅니다
// (artex/web/src/lib/types.ts). db 패키지 구조체를 다시 모양 잡아,
// 내부 DB 모델이 API 밖으로 새지 않게 합니다. db 구조체와 화면이
// 기준 계약이고, 이 파일이 둘을 잇습니다.
// 초보용: 엔진과 그래프의 저장 모양을, 화면이 읽는 JSON으로 바꿉니다.

func i64s(v int64) string { return strconv.FormatInt(v, 10) }

func rfc3339(t time.Time) string { return t.Format(time.RFC3339) }

// rawString은 json.RawMessage를 문자열로 바꿉니다. 비었거나 nil이면 ""이라
// omitempty 필드가 빠집니다.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

// ---- 작업(화면의 Task) ---- created_at은 RFC3339, 상태는 계산해서 붙입니다.
type TaskDTO struct {
	ID                 string             `json:"id"`
	ExplorationID      int64              `json:"exploration_id"`
	Name               string             `json:"name"` // 선택적 작업 이름; 빈 값=이름 없음
	CategoryID         *int64             `json:"category_id,omitempty"`
	CategoryName       string             `json:"category_name,omitempty"`
	Pinned             bool               `json:"pinned"`
	PinnedAt           string             `json:"pinned_at,omitempty"`
	Description        string             `json:"description"`
	Goal               string             `json:"goal"`
	Status             string             `json:"status"` // created=만듦 | running=실행 중 | paused=일시정지 | done=끝남 | failed=실패
	CreatedAt          string             `json:"created_at"`
	CreatedUnix        int64              `json:"created_unix"`       // created_at의 유닉스 초(실행 시간을 계산할 때)
	CompletedAt        string             `json:"completed_at"`       // 끝난 시각(RFC3339, done/failed). 아직 안 끝났으면 ""
	CompletedUnix      int64              `json:"completed_unix"`     // completed_at의 유닉스 초(아직이면 0)
	LastActivity       int64              `json:"last_activity_unix"` // 마지막 활동의 유닉스 초(없으면 0)
	Paused             bool               `json:"paused"`
	Queued             bool               `json:"queued"`
	Tokens             TokenTotalDTO      `json:"tokens"` // 작업 전체의 토큰 사용량
	GoalsTotal         int                `json:"goals_total"`
	GoalsMet           int                `json:"goals_met"`
	InFlight           int                `json:"in_flight"`                // 실행 중인 워커 수(state=running 인 의도)
	Findings           FindingSeverityDTO `json:"findings"`                 // 이 작업에 등록된 발견(finding) 수(findings 표, 심각도별 구간)
	LLMProfileID       *int64             `json:"llm_profile_id,omitempty"` // 이 작업이 쓰는 LLM 설정. nil이면 기본
	LLMProfileIDs      []int64            `json:"llm_profile_ids"`
	ActiveLLMProfileID *int64             `json:"active_llm_profile_id,omitempty"`
	LLMFailoverState   string             `json:"llm_failover_state"`
	LLMFailoverReason  string             `json:"llm_failover_reason,omitempty"`
	SourceTaskIDs      []string           `json:"source_task_ids"`
	ArchiveBlockedBy   string             `json:"archive_blocked_by_task_id,omitempty"`
	CompanyIDs         []int64            `json:"company_ids"`
	CoverageEnabled    bool               `json:"coverage_enabled"` // 자산 커버리지 기능 스위치(생성 때 정해짐)
}

// FindingSeverityDTO 는 작업 목록에서 심각도별로 나눈 발견(finding) 개수다(심각/높음/중간/낮음). 엔진이 작업에 기록한 발견을 목록 UI가 이 칸으로 보여 준다.
type FindingSeverityDTO struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

func applyTaskArchiveBlocker(dto *TaskDTO, blockers map[int64]int64) {
	if dto == nil || len(blockers) == 0 {
		return
	}
	taskID, err := strconv.ParseInt(dto.ID, 10, 64)
	if err != nil {
		return
	}
	if dependentID := blockers[taskID]; dependentID > 0 {
		dto.ArchiveBlockedBy = strconv.FormatInt(dependentID, 10)
	}
}

// TokenTotalDTO는 작업 전체(모든 에이전트)의 토큰 합입니다.
type TokenTotalDTO struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

func tokenTotalDTO(u db.TokenUsage) TokenTotalDTO {
	return TokenTotalDTO{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

func taskDTO(t *Task, status string) TaskDTO {
	lifecycle := t.lifecycleSnapshot()
	llmState := t.llmStateSnapshot()
	sourceIDs := make([]string, 0, len(lifecycle.SourceTaskIDs))
	for _, id := range lifecycle.SourceTaskIDs {
		sourceIDs = append(sourceIDs, i64s(id))
	}
	profileIDs := append(make([]int64, 0, len(llmState.ProfileIDs)), llmState.ProfileIDs...)
	return TaskDTO{
		ID:                 t.ID,
		ExplorationID:      t.ExpID,
		Name:               lifecycle.Name,
		CategoryID:         lifecycle.CategoryID,
		CategoryName:       lifecycle.CategoryName,
		Pinned:             lifecycle.PinnedAt > 0,
		PinnedAt:           completedRFC(lifecycle.PinnedAt),
		Description:        t.Description,
		Goal:               t.Goal,
		Status:             status,
		CreatedAt:          rfc3339(time.Unix(t.CreatedAt, 0)),
		CreatedUnix:        t.CreatedAt,
		CompletedAt:        completedRFC(lifecycle.CompletedAt),
		CompletedUnix:      lifecycle.CompletedAt,
		Paused:             lifecycle.Paused,
		Queued:             lifecycle.Queued,
		LLMProfileID:       llmState.ProfileID,
		LLMProfileIDs:      profileIDs,
		ActiveLLMProfileID: llmState.ActiveID,
		LLMFailoverState:   llmState.FailoverState,
		LLMFailoverReason:  llmState.FailoverReason,
		SourceTaskIDs:      sourceIDs,
		CompanyIDs:         lifecycle.CompanyIDs,
		CoverageEnabled:    t.CoverageEnabled,
	}
}

// completedRFC는 유닉스 완료 시각을 RFC3339로 바꿉니다. 없으면(0) ""입니다.
func completedRFC(unix int64) string {
	if unix == 0 {
		return ""
	}
	return rfc3339(time.Unix(unix, 0))
}

// ---- 트래픽 교환(화면의 TrafficExchange) ---- ts는 RFC3339입니다.
type TrafficExchangeDTO struct {
	ID          string `json:"id"`
	TS          string `json:"ts"`
	Host        string `json:"host"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	RespLen     int    `json:"resp_len"`
}

func trafficDTOs(ex []traffic.ExchangeMeta) []TrafficExchangeDTO {
	out := make([]TrafficExchangeDTO, 0, len(ex))
	for _, e := range ex {
		out = append(out, TrafficExchangeDTO{
			ID:          e.ID,
			TS:          rfc3339(time.Unix(e.TS, 0)),
			Host:        e.Host,
			Method:      e.Method,
			URL:         e.URL,
			Status:      e.Status,
			ContentType: e.ContentType,
			RespLen:     e.RespLen,
		})
	}
	return out
}

// ---- 작업 노드(화면의 TaskNode) — 프론티어, 의도, 탐색 그래프 노드 ----

type TaskNodeDTO struct {
	ID           string `json:"id"`
	Type         string `json:"type"` // db의 Node.Kind — 노드 종류
	Payload      string `json:"payload,omitempty"`
	Priority     int    `json:"priority"`
	State        string `json:"state"`
	Origin       string `json:"origin"`
	TS           string `json:"ts"`
	SourceTaskID string `json:"source_task_id,omitempty"`
	Inherited    bool   `json:"inherited,omitempty"`
	DeleteReason string `json:"delete_reason,omitempty"` // 의도 논리 삭제(state='deleted') 때의 삭제 이유
}

func taskNodeDTO(n *db.Node) TaskNodeDTO {
	d := TaskNodeDTO{
		ID:           i64s(n.ID),
		Type:         n.Kind,
		Payload:      rawString(n.Payload),
		DeleteReason: n.DeleteReason,
		Priority:     n.Priority,
		State:        n.State,
		Origin:       n.Origin,
		TS:           rfc3339(n.CreatedAt),
	}
	if n.SourceTaskID > 0 {
		d.SourceTaskID = i64s(n.SourceTaskID)
	}
	d.Inherited = n.Inherited
	return d
}

// GoalDTO는 목표 노드입니다. payload를 text/vulnclass로 풀어 둔 모양입니다.
// 개요「목표 관리」UI 가 쓰는 값이다(원시 payload JSON 을 담는 TaskNodeDTO 와 대비).
type GoalDTO struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
	State     string `json:"state"`
	Origin    string `json:"origin,omitempty"`
	TS        string `json:"ts"`
}

func goalDTO(n *db.Node) GoalDTO {
	var p struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return GoalDTO{
		ID:        i64s(n.ID),
		Text:      p.Text,
		VulnClass: p.VulnClass,
		State:     n.State,
		Origin:    n.Origin,
		TS:        rfc3339(n.CreatedAt),
	}
}

func goalDTOs(in []*db.Node) []GoalDTO {
	out := make([]GoalDTO, 0, len(in))
	for _, n := range in {
		out = append(out, goalDTO(n))
	}
	return out
}

// ConstraintDTO 는 개요「제약 관리」UI 용 연산 제약 하나다 (allow/deny). 엔진의 가드가 이 허용·거부를 읽어 작업 실행을 가르고, 그 설정이 개요 UI에 그대로 나온다.
type ConstraintDTO struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // allow=허용 | deny=거부
	Text   string `json:"text"`
	Origin string `json:"origin,omitempty"`
	TS     string `json:"ts,omitempty"`
}

func taskNodeDTOs(in []*db.Node) []TaskNodeDTO {
	out := make([]TaskNodeDTO, 0, len(in))
	for _, n := range in {
		out = append(out, taskNodeDTO(n))
	}
	return out
}

// ---- 간선(화면의 Edge) — 탐색 간선과 자산 간선 ----

type EdgeDTO struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

func edgeDTO(e db.Edge) EdgeDTO {
	return EdgeDTO{Src: i64s(e.From), Dst: i64s(e.To), Rel: e.Rel}
}

func edgeDTOs(in []db.Edge) []EdgeDTO {
	out := make([]EdgeDTO, 0, len(in))
	for _, e := range in {
		out = append(out, edgeDTO(e))
	}
	return out
}

// ---- 커버리지가 가리키는 자산 ----

type CoverageAssetRefDTO struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Summary      string `json:"summary"`
	SourceTaskID string `json:"source_task_id,omitempty"`
	Inherited    bool   `json:"inherited,omitempty"`
}

func coverageAssetRefDTO(ref db.AssetRef) CoverageAssetRefDTO {
	out := CoverageAssetRefDTO{
		ID: ref.ID, Kind: ref.Kind, State: ref.State, Summary: ref.Summary, Inherited: ref.Inherited,
	}
	if ref.SourceTaskID > 0 {
		out.SourceTaskID = i64s(ref.SourceTaskID)
	}
	return out
}

// ---- 발견(화면의 Finding) ----

type FindingDTO struct {
	TrafficCount          int                        `json:"traffic_count"`
	EvidenceVersion       int64                      `json:"evidence_version"`
	ReportEvidenceVersion int64                      `json:"report_evidence_version"`
	ReportStale           bool                       `json:"report_stale"`
	TrafficBindings       []db.FindingTrafficBinding `json:"traffic_bindings,omitempty"`

	ID        string `json:"id"`
	FindingID string `json:"finding_id,omitempty"` // 독립 발견 표의 id — 상태를 고칠 때 쓰는 손잡이
	VulnClass string `json:"vulnclass"`
	Name      string `json:"name,omitempty"` // 발견(finding) 이름; 비어 있으면 프론트엔드는 vulnclass 를 대신 보여 준다
	Severity  string `json:"severity"`       // critical=치명 | high=높음 | medium=중간 | low=낮음
	Status    string `json:"status"`         // pending=대기 | in_progress=진행 | confirmed=확인 | resolved=해결 | fixed=수정됨 | false_positive=오탐 | ignored=무시 | duplicate=중복 | risk_accepted=위험 수용
	Summary   string `json:"summary"`
	Evidence  string `json:"evidence"`
	Report    string `json:"report,omitempty"` // 상세 보고서(Markdown); 상세 인터페이스만 반환하고, 목록에서는 비어 있다

	IntentID        string            `json:"intent_id,omitempty"`
	ParamID         string            `json:"param_id,omitempty"`
	TaskID          string            `json:"task_id,omitempty"`
	TaskDescription string            `json:"task_description,omitempty"`
	SourceTaskID    string            `json:"source_task_id,omitempty"`
	Inherited       bool              `json:"inherited,omitempty"`
	Assets          []FindingAssetDTO `json:"assets,omitempty"`
	TS              string            `json:"ts"`
}

// FindingAssetDTO는 발견이 앵커로 묶인 자산 하나입니다. 화면에 보일 이름표를 미리 붙입니다.
type FindingAssetDTO struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

// assetLabel은 짧게 보여 주려고, 자산을 가장 잘 가리키는 필드를 고릅니다.
func assetLabel(a *db.Asset) string {
	switch {
	case a.URL != "":
		if a.Method != "" {
			return a.Method + " " + a.URL
		}
		return a.URL
	case a.Domain != "":
		if a.Port != nil && *a.Port > 0 {
			return fmt.Sprintf("%s:%d", a.Domain, *a.Port)
		}
		return a.Domain
	case a.IP != "":
		if a.Port != nil && *a.Port > 0 {
			return fmt.Sprintf("%s:%d", a.IP, *a.Port)
		}
		return a.IP
	case a.AppName != "":
		return a.AppName
	case a.BundleID != "":
		return a.BundleID
	case a.ServiceName != "":
		return a.ServiceName
	case a.RootDomain != "":
		return a.RootDomain
	default:
		return "#" + i64s(a.ID)
	}
}

// findingPayload는 워커의 report_finding 도구가 쓴 JSON과 같은 모양입니다
// (agent/tools.go의 addFinding): {vulnclass, severity, summary, evidence:{by,poc}}.
type findingPayload struct {
	VulnClass string          `json:"vulnclass"`
	Name      string          `json:"name"`
	Severity  string          `json:"severity"`
	Summary   string          `json:"summary"`
	Evidence  json.RawMessage `json:"evidence"`
}

func findingDTO(n *db.Node) FindingDTO {
	var p findingPayload
	_ = json.Unmarshal(n.Payload, &p)
	d := FindingDTO{
		ID:        i64s(n.ID),
		VulnClass: p.VulnClass,
		Name:      p.Name,
		Severity:  p.Severity,
		Status:    db.FindingPending,
		Summary:   p.Summary,
		Evidence:  rawString(p.Evidence),
		TS:        rfc3339(n.CreatedAt),
	}
	if n.SourceTaskID > 0 {
		d.SourceTaskID = i64s(n.SourceTaskID)
	}
	d.Inherited = n.Inherited
	return d
}

// findingDTOsForTask는 한 작업의 발견 노드를 DTO로 바꿉니다. 각 항목에
// 소속 작업의 id/설명이라, 전역 발견(finding) 페이지가 작업을 가로질러 묶을 수 있다.
// meta는 노드 id에서 독립 발견 행(id, 상태, 자산 id)으로 가는 표입니다. 그래서
// 작업 안 보기도 전역 페이지와 같은 분류 상태와 앵커 자산을 보여 줍니다.
// 행이 없는 노드는 기본값 pending이고 finding_id가 없습니다(고칠 수 없음).
// assets는 이름표를 그리려고 앵커 자산 행을 미리 풀어 둔 것입니다.
// 초보용: 탐색 그래프의 발견 노드를, 화면의 발견 목록과 같은 모양으로 바꿉니다.
func findingDTOsForTask(t *Task, in []*db.Node, meta map[int64]db.FindingMeta, assets map[int64]*db.Asset) []FindingDTO {
	return findingDTOsForOwner(t.ID, t.Description, in, meta, assets)
}

func findingDTOsForOwner(taskID, description string, in []*db.Node, meta map[int64]db.FindingMeta, assets map[int64]*db.Asset) []FindingDTO {
	out := make([]FindingDTO, 0, len(in))
	for _, n := range in {
		d := findingDTO(n)
		d.TaskID = taskID
		d.TaskDescription = description
		if m, ok := meta[n.ID]; ok {
			d.FindingID = i64s(m.ID)
			d.Status = m.Status
			d.TrafficCount = m.TrafficCount
			d.Assets = findingAssetDTOs(m.AssetIDs, assets)
		}
		out = append(out, d)
	}
	return out
}

// findingAssetDTOs는 앵커 자산 id를 화면 DTO로 바꿉니다. 자산 행이
// 없는 id(예를 들어 삭제됨)는 건너뜁니다.
func findingAssetDTOs(ids []int64, assets map[int64]*db.Asset) []FindingAssetDTO {
	var out []FindingAssetDTO
	for _, aid := range ids {
		if a := assets[aid]; a != nil {
			out = append(out, FindingAssetDTO{ID: i64s(a.ID), Type: a.Type, Label: assetLabel(a)})
		}
	}
	return out
}

// findingFromDB는 독립 DBFinding 행을 FindingDTO로 바꿉니다. 원래 작업이
// 삭제돼 NULL이면 task_id와 task_description은 비웁니다.
func findingFromDB(f *db.DBFinding, assets map[int64]*db.Asset) FindingDTO {
	status := f.Status
	if status == "" {
		status = db.FindingPending
	}
	d := FindingDTO{
		ID:           i64s(f.ID),
		FindingID:    i64s(f.ID),
		TrafficCount: f.TrafficCount, EvidenceVersion: f.EvidenceVersion, ReportEvidenceVersion: f.ReportEvidenceVersion,
		ReportStale: f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion, TrafficBindings: f.TrafficBindings,
		VulnClass: f.VulnClass,
		Name:      f.Name,
		Severity:  f.Severity,
		Status:    status,
		Summary:   f.Summary,
		Evidence:  f.Evidence,
		Report:    f.Report,
		TS:        rfc3339(f.CreatedAt),
	}
	d.Assets = findingAssetDTOs(f.AssetIDs, assets)
	if f.TaskID != nil {
		d.TaskID = i64s(*f.TaskID)
		d.TaskDescription = f.TaskDescription
	}
	if f.NodeID != nil {
		d.IntentID = "" // node_id는 의도 노드가 아니라 발견 노드입니다. IntentID는 비워 둡니다.
	}
	return d
}

// ---- 활동(화면의 Activity) ----

type ActivityDTO struct {
	Seq          int64           `json:"seq"`                 // db의 Activity.ID
	IntentID     string          `json:"intent_id,omitempty"` // db의 NodeID
	Worker       string          `json:"worker"`
	TS           string          `json:"ts"` // db의 CreatedAt
	Kind         string          `json:"kind"`
	Tool         string          `json:"tool,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	IsError      bool            `json:"is_error"`
	Summary      string          `json:"summary"`
	Detail       string          `json:"detail,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	SourceTaskID string          `json:"source_task_id,omitempty"`
	Inherited    bool            `json:"inherited,omitempty"`
	MainSeg      *int            `json:"main_seg,omitempty"` // 메인 에이전트 대화 구간(메인 에이전트가 아닌 행은 nil)
	// 토큰 사용량(kind가 result일 때만). 세션별 토큰 합에 씁니다.
	InputTokens      *int `json:"input_tokens,omitempty"`
	OutputTokens     *int `json:"output_tokens,omitempty"`
	CacheReadTokens  *int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int `json:"cache_write_tokens,omitempty"`
}

func activityDTO(a db.Activity) ActivityDTO {
	intent := ""
	if a.NodeID != nil {
		intent = i64s(*a.NodeID)
	}
	d := ActivityDTO{
		Seq:              a.ID,
		IntentID:         intent,
		Worker:           a.Worker,
		TS:               rfc3339(a.CreatedAt),
		Kind:             a.Kind,
		Tool:             a.Tool,
		ToolUseID:        a.ToolUseID,
		IsError:          a.IsError,
		Summary:          a.Summary,
		Detail:           a.Detail, // 목록 API는 여기를 비워 둡니다(나중에 읽음).
		Metadata:         a.Metadata,
		InputTokens:      a.InputTokens,
		OutputTokens:     a.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens,
		MainSeg:          a.MainSeg,
	}
	if a.SourceTaskID > 0 {
		d.SourceTaskID = i64s(a.SourceTaskID)
	}
	d.Inherited = a.Inherited
	return d
}

func activityDTOs(in []db.Activity) []ActivityDTO {
	out := make([]ActivityDTO, 0, len(in))
	for _, a := range in {
		out = append(out, activityDTO(a))
	}
	return out
}

// ---- 에이전트(화면의 Agent) — id는 문자열 ----

type AgentDTO struct {
	ID               string `json:"id"`
	Key              string `json:"key"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Role             string `json:"role"`
	Builtin          bool   `json:"builtin"`
	Enabled          bool   `json:"enabled"`
	MaxTurns         int    `json:"max_turns"`
	RunSecs          int    `json:"run_seconds"`
	WebSearch        bool   `json:"web_search"`
	InteractiveShell bool   `json:"interactive_shell"`
	LLMProfileID     *int64 `json:"llm_profile_id"` // 묶인 LLM 설정; null=작업/전역을 따름
	// P3 트리거 후처리 전략(사용자 정의 agent 에만 의미가 있다).
	TriggerRunMode     string `json:"trigger_run_mode"`
	TriggerMergeMode   string `json:"trigger_merge_mode"`
	TriggerMaxParallel int    `json:"trigger_max_parallel"`
	// 바인딩 개수(목록 API만 채움). 에이전트 카드에 보입니다.
	McpCount   int `json:"mcp_count"`
	SkillCount int `json:"skill_count"`
	ToolCount  int `json:"tool_count"`
}

func agentDTO(a *db.Agent) AgentDTO {
	return AgentDTO{
		ID:                 i64s(a.ID),
		Key:                a.Key,
		Name:               a.Name,
		Description:        a.Description,
		Role:               a.Role,
		Builtin:            a.Builtin,
		Enabled:            a.Enabled,
		MaxTurns:           a.MaxTurns,
		RunSecs:            a.RunSecs,
		WebSearch:          a.WebSearch,
		InteractiveShell:   a.InteractiveShell,
		LLMProfileID:       a.LLMProfileID,
		TriggerRunMode:     a.TriggerRunMode,
		TriggerMergeMode:   a.TriggerMergeMode,
		TriggerMaxParallel: a.TriggerMaxParallel,
	}
}

func agentDTOs(in []*db.Agent) []AgentDTO {
	out := make([]AgentDTO, 0, len(in))
	for _, a := range in {
		out = append(out, agentDTO(a))
	}
	return out
}

// ---- LLM 설정(화면의 LLMProfile) — id는 문자열 ----

type LLMProfileDTO struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Format          string  `json:"format"`
	BaseURL         string  `json:"base_url,omitempty"`
	Proxy           string  `json:"proxy,omitempty"`
	Model           string  `json:"model"`
	APIKeyHint      string  `json:"api_key_hint,omitempty"`
	RatePerSecond   float64 `json:"rate_per_second"`
	RatePerMinute   float64 `json:"rate_per_minute"`
	ContextWindowK  int     `json:"context_window_k"`
	ThinkingType    string  `json:"thinking_type"`
	ReasoningEffort string  `json:"reasoning_effort"`
	IsDefault       bool    `json:"is_default"`
	// 폴링(장애 조치) 매개변수: priority 가 클수록 먼저 선택된다(활성 설정은 항상 체인의 맨 앞);
	// pool_exclude=true 이면 장애 조치 대상이 되지 않지만, agent/작업이 명시적으로 묶을 수는 있다.
	Priority    int  `json:"priority"`
	PoolExclude bool `json:"pool_exclude"`
	// 송수신 모드: true=스트리밍(SSE) | false=비스트리밍. omitempty 가 없다 —— false 는 반드시
	// 응답에 있어야 한다. 그렇지 않으면 프론트엔드가 「비스트리밍」을 읽지 못해, 스위치가 기본값인 스트리밍으로 돌아간다.
	Streaming bool `json:"streaming"`
	// 한 번 답변의 출력 상한(0=보내지 않으며, 서버 기본값이 정한다), 그리고 그것이 쓰는 요청 필드 이름
	// (''=max_tokens | 'max_completion_tokens', openai 형식에서만 의미가 있다).
	MaxTokens      int    `json:"max_tokens"`
	MaxTokensField string `json:"max_tokens_field"`
	// 사용자 정의 세션 헤더 이름: 비어 있지 않으면 매 요청에 그 HTTP 헤더를 실으며, 헤더 값=현재 세션/의도의 session id.
	// ''=보내지 않음. session-id 헤더로 프롬프트 캐시/스티키 라우팅을 하는 게이트웨이에 쓴다.
	SessionHeaderKey string `json:"session_header_key"`
	// 이 설정이 재시도를 덮어쓴다(연결/빈 응답/같은 provider 안전 구간). 각 항목 attempts:
	// 0=전역 전략 상속 | -1=그 층 재시도 끔 | >0=횟수; interval_ms: 0=기본 지수 백오프를 씀 |
	// >0=그 고정 밀리초 간격으로 바꾼다. 모두 0 = 전역을 완전히 따른다, 즉 예전 동작이다.
	Retry db.RetryOverride `json:"retry"`
}

func llmProfileDTO(p *db.LLMProfile) LLMProfileDTO {
	return LLMProfileDTO{
		ID:               i64s(p.ID),
		Name:             p.Name,
		Format:           p.Format,
		BaseURL:          p.BaseURL,
		Proxy:            p.Proxy,
		Model:            p.Model,
		APIKeyHint:       p.APIKeyHint,
		RatePerSecond:    p.RatePerSecond,
		RatePerMinute:    p.RatePerMinute,
		ContextWindowK:   p.ContextWindowK,
		ThinkingType:     p.ThinkingType,
		ReasoningEffort:  p.ReasoningEffort,
		IsDefault:        p.IsDefault,
		Priority:         p.Priority,
		PoolExclude:      p.PoolExclude,
		Streaming:        p.Streaming,
		MaxTokens:        p.MaxTokens,
		MaxTokensField:   p.MaxTokensField,
		SessionHeaderKey: p.SessionHeaderKey,
		Retry:            p.Retry,
	}
}

func llmProfileDTOs(in []*db.LLMProfile) []LLMProfileDTO {
	out := make([]LLMProfileDTO, 0, len(in))
	for _, p := range in {
		out = append(out, llmProfileDTO(p))
	}
	return out
}

// idStrings는 int64 id 목록을 문자열로 바꿉니다(보임 대상 에이전트 id용).
func idStrings(in []int64) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, i64s(v))
	}
	return out
}
