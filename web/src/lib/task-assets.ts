// 자산 종류와 출처를 화면 말로 바꿉니다.
// 키 이름은 자산 그래프에 저장된 값 그대로이고, 보이는 말만 한국어입니다.
import type { NewAssetType } from "@/lib/types";

const ASSET_TYPE_LABELS: Record<NewAssetType, string> = {
  app: "앱",
  endpoint: "엔드포인트",
  ip: "IP",
  root_domain: "루트 도메인",
  service: "서비스",
  subdomain: "서브도메인",
};

const TASK_ASSET_SOURCE_LABELS: Record<string, string> = {
  agent: "Agent 발견",
  anchor: "탐색 그래프 앵커",
  api: "자산 API",
  company: "기업 연결",
  legacy: "과거 연결",
  manual: "사람이 추가",
  system: "시스템 연결",
  task: "작업 초기화",
};

export function taskAssetTypeLabel(type: NewAssetType): string {
  return ASSET_TYPE_LABELS[type];
}

export function taskAssetSourceLabel(source: string): string {
  return TASK_ASSET_SOURCE_LABELS[source] ?? source;
}
