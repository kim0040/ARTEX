"use client";

// 이 화면은 엔진이 남긴 메인 에이전트, 플래너, 워커 기록을 세션별로 읽습니다.

import * as React from "react";

import {
  ArrowUpIcon,
  BrainIcon,
  ChevronDownIcon,
  CircleCheckIcon,
  CircleSlashIcon,
  CircleXIcon,
  ClockIcon,
  HistoryIcon,
  Loader2Icon,
  PaperclipIcon,
  PauseIcon,
  PlusIcon,
  RadioIcon,
  RotateCwIcon,
  ShieldAlertIcon,
  SquareIcon,
  Trash2Icon,
  UserIcon,
  WifiOffIcon,
  XIcon,
  ZapOffIcon,
} from "lucide-react";
import { toast } from "sonner";

import { ApprovalExecutionFocus, useApprovalFocus, useApprovalHistory } from "@/components/approval-execution-focus";
import { MentionTextarea } from "@/components/mention-textarea";
import { SideQuestionButton, SideQuestionWorkspace } from "@/components/side-question-workspace";
import { TodoPopover } from "@/components/todo-popover";
import { Transcript } from "@/components/transcript";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { InputGroup, InputGroupAddon, InputGroupButton } from "@/components/ui/input-group";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { api, sseUrl } from "@/lib/api";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import { MOCK } from "@/lib/mock/enabled";
import { isBtwCommand } from "@/lib/side-questions";
import { taskAssetSourceLabel, taskAssetTypeLabel } from "@/lib/task-assets";
import type {
  Activity,
  ChatAttachment,
  IntentAsset,
  InterceptApprovalRow,
  Session,
  SessionStatus,
  SessionTokenUsage,
  TaskLLMResolution,
  TaskLLMResolutions,
  TaskNode,
  TokenTotal,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// resolutionLabel은 이 세션이 쓰는 LLM 이름입니다. 배지에는 이것만 보이고
// 모델 id는 툴팁에 둡니다. 환경 변수 설정은 이름이 없을 수 있어
// 빈 배지 대신 모델 id를 보여 줍니다.
function resolutionLabel(r: TaskLLMResolution): string {
  return r.name || r.model || "이름 없는 설정";
}

// fmtBytes는 첨부 칩에 사람이 읽기 쉬운 파일 크기를 그립니다(transcript.tsx와 같음).
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// ── 기록 신뢰 방식(docs/task-session-history-sse-remediation.md) ──────────
// 작업 활동을 처음부터 무한정 다시 쌓던 `allActivity` 배열은 더 이상 쓰지 않습니다.
// SSE since=0 대신 이렇게 합니다:
//   • 화면 세션(main | plan | intent:<id>)마다 나중에 불러오고, 거꾸로
//     페이지를 쌓는 캐시(아래 SessionState)가 있습니다. 열면 최신
//     페이지만 오고, 위로 스크롤하면 더 오래된 기록이 붙습니다.
//   • 작업 실시간 스트림은 하나이고, 첫 기록 페이지의 snapshot_cursor부터 엽니다.
//     그 페이지 이후의 모든 에이전트 활동을 session_key로 나눠 넣습니다.
//   • 기록과 실시간 스트림은 snapshot_cursor에서 빈틈 없이 만나고, seq로 합칩니다(중복 제거).
//     그래서 새로고침, 탭 전환, 잠자기, 재연결에도 가장 최근 기록이 빠지지 않습니다.

const PAGE = 200; // 기록 한 페이지 크기
const SYSTEM_SCAN_PAGE = 500; // 드문 시스템 감사 이벤트를 찾기 위해 훑는 일반 활동 페이지 크기
const MAX_KEEP = 4000; // 세션당 메모리 상한. 더 오래된 페이지는 위로 스크롤할 때 다시 가져옵니다
const STREAM_WINDOW_MS = 5000; // "실시간"은 이 시간 안에 활동이 보인 것입니다
const MAX_WORKER_MESSAGE_CHARS = 4000;

function newWorkerMessageRequestID(): string {
  return (
    globalThis.crypto?.randomUUID?.() ??
    `worker-message-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
  );
}

function workerMessageCharCount(value: string): number {
  return Array.from(value).length;
}

// 세션 하나의 느린 로딩·역방향 페이지 캐시입니다. `lastTs`와 `unread`는
// 한 번도 열지 않은 세션도 갱신해서, 목록이 살아 있음과 안 읽음을
// 전체 기록 없이 보여 줍니다.
type SessionState = {
  items: Activity[];
  loaded: boolean;
  loading: boolean;
  loadingMore: boolean;
  hasMore: boolean; // 불러온 창 위에 더 오래된 기록이 있습니다
  earliestSeq: number; // 지금까지 불러온 가장 오래된 id. 위로 넘어갈 기준 번호
  unread: number;
  lastTs: string; // 가장 최근 활동 시각(실시간 배지용. 아직 안 연 세션도 갱신)
  error?: string;
};
type SessionStore = Record<string, SessionState>;

function emptyState(): SessionState {
  return {
    items: [],
    loaded: false,
    loading: false,
    loadingMore: false,
    hasMore: false,
    earliestSeq: 0,
    unread: 0,
    lastTs: "",
  };
}

// sessionKeyOf는 활동을 안정된 세션 키로 보냅니다. worker="planner"는
// 목표 에이전트의 0라운드 분해와 플래너(계획 세션 하나)를 모두 담습니다.
function sessionKeyOf(a: Activity): string {
  if (a.worker === "system" || a.kind === "llm_switch" || a.kind === "llm_failover") return "system";
  if (a.worker === "mainagent") return `main:${a.main_seg ?? 0}`; // 대화 조각마다 키가 하나
  if (a.worker === "planner") return "plan";
  if (a.intent_id) return `intent:${a.intent_id}`;
  return "unknown";
}

// mergeBySeq는 두 활동 목록을 seq로 합치고(중복 제거) seq 오름차순으로 둡니다. 모든
// 출처(최신 페이지, 오래된 페이지, 실시간 보정, 실시간 꼬리)가
// 여기를 지나므로, 기록 응답이 그 사이 받은 실시간 기록을 덮지 못합니다.
function mergeBySeq(current: Activity[], incoming: Activity[]): Activity[] {
  if (!incoming.length) return current;
  const bySeq = new Map<number, Activity>();
  for (const a of current) bySeq.set(a.seq, a);
  for (const a of incoming) bySeq.set(a.seq, a);
  return [...bySeq.values()].sort((p, q) => p.seq - q.seq);
}

// statusIcon은 세션 상태를 아이콘으로 바꿉니다. 워커가 끝난 상태는
// 서로 구분되고 색이 다름: 완료(녹색 체크 원) / 취소 정지(호박색 슬래시 원) / 오류(빨간 X 원) /
// 걸음 수를 다 씀(보라). running=파란 회전, pending(받을 대기)=회색 시계.
function statusIcon(status: SessionStatus) {
  switch (status) {
    case "running": // 실행 중
      return <Loader2Icon className="size-3.5 animate-spin text-blue-500" />;
    case "paused":
      return <PauseIcon className="size-3.5 text-amber-500" />;
    case "pending": // 받을 대기(open intent)
      return <ClockIcon className="size-3.5 text-muted-foreground" />;
    case "done": // 완료
      return <CircleCheckIcon className="size-3.5 text-emerald-500" />;
    case "stopped": // 취소/정지(planner가 종료)
      return <CircleSlashIcon className="size-3.5 text-amber-500" />;
    case "blocked": // 오류
      return <CircleXIcon className="size-3.5 text-red-500" />;
    case "exhausted": // 걸음 수를 다 씀(max_turns에 부딪힘)
      return <ZapOffIcon className="size-3.5 text-violet-500" />;
    case "deleted": // 사용자 표시만 삭제
      return <CircleSlashIcon className="size-3.5 text-muted-foreground" />;
  }
}

// fmtTokens는 토큰 수를 짧게 그립니다(1234 → 1.2k, 2_000_000 → 2M).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

const TokenMetrics = React.forwardRef<
  HTMLSpanElement,
  React.ComponentPropsWithoutRef<"span"> & {
    input: number;
    cache: number;
    output: number;
    labels?: "short" | "long";
  }
>(({ input, cache, output, labels = "short", className, ...props }, ref) => {
  const names = labels === "short" ? ["입력", "캐시", "출력"] : ["input", "cache", "output"];
  const values = [input, cache, output];
  return (
    <span
      ref={ref}
      className={cn("inline-flex min-w-0 flex-wrap items-center gap-1 text-muted-foreground", className)}
      {...props}
    >
      {values.map((value, index) => (
        <React.Fragment key={names[index]}>
          <span>{names[index]}</span>
          <Badge variant="secondary" className="h-5 px-1.5 font-mono tabular-nums">
            {fmtTokens(value)}
          </Badge>
        </React.Fragment>
      ))}
    </span>
  );
});
TokenMetrics.displayName = "TokenMetrics";

// fmtDuration은 지난 밀리초를 짧게 그립니다(90초 → 1분 30초).
function fmtDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h${String(m % 60).padStart(2, "0")}m`;
}

const roleMeta = {
  mainagent: { label: "메인 에이전트", icon: UserIcon },
  planner: { label: "플래너 Planner", icon: BrainIcon },
  worker: { label: "Workers", icon: RadioIcon },
  system: { label: "시스템 감사", icon: HistoryIcon },
} as const;

// 메인 에이전트 세션은 이 탭에서 사람이 대화하는 입구이고,
// 백엔드에 전용 "sessions" 주소는 없습니다. 고정된 화면 장치이고
// 대화 기록은 이 작업의 메인 에이전트 활동(worker="mainagent")입니다.
// 메인 에이전트 세션은 새로 시작할 수 있는 대화 조각입니다. 0번은
// 원래 세션. "세션 만들기"는 이어지는 조각(seq 1, 2, …)을 만들어, agent가
// 깨끗한 기록으로 시작하지만, 작업의 그래프·자산·목표는 같이 씁니다. 각
// 조각은 바꿔 볼 수 있는 화면 세션이고, 지금(가장 큰 번호) 것만 쓸 수 있습니다.
const mainSessionId = (seg: number) => `s-main-${seg}`;
const mainSessionKey = (seg: number) => `main:${seg}`;
const mainSessionTitle = (seg: number) => `메인 에이전트 · 세션 #${seg + 1}`;
const MAIN_ID = mainSessionId(0);
const MAIN_SESSION: Session = {
  id: MAIN_ID,
  role: "mainagent",
  title: mainSessionTitle(0),
  status: "running",
  live: true,
  last_activity: "",
  seg: 0,
};

// 플래너 세션도 메인 에이전트처럼 고정된 화면 장치이고
// 백엔드에 전용 "sessions" 주소는 없습니다. 대화 기록은 플래너가 낸
// 모든 활동 단계입니다(worker === "planner". 목표 에이전트의
// 0라운드 분해도 여기 있습니다. 플래너는 의도를 만들므로 intent_id가 없습니다).
const PLANNER_ID = "s-planner";
const PLANNER_SESSION: Session = {
  id: PLANNER_ID,
  role: "planner",
  title: "플래너 Planner · 상황 판단",
  status: "running",
  live: true,
  last_activity: "",
};

// LLM 제공자 전환은 에이전트 출력이 아니라 작업 단위 감사 사건입니다.
// 기록 주소에 "system" 필터가 없어서, 이 고정 세션은
// 일반 증분 활동 주소를 훑어 채운 뒤 작업 실시간 스트림으로 뒤를 잇습니다.
const SYSTEM_ID = "s-system";
const SYSTEM_SESSION: Session = {
  id: SYSTEM_ID,
  role: "system",
  title: "시스템 이벤트 · LLM 장애 조치",
  status: "done",
  live: false,
  last_activity: "",
};

// keyForSession은 화면 Session을 저장 키로 바꿉니다(main:<seg> | plan | intent:<id>).
function keyForSession(s: Session): string {
  if (s.role === "mainagent") return `main:${s.seg ?? 0}`;
  if (s.role === "planner") return "plan";
  if (s.role === "system") return "system";
  return `intent:${s.intent_id}`;
}

// 탐색 의도(TaskNode) 상태를 화면이 그리는 세션 상태로 바꿉니다.
function intentStatus(state: string): SessionStatus {
  switch (state) {
    case "done":
      return "done";
    case "blocked":
      return "blocked";
    case "exhausted":
      return "exhausted";
    case "stopped":
      return "stopped";
    case "paused":
      return "paused";
    case "open": // 받을 대기. 실행 중과 구분
      return "pending";
    case "deleted": // 사용자 표시만 삭제
      return "deleted";
    default: // 실행 중
      return "running";
  }
}

// 실행 중이거나 열린 의도에서 워커 세션을 만듭니다. 백엔드에
// sessions 주소가 없어서, 의도(워커 단위와 거의 같음)가 가장 가까운 실제 출처입니다.
function intentToSession(n: TaskNode): Session {
  const label = (n.payload ?? "").trim();
  const state = intentStatus(n.state);
  return {
    id: n.id,
    role: "worker",
    title: label || `Intent ${n.id}`,
    status: state,
    live: !n.inherited && state === "running",
    last_activity: n.ts,
    intent_id: n.id,
    source_task_id: n.source_task_id,
    inherited: n.inherited,
  };
}

function SessionItem({
  s,
  active,
  displayTitle,
  hasPending,
  unread,
  onClick,
  onCancel,
  controlling,
  deleted,
}: {
  s: Session;
  active: boolean;
  displayTitle: string;
  hasPending?: boolean;
  unread?: number;
  onClick: () => void;
  onCancel?: () => void;
  controlling?: boolean;
  deleted?: boolean;
}) {
  const icon = deleted ? (
    <Trash2Icon className="size-3.5 text-destructive" />
  ) : s.role === "worker" ? (
    statusIcon(s.status)
  ) : s.live ? (
    <Loader2Icon className="size-3.5 animate-spin text-blue-500" />
  ) : null;

  const cancellable =
    s.role === "worker" &&
    !s.inherited &&
    !deleted &&
    // pending = 받을 대기(open) 의도. 실행 중/일시정지도 삭제를 허용합니다.
    (s.status === "running" || s.status === "paused" || s.status === "pending");
  return (
    <div
      className={cn(
        "group/session flex w-full items-center rounded-md transition-colors",
        active ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
      )}
    >
      <button
        type="button"
        onClick={onClick}
        className="flex min-w-0 flex-1 items-center gap-1 px-2 py-1.5 text-left text-sm"
      >
        {icon ?? <span className="size-3.5 shrink-0" />}
        {s.intent_id && (
          <span className="shrink-0 rounded bg-muted px-1 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
            #{s.intent_id}
          </span>
        )}
        {s.inherited && s.source_task_id && (
          <Badge variant="outline" className="shrink-0">
            출처 #{s.source_task_id}
          </Badge>
        )}
        <span
          className={cn("min-w-0 flex-1 truncate text-sm font-medium", deleted && "text-muted-foreground line-through")}
        >
          {displayTitle}
        </span>
        {deleted && (
          <Badge variant="outline" className="shrink-0 border-destructive/40 text-destructive">
            삭제됨
          </Badge>
        )}
        {hasPending && <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-500" />}
        {!active && unread ? (
          <span className="inline-flex min-w-4 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
            {unread > 99 ? "99+" : unread}
          </span>
        ) : null}
        {s.live && (
          <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
            <span className="size-1 animate-pulse rounded-full bg-blue-500" />
            실시간
          </span>
        )}
      </button>
      {cancellable && (
        <div className="flex shrink-0 items-center gap-0.5 pr-1 opacity-100 sm:opacity-0 sm:transition-opacity sm:group-hover/session:opacity-100 sm:group-focus-within/session:opacity-100">
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            onClick={onCancel}
            disabled={controlling}
            title="이 의도 삭제(이유를 적어야 하며, 가짜 삭제/완전 삭제를 고를 수 있음)"
            aria-label="이 의도 삭제(이유를 적어야 하며, 가짜 삭제/완전 삭제를 고를 수 있음)"
            className="text-destructive hover:text-destructive"
          >
            <Trash2Icon />
          </Button>
        </div>
      )}
    </div>
  );
}

function truncateWorkerAssetLabel(value: string, maxChars = 30): string {
  const chars = Array.from(value.trim());
  if (chars.length <= maxChars) return chars.join("");
  return `${chars.slice(0, Math.max(0, maxChars - 1)).join("")}…`;
}

function WorkerAssetBadge({ assets }: { assets: IntentAsset[] }) {
  const displayAssets = assets.filter(
    (asset) => asset.type === "root_domain" || asset.type === "subdomain" || asset.type === "ip",
  );
  if (displayAssets.length === 0) return null;

  const first = displayAssets[0];
  const firstRawLabel = first.label.trim() || `#${first.asset_id}`;
  const firstLabel = truncateWorkerAssetLabel(firstRawLabel);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="max-w-60 shrink-0 font-normal" title={firstRawLabel}>
          <span className="truncate">현재 자산:{firstLabel}</span>
          {displayAssets.length > 1 && <span className="shrink-0 tabular-nums">+{displayAssets.length - 1}</span>}
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="right" align="start" className="max-w-sm">
        <div className="flex flex-col gap-2">
          {displayAssets.map((asset) => (
            <div key={`${asset.intent_id}-${asset.asset_id}`} className="min-w-0">
              <div className="break-all font-mono text-xs">{asset.label.trim() || `#${asset.asset_id}`}</div>
              <div className="mt-0.5 text-xs text-muted-foreground">
                {taskAssetTypeLabel(asset.type)} · {taskAssetSourceLabel(asset.source)}
                {asset.inherited ? ` · 출처 작업 #${asset.source_task_id}` : ""}
              </div>
              <div className="mt-0.5 [overflow-wrap:anywhere] text-xs">{asset.source_summary}</div>
            </div>
          ))}
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

export function SessionsTab({ taskId }: { taskId: string }) {
  const approvalFocus = useApprovalFocus({ taskId });
  const [selectedSessionId, setActiveId] = React.useState(MAIN_ID);
  const focusSession = React.useMemo<Session | undefined>(() => {
    const source = approvalFocus.state?.source;
    if (!source) return undefined;
    if (source.session.startsWith("main:")) {
      const seg = Number(source.session.slice(5));
      return { ...MAIN_SESSION, id: mainSessionId(seg), seg, title: mainSessionTitle(seg), live: false };
    }
    if (source.session === "plan") return { ...PLANNER_SESSION, live: false };
    if (source.session.startsWith("intent:")) {
      const id = source.session.slice(7);
      return {
        id,
        role: "worker",
        intent_id: id,
        title: `Worker #${id}`,
        status: "done",
        live: false,
        last_activity: source.items[0]?.ts ?? "",
      };
    }
    return undefined;
  }, [approvalFocus.state?.source]);
  // 초점 모드를 빠져나와도, 찾아 둔 보관/이전 세션을 계속 고를 수 있게 둡니다.
  const [locatedSession, setLocatedSession] = React.useState<{ taskId: string; session: Session }>();
  React.useEffect(() => {
    if (focusSession) setLocatedSession({ taskId, session: focusSession });
  }, [taskId, focusSession]);
  const retainedSession = locatedSession?.taskId === taskId ? locatedSession.session : undefined;
  const activeId = focusSession?.id ?? selectedSessionId;
  // 메인 에이전트 대화 조각(최신이 먼저). currentSeg만 쓸 수 있습니다.
  const [mainSegs, setMainSegs] = React.useState<{ seq: number; created_at: string }[]>([{ seq: 0, created_at: "" }]);
  const [currentSeg, setCurrentSeg] = React.useState(0);
  const [creatingMain, setCreatingMain] = React.useState(false);
  const [confirmNewMain, setConfirmNewMain] = React.useState(false);
  // 휴대폰(<lg)에서는 세션 목록이 기본적으로 접힙니다. 화면 높이가 원래 빠듯한데, 목록이 10~15rem을 고정으로 차지하면,
  // 아래 세션 기록이 제목과 입력 칸만 남을 정도로 밀립니다. 접으면 기록 영역이 거의 전체 높이를 받고,
  // 제목 줄을 눌러 세션을 고를 수 있고, 고르면 자동으로 접힙니다. 데스크톱은 영향 없음(lg부터 항상 펼침).
  const [listOpen, setListOpen] = React.useState(false);
  // 세션마다 나중에 불러오는 캐시. 키는 session_key(main | plan | intent:<id>)입니다.
  const [store, setStore] = React.useState<SessionStore>({});
  // 탐색 의도에서 만든 워커 세션(예전 300개 상한을 넘겨 페이지로 가져옴).
  const [intents, setIntents] = React.useState<TaskNode[]>([]);
  const [intentAssets, setIntentAssets] = React.useState<IntentAsset[]>([]);
  const [olderIntents, setOlderIntents] = React.useState<TaskNode[]>([]);
  const [firstIntentsHasMore, setFirstIntentsHasMore] = React.useState(false);
  const [olderIntentsHasMore, setOlderIntentsHasMore] = React.useState(false);
  const [hasLoadedOlderIntentsPage, setHasLoadedOlderIntentsPage] = React.useState(false);
  const [loadingOlderIntents, setLoadingOlderIntents] = React.useState(false);
  const [input, setInput] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  const [mainChatRunning, setMainChatRunning] = React.useState<boolean | null>(null);
  const [controllingIntent, setControllingIntent] = React.useState<string | null>(null);
  const [cancelIntent, setCancelIntent] = React.useState<Session | null>(null);
  const [cancelReason, setCancelReason] = React.useState("");
  // 삭제 모드: soft=표시만 삭제(기본, deleted로 두고 이유를 기록, 데이터 유지) | hard=진짜 삭제(독점 자손을 계단식으로 제거).
  const [deleteMode, setDeleteMode] = React.useState<"soft" | "hard">("soft");
  const [workerMessage, setWorkerMessage] = React.useState("");
  const [workerMessageRequestId, setWorkerMessageRequestId] = React.useState("");
  const [workerMessageSending, setWorkerMessageSending] = React.useState(false);
  // 방법 1 파일 업로드: 고른 첨부(이미 작업 작업 디렉터리 uploads/에 놓임). 다음 메시지와 함께 보냅니다.
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>([]);
  const [uploading, setUploading] = React.useState(false);
  const fileInputRef = React.useRef<HTMLInputElement>(null);

  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    setUploading(true);
    try {
      const r = await api.chatUpload("task", taskId, Array.from(files));
      setAttachments((prev) => [...prev, ...r.attachments]);
    } catch (e) {
      toast.error(`업로드 실패:${(e as Error).message}`);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  }

  // 새 메인 에이전트 세션을 시작합니다. 조각 번호만 올라가고, 작업의
  // 그래프·자산·목표는 그대로라 같은 작업을 이어서
  // 깨끗한 맥락으로 대화합니다. 이전 조각은 읽기 전용 기록으로 남아 돌아갈 수 있습니다.
  async function createMainSession() {
    if (creatingMain) return;
    setCreatingMain(true);
    try {
      const r = await api.newMainSession(taskId);
      setMainSegs((prev) => [{ seq: r.seq, created_at: r.created_at }, ...prev.filter((m) => m.seq !== r.seq)]);
      setCurrentSeg(r.current ?? r.seq);
      // 빈 상태를 미리 넣어, 새(빈) 세션이 바로 그려지게 합니다.
      setStore((prev) => ({ ...prev, [mainSessionKey(r.seq)]: { ...emptyState(), loaded: true } }));
      setActiveId(mainSessionId(r.seq));
      setListOpen(false);
      setInput("");
    } catch (e) {
      toast.error(`새 세션 실패:${(e as Error).message}`);
    } finally {
      setCreatingMain(false);
      setConfirmNewMain(false);
    }
  }

  const patchIntentState = React.useCallback((intentId: string, state?: string) => {
    const patch = (rows: TaskNode[]) =>
      state
        ? rows.map((row) => (row.id === intentId ? { ...row, state } : row))
        : rows.filter((row) => row.id !== intentId);
    setIntents(patch);
    setOlderIntents(patch);
  }, []);

  const controlWorker = React.useCallback(
    async (session: Session, action: "pause" | "resume" | "cancel", reason?: string, mode?: "soft" | "hard") => {
      if (!session.intent_id || session.inherited || controllingIntent) return;
      if (action === "cancel" && !reason?.trim()) {
        toast.error("삭제 이유를 입력하세요");
        return;
      }
      setControllingIntent(session.intent_id);
      try {
        const res = await api.controlIntent(taskId, session.intent_id, action, reason, mode);
        if (action === "pause") {
          patchIntentState(session.intent_id, "paused");
          toast.success(`Worker #${session.intent_id} 이(가) 일시정지되었습니다`);
        } else if (action === "resume") {
          patchIntentState(session.intent_id, "open");
          toast.success(`Worker #${session.intent_id} 이(가) 복구되어 다시 수령되기를 기다립니다`);
        } else if (mode === "hard") {
          // 진짜 삭제: 의도와 독점 하위가 물리적으로 제거됨. 그 행을 목록에서 뺌.
          patchIntentState(session.intent_id);
          const d = res.deleted;
          const extra = d ? `(포함 ${d.intents} 의도 / ${d.facts} 사실 / ${d.findings} 발견)` : "";
          toast.success(`Worker #${session.intent_id} 및 전용 하위 항목이 완전히 삭제되었습니다${extra}`);
          setCancelReason("");
        } else {
          // 표시만 삭제: 의도를 deleted로 두고 삭제 이유를 기록하며, 노드와 산출은 유지.
          patchIntentState(session.intent_id, "deleted");
          toast.success(`Worker #${session.intent_id} 이(가) 삭제되었습니다(이유가 기록되었고, 플래너가 이를 바탕으로 다시 계획합니다)`);
          setCancelReason("");
        }
      } catch (error) {
        toast.error(`워커 동작 실패:${(error as Error).message}`);
      } finally {
        setControllingIntent(null);
        setCancelIntent(null);
      }
    },
    [controllingIntent, patchIntentState, taskId],
  );

  // 실시간 스트림 연결 상태. 연결이 끊기면 보이게 하고,
  // "메시지 없음"처럼 조용히 숨기지 않습니다.
  const [sseLive, setSseLive] = React.useState(false);
  // 작업 전체 토큰 합계(모든 에이전트). 백엔드 합계를 주기적으로 읽습니다.
  const [taskTokens, setTaskTokens] = React.useState<TokenTotal | null>(null);
  const [sessionTokens, setSessionTokens] = React.useState<Record<string, SessionTokenUsage>>({});
  const [llmResolutions, setLLMResolutions] = React.useState<TaskLLMResolutions | null>(null);
  // 이 작업의 대기 중 가로채기. 세션에 경고 아이콘을 붙이는 데 씁니다.
  const [pendingIntercepts, setPendingIntercepts] = React.useState<InterceptApprovalRow[]>([]);

  // 실시간 스트림과 로딩을 다시 그리지 않고 들고 있는 ref입니다.
  const snapshotRef = React.useRef(0); // 작업 단위 그 시점 기준 번호 → 실시간 스트림 since=
  const esRef = React.useRef<EventSource | null>(null);
  const activeKeyRef = React.useRef(mainSessionKey(0)); // 지금 세션 키(실시간 분배와 안 읽음용)
  const atBottomRef = React.useRef(true); // 대화 기록이 맨 아래에 붙어 있나?
  const llmToastSeqRef = React.useRef<Set<number>>(new Set());
  const chatStatusRequestRef = React.useRef(0);
  const firstIntentsRef = React.useRef<TaskNode[]>([]);
  // 키마다 요청 번호. 그 키의 늦은 응답은 무시합니다(최신/이전 페이지가
  // 빨리 겹치는 것을 막음). 쓰기는 항상 키로 하므로, 늦은 응답은
  // 자기 세션 캐시만 건드립니다. 지금 보는 세션은 건드리지 않습니다(§7.5).
  const reqTokenRef = React.useRef<Record<string, number>>({});
  // 최신 페이지를 불러오는 중인 키. 처음 로드와 활성 세션 효과가
  // 마운트 때 둘 다 "main"을 원하지 않게 겹침을 막습니다.
  // 처음 로드가 실시간 스트림도 열므로, 그것이 밀리면 안 됩니다.
  const loadingKeysRef = React.useRef<Set<string>>(new Set());

  // ── 저장 도우미 ──────────────────────────────────────────────────────────────
  const patchStore = React.useCallback((key: string, fn: (s: SessionState) => SessionState) => {
    setStore((prev) => ({ ...prev, [key]: fn(prev[key] ?? emptyState()) }));
  }, []);

  // 세션을 처음 열 때 최신 페이지(before=0)를 불러옵니다. 키와 요청 번호로
  // 지켜서, 다른 세션으로 바꿔도 화면이 깨지지 않습니다.
  const loadSession = React.useCallback(
    (key: string) => {
      if (MOCK) return; // 목업은 처음부터 전부를 넣어 둡니다
      if (loadingKeysRef.current.has(key)) return; // 이미 불러오는 중(예: 마운트 때의 main)
      loadingKeysRef.current.add(key);
      const token = (reqTokenRef.current[key] ?? 0) + 1;
      reqTokenRef.current[key] = token;
      patchStore(key, (s) => ({ ...s, loading: true, error: undefined }));

      if (key === "system") {
        void (async () => {
          let since = 0;
          let systemItems: Activity[] = [];
          for (;;) {
            const page = await api.activity(taskId, { since, limit: SYSTEM_SCAN_PAGE });
            if (reqTokenRef.current[key] !== token) return;
            systemItems = mergeBySeq(
              systemItems,
              page.items.filter(
                (item) => item.worker === "system" || item.kind === "llm_switch" || item.kind === "llm_failover",
              ),
            );
            if (page.items.length < SYSTEM_SCAN_PAGE || page.cursor <= since) break;
            since = page.cursor;
          }
          patchStore(key, (s) => {
            const items = mergeBySeq(systemItems, s.items);
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: false,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              lastTs: items.length ? items[items.length - 1].ts : s.lastTs,
              error: undefined,
            };
          });
        })()
          .catch((error) => {
            if (reqTokenRef.current[key] !== token) return;
            patchStore(key, (s) => ({
              ...s,
              loading: false,
              error: (error as Error).message || "불러오기 실패",
            }));
          })
          .finally(() => loadingKeysRef.current.delete(key));
        return;
      }

      api
        .activityHistory(taskId, key, 0, PAGE)
        .then((r) => {
          if (reqTokenRef.current[key] !== token) return; // 더 새로운 요청으로 교체됨
          if (r.snapshotCursor > snapshotRef.current) snapshotRef.current = r.snapshotCursor;
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items); // 그 사이 도착한 실시간 프레임은 유지
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              error: undefined,
            };
          });
        })
        .catch((e) => {
          if (reqTokenRef.current[key] !== token) return;
          patchStore(key, (s) => ({ ...s, loading: false, error: (e as Error).message || "불러오기 실패" }));
        })
        .finally(() => loadingKeysRef.current.delete(key));
    },
    [taskId, patchStore],
  );

  // 세션의 더 오래된 페이지 하나를 불러오고(위로 스크롤), 스크롤 위치는 유지합니다.
  const loadEarlier = React.useCallback(
    (key: string, viewport: () => HTMLElement | null) => {
      const st = store[key];
      if (!st || st.loadingMore || !st.hasMore || !st.earliestSeq) return;
      const vp = viewport();
      const prevH = vp?.scrollHeight ?? 0;
      const prevTop = vp?.scrollTop ?? 0;
      patchStore(key, (s) => ({ ...s, loadingMore: true }));
      api
        .activityHistory(taskId, key, st.earliestSeq, PAGE)
        .then((r) => {
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items);
            return {
              ...s,
              items,
              loadingMore: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : s.earliestSeq,
            };
          });
          requestAnimationFrame(() => {
            const v = viewport();
            if (v) v.scrollTop = prevTop + (v.scrollHeight - prevH);
          });
        })
        .catch(() => {
          patchStore(key, (s) => ({ ...s, loadingMore: false }));
        });
    },
    [taskId, store, patchStore],
  );

  // ── 작업 토큰 합계(작업 전체, 모든 에이전트) ───────────────────────────────────
  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .tokenStats(taskId)
        .then((r) => {
          if (!alive) return;
          setTaskTokens(r.total);
          setSessionTokens(Object.fromEntries(r.sessions.map((item) => [item.session, item])));
        })
        .catch(() => {
          // 주기 조회는 최선을 다할 뿐이고, 다음 주기에 다시 시도합니다.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    setMainChatRunning(null);
    const load = () => {
      const request = ++chatStatusRequestRef.current;
      return api
        .chatStatus(taskId)
        .then(({ running }) => {
          if (alive && chatStatusRequestRef.current === request) setMainChatRunning(running);
        })
        .catch(() => {
          // 마지막으로 확인된 값을 유지합니다. 첫 성공 전에는 최근
          // 활동을 보수적인 대체값으로 씁니다.
        });
    };
    void load();
    const timer = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .taskLLMResolution(taskId)
        .then((value) => {
          if (alive) setLLMResolutions(value);
        })
        .catch(() => {
          // 주기 조회는 최선을 다할 뿐이고, 다음 주기에 다시 시도합니다.
        });
    void load();
    const timer = setInterval(load, 10_000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .interceptTask(taskId)
        .then((rows) => {
          if (alive) setPendingIntercepts(rows.filter((r) => r.status === "pending"));
        })
        .catch(() => {
          // 주기 조회는 최선을 다할 뿐이고, 다음 주기에 다시 시도합니다.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId]);

  // 타이머로 다시 그려, 활동이 오래되면 "스트리밍" 표시가 다시 계산되게 합니다.
  const [, setTick] = React.useState(0);
  React.useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 1500);
    return () => clearInterval(id);
  }, []);

  // ── 첫 로드와 작업 실시간 스트림 하나 ────────────────────────────────────────────────
  // 작업을 열면 메인의 최신 페이지를 읽고, 거기서 작업 단위 기준 번호를 취한 다음
  // 그제야 실시간 스트림 하나를 since=snapshot_cursor로 엽니다. 기록은 id≤커서,
  // 스트림은 id>커서를 빈틈 없이 덮습니다. 스트림은 모든 에이전트를 따라가고, 프레임은
  // session_key로 나눕니다. EventSource는 자동으로 다시 붙고(`id:` 줄 → Last-Event-
  // ID) DB부터 이어 받아, 끊긴 실시간 연결이 스스로 회복됩니다. seq 합치기로 중복을 뺍니다.
  React.useEffect(() => {
    setStore({});
    setIntents([]);
    setIntentAssets([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    setHasLoadedOlderIntentsPage(false);
    setTaskTokens(null);
    setSessionTokens({});
    setLLMResolutions(null);
    setSseLive(false);
    setActiveId(MAIN_ID); // 이전 작업의 오래된 워커 id가 새어 들어오면 안 됩니다
    setMainSegs([{ seq: 0, created_at: "" }]);
    setCurrentSeg(0);
    snapshotRef.current = 0;
    llmToastSeqRef.current = new Set();
    reqTokenRef.current = {};
    // 처음 main 키를 예약해서, 마운트 때 main에도 켜지는 활성 세션 효과가
    // 같은 것을 두 번 불러 여기서 연 실시간 스트림을 밀지 않게 합니다.
    loadingKeysRef.current = new Set([mainSessionKey(0)]);
    let alive = true;

    // 목업 데모: 실시간 스트림 백엔드가 없어, 활동 스냅샷 하나를 받아 세션별로 나눕니다.
    if (MOCK) {
      api
        .activity(taskId)
        .then((r) => {
          if (!alive) return;
          const buckets: SessionStore = {};
          for (const a of r.items) {
            const k = sessionKeyOf(a);
            if (!buckets[k]) buckets[k] = emptyState();
            buckets[k].items.push(a);
          }
          for (const k of Object.keys(buckets)) {
            const st = buckets[k];
            st.items.sort((p, q) => p.seq - q.seq);
            st.loaded = true;
            st.hasMore = false;
            st.earliestSeq = st.items.length ? st.items[0].seq : 0;
            st.lastTs = st.items.length ? st.items[st.items.length - 1].ts : "";
          }
          buckets[mainSessionKey(0)] ??= { ...emptyState(), loaded: true };
          buckets.system ??= { ...emptyState(), loaded: true };
          setStore(buckets);
        })
        .catch(() =>
          setStore({
            [mainSessionKey(0)]: { ...emptyState(), loaded: true },
            system: { ...emptyState(), loaded: true },
          }),
        );
      return () => {
        alive = false;
      };
    }

    const token = (reqTokenRef.current.mainboot ?? 0) + 1;
    reqTokenRef.current.mainboot = token;
    let bootKey = mainSessionKey(0);
    // 메인 에이전트 조각을 먼저 정한 다음, 지금 조각의 기록을 읽고
    // 그 기준 번호부터 실시간 스트림을 엽니다. 스트림은 모든 조각을 따라가고 각
    // 프레임을 session_key(main:<seg>)로 보내므로, 조각을 바꿔도 새 스트림은 필요 없습니다.
    api
      .mainSessions(taskId)
      .then((ms) => {
        if (!alive || reqTokenRef.current.mainboot !== token) throw new Error("superseded");
        const segs = ms.sessions.length ? ms.sessions : [{ seq: 0, created_at: "" }];
        setMainSegs(segs);
        setCurrentSeg(ms.current);
        bootKey = mainSessionKey(ms.current);
        if (bootKey !== mainSessionKey(0)) loadingKeysRef.current.delete(mainSessionKey(0));
        loadingKeysRef.current.add(bootKey);
        setActiveId(mainSessionId(ms.current));
        patchStore(bootKey, (s) => ({ ...s, loading: true }));
        return api.activityHistory(taskId, bootKey, 0, PAGE);
      })
      .then((r) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        snapshotRef.current = r.snapshotCursor;
        patchStore(bootKey, (s) => {
          const items = mergeBySeq(r.items, s.items);
          return {
            ...s,
            items,
            loaded: true,
            loading: false,
            hasMore: r.hasMore,
            earliestSeq: items.length ? items[0].seq : 0,
            unread: 0,
          };
        });
        // 기준 번호부터 작업 실시간 스트림 하나를 엽니다.
        const es = new EventSource(
          sseUrl(`/api/exploration/activity/stream?task=${encodeURIComponent(taskId)}&since=${snapshotRef.current}`),
        );
        esRef.current = es;
        es.onopen = () => setSseLive(true);
        es.onerror = () => setSseLive(false); // EventSource는 자동으로 다시 붙고, DB가 빈 구간을 메웁니다
        es.onmessage = (e) => {
          let a: Activity;
          try {
            a = JSON.parse(e.data) as Activity;
          } catch {
            return; // 깨진 프레임은 무시
          }
          if ((a.kind === "llm_switch" || a.kind === "llm_failover") && !llmToastSeqRef.current.has(a.seq)) {
            llmToastSeqRef.current.add(a.seq);
            const transition = a.metadata?.llm_transition;
            if (transition?.mode === "exhausted" || a.is_error) {
              toast.error(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            } else if (transition?.mode === "automatic") {
              toast.success(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            } else {
              toast.info(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            }
            void api
              .taskLLMResolution(taskId)
              .then((value) => {
                if (alive) setLLMResolutions(value);
              })
              .catch(() => {
                // 이 사건으로 켠 새로고침이 실패하면, 주기적인 해석 조회가 다시 시도합니다.
              });
          }
          const k = sessionKeyOf(a);
          setStore((prev) => {
            const cur = prev[k] ?? emptyState();
            const activeK = activeKeyRef.current;
            // 이미 불러온 세션, 지금 불러오는 세션, 또는 현재
            // 보기에 합칩니다(돌아오면 바로 보이고, 로딩 중에 온 프레임도 잃지 않음).
            // 아직 차갑고 비활성인 세션도 아주 짧은 장부 꼬리를 남겨
            // 기록을 다 안 읽어도 사이드바가 최근 미완료 사용량을 더할 수 있습니다.
            if (!cur.loaded && !cur.loading && k !== activeK) {
              const accountingItems =
                a.kind === "usage" || a.kind === "result" ? mergeBySeq(cur.items, [a]).slice(-4) : cur.items;
              return {
                ...prev,
                [k]: { ...cur, items: accountingItems, lastTs: a.ts, unread: cur.unread + 1 },
              };
            }
            let items = mergeBySeq(cur.items, [a]);
            // 메모리 상한: 넘치면 가장 오래된 것을 자릅니다(위로 스크롤하면 다시 가져옴).
            // 다만 사용자가 이 세션 기록을 읽는 중(위로 스크롤)에는 자르지 않습니다.
            let hasMore = cur.hasMore;
            let earliestSeq = cur.earliestSeq;
            const trimmable = k !== activeK || atBottomRef.current;
            if (trimmable && items.length > MAX_KEEP) {
              items = items.slice(items.length - MAX_KEEP);
              hasMore = true;
              earliestSeq = items[0].seq;
            }
            const unread = k === activeK ? 0 : cur.unread + 1;
            return { ...prev, [k]: { ...cur, items, lastTs: a.ts, unread, hasMore, earliestSeq } };
          });
        };
      })
      .catch((err) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        if ((err as Error).message === "superseded") return;
        patchStore(bootKey, (s) => ({ ...s, loading: false, error: (err as Error).message || "불러오기 실패" }));
      })
      .finally(() => loadingKeysRef.current.delete(bootKey));

    return () => {
      alive = false;
      esRef.current?.close();
      esRef.current = null;
    };
  }, [taskId, patchStore]);

  React.useEffect(() => {
    let active = true;
    const load = () =>
      api
        .taskIntentAssets(taskId)
        .then((assets) => {
          if (active) setIntentAssets(assets);
        })
        .catch(() => {
          // 다음 주기 조회가 다시 시도합니다. 워커 조작과 대화 기록은 그대로 쓸 수 있습니다.
        });
    void load();
    const timer = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [taskId]);

  // ── 워커(의도) 세션 목록 — 페이지로, 첫 페이지만 가볍게 주기 조회 ───────────────
  React.useEffect(() => {
    let active = true;
    firstIntentsRef.current = [];
    setIntents([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    const load = () =>
      api
        .intentsPage(taskId, 0, 300)
        .then((r) => {
          if (!active) return;
          const freshIds = new Set(r.items.map((item) => item.id));
          const oldestFreshId = r.items.reduce((minimum, item) => Math.min(minimum, Number(item.id)), Infinity);
          const displaced = r.hasMore
            ? firstIntentsRef.current.filter((item) => !freshIds.has(item.id) && Number(item.id) < oldestFreshId)
            : [];
          if (displaced.length > 0) {
            setOlderIntents((previous) => {
              const byId = new Map(previous.map((item) => [item.id, item]));
              for (const item of displaced) byId.set(item.id, item);
              return [...byId.values()];
            });
          }
          firstIntentsRef.current = r.items;
          setIntents(r.items);
          setFirstIntentsHasMore(r.hasMore);
        })
        .catch(() => {
          // 주기 조회는 최선을 다할 뿐이고, 다음 주기에 다시 시도합니다.
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [taskId]);

  const loadOlderIntents = React.useCallback(() => {
    if (loadingOlderIntents) return;
    const all = [...intents, ...olderIntents];
    const minId = all.reduce((m, n) => Math.min(m, Number(n.id)), Infinity);
    if (!Number.isFinite(minId)) return;
    setLoadingOlderIntents(true);
    api
      .intentsPage(taskId, minId, 300)
      .then((r) => {
        setOlderIntents((prev) => {
          const seen = new Set([...intents, ...prev].map((n) => n.id));
          return [...prev, ...r.items.filter((n) => !seen.has(n.id))];
        });
        setOlderIntentsHasMore(r.hasMore);
        setHasLoadedOlderIntentsPage(true);
      })
      .catch(() => {
        // 나중에 사람이 다시 시도하면 이 페이지를 또 가져올 수 있습니다.
      })
      .finally(() => setLoadingOlderIntents(false));
  }, [taskId, intents, olderIntents, loadingOlderIntents]);

  // 합치고 중복을 뺀 워커 목록(최신 첫 페이지 + 더 불러온 이전 페이지).
  const allIntents = React.useMemo(() => {
    const byId = new Map<string, TaskNode>();
    for (const n of olderIntents) byId.set(n.id, n);
    for (const n of intents) byId.set(n.id, n); // 새 주기 조회가 오래된 스냅샷보다 우선합니다
    return [...byId.values()].sort((a, b) => Number(b.id) - Number(a.id));
  }, [intents, olderIntents]);
  const intentAssetsByID = React.useMemo(() => {
    const grouped = new Map<string, IntentAsset[]>();
    for (const asset of intentAssets) {
      const key = String(asset.intent_id);
      const current = grouped.get(key);
      if (current) current.push(asset);
      else grouped.set(key, [asset]);
    }
    return grouped;
  }, [intentAssets]);

  const workerSessions = React.useMemo(() => allIntents.map(intentToSession), [allIntents]);
  const intentsHasMore = hasLoadedOlderIntentsPage ? olderIntentsHasMore : firstIntentsHasMore;

  // 워커 세션(의도)마다 제목은 의도
  // 요약에서 만듭니다. 호버 JSON 툴팁용으로 TaskNode 전체도 저장합니다.
  const sessionMeta = React.useMemo(() => {
    const map = new Map<string, { title: string; json: unknown; deleted: boolean; deleteReason: string }>();
    for (const node of allIntents) {
      let title = `Intent ${node.id}`;
      let parsedPayload: unknown = node.payload;
      // 표시만 삭제: 의도 state='deleted'. 삭제 이유는 독립 필드 delete_reason에 있습니다.
      const deleted = node.state === "deleted";
      const deleteReason = node.delete_reason ?? "";
      if (node.payload) {
        try {
          const p = JSON.parse(node.payload);
          parsedPayload = p;
          if (p?.summary) title = String(p.summary);
        } catch {
          title = node.payload.trim() || title;
        }
      }
      const json = { ...node, payload: parsedPayload };
      map.set(node.id, { title, json, deleted, deleteReason });
    }
    return map;
  }, [allIntents]);

  // 이 세션에 대기 중 가로채기가 하나라도 있으면 참입니다.
  // 워커 agent_name 형식: "work#N · #intentID". 메인/플래너는 역할 키로 맞춥니다.
  const hasPendingForSession = React.useCallback(
    (s: Session): boolean => {
      if (s.inherited) return false;
      if (!pendingIntercepts.length) return false;
      if (s.role === "mainagent") return pendingIntercepts.some((r) => r.agent_name === "mainagent");
      if (s.role === "planner") return pendingIntercepts.some((r) => r.agent_name === "planner");
      // 워커: "work#N · #<intentID>"에서 의도 id를 꺼냅니다
      return pendingIntercepts.some((r) => {
        const m = r.agent_name.match(/·\s*#(\d+)$/);
        return m ? m[1] === s.intent_id : false;
      });
    },
    [pendingIntercepts],
  );

  // 각 세션이 마지막으로 보인 활동 시각으로 살아 있음을 판단합니다(1.5초 틱으로 갱신).
  const recentLive = React.useCallback(
    (key: string) => {
      const ts = store[key]?.lastTs;
      if (!ts) return false;
      const t = Date.parse(ts);
      return t > 0 && Date.now() - t < STREAM_WINDOW_MS;
    },
    [store],
  );
  const currentMainKey = mainSessionKey(currentSeg);
  const plannerLive = recentLive("plan");

  // 지금 스트리밍 중인 메인 조각. 메인 차례는 작업당 하나라, 동시에
  // 살아 있는 조각은 최대 하나입니다. "최근 활동"이 아니라 실제 실행 표시(sending / mainChatRunning)로
  // 판단하고, 차례의 끝 기록
  // (kind='result' 또는 오류)이 오면 바로 내립니다. 아니면 배지가 STREAM_WINDOW_MS 동안
  // 에이전트가 끝난 뒤에도 남습니다. null이면 실행 중인 것이 없습니다.
  const liveMainSeg = React.useMemo<number | null>(() => {
    if (!(sending || mainChatRunning)) return null;
    // 스트리밍 조각은 활동이 가장 최근인 쪽입니다(방금 보낸 차례 포함)
    let seg = currentSeg;
    let bestTs = -1;
    for (const m of mainSegs) {
      const raw = store[mainSessionKey(m.seq)]?.lastTs;
      const ts = raw ? Date.parse(raw) : -1;
      if (ts > bestTs) {
        bestTs = ts;
        seg = m.seq;
      }
    }
    const items = store[mainSessionKey(seg)]?.items ?? [];
    const last = items[items.length - 1];
    if (last && (last.kind === "result" || (last.kind === "text" && last.is_error))) return null;
    return seg;
  }, [sending, mainChatRunning, mainSegs, store, currentSeg]);

  // 메인 에이전트 조각은 모두 독립된 대화 세션입니다(위쪽
  // 채팅 대화와 같음). 어느 조각이든 말할 수 있고, 최신이 먼저입니다.
  const mainSessions = React.useMemo<Session[]>(
    () =>
      mainSegs.map((m) => ({
        id: mainSessionId(m.seq),
        role: "mainagent",
        title: mainSessionTitle(m.seq),
        status: "running",
        live: m.seq === liveMainSeg,
        last_activity: m.created_at,
        seg: m.seq,
      })),
    [mainSegs, liveMainSeg],
  );

  const sessions = React.useMemo(() => {
    const items = [...mainSessions, { ...PLANNER_SESSION, live: plannerLive }, ...workerSessions, SYSTEM_SESSION];
    const located = focusSession ?? retainedSession;
    if (located && !items.some((s) => s.id === located.id)) items.push(located);
    return items;
  }, [mainSessions, workerSessions, plannerLive, focusSession, retainedSession]);

  const grouped = {
    mainagent: sessions.filter((s) => s.role === "mainagent"),
    planner: sessions.filter((s) => s.role === "planner"),
    worker: sessions.filter((s) => s.role === "worker"),
    system: sessions.filter((s) => s.role === "system"),
  };

  const active = sessions.find((s) => s.id === activeId) ?? MAIN_SESSION;
  const side = useSideQuestions(
    active.role === "mainagent"
      ? `/api/tasks/${taskId}/chat`
      : active.role === "worker" && !active.inherited && active.intent_id
        ? `/api/tasks/${taskId}/intents/${active.intent_id}`
        : null,
  );
  const isMain = active.role === "mainagent";
  const isPlanner = active.role === "planner";
  const isSystem = active.role === "system";
  const activeKey = keyForSession(active);
  const activeState = store[activeKey];
  // 메인 차례는 작업당 하나라(채팅 잠금), "바쁨"은 작업 전체입니다. 어떤
  // 메인 조각이든 차례 중이면 활성 입력칸을 막습니다. 그 순간
  // 활성 세션의 끝 기록(kind='result' 또는 오류)이 오면 풀어서, 입력이
  // 다음 채팅 상태 조회를 기다리지 않고 바로 다시 켜지게 합니다.
  const activeItems = activeState?.items ?? [];
  const activeLast = activeItems[activeItems.length - 1];
  const activeSettled =
    !!activeLast && (activeLast.kind === "result" || (activeLast.kind === "text" && activeLast.is_error));
  const mainBusy = isMain && (sending || (!activeSettled && (mainChatRunning ?? recentLive(activeKey))));
  // 접힌 상태(휴대폰)의 제목 줄이 목록 전체를 대신합니다. 현재 세션 이름 + 다른 세션의 안 읽음 합계를 보여 주고,
  // 그렇지 않으면 접은 뒤 어느 세션을 보는지 모르고, 다른 곳의 새 메시지도 보이지 않습니다.
  const activeDisplayTitle = (active.role === "worker" ? sessionMeta.get(active.id)?.title : "") || active.title;
  const hiddenUnread = React.useMemo(
    () => Object.entries(store).reduce((sum, [key, s]) => (key === activeKey ? sum : sum + s.unread), 0),
    [store, activeKey],
  );

  // 실시간 분배기가 아는 활성 세션을 최신으로 유지하고, 활성 세션이 바뀔 때
  // 나중에 불러오며 안 읽음을 지웁니다.
  // biome-ignore lint/correctness/useExhaustiveDependencies: 캐시 갱신이 현재 세션을 다시 활성화하면 안 됩니다.
  React.useEffect(() => {
    activeKeyRef.current = activeKey;
    const st = store[activeKey];
    if (!st || (!st.loaded && !st.loading)) {
      loadSession(activeKey);
    } else if (st.unread) {
      patchStore(activeKey, (s) => ({ ...s, unread: 0 }));
    }
  }, [activeKey]);

  const focusKey = approvalFocus.state?.source?.session;
  const loadFocusPage = React.useCallback(
    (before: number) => api.activityHistory(taskId, focusKey ?? "main", before, PAGE),
    [taskId, focusKey],
  );
  const mergeFocusPage = React.useCallback(
    (page: { items: Activity[]; hasMore: boolean }) => {
      if (!focusKey) return;
      patchStore(focusKey, (s) => {
        const items = mergeBySeq(page.items, s.items);
        return { ...s, items, hasMore: page.hasMore, earliestSeq: items[0]?.seq ?? s.earliestSeq };
      });
    },
    [focusKey, patchStore],
  );
  const focusHistory = useApprovalHistory(
    approvalFocus.state?.source,
    !!focusKey && !!store[focusKey]?.loaded,
    focusKey ? (store[focusKey]?.items ?? []) : [],
    loadFocusPage,
    mergeFocusPage,
  );
  React.useEffect(() => {
    if (approvalFocus.state) atBottomRef.current = false;
  }, [approvalFocus.state]);

  // 메인 에이전트는 사람과 조율자의 콘솔입니다. 대화만 보여 줍니다(사용자 메시지와
  // 메인 에이전트 자신의 답과 단계). 플래너 세션은 플래너 단계를,
  // 워커 세션은 그 의도의 활동만, 의도 목표를 앞에 두고 보여 줍니다.
  const activity = React.useMemo(() => {
    const items = activeState?.items ?? [];
    // 메인 에이전트 콘솔은 서버 데이터만으로 그립니다. 사람 차례는
    // 백엔드가 응답을 돌려주기 전에 저장하고 방송하므로,
    // 다른 에이전트 단계와 같은 실시간 스트림(worker="mainagent")으로 옵니다. 브라우저에서
    // 먼저 그리지 않으므로, 진짜 DB id와 부딪힐 가짜 seq도 없습니다.
    if (isMain) return items;
    if (isPlanner || isSystem) return items;
    // 워커 세션: 의도가 대화 기록 맨 앞에 오른쪽 "사용자"형
    // 메시지로 옵니다(이 워커에게 맡긴 일). 그 다음 실행 단계가 이어집니다.
    const intentTitle = sessionMeta.get(active.id)?.title ?? active.title;
    const intentMsg: Activity = {
      seq: -1, // 실제 단계보다 앞에 정렬됩니다(실제 seq는 0 이상)
      worker: items[0]?.worker ?? active.id, // 같은 레인을 써서 워커 칩이 보이지 않게 합니다
      ts: active.last_activity || "",
      kind: "intent", // 모델이 만든 목표. 사람 말풍선과 구분되는 말풍선으로 그립니다
      summary: intentTitle,
      source_task_id: active.source_task_id,
      inherited: active.inherited,
    };
    return [intentMsg, ...items];
  }, [
    isMain,
    isPlanner,
    isSystem,
    activeState,
    active.id,
    active.title,
    active.last_activity,
    active.source_task_id,
    active.inherited,
    sessionMeta,
  ]);

  // 이 세션에서 가장 최근 TodoWrite 호출의 seq. 할 일 팝오버용.
  const latestTodoSeq = React.useMemo(() => {
    for (let i = activity.length - 1; i >= 0; i--) {
      const a = activity[i];
      if (a.kind === "tool_use" && a.tool === "TodoWrite") return a.seq;
    }
    return null;
  }, [activity]);

  // 저장된 결과 합계는 백엔드의 전체 활동 기록에서 옵니다.
  // 아직 끝나지 않은 최신 사용량 프레임만 브라우저에서 더하므로, 일부 기록
  // 페이지가 끝난 실행을 덜 세거나 진행 중을 두 번 세지 않습니다.
  const tokenForSession = React.useCallback(
    (key: string): TokenTotal => {
      const saved = sessionTokens[key];
      const total: TokenTotal = {
        input_tokens: saved?.input_tokens ?? 0,
        output_tokens: saved?.output_tokens ?? 0,
        cache_read_tokens: saved?.cache_read_tokens ?? 0,
        cache_write_tokens: saved?.cache_write_tokens ?? 0,
      };
      const items = store[key]?.items ?? [];
      for (let index = items.length - 1; index >= 0; index--) {
        const item = items[index];
        if (item.kind === "result") break;
        if (item.kind !== "usage") continue;
        total.input_tokens += item.input_tokens ?? 0;
        total.output_tokens += item.output_tokens ?? 0;
        total.cache_read_tokens += item.cache_read_tokens ?? 0;
        total.cache_write_tokens += item.cache_write_tokens ?? 0;
        break;
      }
      return total;
    },
    [sessionTokens, store],
  );

  const activeTokens = tokenForSession(activeKey);
  const tokenTotal = {
    i: activeTokens.input_tokens,
    o: activeTokens.output_tokens,
    cr: activeTokens.cache_read_tokens,
    any:
      activeTokens.input_tokens +
        activeTokens.output_tokens +
        activeTokens.cache_read_tokens +
        activeTokens.cache_write_tokens >
      0,
  };

  // 실행 시간 = 이 세션의 첫 단계에서 마지막 단계까지(실시간
  // 세션은 "지금"까지라 숫자가 올라갑니다. 위의 1.5초 setTick이 다시 그립니다).
  const runDuration = React.useMemo(() => {
    let min = Infinity,
      max = 0;
    for (const a of activity) {
      const t = Date.parse(a.ts);
      if (!Number.isFinite(t)) continue;
      if (t < min) min = t;
      if (t > max) max = t;
    }
    if (!Number.isFinite(min) || max === 0) return null;
    const end = active.live ? Date.now() : max;
    return Math.max(0, end - min);
  }, [activity, active.live]);

  // ---- 대화 기록 자동 스크롤(열면 맨 아래, 올려 보면 맨 아래에 붙이지 않음) ----
  const contentRef = React.useRef<HTMLDivElement | null>(null);
  const viewport = React.useCallback(
    () => (contentRef.current?.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null) ?? null,
    [],
  );
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId가 바뀌면 스크롤 듣기를 일부러 다시 겁니다.
  React.useEffect(() => {
    const vp = viewport();
    if (!vp) return;
    const onScroll = () => {
      if (approvalFocus.state && !focusHistory.ready) return;
      atBottomRef.current = vp.scrollTop + vp.clientHeight >= vp.scrollHeight - 60;
      if (vp.scrollTop <= 80) loadEarlier(activeKeyRef.current, viewport); // 맨 위 근처 → 더 오래된 페이지
    };
    vp.addEventListener("scroll", onScroll, { passive: true });
    return () => vp.removeEventListener("scroll", onScroll);
  }, [viewport, activeId, loadEarlier, approvalFocus.state, focusHistory.ready]);
  // 세션을 열거나 바꾸면 최신(맨 아래)으로 점프
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId가 바뀌면 새로 고른 세션을 일부러 스크롤합니다.
  React.useLayoutEffect(() => {
    const vp = viewport();
    if (vp && !approvalFocus.state) {
      vp.scrollTop = vp.scrollHeight;
      atBottomRef.current = true;
    }
  }, [activeId, viewport, approvalFocus.state]);
  // 새 활동은 사용자가 이미 맨 아래에 붙어 있을 때만 맨 아래에 붙입니다
  // biome-ignore lint/correctness/useExhaustiveDependencies: 활동이 늘면 실시간 끝 스크롤을 일부러 갱신합니다.
  React.useLayoutEffect(() => {
    if (approvalFocus.state || !atBottomRef.current) return;
    const vp = viewport();
    if (vp) vp.scrollTop = vp.scrollHeight;
  }, [activity, viewport, approvalFocus.state]);
  // 느린 상세 로드(AnswerBlock / ToolBlock / Markdown)는 활동
  // 배열이 잠잠해진 뒤에 내용을 키우고, 배열 참조는 바꾸지 않습니다. 그래서 위 레이아웃 효과는
  // 다시 실행되지 않고, 방금 연 세션의 마지막 메시지가
  // 화면 밖으로 잘릴 수 있습니다(최종 답이 한 줄 요약에서
  // 접힌 곳 아래의 전체 마크다운으로 커짐). ResizeObserver가 맨 아래에 있는 동안
  // 높이가 변하면 다시 맨 아래에 붙여, 메인 에이전트를 열면
  // 마지막 메시지가 전부 보이게 합니다. contentRef의 div는 항상 마운트되므로
  // 관찰자가 대화 기록이 붙는 것과 상세가 커지는 것을 모두 잡습니다.
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId가 바뀌면 관찰자를 새 세션 내용에 일부러 다시 겁니다.
  React.useEffect(() => {
    const el = contentRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      if (approvalFocus.state || !atBottomRef.current) return;
      const vp = viewport();
      if (vp) vp.scrollTop = vp.scrollHeight;
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [activeId, viewport, approvalFocus.state]);

  function stop() {
    if (stopping) return;
    setStopping(true);
    void api.stopChat(taskId).finally(() => setStopping(false));
  }

  function send() {
    const text = input.trim();
    const atts = attachments;
    if (side.handleCommand(text, () => setInput(""))) return;
    if ((!text && atts.length === 0) || sending || mainBusy) return;
    // 먼저 그리지 않습니다. 백엔드는 사람 차례를 응답 전에 저장하고 방송하므로
    // 돌아오기 전에 실시간 스트림(worker="mainagent")으로 진짜 DB
    // seq와 함께 흘러옵니다. 대화 기록은 다른 단계처럼 서버 데이터로 그립니다. 반응을 위해
    // 입력칸은 바로 비우고, 보내기가 실패하면 되돌립니다.
    setInput("");
    setAttachments([]);
    setSending(true);
    chatStatusRequestRef.current++;
    api
      .chat(text, taskId, atts.length > 0 ? atts : undefined, active.seg ?? 0)
      .then(({ mode }) => {
        chatStatusRequestRef.current++;
        setMainChatRunning(mode === "llm");
      })
      .catch((e) => {
        setInput(text); // 사용자가 글과 첨부를 잃지 않게 되돌립니다
        setAttachments(atts);
        toast.error(`전송 실패:${(e as Error).message || "잠시 후 다시 시도하세요"}`);
      })
      .finally(() => setSending(false));
  }

  function sendWorkerChat() {
    const intentId = active.intent_id;
    const message = workerMessage.trim();
    if (side.handleCommand(message, () => setWorkerMessage(""))) return;
    if (!intentId || active.inherited || active.status !== "paused" || workerMessageSending || !message) return;
    if (workerMessageCharCount(message) > MAX_WORKER_MESSAGE_CHARS) {
      toast.error(`메시지는 다음을 넘을 수 없습니다 ${MAX_WORKER_MESSAGE_CHARS} 자`);
      return;
    }
    const requestId = workerMessageRequestId || newWorkerMessageRequestID();
    if (!workerMessageRequestId) setWorkerMessageRequestId(requestId);
    setWorkerMessageSending(true);
    api
      .sendWorkerMessage(taskId, intentId, message, requestId)
      .then((result) => {
        // 서버가 사용자 차례를 기록하고 실행을 실시간 스트림으로 보내므로
        // 먼저 끼워 넣지 않습니다. 메시지와 이어지는 내용이 실시간으로 옵니다.
        patchIntentState(intentId, result.state);
        setWorkerMessage("");
        setWorkerMessageRequestId("");
        toast.success(`메시지를 워커 #${intentId}, 바로 계속 실행함`);
      })
      .catch((error) => {
        toast.error(`전송 실패:${(error as Error).message || "잠시 후 다시 시도하세요"}`);
      })
      .finally(() => setWorkerMessageSending(false));
  }

  const mainLoaded = !!store[currentMainKey]?.loaded;
  // 전송 키는 시스템 설정이 정합니다(localStorage). 기본은 Enter로 전송.
  const sendMode = useChatSendMode();
  // 활성 세션의 대화 기록 칸에 무엇을 보일지.
  const showLoader = !activeState || (activeState.loading && !activeState.loaded);
  const resolutionForSession = (session: Session): TaskLLMResolution | undefined => {
    if (!llmResolutions || session.inherited || session.role === "system") return undefined;
    if (session.role === "mainagent") return llmResolutions.mainagent;
    if (session.role === "planner") return llmResolutions.planner;
    return llmResolutions.worker;
  };
  const activeResolution = resolutionForSession(active);
  const activeAssets =
    active.role === "worker" && active.intent_id ? intentAssetsByID.get(active.intent_id) : undefined;

  return (
    <TooltipProvider delayDuration={300}>
      {/* 높이 예약: 페이지 머리(제목 줄 + 목표 + Tabs ≈ 7.5rem) + 내용 안쪽 여백. 휴대폰은 p-4,
        데스크톱은 lg:p-6이고 데스크톱은 스크롤 여유도 남겨야 하므로 두 단계에 10rem / 13rem을 예약합니다. 휴대폰에서
        13rem을 그대로 쓰면 기록 높이 3rem을 괜히 잃습니다. */}
      <div
        className={cn(
          "grid h-[calc(100svh-10rem)] min-h-0 grid-cols-1 gap-4",
          listOpen ? "grid-rows-[minmax(0,40svh)_minmax(0,1fr)]" : "grid-rows-[auto_minmax(0,1fr)]",
          "lg:h-[calc(100svh-13rem)] lg:grid-cols-[18rem_1fr] lg:grid-rows-[minmax(0,1fr)]",
        )}
      >
        {/* 왼쪽: 세션 목록 */}
        <div className="flex min-h-0 flex-col overflow-hidden rounded-lg border bg-card">
          <div className="border-b px-3 py-2">
            <div className="flex items-center justify-between gap-2">
              <button
                type="button"
                onClick={() => setListOpen((open) => !open)}
                aria-expanded={listOpen}
                aria-controls="session-list-panel"
                className="flex min-w-0 flex-1 items-center gap-1 text-left text-xs font-medium text-muted-foreground lg:pointer-events-none"
              >
                <ChevronDownIcon
                  className={cn("size-3.5 shrink-0 transition-transform lg:hidden", !listOpen && "-rotate-90")}
                />
                <span className="shrink-0">세션 목록</span>
                {!listOpen && (
                  <>
                    <span className="min-w-0 truncate text-foreground lg:hidden" title={activeDisplayTitle}>
                      · {activeDisplayTitle}
                    </span>
                    {hiddenUnread > 0 && (
                      <span className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 lg:hidden dark:text-blue-400">
                        {hiddenUnread > 99 ? "99+" : hiddenUnread}
                      </span>
                    )}
                  </>
                )}
              </button>
              {!MOCK && (
                <span
                  className={cn(
                    "inline-flex items-center gap-1 text-[10px]",
                    sseLive ? "text-emerald-500" : "text-amber-500",
                  )}
                  title={sseLive ? "실시간 연결이 정상입니다" : "실시간 연결이 끊겼습니다. 자동으로 다시 연결하는 중(과거 기록은 그대로 보입니다)"}
                >
                  {sseLive ? (
                    <>
                      <span className="size-1 animate-pulse rounded-full bg-emerald-500" />
                      실시간
                    </>
                  ) : (
                    <>
                      <WifiOffIcon className="size-3" />
                      다시 연결 중
                    </>
                  )}
                </span>
              )}
            </div>
            {taskTokens && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <div className="mt-2 flex min-w-0 flex-wrap items-center gap-1 text-xs text-muted-foreground">
                    <span>작업 합계</span>
                    <span>·</span>
                    <TokenMetrics
                      input={taskTokens.input_tokens}
                      cache={taskTokens.cache_read_tokens}
                      output={taskTokens.output_tokens}
                    />
                  </div>
                </TooltipTrigger>
                <TooltipContent>
                  입력 {taskTokens.input_tokens.toLocaleString()} · 출력 {taskTokens.output_tokens.toLocaleString()} · 캐시 읽기 {taskTokens.cache_read_tokens.toLocaleString()} · 캐시 쓰기{" "}
                  {taskTokens.cache_write_tokens.toLocaleString()}
                </TooltipContent>
              </Tooltip>
            )}
          </div>
          <ScrollArea
            id="session-list-panel"
            type="auto"
            className={cn(
              "min-h-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!",
              !listOpen && "max-lg:hidden",
            )}
          >
            <div className="flex w-full flex-col gap-3 p-2">
              {(["mainagent", "planner", "system", "worker"] as const).map((role) => {
                const items = grouped[role];
                if (!items.length) return null;
                const Meta = roleMeta[role];
                return (
                  <div key={role} className="flex flex-col gap-0.5">
                    <div className="flex items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted-foreground">
                      <Meta.icon className="size-3.5" />
                      {Meta.label}
                      {role === "mainagent" && (
                        <button
                          type="button"
                          onClick={() => setConfirmNewMain(true)}
                          disabled={creatingMain}
                          title="새 메인 에이전트 세션(컨텍스트는 비우고, 작업 상태는 유지)"
                          aria-label="새 메인 에이전트 세션"
                          className="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:bg-accent/60 hover:text-foreground disabled:opacity-50"
                        >
                          {creatingMain ? (
                            <Loader2Icon className="size-3.5 animate-spin" />
                          ) : (
                            <PlusIcon className="size-3.5" />
                          )}
                          새로 만들기
                        </button>
                      )}
                    </div>
                    {items.map((s) => {
                      const meta = s.role === "worker" ? sessionMeta.get(s.id) : undefined;
                      return (
                        <SessionItem
                          key={s.id}
                          s={s}
                          active={s.id === activeId}
                          displayTitle={meta?.title ?? s.title}
                          hasPending={hasPendingForSession(s)}
                          unread={store[keyForSession(s)]?.unread}
                          onClick={() => {
                            approvalFocus.close();
                            setActiveId(s.id);
                            setListOpen(false); // 휴대폰에서는 고르면 바로 접어, 높이를 세션 기록에 돌려줍니다
                            setWorkerMessage("");
                            setWorkerMessageRequestId("");
                          }}
                          controlling={controllingIntent === s.intent_id}
                          deleted={meta?.deleted}
                          onCancel={() => {
                            setCancelIntent(s);
                          }}
                        />
                      );
                    })}
                    {role === "worker" && intentsHasMore && (
                      <button
                        type="button"
                        onClick={loadOlderIntents}
                        disabled={loadingOlderIntents}
                        className="mt-0.5 flex items-center justify-center gap-1 rounded-md px-2 py-1 text-xs text-muted-foreground hover:bg-accent/50"
                      >
                        {loadingOlderIntents ? (
                          <Loader2Icon className="size-3.5 animate-spin" />
                        ) : (
                          <RotateCwIcon className="size-3.5" />
                        )}
                        더 이전 워커 불러오기
                      </button>
                    )}
                  </div>
                );
              })}
              {mainLoaded && !workerSessions.length && (
                <div className="px-2 py-1 text-xs text-muted-foreground">실행 중인 워커 세션이 없습니다.</div>
              )}
            </div>
          </ScrollArea>
        </div>

        {/* 오른쪽: 대화 기록 */}
        <SideQuestionWorkspace
          side={side}
          label={active.role === "worker" ? `Worker #${active.intent_id} · ${activeDisplayTitle}` : activeDisplayTitle}
        >
          <div className="flex min-h-0 flex-1 min-w-0 flex-col overflow-hidden rounded-lg border bg-card">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 border-b px-3 py-2 sm:px-4 sm:py-2.5">
              {(() => {
                const isWorker = active.role === "worker";
                const meta = isWorker ? sessionMeta.get(active.id) : undefined;
                // 워커: 의도는 대화 기록 속 메시지로 옮겨 갔으므로
                // 머리에는 안정된 일반 이름을 보여 줍니다(의도 JSON은 호버에 남음).
                const title = isWorker ? "워커 실행 세션" : active.title;
                const titleEl = <span className="min-w-0 truncate text-sm font-medium">{title}</span>;
                return meta?.json ? (
                  <Tooltip>
                    <TooltipTrigger asChild>{titleEl}</TooltipTrigger>
                    <TooltipContent side="bottom" align="start" className="max-h-80 max-w-sm overflow-auto p-0">
                      <pre className="p-2 text-[10px] leading-relaxed">{JSON.stringify(meta.json, null, 2)}</pre>
                    </TooltipContent>
                  </Tooltip>
                ) : (
                  titleEl
                );
              })()}
              <SideQuestionButton side={side} />
              {activeResolution && (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Badge
                      variant="outline"
                      className="max-w-28 shrink-0 font-normal"
                      aria-label={
                        activeResolution.available ? `현재 설정:${resolutionLabel(activeResolution)}` : "모델을 사용할 수 없음"
                      }
                    >
                      <span className="truncate">
                        {activeResolution.available ? resolutionLabel(activeResolution) : "모델을 사용할 수 없음"}
                      </span>
                    </Badge>
                  </TooltipTrigger>
                  <TooltipContent side="bottom" className="max-w-xs [overflow-wrap:anywhere]">
                    {activeResolution.available
                      ? [resolutionLabel(activeResolution), activeResolution.model].filter(Boolean).join(" / ")
                      : activeResolution.reason || "사용 가능한 LLM 설정이 없습니다"}
                  </TooltipContent>
                </Tooltip>
              )}
              {activeAssets && activeAssets.length > 0 && <WorkerAssetBadge assets={activeAssets} />}
              {active.inherited && active.source_task_id && (
                <Badge variant="outline">출처 작업 #{active.source_task_id} · 읽기 전용 기록</Badge>
              )}
              {active.live && (
                <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
                  <span className="size-1 animate-pulse rounded-full bg-blue-500" />
                  실시간
                </span>
              )}
              {activeState?.hasMore && (
                <span className="text-[10px] text-muted-foreground" title="위로 스크롤하면 더 이전 기록을 불러옵니다">
                  ↑ 이전 기록
                </span>
              )}
              <div className="ml-auto flex min-w-0 max-w-full items-center justify-end gap-x-3 gap-y-1 text-xs text-muted-foreground max-sm:w-full max-sm:flex-wrap">
                {tokenTotal.any && (
                  <Tooltip>
                    {/* 휴대폰에서는 짧은 라벨(입/캐/출)을 씁니다. 긴 라벨은 이 줄을 두 줄로 밀어 기록 영역을 더 줄입니다. */}
                    <TooltipTrigger asChild>
                      <span className="inline-flex min-w-0 items-center">
                        <TokenMetrics
                          input={tokenTotal.i}
                          cache={tokenTotal.cr}
                          output={tokenTotal.o}
                          labels="short"
                          className="sm:hidden"
                        />
                        <TokenMetrics
                          input={tokenTotal.i}
                          cache={tokenTotal.cr}
                          output={tokenTotal.o}
                          labels="long"
                          className="max-sm:hidden"
                        />
                      </span>
                    </TooltipTrigger>
                    <TooltipContent>
                      입력 {activeTokens.input_tokens.toLocaleString()} · 출력{" "}
                      {activeTokens.output_tokens.toLocaleString()} · 캐시 읽기{" "}
                      {activeTokens.cache_read_tokens.toLocaleString()} · 캐시 쓰기{" "}
                      {activeTokens.cache_write_tokens.toLocaleString()}
                    </TooltipContent>
                  </Tooltip>
                )}
                {runDuration != null && (
                  <span className="inline-flex items-center gap-1" title="실행 시간(첫 단계 → 마지막 단계)">
                    <ClockIcon className="size-3" />
                    {fmtDuration(runDuration)}
                  </span>
                )}
                {isMain && <span>조작 가능</span>}
              </div>
            </div>
            {(() => {
              const dm = active.role === "worker" ? sessionMeta.get(active.id) : undefined;
              if (!dm?.deleted) return null;
              return (
                <div className="flex items-start gap-2 border-b border-destructive/30 bg-destructive/5 px-4 py-2.5 text-xs">
                  <Trash2Icon className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                  <div className="min-w-0">
                    <span className="font-medium text-destructive">이 의도는 사용자가 삭제했습니다</span>
                    <span className="text-muted-foreground">
                      (실행이 중지되었습니다. 플래너에게 알렸습니다. 의도와 결과는 남아 있으며, 아래에서 기록을 볼 수 있습니다)
                    </span>
                    {dm.deleteReason && (
                      <p className="mt-1 break-words text-foreground">
                        <span className="text-muted-foreground">삭제 이유:</span>
                        {dm.deleteReason}
                      </p>
                    )}
                  </div>
                </div>
              );
            })()}
            <ApprovalExecutionFocus
              focus={{
                ...approvalFocus,
                close: () => {
                  setActiveId(activeId);
                  approvalFocus.close();
                },
              }}
              history={focusHistory}
            />
            {/* Radix 안쪽 뷰포트(display:table이라 내용만큼 커짐)를
            block으로 바꿔, 긴 명령·코드·주소가
            너비를 밀어 아래 말줄임을 깨지 않게 합니다. 대화 기록은
            패널 너비에 맞춰 줄바꿈되고 가로로 넘치지 않습니다. */}
            <ScrollArea type="auto" className="min-h-0 min-w-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!">
              <div className="min-w-0 max-w-full p-4" ref={contentRef}>
                {activeState?.loadingMore && (
                  <div className="flex items-center justify-center gap-2 pb-2 text-xs text-muted-foreground">
                    <Loader2Icon className="size-3.5 animate-spin" />
                    이전 기록 더 불러오기…
                  </div>
                )}
                {showLoader ? (
                  <div className="flex items-center gap-2 pl-9 text-xs text-muted-foreground">
                    <Loader2Icon className="size-3.5 animate-spin" />
                    활동 흐름 불러오는 중…
                  </div>
                ) : activeState?.error ? (
                  <div className="flex items-center gap-2 pl-9 text-xs text-red-500">
                    <CircleXIcon className="size-3.5" />
                    불러오기 실패:{activeState.error}
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 px-2 text-xs"
                      onClick={() => loadSession(activeKey)}
                    >
                      재시도
                    </Button>
                  </div>
                ) : activity.length ? (
                  <Transcript
                    activity={activity}
                    live={active.live}
                    taskId={taskId}
                    chat={isMain}
                    focusedSeq={focusHistory.ready ? approvalFocus.state?.source?.seq : undefined}
                  />
                ) : (
                  <div className="pl-9 text-xs text-muted-foreground">
                    {isMain ? "아직 대화가 없습니다. 아래에서 메인 에이전트에게 메시지를 보내 탐색 방향을 안내하거나 흐름에 개입하세요." : "활동 기록이 아직 없습니다."}
                  </div>
                )}
              </div>
            </ScrollArea>
            {isMain ? (
              <div className="border-t p-3">
                {attachments.length > 0 && (
                  <div className="mb-2 flex flex-wrap gap-1.5">
                    {attachments.map((a) => (
                      <div
                        key={a.path}
                        className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
                        title={a.path}
                      >
                        <PaperclipIcon className="size-3 shrink-0 text-primary" />
                        <span className="max-w-[160px] truncate">{a.name}</span>
                        <span className="text-muted-foreground">{fmtBytes(a.size)}</span>
                        <button
                          type="button"
                          className="ml-0.5 text-muted-foreground hover:text-foreground"
                          onClick={() => setAttachments((p) => p.filter((x) => x.path !== a.path))}
                          title="제거"
                        >
                          <XIcon className="size-3" />
                        </button>
                      </div>
                    ))}
                  </div>
                )}
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => void pickFiles(e.target.files)}
                />
                <InputGroup className="min-h-9 has-disabled:opacity-100">
                  <MentionTextarea
                    inputGroup
                    rows={1}
                    aria-label="메인 에이전트에게 메시지 보내기"
                    placeholder={
                      mainBusy ? "메인 에이전트가 실행 중입니다. /btw로 질문할 수 있습니다…" : "메인 에이전트에게 메시지 보내기, @로 발견, 자산 등을 인용…"
                    }
                    value={input}
                    disabled={sending}
                    onValueChange={setInput}
                    onKeyDown={(e) => {
                      if (!shouldSubmitOnKey(e, sendMode)) return;
                      e.preventDefault();
                      send();
                    }}
                    className="max-h-36 min-h-9 overflow-y-auto"
                  />
                  <InputGroupAddon align="block-end">
                    <InputGroupButton
                      size="icon-xs"
                      variant="ghost"
                      onClick={() => fileInputRef.current?.click()}
                      disabled={mainBusy || uploading}
                      title="파일 업로드"
                      aria-label="파일 업로드"
                    >
                      {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
                    </InputGroupButton>
                    {mainBusy && isBtwCommand(input) && (
                      <InputGroupButton size="icon-xs" onClick={send} aria-label="옆길 질문 보내기">
                        <ArrowUpIcon />
                      </InputGroupButton>
                    )}
                    {mainBusy ? (
                      <InputGroupButton
                        className="ml-auto"
                        size="icon-xs"
                        variant="destructive"
                        onClick={stop}
                        disabled={stopping}
                        title="현재 실행 중지"
                        aria-label="현재 실행 중지"
                      >
                        {stopping ? <Loader2Icon className="animate-spin" /> : <SquareIcon />}
                      </InputGroupButton>
                    ) : (
                      <InputGroupButton
                        className="ml-auto"
                        size="icon-xs"
                        variant="default"
                        onClick={send}
                        disabled={(!input.trim() && attachments.length === 0) || sending}
                        title="메시지 보내기"
                        aria-label="메시지 보내기"
                      >
                        {sending ? <Loader2Icon className="animate-spin" /> : <ArrowUpIcon />}
                      </InputGroupButton>
                    )}
                  </InputGroupAddon>
                </InputGroup>
              </div>
            ) : active.role === "worker" &&
              !active.inherited &&
              (active.status === "running" || active.status === "paused") ? (
              <div className="border-t p-3">
                <InputGroup className="min-h-9 has-disabled:opacity-100">
                  <MentionTextarea
                    inputGroup
                    rows={1}
                    aria-label={`워커 #${active.intent_id} 에게 메시지`}
                    placeholder={`워커 #${active.intent_id} 에게 메시지, @로 기록을 인용하고 실행 방향을 조정…`}
                    value={workerMessage}
                    onValueChange={(value) => {
                      setWorkerMessage(value);
                      setWorkerMessageRequestId("");
                    }}
                    onKeyDown={(event) => {
                      if (!shouldSubmitOnKey(event, sendMode)) return;
                      event.preventDefault();
                      sendWorkerChat();
                    }}
                    disabled={workerMessageSending}
                    aria-invalid={workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS}
                    className="max-h-36 min-h-9 overflow-y-auto"
                  />
                  <InputGroupAddon align="block-end">
                    <span
                      className={cn(
                        "px-1 text-[10px] tabular-nums text-muted-foreground",
                        workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS && "text-destructive",
                      )}
                    >
                      {workerMessageCharCount(workerMessage)}/{MAX_WORKER_MESSAGE_CHARS}
                    </span>
                    {active.status === "running" && isBtwCommand(workerMessage) && (
                      <InputGroupButton size="icon-xs" onClick={sendWorkerChat} aria-label="옆길 질문 보내기">
                        <ArrowUpIcon />
                      </InputGroupButton>
                    )}
                    {active.status === "running" ? (
                      <InputGroupButton
                        className="ml-auto"
                        size="icon-xs"
                        variant="destructive"
                        onClick={() => void controlWorker(active, "pause")}
                        disabled={controllingIntent === active.intent_id}
                        title="현재 워커 일시정지"
                        aria-label="현재 워커 일시정지"
                      >
                        {controllingIntent === active.intent_id ? (
                          <Loader2Icon className="animate-spin" />
                        ) : (
                          <SquareIcon />
                        )}
                      </InputGroupButton>
                    ) : (
                      <>
                        <InputGroupButton
                          className="ml-auto"
                          size="xs"
                          variant="ghost"
                          onClick={() => void controlWorker(active, "resume")}
                          disabled={controllingIntent === active.intent_id || workerMessageSending}
                          title="메시지를 보내지 않고, 바로 계속 실행"
                          aria-label="바로 계속 실행"
                        >
                          {controllingIntent === active.intent_id ? (
                            <Loader2Icon className="animate-spin" />
                          ) : (
                            "바로 계속"
                          )}
                        </InputGroupButton>
                        <InputGroupButton
                          size="icon-xs"
                          variant="default"
                          onClick={sendWorkerChat}
                          disabled={
                            workerMessageSending ||
                            !workerMessage.trim() ||
                            workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS
                          }
                          title="메시지 보내기"
                          aria-label="메시지 보내기"
                        >
                          {workerMessageSending ? <Spinner /> : <ArrowUpIcon />}
                        </InputGroupButton>
                      </>
                    )}
                  </InputGroupAddon>
                </InputGroup>
              </div>
            ) : (
              <div className="flex items-center border-t px-4 py-2">
                <TodoPopover
                  seq={latestTodoSeq}
                  fetchDetail={(seq) => api.activityDetail(seq, taskId).then((r) => r.detail ?? "")}
                />
              </div>
            )}
          </div>
        </SideQuestionWorkspace>
        <AlertDialog
          open={cancelIntent !== null}
          onOpenChange={(open) => {
            if (!open) {
              setCancelIntent(null);
              setCancelReason("");
              setDeleteMode("soft");
            }
          }}
        >
          <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
            <AlertDialogHeader>
              <AlertDialogTitle>워커 # 삭제{cancelIntent?.intent_id}？</AlertDialogTitle>
              <AlertDialogDescription className="break-words whitespace-normal">
                {deleteMode === "hard" ? (
                  <>
                    <strong>완전 삭제</strong>이 의도를 완전히 제거하고, 다음도 함께 제거합니다<strong>이것만으로 유지</strong>
                    의 하위 노드(잎까지 이어서, 고립된 데이터가 남지 않게 함). 공유 노드, 목표, 작업 루트 사실은 남습니다.
                    <strong>이 작업은 복구할 수 없습니다.</strong>플래너는 삭제 알림을 받고 그에 따라 다시 계획합니다.
                  </>
                ) : (
                  <>
                    <strong>임시 삭제</strong>이 의도를 "삭제됨"으로 바꾸고 삭제 이유를 기록합니다. 의도 노드, 실행 기록, 이미 등록된 사실과 발견은<strong>모두 남습니다</strong>. 플래너는 "이 의도를 사용자가 삭제함 + 이유"를 받고 그에 맞춰 다시 계획합니다.
                  </>
                )}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="grid gap-3 py-1">
              <div className="grid grid-cols-2 gap-2">
                <button
                  type="button"
                  onClick={() => setDeleteMode("soft")}
                  className={cn(
                    "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                    deleteMode === "soft" ? "border-primary bg-primary/5" : "hover:bg-accent",
                  )}
                >
                  <div className="font-medium">임시 삭제</div>
                  <div className="text-xs text-muted-foreground">데이터를 남겨 나중에 추적</div>
                </button>
                <button
                  type="button"
                  onClick={() => setDeleteMode("hard")}
                  className={cn(
                    "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                    deleteMode === "hard" ? "border-destructive bg-destructive/5" : "hover:bg-accent",
                  )}
                >
                  <div className="font-medium">완전 삭제</div>
                  <div className="text-xs text-muted-foreground">연쇄로 제거되며, 되돌릴 수 없습니다</div>
                </button>
              </div>
              <div className="grid gap-2">
                <label htmlFor="cancel-reason" className="text-sm font-medium">
                  삭제 이유(필수)
                </label>
                <Textarea
                  id="cancel-reason"
                  value={cancelReason}
                  onChange={(e) => setCancelReason(e.target.value)}
                  placeholder="이 의도를 삭제하는 이유를 적으세요. 예: 방향 판단이 틀림 / 목표가 무효가 됨 / 다른 의도와 중복…"
                  rows={3}
                  autoFocus
                />
              </div>
            </div>
            <AlertDialogFooter>
              <AlertDialogCancel>돌아가기</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                disabled={!cancelIntent || controllingIntent !== null || !cancelReason.trim()}
                onClick={() => cancelIntent && void controlWorker(cancelIntent, "cancel", cancelReason, deleteMode)}
              >
                {controllingIntent ? <Loader2Icon className="animate-spin" /> : <Trash2Icon />}
                {deleteMode === "hard" ? "완전 삭제" : "삭제 확인"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>

        <AlertDialog open={confirmNewMain} onOpenChange={(open) => !open && setConfirmNewMain(false)}>
          <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
            <AlertDialogHeader>
              <AlertDialogTitle>새 세션을 시작할까요?</AlertDialogTitle>
              <AlertDialogDescription className="break-words whitespace-normal">
                현재 세션은 보관됩니다(언제든 다시 전환할 수 있음). 메인 에이전트는 깨끗한 컨텍스트로 이어갑니다. 작업의 그래프, 자산, 목표는 영향받지 않습니다.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={creatingMain}>취소</AlertDialogCancel>
              <AlertDialogAction disabled={creatingMain} onClick={() => void createMainSession()}>
                {creatingMain ? "켜는 중…" : "새 세션 켜기"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </div>
    </TooltipProvider>
  );
}
