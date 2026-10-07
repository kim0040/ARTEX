// 상태마다 색과 글자를 한곳에서 정해, 앱 전체가 같이 씁니다.
// Spec §8.3: 의도 / 커버리지 / 작업 / 심각도는 각각 일관된 색 묶음을 가집니다.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose는 「심각」에 씁니다. 속을 채운 강한 강조로, 「높음」의 옅은 빨간 테두리보다 눈에 훨씬 띕니다.
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "미할당", tone: "slate" },
  running: { label: "실행 중", tone: "blue" },
  paused: { label: "일시정지", tone: "amber" },
  done: { label: "완료", tone: "green" },
  // blocked = 모델/API/네트워크 오류 재시도를 다 씀. 이 의도는 사실상 탐색되지 않았습니다(목표 가로채기가 아님).
  blocked: { label: "실행 오류", tone: "red" },
  // exhausted = 걸음 수/시간 예산에 걸려 중간에 잘림. 일부 결과만 다시 씀(방향을 다 탐색한 것이 아님).
  exhausted: { label: "예산 소진", tone: "violet" },
  // stopped = 과거의 소프트 삭제 상태(유지, 과거 데이터).
  stopped: { label: "중지됨", tone: "slate" },
  // deleted = 사용자가 이 의도를 표시만 삭제함(노드와 혈통은 유지, 삭제 이유는 delete_reason 필드).
  deleted: { label: "삭제됨", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "생성됨", tone: "slate" },
  queued: { label: "대기열", tone: "amber" },
  running: { label: "실행 중", tone: "blue" },
  paused: { label: "일시정지", tone: "amber" },
  done: { label: "완료", tone: "green" },
  failed: { label: "실패", tone: "red" },
  timeout: { label: "시간 초과", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "심각", tone: "rose" },
  high: { label: "높음", tone: "red" },
  medium: { label: "중간", tone: "amber" },
  low: { label: "낮음", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "처리 대기", tone: "amber" },
  in_progress: { label: "처리 중", tone: "blue" },
  confirmed: { label: "확인됨", tone: "red" },
  resolved: { label: "처리됨", tone: "green" },
  fixed: { label: "수정됨", tone: "green" },
  false_positive: { label: "오탐", tone: "slate" },
  ignored: { label: "무시", tone: "neutral" },
  duplicate: { label: "중복", tone: "neutral" },
  risk_accepted: { label: "위험 수용", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "탐색 중", tone: "blue" },
  paused: { label: "일시정지", tone: "amber" },
  stalled: { label: "정체", tone: "red" },
  idle: { label: "유휴", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "진행 중", tone: "blue" },
  met: { label: "달성", tone: "green" },
  abandoned: { label: "포기", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "허용", tone: "green" },
  block: { label: "차단", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "관측", tone: "slate" },
  confirmed: { label: "확인", tone: "green" },
  tombstoned: { label: "폐기", tone: "neutral" },
};

// 푸시 전달 상태. sending은 amber가 아니라 blue를 씁니다. 「문제 있음」이 아니고,
// 이 아니라 「이미 받아, 보내는 중」입니다. pending의 대기 의미와 구분되어야 합니다.
const delivery: Record<string, StatusMeta> = {
  pending: { label: "전송 대기", tone: "amber" },
  sending: { label: "전송 중", tone: "blue" },
  sent: { label: "전달됨", tone: "green" },
  failed: { label: "실패", tone: "red" },
  skipped: { label: "건너뜀", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
