// 화면과 엔진이 주고받는 데이터 모양입니다. 자산 그래프와 탐색 그래프의 노드가 이 타입으로 표현됩니다.
// 화면 곳곳에서 쓰는 도메인 타입입니다.
// 기능 명세에서 가져옴(7절: 핵심 데이터 모양).

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle";

export interface Task {
  id: string;
  name?: string; // 선택 작업 이름. 비움/없음=이름 없음. 보여 줄 때는 설명으로 되돌림
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at의 유닉스 초(실행 시간 계산)
  completed_at?: string; // RFC3339 끝난 시각(완료/실패). 아직이면 ""
  completed_unix?: number; // completed_at의 유닉스 초(아직이면 0 또는 없음)
  last_activity_unix?: number; // 마지막 활동의 유닉스 초(없으면 0 또는 없음)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 등록된 발견 수(심각도별)
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // 작업 전체 토큰 사용량
  llm_profile_id?: number; // 쓴 LLM 프로필. 없으면 기본 프로필
  llm_profile_ids?: number[]; // 작업 단위 장애 전환 사슬(순서 있음)
  active_llm_profile_id?: number; // 다음 LLM 호출이 쓸 프로필
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // 읽기 전용 맥락으로 물려받은, 직접 관련된 작업
  archive_blocked_by_task_id?: string; // 먼저 보관해야 하는, 아직 살아있는 직접 의존 작업
  company_ids?: number[]; // 연결된 기업 범위. 만들 때 그 기업의 현재 자산이 작업에 들어옵니다
  coverage_enabled?: boolean; // 자산 커버리지 기능 스위치(만들 때 정함, 기본 켬). false=커버리지를 계산/표시하지 않음
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // 미리 둔 분류. null/없음=없음
  intercept_rules?: AssetInterceptRuleInput[]; // 미리 둔 작업 단위 가로채기/허용 규칙
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- 자산 그래프(전역, 작업들이 공유) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey 키
  value?: string;
  company_id?: string; // 소속 회사 자산 id. 비어 있음=소속 없음
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// 작업 자산 보기. 서버가 보강해 페이지로 나눕니다.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- 새 통합 자산 모델(새 백엔드) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- 자산 커버리지 그림(작업마다) ----
// 힘 기반 「자산 커버리지 그림」의 한 노드. key는 유일합니다. 자산="a:<id>", 회사="c:<id>", （자산 그래프는 자산이 서로 어떻게 연결되는지 보여주는 그림입니다）
// 자산 행이 없는 루트 도메인="r:<domain>". in_scope=false는 선을 잇기 위한 회색 맥락 노드입니다.
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// 어떤 자산이 이 작업 탐색 그림에서 연결된 의도/사실/발견(커버리지 그림 노드 서랍용). （탐색 그래프는 작업이 어디까지 이어졌는지 보여주는 그림입니다）
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- 작업 폴더 파일 관리(workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // 작업 폴더 기준 경로, 슬래시는 /
  dir: boolean;
  size: number;
  mtime: number; // 유닉스 밀리초
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// 작업 테스트 범위의 한 줄(커버리지 분모 + 허용된 경계).
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // 백엔드가 JOIN companies로 해석. kind=company일 때만 값이 있음
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 기업을 추가할 때 보내는 구조화된 자산 범위 규칙.
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 자산 범위 쓰기의 결과. errors는 이번 제출에서 맞지 않은 행입니다. warnings는 이번 제출과 무관하고,
// 그러나 소속 결과가 기대와 다른, 이미 있는 데이터 문제를 만듭니다(예: ip 필드에 호스트 이름이 저장된 자산).
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 회사 자산 범위 규칙의 한 줄(소속의 유일한 참값 출처).
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain일 때 값이 있음
  net?: string; // kind=ip|cidr일 때 값이 있음
  value?: string; // kind=icp|keyword일 때 백엔드가 직접 돌려줄 수 있음
  raw: string; // 원래 사용자 입력. 표시와 다시 채우기에 씀
  reason?: string;
}

// 기업: type=company인 자산 노드 + 아이콘 + 자산 개수 + 자산 범위 규칙.
export interface Company {
  id: number;
  name: string;
  logo?: string; // 원격 아이콘 URL. 비어 있으면 프론트는 이름 첫 글자를 씀
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- 탐색 그래프(작업마다) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState 중 하나
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 의도를 표시만 삭제(state='deleted')했을 때의 삭제 이유
}

// 진행판 한 페이지: 만든 순서로 나눈 노드 + 이 페이지에 관련된 간선 + 간선 반대쪽 노드(refs, id로 색인),
// 이렇게 하면 진행 소식마다 「어디서 왔고, 무엇을 냈는지」를 말할 수 있어, 그림 전체를 끌어오지 않아도 됩니다.
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // 노드 id → 그 노드에 묶인 자산(진행판을 펼칠 때 함께 보여 줌. 이 페이지 노드와 이웃 포함). （앵커는 노드에 묶여 있는 자산입니다）
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 목표 관리 카드가 쓰는 목표(백엔드가 이미 payload를 text/vulnclass로 나눔).
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState 값
  origin?: string;
  ts: string;
}

