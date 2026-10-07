"use client";

import * as React from "react";
import {
  CheckIcon,
  ChevronDown,
  ChevronRight,
  CrosshairIcon,
  Flag,
  MessageSquare,
  PaperclipIcon,
  ShieldAlertIcon,
  Terminal,
  UserIcon,
  Wrench,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { Markdown } from "@/components/markdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { ApprovalDetail } from "@/components/approval-records";
import type { Activity, InterceptPending } from "@/lib/types";

// ---- 에이전트별 레인 색(플래너 + work#1/#2/#3 …) ---------------------------
const workerColors = [
  "bg-sky-600",
  "bg-violet-600",
  "bg-teal-600",
  "bg-pink-600",
  "bg-orange-600",
];
function workerColor(name: string): string {
  if (name === "planner") return "bg-amber-600"; // 의도를 만드는 쪽. 색을 다르게
  if (name === "mainagent") return "bg-primary";
  const m = /#(\d+)/.exec(name);
  const i = m ? (parseInt(m[1], 10) - 1) % workerColors.length : 0;
  return workerColors[Math.max(0, i)];
}

const chip = (worker: string) =>
  "mt-0.5 shrink-0 rounded px-1 text-[9px] font-medium text-white " + workerColor(worker);

// useInView는 ref 요소가 스크롤 영역 `rootMargin` 안에 처음 들어오면
// 참을 고정합니다. 본문을 항상 다 보여 주는 블록(사용자
// 말풍선, 답)은 곧 보이기 직전에 본문을 가져와
// 긴 대화를 열 때 화면 밖 단계마다 상세를 요청하지 않습니다.
// ScrollArea 뷰포트를 관찰합니다(IntersectionObserver가 없으면, 예를 들어 SSR,
// 바로 불러옵니다).
function useInView(rootMargin = "400px"): [React.RefObject<HTMLDivElement | null>, boolean] {
  const ref = React.useRef<HTMLDivElement | null>(null);
  const [inView, setInView] = React.useState(false);
  React.useEffect(() => {
    if (inView) return; // 한 번 보면 관찰을 멈춥니다
    const el = ref.current;
    if (!el) return;
    if (typeof IntersectionObserver === "undefined") {
      setInView(true);
      return;
    }
    const root = el.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null;
    const ob = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setInView(true);
      },
      { root, rootMargin },
    );
    ob.observe(el);
    return () => ob.disconnect();
  }, [inView, rootMargin]);
  return [ref, inView];
}

// 도구 묶음은 tool_use와 같은 tool_use_id의 tool_result를 짝짓습니다.
// 같은 에이전트의 이어진 대화 단계(text/thinking/result)는
// 하나의 "메시지"입니다.
type Group =
  | { type: "user"; key: number; step: Activity; intent?: boolean }
  | { type: "answer"; key: number; step: Activity }
  | { type: "round"; key: number; label: string }
  | { type: "tool"; key: number; worker: string; use?: Activity; result?: Activity }
  | { type: "msg"; key: number; worker: string; steps: Activity[] }
  | { type: "intercept"; key: number; step: Activity };

