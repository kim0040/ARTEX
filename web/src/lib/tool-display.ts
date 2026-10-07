import type { Tool } from "@/lib/types";

// 서버에서 system=true로 심는 내장 도구의 화면용 요약입니다. 이 표는 모델에
// 전달되는 Tool.description이나 JSON Schema를 바꾸지 않고, 도구 목록에서만 씁니다.
export const BUILTIN_TOOL_DISPLAY_SUMMARIES: Readonly<Record<string, string>> = {
  graph_overview: "탐색 그래프의 자산·프론티어·발견·힌트 요약을 조회합니다.",
  list_findings: "작업에서 확인된 발견(finding)을 조회합니다.",
  list_facts: "탐색 그래프에 기록된 사실을 페이지 단위로 조회합니다.",
  node_detail: "탐색 노드의 상세 정보와 연결된 증거를 조회합니다.",
  expand_digest: "접힌 탐색 요약을 구성원 노드와 상세 정보로 펼칩니다.",
  get_worker_output: "워커가 생성한 작업 결과를 조회합니다.",
  get_worker_trace: "특정 워커 작업의 실행 단계와 출력을 조회합니다.",
  search_all_worker_traces: "모든 워커 작업의 실행 기록을 키워드로 검색합니다.",
  add_hint: "플래너가 다음 의도를 만들 때 참고할 전략 힌트를 추가합니다.",
  bash: "시스템 셸에서 명령을 실행합니다. 허용 범위와 실행 내용을 먼저 확인하세요.",
  Bash: "시스템 셸에서 명령을 실행합니다. 허용 범위와 실행 내용을 먼저 확인하세요.",
  upsert_asset: "자산을 새로 등록하거나 기존 자산 정보를 갱신합니다(이전 버전 샘플 키).",
  add_intent: "탐색 그래프에 실행할 의도를 추가합니다.",
  steer_work: "실행 중인 워커 작업에 중단 없이 방향을 전달합니다.",
  set_goals: "현재 작업의 검증 가능한 최종 목표를 추가합니다.",
  set_constraints: "현재 작업의 허용·금지 조작 제약을 추가합니다.",
  insert_assets: "자산을 저장하고 현재 작업 범위와의 관계를 기록합니다.",
  add_company_scope: "회사에 속한 자산 범위 규칙을 추가하거나 갱신합니다.",
  list_assets: "조건(DSL·ID)으로 작업 범위의 자산을 조회합니다.",
  report_finding: "검증된 발견을 증거와 함께 기록합니다.",
  record_fact: "탐색 그래프에 관찰된 사실을 기록합니다.",
  add_task_scope: "현재 작업의 테스트 범위를 넓힙니다.",
  list_untested_assets: "아직 사실 앵커가 없는 작업 범위 자산을 조회합니다.",
  list_goals: "현재 작업의 목표와 달성 상태를 조회합니다.",
  prove_goal: "목표 하나를 증거와 함께 달성으로 표시합니다.",
  goal_met: "모든 목표가 달성됐을 때 작업 전체 완료를 표시합니다.",
  kill_work: "실행 중인 워커 작업을 중지합니다.",
  list_worker_traces: "작업에서 실행된 워커 목록과 단계 수를 조회합니다.",
  list_companies: "회사 목록·범위·자산 수를 조회합니다.",
  traffic_search: "기록된 트래픽 인덱스를 요청·응답 조건으로 검색합니다.",
  traffic_get: "트래픽 ID의 요청·응답 원문을 조회합니다.",
  traffic_blob: "큰 트래픽 본문을 구간 단위로 조회합니다.",
  list_tasks: "여러 작업의 상태와 요약을 조회합니다.",
  list_llm_profiles: "사용 가능한 LLM 프로필을 조회합니다.",
  spawn_task: "현재 작업과 연결된 새 작업을 생성하고 실행을 시작합니다.",
  pause_task: "지정한 작업의 플래너·워커 실행을 일시정지합니다.",
  get_task_graph: "지정한 작업의 탐색 그래프 요약을 조회합니다.",
  list_task_findings: "지정한 작업의 확인된 발견 목록을 조회합니다.",
  add_task_hint: "지정한 작업의 플래너에 전략 힌트를 추가합니다.",
  get_task_worker_trace: "지정한 작업의 워커 실행 기록 상세를 조회합니다.",
  list_task_worker_traces: "지정한 작업에서 실행된 워커 목록과 단계 수를 조회합니다.",
  search_task_worker_traces: "지정한 작업의 워커 실행 기록을 키워드로 검색합니다.",
  get_task_node_detail: "지정한 작업의 탐색 노드 상세를 조회합니다.",
  update_finding_report: "발견 보고서의 최신 내용을 증거 버전에 맞춰 갱신합니다.",
  get_finding_traffic: "발견에 묶인 HTTP 트래픽 증거를 조회합니다.",
  bind_finding_traffic: "검증된 HTTP 트래픽을 발견에 연결합니다.",
  delete_assets_by_host: "정확히 일치하는 호스트와 하위 자산을 영구 삭제합니다.",
  create_skill: "새 스킬과 기본 SKILL.md를 만듭니다.",
  update_skill_file: "기존 스킬의 파일을 작성하거나 덮어씁니다.",
  create_custom_tool: "사용자 지정 shell/command/script/http 도구를 만듭니다.",
  update_custom_tool: "기존 사용자 지정 도구를 수정합니다.",
  create_mcp: "stdio/http/sse MCP 서버 연결을 만듭니다.",
  update_mcp: "기존 MCP 서버 연결을 수정합니다.",
  get_finding_retest_context: "현재 재검증 세션의 증거 스냅샷과 제약을 조회합니다.",
  record_finding_retest_result: "현재 재검증 세션의 결론과 실제 증거를 저장합니다.",
};

const UNKNOWN_BUILTIN_SUMMARY = "이 시스템 도구의 화면 설명을 준비 중입니다.";

export function toolDisplaySummary(tool: Pick<Tool, "key" | "system" | "description">): string {
  if (!tool.system) return tool.description || "(설명 없음)";
  return BUILTIN_TOOL_DISPLAY_SUMMARIES[tool.key] ?? UNKNOWN_BUILTIN_SUMMARY;
}