// 제약 관리 카드가 쓰는 동작 제약(allow=허용 / deny=금지).
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- 발견 ----
export type Severity = "critical" | "high" | "medium" | "low";

// 발견 처리 상태: 처리 대기 / 처리 중 / 확인됨 / 처리됨 / 고침 / 오탐 / 무시 / 중복 / 위험 수용.
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset은 한 발견에 연결된 자산입니다(백엔드에서 label을 미리 그림).
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 독립 findings 표의 행 id. 상태 업데이트의 손잡이(작업 안의 예전 노드는 없을 수 있음)
  vulnclass: string;
  name?: string; // 발견 이름. 비어 있으면 표시는 vulnclass로 되돌림
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 자세한 보고서(Markdown). 상세 API만 돌려주고, 목록은 비어 있음
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage는 발견 목록의 서버 페이지 응답입니다.
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 선택 작업 이름. 비움/없음=이름 없음
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats는 발견 전체 표의 집계입니다(통계 카드 + 발견 유형 드롭다운). 서버에서 계산하며 페이지에 영향받지 않습니다.
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption은 발견 페이지 「작업별」 필터 드롭다운의 한 항목입니다. 발견이 있는 작업(설명이 비면 작업이 삭제됨,
// 프론트 대비로 id를 보여 줌)과 그 발견 건수.
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 선택 작업 이름. 비움/없음=이름 없음
  description: string;
  count: number;
}

// FindingQuery는 발견 목록의 페이지/필터/정렬 매개변수입니다.
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // 작업 id. "all"/비어 있음 = 작업으로 거르지 않음
  query?: string;
  sort?: "severity" | "time";
  // 자산 나무 노드 key. 노드 하나를 고르면 = 그 하위 나무 전체를 고름. 비어 있음 = 자산으로 거르지 않음.
  assetScope?: string;
}

// ---- Findings by asset (자산 보기) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode는 자산 나무의 한 노드입니다. key 형태는 a:<id>(자산), c:<id>(기업),
// r:<domain>(저장소에 자산 행이 없는 루트 도메인), __none__(자산에 연결 안 됨).
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // 그 자산에 직접 달린 발견 수
  total: number; // 자손을 포함하고, 발견 기준으로 중복 제거
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET은 백엔드 db.FindingUnassignedAsset에 대응합니다.
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- 활동 / 세션 ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // 워커 세션을 이끄는, 모델이 만든 탐색 목표(화면이 만듦)
  | "round" // 플래너 라운드 경계 표시(엔진이 냄)
  | "usage" // 실시간 누적 토큰 사용량(모델 차례마다). 화면에 그리지 않음
  | "llm_switch" // 자동/수동 작업 단위 LLM 전환
  | "llm_failover" // 작업 단위 제공자 전환 또는 사슬이 바닥난 감사 사건
  | "intercept_request"; // 가로채기 층이 보낸 사용자 승인 요청