function groupSteps(steps: Activity[], chat: boolean): Group[] {
  const out: Group[] = [];
  const byToolId = new Map<string, Extract<Group, { type: "tool" }>>();
  for (const s of steps) {
    if (s.kind === "usage") continue; // 실시간 토큰 사용 표시. 그리는 단계가 아님
    if (s.kind === "round") {
      out.push({ type: "round", key: s.seq, label: s.summary || "새 라운드" }); // 플래너 라운드 경계
      continue;
    }
    if (s.kind === "intercept_request") {
      out.push({ type: "intercept", key: s.seq, step: s });
      continue;
    }
    if (s.kind === "user" || s.kind === "intent") {
      // 사람 차례, 또는 워커 세션을 이끄는 모델이 만든 의도. 둘 다
      // 오른쪽 말풍선입니다. `intent`면 사람 아이콘 대신 다른 아이콘을 씁니다.
      out.push({ type: "user", key: s.seq, step: s, intent: s.kind === "intent" });
      continue;
    }
    // 채팅(메인 에이전트)에서 조수의 text/result가 바로 답입니다. 접지 않고
    // 마크다운으로 다 보여 줍니다. thinking은 여전히 작은 블록으로 접습니다.
    if (s.kind === "result" || (chat && s.kind === "text")) {
      out.push({ type: "answer", key: s.seq, step: s });
      continue;
    }
    if (s.kind === "tool_use") {
      const g: Extract<Group, { type: "tool" }> = { type: "tool", key: s.seq, worker: s.worker, use: s };
      if (s.tool_use_id) byToolId.set(s.tool_use_id, g);
      out.push(g);
      continue;
    }
    if (s.kind === "tool_result") {
      // id로 tool_use에 묶습니다(이웃이 아님. 도구는 동시에 돌 수 있음)
      const g = s.tool_use_id ? byToolId.get(s.tool_use_id) : undefined;
      if (g && !g.result) g.result = s;
      else out.push({ type: "tool", key: s.seq, worker: s.worker, result: s }); // 짝이 없는 결과
      continue;
    }
    const last = out[out.length - 1];
    if (last && last.type === "msg" && last.worker === s.worker) last.steps.push(s);
    else out.push({ type: "msg", key: s.seq, worker: s.worker, steps: [s] });
  }
  return out;
}

const kindLabel = (k: string) => (k === "thinking" ? "추론" : k === "result" ? "요약" : "설명");

function ActivityTime({ ts }: { ts: string }) {
  const date = new Date(ts);
  if (!ts || Number.isNaN(date.getTime())) return null;
  return (
    <time
      dateTime={date.toISOString()}
      title={date.toLocaleString("ko-KR")}
      className="text-[10px] text-muted-foreground tabular-nums"
      suppressHydrationWarning
    >
      {date.toLocaleString("ko-KR", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
      })}
    </time>
  );
}