// ChatAttachment는 한 번의 업로드 파일입니다. path는 그 세션/작업 작업 디렉터리 기준입니다(즉 agent의 CWD).
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 절대 경로(scope=staging 임시 업로드가 돌려줌. 작업을 만들기 전에 설명에 적음)
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // 세션 주인: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // 메인 에이전트 대화 조각(worker="mainagent" 줄에만 있음)
  // 토큰 사용량(kind='result'일 때만)
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 스케줄, 사용자 지정 agent만) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 예약: N초마다(0=예약 없음)
  on_finding: boolean; // 아무 작업이나 발견 finding이 생길 때 트리거
  on_goal_met: boolean; // 아무 작업이나 목표를 달성할 때 트리거
  on_task_timeout: boolean; // 아무 작업이나 시간을 넘길 때 트리거
  on_tool_call: boolean; // 고른 도구가 호출될 때(실행이 끝날 때) 트리거
  on_task_create: boolean; // 아무 작업이나 만들어질 때 트리거
  interval_message: string; // 각 트리거 조건의 독립 사용자 메시지
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call에서 고른 도구 key(적어도 하나)
  last_fire?: string;
}

// ---- 대화(채팅 화면) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // 서버의 실시간 상태. 대화 목록과 함께 옵니다
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- 백엔드 로그(/logs 화면) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id. DB에 남은 줄에만 있음
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// 하루 토큰 합계 통(GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // 날짜 형식 "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// 워커별 토큰 사용량(GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 분류를 일괄로 바꿀 때의 작업별 결과. 실패는 작업이 이미 삭제된 경우뿐이고, 분류 쓰기 자체는 원자적입니다.
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// 작업 전체(모든 에이전트) 토큰 합계.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// llm_usage 장부의 프로필별 전역 토큰 사용(GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// 대시보드 일별 차트의 (프로필, UTC 날짜) 토큰 통(새 출처).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // 날짜 YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// GET /api/tokens/usage 응답. 대시보드의 새(llm_usage) 토큰 보기.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// 작업 하나의 모델별 토큰 사용량(GET /api/llm/records/by-model).
// 항상 켜진 llm_usage 장부에서 옵니다. calls는 이 모델의 LLM 호출 수.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // 메인 에이전트 세션: 어느 대화 조각인지(0 = 원래)
}

// ---- 보안 ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- 트래픽 ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // 전체 개수(필터 없음)
  total?: number; // 지금 필터에 맞는 줄 수(페이지용)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// 왕래 하나의 원문 요청/응답(줄을 고르면 나중에 불러옴).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// 기록된 호스트 하나와 그 왕래 수(대상 고르기).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- 앱 설정(실행 중 스위치) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent가 트래픽 증거를 자동으로 연결. 기본은 끔. 사람이 연결하는 것에는 영향 없음
  llm_record: boolean; // LLM 기록 스위치(기본 끔). 끄면 LLM 호출을 전혀 기록하지 않습니다
  // 웹 검색. brave_key_set / tavily_key_set는 키가 저장돼 있는지만 알려 줍니다
  // (값 자체는 돌려주지 않음). PUT할 때 해당 필드를 보내 저장하거나 지웁니다.
  web_search_enabled: boolean;
  web_search_backend: string; // 웹 검색 백엔드: "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // 쓰기 전용. PUT으로 키를 저장하거나 지울 때만 보냅니다.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 독립 출구 프록시(http/https/socks5). 검색 엔드포인트에 접속하는 데 씁니다. 트래픽을 기록하는 MITM 프록시와는 무관합니다. 비어 있음=바로 연결. （기록 프록시는 오가는 트래픽을 잡아 두는 중간 서버입니다）
  web_search_proxy?: string;
  // 전역 출구 프록시(http/https/socks5, user:pass 가능). 모든 대상 트래픽이 여기를 탑니다. 트래픽 캡처를 켜면 그 상위가 됩니다. （기록 프록시는 오가는 트래픽을 잡아 두는 중간 서버입니다）
  // MITM 상위. 캡처를 끄면 agent의 bash/WebFetch에 직접 넣습니다. 비어 있음=바로 연결. （기록 프록시는 오가는 트래픽을 잡아 두는 중간 서버입니다）
  global_proxy?: string;
  python_interpreter?: string; // 사용자 지정 스크립트 도구의 python 해석기 경로(비어 있음=실행 때 탐지)
  workers?: number; // 동시에 일하는 agent 수(기본 3). 그 뒤에 시작하는 작업부터 적용
  // 작업 동시 상한: 동시에 「실행 중」인 작업 수의 상한. 끄면 제한 없음. 켜면 새 작업이 상한을 넘으면 대기하고, 자리가 나면 자동으로 시작합니다.
  task_concurrency_enabled?: boolean; // 기본 false
  task_concurrency_limit?: number; // 켠 뒤 기본 5
  // LLM 주기 조회(장애 전환). 기본 끔. 켜면 「모델을 지정하지 않은」 agent는 현재 설정을 쓸 수 없을 때
  // (잔액 부족/key 무효/제한/서비스 이상)일 때 다음 설정으로 자동 전환.
  llm_pool_enabled?: boolean; // 기본 false
  // 지정 설정에 연결된 agent/작업이 실패할 때도 주기 조회 사슬로 떨어질지. 기본 false = 연결하면 독점.
  llm_pool_bind_fallback?: boolean;
  // 동작 제약을 넣는 범위(기본은 모두 켬): 작업의 allow/deny 제약을 해당 agent의 시스템 프롬프트에 붙입니다.
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 실험 기능: noa 모델 주도 맥락 압축(기본 끔). 켜면 플랫폼에 연결된 네 종류 agent(planner/
  // worker/메인 에이전트/대화)는 noa가 맥락 압축을 맡아, 내장 compaction을 대신합니다. run마다 한 번 읽고,
  // 그 뒤에 시작하는 run부터 적용.
  noa_compaction?: boolean;
  // ---- 발견 IM 푸시(채널 자체는 독립 자원, /api/notify/* 참고. 여기에는 전역 설정 세 가지만) ----
  notify_enabled?: boolean; // 푸시 전체 스위치. 기본 켬. 점검 기간에 한 번에 멈추는 데 씀
  notify_public_base_url?: string; // 발견 상세로 돌아가는 외부 접속 주소. 비어 있음=메시지에 되돌아가는 링크를 넣지 않음
  notify_digest_interval_min?: number; // 모아 보내기 주기(분). 기본 30
}

// ---- 발견 IM 푸시 ----

// NotificationFilter는 채널의 필터 조건입니다. 필드는 모두 선택이며, 없으면 거르지 않습니다.
// 백엔드는 모든 필드를 검사하지 않습니다. 설정이 잘못되면 「맞음」으로 처리합니다(더 보내도 되고, 빠뜨리면 안 됨).
export interface NotificationFilter {
  min_severity?: string; // 비움 또는 low | medium | high | critical
  task_ids?: number[]; // 비어 있음=제한 없음. 비어 있지 않으면 발견이 속한 작업과 교집합이 있어야 함
  asset_ids?: number[]; // 비어 있음=제한 없음. 비어 있지 않으면 발견에 묶인 자산과 교집합이 있어야 함
  vulnclass_include?: string[]; // 비어 있음=모두 받음. 비어 있지 않으면 발견 유형이 키워드 중 하나에 맞아야 함(대소문자 무시 부분 문자열)
  vulnclass_exclude?: string[]; // 키워드 중 하나만 맞아도 제외(제외가 포함보다 우선)
  on_status_change?: boolean; // 발견 처리 상태 변경 이벤트도 받을지
}

// NotificationChannel은 채널 인스턴스 하나입니다. config 필드는 kind에 따라 다르고,
// 그리고 자격 필드는 읽을 때 "__masked__"로 시작하는 가림 값으로 바뀝니다. 그대로 돌려보내면 「안 바꿈」입니다.
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys는 백엔드가 채널 유형별로 줍니다. 프론트는 그것으로 비밀번호 칸과 「비워 두면 안 바꿈」 안내를 그리고,
  // 어떤 채널 지식도 하드코딩하지 않습니다.
  secret_keys: string[];
}