// toolInputText는 tool_use 입력을 보여 줍니다. Bash는 원문 JSON에서
// 셸 명령을 꺼냅니다({"command":…,"description":…}). 화면에는 JSON 대신
// 명령 자체가 보입니다. 다른 도구는 원문으로 돌아갑니다.
function toolInputText(tool: string, raw: string): string {
  if (tool !== "Bash") return raw;
  // 펼친 상세의 전체 입력: 파싱해서 명령을 꺼냅니다.
  try {
    const o = JSON.parse(raw);
    if (o && typeof o.command === "string") return o.command;
  } catch {
    // 접힌 줄 요약은 백엔드가 약 200자로 자르므로
    // JSON 끝이 잘려 JSON.parse가 실패합니다. 그때는 직접
    // "command" 필드를 꺼내고, 닫는 따옴표가 없어도 참습니다.
  }
  const m = raw.match(/"command"\s*:\s*"((?:\\.|[^"\\])*)/);
  if (m) {
    try {
      // 잡은 본문을 다시 감싸 파싱해서 \n, \", \\ 등을 풉니다.
      return JSON.parse('"' + m[1] + '"');
    } catch {
      // 이스케이프 중간에 잘림. 흔한 서열은 최선을 다해 풉니다.
      return m[1].replace(/\\(["\\/nrt])/g, (_s, c) =>
        c === "n" ? "\n" : c === "r" ? "\r" : c === "t" ? "\t" : c,
      );
    }
  }
  return raw;
}

// InterceptCard는 가로채기 승인 카드를 대화 안에 그립니다. pending_id는
// summary에서 꺼냅니다(형식: "도구 X 승인 요청 (#N)"). 그래서 버튼은
// 상세를 기다리지 않아도 바로 쓸 수 있습니다.
function InterceptCard({
  step,
  getDetail,
}: {
  step: Activity;
  getDetail: (seq: number) => Promise<string>;
}) {
  // summary에서 pending_id를 꺼냄: "도구 Bash 승인 요청 (#42)"
  const pendingId = React.useMemo(() => {
    const m = /\(#(\d+)\)/.exec(step.summary);
    return m ? parseInt(m[1], 10) : null;
  }, [step.summary]);

  const toolName = React.useMemo(() => {
    const m = /(?:도구|工具)\s+(\S+)\s+(?:요청|请求)/.exec(step.summary); // han-allow 옛 요약 형식
    return m ? m[1] : step.summary;
  }, [step.summary]);

  const [detail, setDetail] = React.useState<Record<string, unknown> | null>(null);
  const [decided, setDecided] = React.useState<"allowed" | "denied" | "timeout" | null>(null);
  const [deciding, setDeciding] = React.useState(false);
  const [expanded, setExpanded] = React.useState(false);
  const [pending, setPending] = React.useState<InterceptPending | null>(null);
  const [statusError, setStatusError] = React.useState("");
  const [retry, setRetry] = React.useState(0);

  // 저장된 상세 JSON을 읽고, 백엔드의 실제 현재 상태도 확인해서
  // 새로고침 뒤에는 이미 결정된 상태를 보여주고 버튼을 다시 주지 않습니다.
  React.useEffect(() => {
    let live = true;
    getDetail(step.seq)
      .then((raw) => {
        if (!live || !raw) return;
        try { setDetail(JSON.parse(raw)); } catch { /* 무시 */ }
      })
      .catch(() => {/* 무시 */});
    return () => { live = false; };
  }, [step.seq, getDetail]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: 다시 시도는 실패 뒤 같은 승인을 일부러 다시 읽습니다.
  React.useEffect(() => {
    if (!pendingId) return;
    let live = true;
    api.interceptGetOne(pendingId)
      .then((p) => {
        if (!live) return;
        setPending(p);
        setStatusError("");
        if (p.status !== "pending") setDecided(p.status as "allowed" | "denied" | "timeout");
      })
      .catch((error) => { if (live) setStatusError((error as Error).message || "승인 상세 불러오기 실패"); });
    return () => { live = false; };
  }, [pendingId, retry]);

  async function decide(decision: "allowed" | "denied") {
    if (step.inherited || !pendingId || deciding) return;
    setDeciding(true);
    try {
      await api.interceptDecide(pendingId, decision);
      setDecided(decision);
      toast.success(decision === "allowed" ? "실행을 허용함" : "실행을 거부함");
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setDeciding(false);
    }
  }

  const inputStr = detail?.input
    ? JSON.stringify(detail.input).slice(0, 200)
    : null;

  const row = pending ? { ...pending, status: decided || pending.status, conv_title: "", conv_agent_key: "", rule_name: "" } : null;

  return (
    <div className="my-2 rounded-lg border border-amber-400/50 bg-amber-50/40 dark:bg-amber-950/15 p-3 text-xs">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex items-start gap-2 min-w-0">
          <ShieldAlertIcon className="mt-0.5 h-3.5 w-3.5 shrink-0 text-amber-500" />
          <div className="min-w-0 space-y-0.5">
            <div className="flex items-center gap-1.5 font-medium">
              <span className="text-amber-700 dark:text-amber-400">승인 요청</span>
              <code className="rounded bg-amber-100 dark:bg-amber-900/50 px-1 font-mono text-amber-800 dark:text-amber-300">
                {toolName}
              </code>
              {pendingId && (
                <span className="text-muted-foreground">#{pendingId}</span>
              )}
            </div>
            {inputStr && (
              <p className="font-mono text-muted-foreground truncate">{inputStr}</p>
            )}
          </div>
        </div>

        {step.inherited ? (
          <Badge variant="outline">기록 · 읽기 전용</Badge>
        ) : decided ? (
          <span className={
            "shrink-0 rounded px-2 py-0.5 text-[11px] font-medium " +
            (decided === "allowed"
              ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-400"
              : decided === "timeout"
                ? "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400"
                : "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-400")
          }>
            {decided === "allowed" ? "허용함" : decided === "timeout" ? "시간 초과됨" : "거부함"}
          </span>
        ) : (
          <div className="flex shrink-0 gap-1.5">
            <Button
              size="sm"
              className="h-6 px-2 text-xs"
              disabled={deciding || !pendingId}
              onClick={() => decide("allowed")}
            >
              <CheckIcon className="h-3 w-3" />
              허용
            </Button>
            <Button
              size="sm"
              variant="destructive"
              className="h-6 px-2 text-xs"
              disabled={deciding || !pendingId}
              onClick={() => decide("denied")}
            >
              <XIcon className="h-3 w-3" />
              거부
            </Button>
          </div>
        )}
      </div>
      {pendingId ? (
        <Collapsible open={expanded} onOpenChange={setExpanded} className="mt-2 min-w-0">
          <CollapsibleTrigger asChild>
            <Button variant="ghost" size="sm" aria-label={expanded ? "승인 상세 접기" : "승인 상세 펼치기"}>
              {expanded ? <ChevronDown data-icon="inline-start" /> : <ChevronRight data-icon="inline-start" />}
              {expanded ? "승인 상세 접기" : "승인 상세 펼치기"}
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            {expanded && row ? (
              <ApprovalDetail
                row={row}
                busy={deciding}
                decide={(_id, decision) => decide(decision)}
                revision={retry}
                readOnly={Boolean(step.inherited)}
                defaultExpanded
                onResolved={setDecided}
              />
            ) : statusError ? (
              <div className="flex flex-wrap items-center gap-2 p-3" role="alert">
                <span>{statusError}</span>
                <Button variant="outline" size="sm" onClick={() => setRetry((value) => value + 1)}>재시도 상세</Button>
              </div>
            ) : <p className="p-3 text-muted-foreground">승인 상세를 불러오는 중…</p>}
          </CollapsibleContent>
        </Collapsible>
      ) : null}
    </div>
  );
}

// ToolBlock은 도구 호출 하나를 그립니다. 명령과 결과를 한
// 줄에 묶습니다(접히면 명령과 상태/결과 미리보기, 펼치면
// 입력과 출력을 함께). 펼칠 때 두 상세를 나중에 불러옵니다.
function ToolBlock({
  group,
  getDetail,
  showWorker,
  focused = false,
}: {
  group: Extract<Group, { type: "tool" }>;
  getDetail: (seq: number) => Promise<string>;
  showWorker?: boolean;
  focused?: boolean;
}) {
  const [open, setOpen] = React.useState(focused);
  const targetRef = React.useRef<HTMLElement>(null);
  const [detail, setDetail] = React.useState<string | null>(null);
  // 마지막으로 불러온 것. 키는 단계 seq입니다. 실행 중에 펼친 뒤(명령만)
  // 도구 결과가 오면 이 키가 바뀌고 아래 효과가
  // 다시 가져와, 출력이 캐시에 가려지지 않습니다.
  const loadedKey = React.useRef<string | null>(null);
  const { use, result } = group;
  const toolName = use?.tool || result?.tool || "도구";
  const ToolIcon = toolName === "Bash" ? Terminal : Wrench;
  const running = !result;
  const ok = !!result && !result.is_error;
  const statusTone = running
    ? "text-muted-foreground"
    : ok
      ? "text-emerald-600 dark:text-emerald-400"
      : "text-red-600 dark:text-red-400";
  const rawCmd =
    use && use.summary.startsWith(toolName) ? use.summary.slice(toolName.length).trimStart() : (use?.summary ?? "");
  const cmd = toolInputText(toolName, rawCmd);
  // 상태만. 전체 결과는 펼침(【출력】) 뒤에 있고, 줄 안에서 미리 보지 않음
  const statusText = running ? "실행 중…" : ok ? "✓" : "✕ 실패";

  // 불러올 seq들의 키. 결과(또는 명령)가 오면 바뀝니다.
  const detailKey = `${use?.seq ?? ""}:${result?.seq ?? ""}`;
  React.useEffect(() => {
    if (!open || loadedKey.current === detailKey) return;
    let live = true;
    const segs: { label: string; seq: number }[] = [];
    if (use) segs.push({ label: "명령", seq: use.seq });
    if (result) segs.push({ label: "출력" + (result.is_error ? " ✕" : " ✓"), seq: result.seq });
    void Promise.all(
      segs.map((x) =>
        getDetail(x.seq)
          .then((d) => d || "(비어 있음)")
          .catch(() => "(불러오기 실패)"),
      ),
    ).then((parts) => {
      if (!live) return;
      setDetail(
        segs
          .map((x, i) => `【${x.label}】\n${x.label === "명령" ? toolInputText(toolName, parts[i]) : parts[i]}`)
          .join("\n\n"),
      );
      loadedKey.current = detailKey;
    });
    return () => {
      live = false;
    };
  }, [open, detailKey, use, result, getDetail, toolName]);

  const scrolledRef = React.useRef(false);
  React.useEffect(() => {
    scrolledRef.current = false;
    if (focused) setOpen(true);
  }, [focused]);
  React.useEffect(() => {
    if (!focused || !open || detail === null || scrolledRef.current) return;
    const el = targetRef.current;
    const viewport = el?.closest('[data-slot="scroll-area-viewport"]');
    if (!el || !viewport) return;
    scrolledRef.current = true;
    const center = () => {
      const rect = el.getBoundingClientRect();
      const bounds = viewport.getBoundingClientRect();
      viewport.scrollTop += rect.top + rect.height / 2 - bounds.top - bounds.height / 2;
    };
    // 사용자 말풍선은 전체 글을 나중에 불러옵니다. 처음 배치는
    // 자리 잡게 두되, 사용자가 건드리거나 2초가 지나면 맨 아래 고정을 바로 멈춥니다.
    const observer = new ResizeObserver(center);
    observer.observe(el.parentElement ?? el);
    observer.observe(viewport);
    const stop = () => observer.disconnect();
    viewport.addEventListener("wheel", stop, { passive: true, once: true });
    viewport.addEventListener("touchstart", stop, { passive: true, once: true });
    viewport.addEventListener("pointerdown", stop, { once: true });
    const frame = requestAnimationFrame(center);
    const timer = setTimeout(stop, 2000);
    return () => {
      cancelAnimationFrame(frame);
      clearTimeout(timer);
      stop();
      viewport.removeEventListener("wheel", stop);
      viewport.removeEventListener("touchstart", stop);
      viewport.removeEventListener("pointerdown", stop);
    };
  }, [focused, open, detail]);

  function toggle() {
    setOpen((o) => !o);
  }

  return (
    <section
      ref={targetRef}
      aria-label={focused ? `찾은 도구 호출 #${use?.seq}` : undefined}
      className={focused ? "rounded-lg border-2 border-primary bg-primary/5 p-3 text-xs" : "text-xs"}
    >
      <button type="button" onClick={toggle} className="flex w-full items-start gap-2 py-1 text-left hover:bg-muted/40">
        <span className="mt-0.5 text-muted-foreground">
          {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
        </span>
        <ToolIcon className={"mt-0.5 size-3.5 shrink-0 " + (running ? "text-sky-600 dark:text-sky-400" : statusTone)} />
        {showWorker && <span className={chip(group.worker)}>{group.worker}</span>}
        <span className="shrink-0 font-medium text-sky-600 dark:text-sky-400">{toolName}</span>
        {cmd && <span className="min-w-0 flex-1 truncate font-mono text-muted-foreground">{cmd}</span>}
        <span className={"ml-auto shrink-0 font-medium " + statusTone}>{statusText}</span>
      </button>
      {open && (
        <pre className="ml-7 mb-1 max-h-72 overflow-auto whitespace-pre-wrap break-all rounded bg-muted/50 p-2 font-mono text-[11px] leading-relaxed">
          {detail ?? "불러오는 중…"}
        </pre>
      )}
    </section>
  );
}

// MessageBlock은 이어 붙인 에이전트 메시지를 그립니다(흐르는 text/thinking/
// result 조각을 한 블록으로). 대화가 줄이 아니라 메시지로 읽히게 합니다.
function MessageBlock({
  group,
  getDetail,
  showWorker,
}: {
  group: Extract<Group, { type: "msg" }>;
  getDetail: (seq: number) => Promise<string>;
  showWorker?: boolean;
}) {
  const [open, setOpen] = React.useState(false);
  const [detail, setDetail] = React.useState<string | null>(null);
  // ToolBlock처럼 묶음 단계 seq가 키라, 일찍 펼친 뒤 도착한
  // 스트림 단계는 캐시에 가려지지 않고 다시 가져옵니다.
  const loadedKey = React.useRef<string | null>(null);
  const speak = group.steps.filter((s) => s.kind !== "thinking");
  const hasThinking = group.steps.some((s) => s.kind === "thinking");
  const isError = group.steps.some((s) => s.is_error);
  const body = (speak.length ? speak : group.steps).map((s) => s.summary).join("  ") || "…";
  const Icon = isError ? Flag : MessageSquare;
  const tone = isError ? "text-red-600 dark:text-red-400" : "text-foreground";

  const detailKey = group.steps.map((s) => s.seq).join(",");
  React.useEffect(() => {
    if (!open || loadedKey.current === detailKey) return;
    let live = true;
    void Promise.all(
      group.steps.map((s) =>
        getDetail(s.seq)
          .then((d) => d || s.summary)
          .catch(() => s.summary),
      ),
    ).then((parts) => {
      if (!live) return;
      setDetail(group.steps.map((s, i) => `【${kindLabel(s.kind)}】\n${parts[i]}`).join("\n\n"));
      loadedKey.current = detailKey;
    });
    return () => {
      live = false;
    };
  }, [open, detailKey, group.steps, getDetail]);

  function toggle() {
    setOpen((o) => !o);
  }

  return (
    <div className="text-xs">
      <button type="button" onClick={toggle} className="flex w-full items-start gap-2 py-1 text-left hover:bg-muted/40">
        <span className="mt-0.5 text-muted-foreground">
          {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
        </span>
        <Icon className={"mt-0.5 size-3.5 shrink-0 " + tone} />
        {showWorker && <span className={chip(group.worker)}>{group.worker}</span>}
        <span className={"min-w-0 flex-1 truncate " + tone}>
          {body}
          {hasThinking && <span className="ml-1 text-[10px] text-muted-foreground">· 추론 포함</span>}
        </span>
      </button>
      {open && (
        <pre className="ml-7 mb-1 max-h-72 overflow-auto whitespace-pre-wrap break-all rounded bg-muted/50 p-2 font-mono text-[11px] leading-relaxed">
          {detail ?? "불러오는 중…"}
        </pre>
      )}
    </div>
  );
}

// UserRow는 오른쪽 채팅 말풍선입니다. 사람이 메인 에이전트에게 보낸 차례이거나
// 워커 세션을 이끄는, 모델이 만든 의도입니다
// (intent=true). 같은 말풍선이지만 사람 아이콘 대신 과녁 아이콘입니다.
// summary는 잘린 첫 줄이라, 전체 메시지는 상세에서 가져와
// 다 보여 줍니다(말풍선은 whitespace-pre-wrap이라 긴 글도 줄바꿈됩니다).
// fmtBytes는 첨부 칩에 사람이 읽기 쉬운 파일 크기를 그립니다.
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

type MsgAttachment = { name: string; path: string; size: number };

// parseUserBody는 사용자 차례 본문을 글과 첨부로 나눕니다. 백엔드는
// 파일이 있으면 Detail을 JSON {text, attachments}로, 아니면 일반 글로 저장합니다. 그래서
// 조심스럽게 파싱하고, 안 되면 본문 전체를 글로 봅니다.
function parseUserBody(body: string): { text: string; attachments: MsgAttachment[] } {
  if (body.startsWith("{")) {
    try {
      const p = JSON.parse(body);
      if (p && Array.isArray(p.attachments)) {
        return { text: typeof p.text === "string" ? p.text : "", attachments: p.attachments };
      }
    } catch {
      /* "{"로 시작할 뿐인 일반 글 */
    }
  }
  return { text: body, attachments: [] };
}

function UserRow({ step, intent, getDetail }: { step: Activity; intent?: boolean; getDetail: (seq: number) => Promise<string> }) {
  const Icon = intent ? CrosshairIcon : UserIcon;
  const [ref, inView] = useInView();
  // 먼저 그린 메아리는 상세를 안에 갖고, 저장된 줄은 스크롤 때 나중에 불러옵니다.
  const inline = step.detail && step.detail.length > 0 ? step.detail : null;
  const [full, setFull] = React.useState<string | null>(inline);
  React.useEffect(() => {
    if (inline) return; // 본문을 이미 갖고 있음(먼저 그린 메아리)
    if (!inView) return; // 말풍선이 보이기 직전에만 전체 메시지를 가져옵니다
    let live = true;
    getDetail(step.seq)
      .then((d) => {
        if (live) setFull(d || step.summary);
      })
      .catch(() => {
        if (live) setFull(step.summary);
      });
    return () => {
      live = false;
    };
  }, [inView, step.seq, getDetail, step.summary, inline]);
  const { text, attachments } = parseUserBody(full ?? step.summary);
  return (
    <div ref={ref} className="mt-3 mb-2 flex min-w-0 justify-end gap-2">
      <div className="flex min-w-0 max-w-[85%] flex-col items-end gap-1.5">
        {attachments.length > 0 && (
          <div className="flex min-w-0 max-w-full flex-wrap justify-end gap-1.5">
            {attachments.map((a) => (
              <div
                key={a.path}
                className="flex min-w-0 max-w-full items-center gap-1.5 rounded-md border bg-card px-2 py-1 text-xs shadow-sm"
                title={a.path}
              >
                <PaperclipIcon className="size-3 shrink-0 text-primary" />
                <span className="max-w-[180px] truncate font-medium">{a.name}</span>
                <span className="shrink-0 text-muted-foreground">{fmtBytes(a.size)}</span>
              </div>
            ))}
          </div>
        )}
        {text && (
          <div className="min-w-0 max-w-full whitespace-pre-wrap rounded-lg rounded-tr-sm bg-primary px-3 py-1.5 text-sm text-primary-foreground [overflow-wrap:anywhere]">
            {text}
          </div>
        )}
        <ActivityTime ts={step.ts} />
      </div>
      <div className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10">
        <Icon className="size-3.5 text-primary" />
      </div>
    </div>
  );
}

// AnswerBlock은 에이전트의 최종 답(kind="result")을 접지 않고
// 다 그립니다. summary는 잘린 첫 줄이라, 전체 글은 상세에서 가져와
// 그 자리에 보여 줍니다.
function AnswerBlock({ step, getDetail }: { step: Activity; getDetail: (seq: number) => Promise<string> }) {
  const [ref, inView] = useInView();
  const [full, setFull] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (!inView) return; // 답이 보이기 직전에만 전체 답을 가져옵니다
    let live = true;
    getDetail(step.seq)
      .then((d) => {
        if (live) setFull(d || step.summary);
      })
      .catch(() => {
        if (live) setFull(step.summary);
      });
    return () => {
      live = false;
    };
  }, [inView, step.seq, getDetail, step.summary]);
  return (
    <div ref={ref} className="mb-2 mt-1 flex min-w-0 flex-col gap-1">
      <div
        className={
          "min-w-0 flex-1 break-words rounded-lg bg-muted px-3 py-2 " +
          (step.is_error ? "text-sm text-red-600 dark:text-red-400" : "")
        }
      >
        {step.is_error ? (
          <span className="whitespace-pre-wrap">{full ?? step.summary}</span>
        ) : (
          <Markdown text={full ?? step.summary} />
        )}
      </div>
      <ActivityTime ts={step.ts} />
    </div>
  );
}

// ExecView는 에이전트 실행 재생(플래너 / 워커 / 메인 에이전트)을
// 압축해 묶고, 펼치면 상세가 나오는 형식으로 그립니다. 생각, 도구 호출/결과,
// 그리고 메인 에이전트의 사람 차례. 워커 레인 칩은
// 보기가 실제로 에이전트를 섞을 때만 나옵니다.
function ExecView({
  activity,
  taskId,
  chat,
  fetchDetail,
  focusedSeq,
}: {
  activity: Activity[];
  taskId?: string;
  chat?: boolean;
  fetchDetail?: (seq: number) => Promise<string>;
  focusedSeq?: number;
}) {
  const showWorker = new Set(activity.map((a) => a.worker)).size > 1;
  // 기본 상세 조회: 작업 범위 활동 주소. 채팅 화면은
  // 자기 대화 범위 조회를 대신 넘깁니다.
  const getDetail = React.useCallback(
    (seq: number) => (fetchDetail ? fetchDetail(seq) : api.activityDetail(seq, taskId).then((r) => r.detail ?? "")),
    [fetchDetail, taskId],
  );
  return (
    <div className="flex flex-col">
      {groupSteps(activity, !!chat).map((g) =>
        g.type === "round" ? (
          <div key={"r" + g.key} className="my-2 flex items-center gap-2 text-[10px] font-medium text-muted-foreground">
            <span className="h-px flex-1 bg-border" />
            {g.label}
            <span className="h-px flex-1 bg-border" />
          </div>
        ) : g.type === "user" ? (
          <UserRow key={"u" + g.key} step={g.step} intent={g.intent} getDetail={getDetail} />
        ) : g.type === "answer" ? (
          <AnswerBlock key={"a" + g.key} step={g.step} getDetail={getDetail} />
        ) : g.type === "tool" ? (
          <ToolBlock
            key={"t" + g.key}
            group={g}
            getDetail={getDetail}
            showWorker={showWorker}
            focused={focusedSeq != null && g.use?.seq === focusedSeq}
          />
        ) : g.type === "intercept" ? (
          <InterceptCard key={"ic" + g.key} step={g.step} getDetail={getDetail} />
        ) : (
          <MessageBlock key={"m" + g.key} group={g} getDetail={getDetail} showWorker={showWorker} />
        ),
      )}
    </div>
  );
}

// Transcript는 세션 활동을 압축해 묶은 실행 재생으로 그립니다.
// 사람 차례, 생각, 도구 호출(명령+결과 짝), 메시지. 메인 에이전트(대화 가능)와
// 워커/플래너(읽기 전용) 모두 같습니다.
export function Transcript({
  activity,
  live,
  taskId,
  chat,
  fetchDetail,
  focusedSeq,
}: {
  activity: Activity[];
  live?: boolean;
  taskId?: string;
  chat?: boolean;
  fetchDetail?: (seq: number) => Promise<string>;
  focusedSeq?: number;
}) {
  const transcriptRef = React.useRef<HTMLDivElement>(null);
  const [focusPadding, setFocusPadding] = React.useState(0);
  React.useLayoutEffect(() => {
    if (focusedSeq == null) { setFocusPadding(0); return; }
    const viewport = transcriptRef.current?.closest('[data-slot="scroll-area-viewport"]');
    if (!viewport) return;
    const measure = () => setFocusPadding(viewport.clientHeight / 2);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(viewport);
    return () => observer.disconnect();
  }, [focusedSeq]);
  return (
    <div ref={transcriptRef} className="flex flex-col gap-1" style={focusPadding ? { paddingBlock: focusPadding } : undefined}>
      <ExecView activity={activity} taskId={taskId} chat={chat} fetchDetail={fetchDetail} focusedSeq={focusedSeq} />
      {live && (
        <div className="flex items-center gap-2 pl-2 pt-1 text-xs text-muted-foreground">
          <span className="flex gap-1">
            <span className="size-1.5 animate-bounce rounded-full bg-blue-500 [animation-delay:-0.3s]" />
            <span className="size-1.5 animate-bounce rounded-full bg-blue-500 [animation-delay:-0.15s]" />
            <span className="size-1.5 animate-bounce rounded-full bg-blue-500" />
          </span>
          실시간 스트리밍 중…
        </div>
      )}
    </div>
  );
}