// NotificationKind는 /api/notify/meta가 돌려주는 채널 유형 메타데이터입니다.
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery는 전달 기록 한 건입니다. 전달 이력과 실패 재발송에 씁니다.
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed 사건
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM 설정 ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 생각 스위치(thinking.type): ""=보내지 않음(기본) | "disabled"=끄기 | "enabled"=켜기
  thinking_type?: string;
  // 생각 강도: ""=보내지 않음(기본) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // 주기 조회 순번: 클수록 먼저 선택됩니다. 활성 설정은 항상 사슬의 맨 앞이며, 이 값과 무관합니다.
  priority?: number;
  // true = 장애 전환 대상으로 쓰지 않음(agent/작업이 명시적으로 연결해 쓰는 것은 여전히 가능).
  pool_exclude?: boolean;
  // true(기본) = 스트리밍(SSE) | false = 진짜 비스트리밍(stream:false, 한 번에 반환).
  streaming?: boolean;
  // 답 한 번의 출력 상한(token). 0 = 그 필드를 보내지 않음. 서버 기본값이 정합니다.
  // context_window_k와 구분: 후자는 모델 전체 용량이고, 로컬에서 압축 기준에만 씁니다.
  max_tokens?: number;
  // 상한에 어떤 요청 필드 이름을 쓰는지. format="openai"일 때만 의미가 있습니다:
  // ""=max_tokens(기본) | "max_completion_tokens"(OpenAI 추론 모델은 이것만 알아봄)
  max_tokens_field?: string;
  // 사용자 지정 세션 헤더 이름: 비어 있지 않으면 요청마다 그 HTTP 헤더를 붙입니다. 헤더 값=현재 세션/의도의 session id.
  // ""=보내지 않음. session-id 헤더로 프롬프트 캐시/고정 라우팅을 하는 게이트웨이에 씁니다.
  session_header_key?: string;
  // 이 설정이 재시도를 덮어씀(연결/빈 응답/같은 provider 안전 창). 비움/모두 0 = 전역 방식을 따라감.
  retry?: LLMRetryOverride;
}

// ---- LLM 재시도 방식 ----
// 한 층 재시도의 두 조절값. 둘 다 「0 = 설정 안 함」:
//   attempts    0=기본 횟수 사용 | -1=이 층 재시도 끄기 | >0=재시도 횟수
//   interval_ms 0=기본 지수 백오프 사용 | >0=이 고정 밀리초 간격으로 바꿈
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 한 LLM 설정이 덮을 수 있는 세 층(모두 「엔드포인트를 따라가는」 재시도).
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 연결 재시도: 연결 리셋/시간 초과/429/5xx, 스트림이 시작되기 전
  empty: LLMRetryRule; // 빈 응답 재시도: 끝났지만 내용이 전혀 없음(openai 형식만)
  stream: LLMRetryRule; // 같은 provider 안전 창 재시도: 출력이 전달되기 전 끊긴 흐름을 다시 재생
}

// 전역 방식 = 위 세 층의 기본값 + 전역에만 있는 두 층:
//   breaker 주기 조회 차단기(attempts=순간 실패가 몇 번 연속이면 차단, interval_ms=고정 냉각 시간)
//   intent  의도 다시 실행(worker가 model_error로 끝난 뒤 의도 전체를 다시 실행) （워커는 의도를 실행하는 역할입니다）
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM 주기 조회(장애 전환) ----
// 설정 하나가 주기 조회 사슬에서 있는 위치와 건강 상태. state:
//   ok       정상
//   degraded 연속 실패는 있지만 차단 기준에는 못 미침
//   tripped  이미 차단됨. 냉각 동안 건너뜀(cooldown_secs는 남은 초)
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 현재 활성 설정인지(항상 사슬의 맨 앞)
  excluded: boolean; // pool_exclude: 주기 조회에 참여하지 않음
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- 에이전트 ----
export interface Agent {
  id: string;
  key: string; // 내장은 goals/planner/mainagent/worker입니다. 사용자 지정은 사용자가 정한 key입니다 （플래너는 방향을 정하고, 워커는 그 의도를 실행합니다）
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // 연결된 LLM 설정. null/없음 = 작업/세션/전역을 따라감
  max_turns?: number; // 0 = 제한 없음
  run_seconds?: number; // worker 한 번 실행의 벽시계 상한(초). 0 = 제한 없음 （워커는 의도를 실행하는 역할입니다）
  web_search?: boolean; // 웹 검색을 켤지(시스템 전역 스위치의 게이트를 받음)
  interactive_shell?: boolean; // 대화형 shell을 켤지(오래 유지되는 PTY 세션 도구 모음)
  // P3 트리거 후 처리 방식(사용자 지정 agent에만 의미가 있음)
  trigger_run_mode?: "serial" | "parallel"; // 직렬 대기열 / 트리거마다 각자 세션을 동시에 하나
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial만: 같은 작업 합치기 / 전부 합치기 / 합치지 않음
  trigger_max_parallel?: number; // parallel만: agent마다 동시 상한. 0=제한 없음
  // 연결 수(목록 API만 돌려줌): 보이는 MCP / 보이는 Skill / 연결된 도구
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // 연결할 수 있는 LLM 설정 후보(「기본 모델」 드롭다운용). 현재 연결은 agent.llm_profile_id 참고
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 저장된 마무리 프롬프트(비어 있음=내장 기본값 사용)
  wrapup_default?: string; // 내장 기본 마무리 프롬프트(자리표시/기본값 복구)
  wrapup_max_turns?: number; // 저장된 마무리 회전 수(0=내장 기본값 사용)
  wrapup_max_turns_default?: number; // 내장 기본 마무리 회전 수("0=기본 N" 안내에 씀)
  // 작업 단위 시간 초과 마무리 문구(worker/planner만, task_timeout_wrapup_supported=true일 때만 그 구역을 표시)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: TLS 인증서 확인을 건너뜀(자체 서명 서버)
  tools?: string[]; // mcp_tools_cache(개수용 이름만)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- 스킬 ----
// 필드는 agentskills.io 공개 명세에 맞춥니다.
// description은 "무엇을 하는지"와 "언제 쓰는지"를 모두 담습니다.
export interface SkillItem {
  name: string; // 유일한 키 = 디렉터리 이름
  description?: string; // 명세상 필수. 무엇을 하는지와 언제 쓰는지
  license?: string; // 선택: SPDX 식별자 또는 자유 문장
  compatibility?: string; // 선택: 필요한 실행 환경
  mcps?: string[]; // 이 스킬을 불러올 때 열리는 MCP 서버 이름
  files: string[]; // 스킬 디렉터리 안의 파일
  // 호출 통계(skill_usage 장부). 한 번도 호출되지 않은 skill: calls=0, last_used 없음.
  calls: number;
  tasks: number; // 이것을 불러 본 작업 수(chat 세션은 세지 않음)
  usage_agents: string[]; // 이것을 불러 본 agent key
  last_used?: string;
}

// SkillCall은 Skill() 호출 한 번입니다(한 skill의 최근 호출 목록).
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = 작업이 아닌 상황(대화 세션)
  session_id: string;
  args_len: number;
}

// MissingSkill은 이름으로 불렸지만 없는 skill입니다. "쓰고 싶은데 없음"의 빈칸.
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (내장 도구 목록) ----
// key와 handler는 Go에 있습니다. 이 필드만 화면에서 고칩니다. 시스템 도구는
// key와 매개변수 *구조*(name/type/required)를 잠그고, 매개변수별
// description/default와 에이전트 연결만 바뀝니다.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // 데모 기록입니다. 영향만 적었고 명령과 자격 증명은 생략했습니다.
  schema: Record<string, any>; // 전체 JSON 스키마(properties가 있는 객체)
  agents: string[]; // 연결된 에이전트 키
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // 사용자 지정 도구 유형
  // 데모 기록입니다. 영향만 적었고 명령과 자격 증명은 생략했습니다.
  exec?: Record<string, any>; // 사용자 지정 도구 실행 명세(kind!=builtin)
  deferred?: boolean; // schema 지연(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // 실행 중 호출 횟수(저장됨. 예전 API는 빠질 수 있음)
}

// ---- 통계 ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- 가로채기 규칙 ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules(자산 가로채기: 전역 차단 목록) ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action은 작업 단위 규칙에만 씁니다. block=가로채기(테스트 금지) allow=허용(허용 목록).
export type AssetInterceptAction = "block" | "allow";

// 작업 단위 자산 가로채기/허용 규칙의 입력 항목(작업 만들기, 작업 상세 편집에서 사용).
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // 전역 규칙에는 이 필드가 없습니다(항상 가로채기). 작업 단위 규칙은 block/allow를 구분
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // 데모 기록입니다. 영향만 적었고 명령과 자격 증명은 생략했습니다.
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // 규칙 message 또는 모델 판정 이유(모델 판정은 [模型] 접두사를 붙임) // han-allow 프로토콜 토큰
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: 모델 최후 승인(어떤 가로채기 규칙도 맞지 않을 때만 모델이 판단)의 전역 설정.
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = 활성/기본 설정을 따라감
  prompt: string; // 판정 프롬프트. GET에서 설정이 없으면 백엔드가 내장 템플릿 전문을 채움
  timeout_seconds: number; // 모델 호출 시간 초과
  fail_action: "allow" | "ask" | "deny"; // 모델이 오류/시간 초과/해석 불가일 때의 되돌림
  ask_timeout_seconds: number; // 모델이 ask로 사람에게 넘긴 뒤의 승인 대기 시간 초과
  ask_timeout_action: "allow" | "deny"; // 승인 시간 초과 후의 기본 동작
}

// JudgeUsage: 모델 최후 승인(judge 채널)의 누적 token 사용량 + 최근 N일 하루 단위 수열.
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC 날짜)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow는 InterceptPending에 대화/작업과 규칙 맥락을 붙입니다.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // 연결된 대화가 없으면 ""
  conv_agent_key: string; // 연결된 대화가 없으면 ""
  rule_name: string; // 규칙이 지워졌으면 ""
}

// ── 자산 동기화 (ScopeSentry 데이터 출처) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID. filter.project로 씁니다
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // filter.task로 씁니다
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary. 대화 하나의 토큰 합계(프로필/날짜 포함). 채팅 사용량을
// 대시보드 토큰 통계에 합칩니다. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- 명령 기록(Bash 실행 기록) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // 도구에 들어간 원문(JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// 도구 하나의 호출 통계(/commands/stats). errors는 그중 실패한 횟수.
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM 기록 ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider가 실제로 주고받은 HTTP 원문: 요청은 buildBody()가 보낸 전체 body(도구
  // schema). 응답은 원본 SSE 프레임입니다. 위의 request_body/response_body는 정규화 보기이고,
  // 도구 schema와 tool_use 블록을 버렸습니다. 예전 기록은 비어 있습니다.
  raw_request?: string;
  raw_response?: string;
}

// LLM 기록이 있는 작업 하나와 그 개수(기록 화면의 작업 고르기).
export interface LLMTask {
  task_id: string;
  count: number;
}

// 검토 모델에 보낸 JSON 그대로. 모든 모델 판정에 남겨 둡니다.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary는 바꾸지 않는 v2/v3 스냅샷에만 남습니다.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // 버전 1 스냅샷은 바꾸지 않으며, 과거 감사에서 계속 읽을 수 있습니다.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // 예전 v1/v2 스냅샷만. v3는 실행 기록을 보내지 않습니다.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// 바꾸지 않는 검토 스냅샷과, 따로 기록한 실행 결과.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check —— 현재 버전과 GitHub 최신 정식판을 비교한 결과. */
export interface UpdateCheck {
  /** 지금 실행 중인 버전. 개발 빌드는 "dev"이거나 git describe의 접미사가 붙은 형태입니다. */
  current: string;
  /** 실행 형태. docker에서는 교체가 컨테이너의 쓸 수 있는 층에만 적용되고, 컨테이너를 다시 만들면 이미지 버전으로 돌아갑니다. */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** 되돌릴 수 있는 이전 버전이 있는지(artex.old). */
  has_backup: boolean;
  /** 이번 시작 때 자체 업데이트가 스스로 시작한 결론(교체 실패 / 이미 되돌림 등). 아무 일도 없으면 비어 있습니다. */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub 조회에 실패하면 이유를 줍니다. 이때 아래 필드는 하나도 없습니다. */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 현재 플랫폼에 해당하는 배포 패키지 이름, 그리고 그 Release가 정말 그것을 포함하는지. */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 양쪽 버전 번호를 비교할 수 있는지. 개발 빌드는 false이며, 이때 한 번에 업데이트를 막습니다. */
  comparable?: boolean;
  /** comparable이 false일 때의 설명. */
  reason?: string;
}

/** /api/update/stream 이 밀어 주는 업데이트 진행 한 건. */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** 다운로드 단계에서만 의미가 있습니다(0-100). 나머지 단계는 -1입니다. */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// 승인에서 고른 원래 실행. 검토 모델에는 보내지 않습니다.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
