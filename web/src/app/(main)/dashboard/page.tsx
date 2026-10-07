"use client";

import * as React from "react";

import Link from "next/link";

import {
  ActivityIcon,
  ArrowUpRightIcon,
  BugIcon,
  ClockIcon,
  NetworkIcon,
  ShieldCheckIcon,
  TargetIcon,
  ZapIcon,
} from "lucide-react";
import { Bar, BarChart, CartesianGrid, Tooltip as RechartsTooltip, XAxis, YAxis } from "recharts";

import { StatusBadge } from "@/components/status-badge";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { type ChartConfig, ChartContainer, ChartTooltip, ChartTooltipContent } from "@/components/ui/chart";
import { Progress } from "@/components/ui/progress";
import { api } from "@/lib/api";
import type {
  Activity,
  Agent,
  ConvTokenSummary,
  Finding,
  InterceptPending,
  LLMProfile,
  MCPServer,
  Settings,
  SkillItem,
  Stats,
  Task,
  TokenTotal,
  Tool,
  TrafficExchange,
  UsageStats,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// ── 차트 상수 ───────────────────────────────────────────────────────────

const dailyTrendConfig = {
  input: { label: "입력", color: "hsl(217 91% 60%)" },
  output: { label: "출력", color: "hsl(263 70% 60%)" },
  cacheRead: { label: "캐시 읽기", color: "hsl(160 60% 45%)" },
} satisfies ChartConfig;

// ── 도우미 ──────────────────────────────────────────────────────────────────

function fmtRel(ts?: string | number): string {
  if (!ts) return "—";
  const ms = Date.now() - (typeof ts === "number" ? ts * 1000 : Date.parse(ts as string));
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}초 전`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}분 전`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}시간 전`;
  return `${Math.floor(h / 24)}일 전`;
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(1) + "k";
  return String(n);
}

const ASSET_TYPE_LABELS: Record<string, string> = {
  root_domain: "루트 도메인",
  ip: "IP",
  subdomain: "서브도메인",
  app: "앱",
  service: "서비스",
  endpoint: "엔드포인트",
};

const ASSET_COLORS: Record<string, string> = {
  root_domain: "bg-blue-500",
  ip: "bg-indigo-400",
  subdomain: "bg-violet-500",
  app: "bg-orange-400",
  service: "bg-cyan-500",
  endpoint: "bg-emerald-500",
};

function statusColor(code: number): string {
  if (code < 300) return "text-emerald-500";
  if (code < 400) return "text-blue-400";
  if (code < 500) return "text-amber-400";
  return "text-red-400";
}

function statusBg(code: number): string {
  if (code < 300) return "bg-emerald-500";
  if (code < 400) return "bg-blue-400";
  if (code < 500) return "bg-amber-400";
  return "bg-red-400";
}

// ── 부분 컴포넌트 ────────────────────────────────────────────────────────────

function LiveDot({ className }: { className?: string }) {
  return <span className={cn("inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-blue-400", className)} />;
}

function SectionTitle({
  icon: Icon,
  children,
  sub,
}: {
  icon: React.ElementType;
  children: React.ReactNode;
  sub?: React.ReactNode;
}) {
  return (
    <div className="mb-3 flex items-center justify-between">
      <div className="flex items-center gap-1.5 text-xs font-semibold">
        <Icon className="size-3.5 text-muted-foreground" />
        {children}
      </div>
      {sub && <span className="text-[10px] text-muted-foreground">{sub}</span>}
    </div>
  );
}

// ── 페이지 ─────────────────────────────────────────────────────────────────────

export default function DashboardPage() {
  // 데이터 상태
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [findings, setFindings] = React.useState<Finding[]>([]);
  const [stats, setStats] = React.useState<Stats | null>(null);
  const [settings, setSettings] = React.useState<Settings | null>(null);
  const [pending, setPending] = React.useState<InterceptPending[]>([]);
  const [activity, setActivity] = React.useState<Activity[]>([]);
  const [tokens, setTokens] = React.useState<TokenTotal | null>(null);
  const [convTokens, setConvTokens] = React.useState<ConvTokenSummary[]>([]);
  const [usageStats, setUsageStats] = React.useState<UsageStats | null>(null);
  const [assetCounts, setAssetCounts] = React.useState<Record<string, number>>({});
  const [traffic, setTraffic] = React.useState<TrafficExchange[]>([]);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [mcpServers, setMcpServers] = React.useState<MCPServer[]>([]);
  const [skills, setSkills] = React.useState<SkillItem[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [llmProfiles, setLLMProfiles] = React.useState<LLMProfile[]>([]);

  // 작업 목록은 대시보드에서 가장 무거운 출처입니다. 따로 주기 조회해서
  // 긴 기록이 다른 패널을 붙잡지 않게 합니다.
  React.useEffect(() => {
    let alive = true;
    let loading = false;
    let signature = "";
    const load = async () => {
      if (loading) return;
      loading = true;
      try {
        const tr = await api.tasks();
        if (!alive) return;
        const nextSignature = JSON.stringify(tr.tasks);
        if (nextSignature !== signature) {
          signature = nextSignature;
          setTasks(tr.tasks);
        }
      } catch {
        /* 잠깐 오류. 다음 주기 조회가 다시 시도 */
      } finally {
        loading = false;
      }
    };
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  // 나머지 빠른 출처는 한 번에 묶어, 주기 조회 한 번이 화면을 한 번만 다시 그리게 합니다.
  React.useEffect(() => {
    let alive = true;
    let loading = false;
    const load = async () => {
      if (loading) return;
      loading = true;
      try {
        const [findings, stats, settings, pending, activity, tokens, conversationTokens] = await Promise.all([
          api.findings(),
          api.stats(),
          api.settings(),
          api.interceptPending(),
          api.activity(undefined, { limit: 30 }),
          api.tokenStats(),
          api.conversationTokens(),
        ]);
        if (!alive) return;
        setFindings(findings);
        setStats(stats);
        setSettings(settings);
        setPending(pending);
        setActivity(activity.items);
        setTokens(tokens.total ?? null);
        setConvTokens(conversationTokens);
      } catch {
        // 오래된 데이터를 유지하고 다음 주기에 다시 시도합니다.
      } finally {
        loading = false;
      }
    };
    void load();
    const timer = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, []);

  // 느린 주기 조회: 트래픽, 자산, 시스템 고정값(15초마다)
  React.useEffect(() => {
    let alive = true;
    const load = async () => {
      try {
        const [traf, counts, agentList, mcpList, skillList, toolList, profileList, usage] = await Promise.all([
          api.traffic(0, 50),
          api.assetCounts(),
          api.agents(),
          api.mcpServers(),
          api.skills(),
          api.tools(),
          api.llmProfiles(),
          api.usageStats(365),
        ]);
        if (!alive) return;
        setTraffic(traf.exchanges ?? []);
        setAssetCounts(counts ?? {});
        setAgents(agentList);
        setMcpServers(mcpList);
        setSkills(skillList);
        setTools(toolList);
        setLLMProfiles(profileList);
        setUsageStats(usage);
      } catch {
        /* 잠깐 오류 */
      }
    };
    void load();
    const t = setInterval(load, 15000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  // ── 계산된 값 ───────────────────────────────────────────────────────────────

  const tasksByStatus = React.useMemo(() => {
    const m: Record<string, number> = {};
    for (const t of tasks) m[t.status] = (m[t.status] ?? 0) + 1;
    return m;
  }, [tasks]);

  const findingsBySev = React.useMemo(() => {
    const m = { critical: 0, high: 0, medium: 0, low: 0 };
    for (const f of findings) {
      if (f.severity in m) m[f.severity as keyof typeof m]++;
    }
    return m;
  }, [findings]);

  const sortedTasks = React.useMemo(
    () =>
      [...tasks]
        .sort((a, b) => (b.last_activity_unix ?? b.created_unix ?? 0) - (a.last_activity_unix ?? a.created_unix ?? 0))
        .slice(0, 5),
    [tasks],
  );

  const recentFindings = React.useMemo(
    () => [...findings].sort((a, b) => Date.parse(b.ts) - Date.parse(a.ts)).slice(0, 8),
    [findings],
  );

  const recentActivity = React.useMemo(
    () =>
      [...activity]
        .filter((a) => a.kind !== "usage")
        .sort((a, b) => b.seq - a.seq)
        .slice(0, 6),
    [activity],
  );

  const totalAssets = React.useMemo(() => Object.values(assetCounts).reduce((a, b) => a + b, 0), [assetCounts]);

  // 자산 종류별 나눔
  const assetByType = React.useMemo(() => {
    return Object.entries(assetCounts)
      .filter(([, n]) => n > 0)
      .sort(([, a], [, b]) => b - a)
      .slice(0, 8);
  }, [assetCounts]);

  const assetMax = assetByType[0]?.[1] ?? 1;

  // 트래픽 상태 코드별 나눔
  const trafficByCodes = React.useMemo(() => {
    const m: Record<number, number> = {};
    for (const e of traffic) {
      const bucket = Math.floor(e.status / 100) * 100;
      m[bucket] = (m[bucket] ?? 0) + 1;
    }
    return Object.entries(m)
      .sort(([a], [b]) => Number(a) - Number(b))
      .map(([code, n]) => ({ code: Number(code), n }));
  }, [traffic]);

  const trafficMax = Math.max(...trafficByCodes.map((x) => x.n), 1);
  const recentTraffic = [...traffic].sort((a, b) => Date.parse(b.ts) - Date.parse(a.ts)).slice(0, 5);

  // 시스템
  const activeProfile = llmProfiles.find((p) => p.is_default);
  const enabledTools = tools.filter((t) => t.enabled);
  const pendingCount = pending.length;

  // ── LLM 프로필별 토큰 통계 ──────────────────────────────────────────
  // llm_profile_id가 null/없음인 작업은 그때의 기본 프로필을 썼습니다
  const defaultProfileId = activeProfile ? Number(activeProfile.id) : null;

  // 데이터 출처 스위치: 예전 = activity(task.tokens + 세션), 새 버전 = llm_usage 계량 장부.
  const [tokenVersion, setTokenVersion] = React.useState<"old" | "new">("old");
  // 고른 profile tab: "all" = 전체. number = 특정 profile id
  const [tokenTab, setTokenTab] = React.useState<number | null | "all">("all");
  // 일별 막대 차트의 날짜 범위
  const [tokenDays, setTokenDays] = React.useState<7 | 30 | 90 | 180 | 365>(30);

  // profile 이름 → id. llm_usage의 profile_name을 기존 profile 칸에 대응시킬 때 씁니다.
  const profileIdByName = React.useMemo(() => {
    const m = new Map<string, number>();
    for (const p of llmProfiles) m.set(p.name, Number(p.id));
    return m;
  }, [llmProfiles]);

  type Bucket = { input: number; output: number; cacheRead: number; cacheWrite: number; taskCount: number };

  // 예전: profile별로 통에 넣음(activity의 task.tokens + 세션 사용량에서).
  const tokenByProfileOld = React.useMemo<Map<number | null, Bucket>>(() => {
    const m = new Map<number | null, Bucket>();
    const fold = (key: number | null, inp: number, out: number, cr: number, cw: number, addTask: boolean) => {
      const prev = m.get(key) ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, taskCount: 0 };
      m.set(key, {
        input: prev.input + inp,
        output: prev.output + out,
        cacheRead: prev.cacheRead + cr,
        cacheWrite: prev.cacheWrite + cw,
        taskCount: prev.taskCount + (addTask ? 1 : 0),
      });
    };
    for (const t of tasks) {
      fold(
        t.llm_profile_id ?? defaultProfileId,
        t.tokens?.input_tokens ?? 0,
        t.tokens?.output_tokens ?? 0,
        t.tokens?.cache_read_tokens ?? 0,
        t.tokens?.cache_write_tokens ?? 0,
        true,
      );
    }
    for (const c of convTokens) {
      fold(
        c.llm_profile_id ?? defaultProfileId,
        c.input_tokens,
        c.output_tokens,
        c.cache_read_tokens,
        c.cache_write_tokens,
        false,
      );
    }
    return m;
  }, [tasks, convTokens, defaultProfileId]);

  // 새 버전: profile별로 통에 넣음(llm_usage 전역 집계에서, 호출마다 정확).
  const tokenByProfileNew = React.useMemo<Map<number | null, Bucket>>(() => {
    const m = new Map<number | null, Bucket>();
    for (const p of usageStats?.by_profile ?? []) {
      // 기존 profile에 안 맞음(이름 변경/삭제/빈 이름) → 기본 통으로 떨어지며, 여전히 「전체」에 포함됩니다.
      const key = profileIdByName.get(p.profile_name) ?? defaultProfileId;
      const prev = m.get(key) ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, taskCount: 0 };
      m.set(key, {
        input: prev.input + p.input_tokens,
        output: prev.output + p.output_tokens,
        cacheRead: prev.cacheRead + p.cache_read_tokens,
        cacheWrite: prev.cacheWrite + p.cache_write_tokens,
        taskCount: prev.taskCount + p.tasks,
      });
    }
    return m;
  }, [usageStats, profileIdByName, defaultProfileId]);

  const tokenByProfile = tokenVersion === "new" ? tokenByProfileNew : tokenByProfileOld;

  const displayedTokens = React.useMemo(() => {
    if (tokenTab === "all") {
      let input = 0,
        output = 0,
        cacheRead = 0,
        cacheWrite = 0,
        taskCount = 0;
      for (const v of tokenByProfile.values()) {
        input += v.input;
        output += v.output;
        cacheRead += v.cacheRead;
        cacheWrite += v.cacheWrite;
        taskCount += v.taskCount;
      }
      return { input, output, cacheRead, cacheWrite, taskCount };
    }
    const v = tokenByProfile.get(tokenTab as number | null);
    return v
      ? { input: v.input, output: v.output, cacheRead: v.cacheRead, cacheWrite: v.cacheWrite, taskCount: v.taskCount }
      : { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, taskCount: 0 };
  }, [tokenTab, tokenByProfile]);

  // 예전 하루 단위: 작업/세션 총량을 만든 날짜의 통에 넣음(근사, 실제 하루 소모가 아님).
  const dailyTokenDataOld = React.useMemo(() => {
    const now = new Date();
    now.setDate(now.getDate() - tokenDays);
    const cutoff = now.toISOString().slice(0, 10);
    const m = new Map<string, { input: number; output: number; cacheRead: number }>();
    const fold = (date: string, inp: number, out: number, cr: number) => {
      if (date < cutoff) return;
      const prev = m.get(date) ?? { input: 0, output: 0, cacheRead: 0 };
      m.set(date, { input: prev.input + inp, output: prev.output + out, cacheRead: prev.cacheRead + cr });
    };
    for (const t of tasks) {
      if (tokenTab !== "all" && (t.llm_profile_id ?? defaultProfileId) !== tokenTab) continue;
      fold(
        t.created_at.slice(0, 10),
        t.tokens?.input_tokens ?? 0,
        t.tokens?.output_tokens ?? 0,
        t.tokens?.cache_read_tokens ?? 0,
      );
    }
    for (const c of convTokens) {
      if (tokenTab !== "all" && (c.llm_profile_id ?? defaultProfileId) !== tokenTab) continue;
      fold(c.created_at.slice(0, 10), c.input_tokens, c.output_tokens, c.cache_read_tokens);
    }
    return [...m.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([date, v]) => ({ date: date.slice(5), ...v }));
  }, [tasks, convTokens, tokenDays, tokenTab, defaultProfileId]);

  // 새 버전 하루 단위: llm_usage의 실제 하루 소모(ts는 실제 호출 시각).
  const dailyTokenDataNew = React.useMemo(() => {
    const now = new Date();
    now.setDate(now.getDate() - tokenDays);
    const cutoff = now.toISOString().slice(0, 10);
    const m = new Map<string, { input: number; output: number; cacheRead: number }>();
    for (const d of usageStats?.daily ?? []) {
      if (d.date < cutoff) continue;
      if (tokenTab !== "all" && (profileIdByName.get(d.profile_name) ?? defaultProfileId) !== tokenTab) continue;
      const prev = m.get(d.date) ?? { input: 0, output: 0, cacheRead: 0 };
      m.set(d.date, {
        input: prev.input + d.input_tokens,
        output: prev.output + d.output_tokens,
        cacheRead: prev.cacheRead + d.cache_read_tokens,
      });
    }
    return [...m.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([date, v]) => ({ date: date.slice(5), ...v }));
  }, [usageStats, tokenDays, tokenTab, profileIdByName, defaultProfileId]);

  const dailyTokenData = tokenVersion === "new" ? dailyTokenDataNew : dailyTokenDataOld;

  // 활동 종류 이름
  function kindLabel(a: Activity): string {
    if (a.kind === "tool_use") return a.tool ?? "도구 호출";
    return ({ tool_result: "도구 결과", text: "설명", thinking: "추론", result: "요약" } as Record<string, string>)[a.kind] ?? a.kind;
  }

  function workerColor(w: string): string {
    if (w === "planner") return "text-violet-400";
    if (w === "mainagent") return "text-cyan-400";
    return "text-blue-400";
  }

  function workerBg(w: string): string {
    if (w === "planner") return "bg-violet-500/10 text-violet-400 border-violet-500/20";
    if (w === "mainagent") return "bg-cyan-500/10 text-cyan-400 border-cyan-500/20";
    return "bg-blue-500/10 text-blue-400 border-blue-500/20";
  }

  // ── 그리기 ────────────────────────────────────────────────────────────────

  return (
    <div className="flex flex-1 flex-col gap-4 pb-6">
      {/* ── 머리 ── */}
      <div>
        <div>
          <h1 className="text-lg font-semibold tracking-tight">전체 보기</h1>
          <p className="text-xs text-muted-foreground">시스템 전체 상태 · 실시간 새로고침</p>
        </div>
      </div>

      {/* ── 1행: 통계 카드 5개 ── */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        {/* 진행 중인 작업 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <TargetIcon className="size-3" /> 진행 중인 작업
            </div>
            <div className="flex items-baseline gap-1.5">
              <span className="text-2xl font-semibold tabular-nums">{tasksByStatus.running ?? 0}</span>
              <span className="text-xs text-muted-foreground">/ {tasks.length}</span>
            </div>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-x-2.5 gap-y-0.5 text-[10px]">
            {(tasksByStatus.running ?? 0) > 0 && <span className="text-blue-400">탐색 {tasksByStatus.running}</span>}
            {(tasksByStatus.paused ?? 0) > 0 && <span className="text-amber-400">일시 중지 {tasksByStatus.paused}</span>}
            {(tasksByStatus.done ?? 0) > 0 && <span className="text-emerald-400">완료 {tasksByStatus.done}</span>}
            {tasks.length === 0 && <span className="text-muted-foreground">작업 없음</span>}
          </CardContent>
        </Card>

        {/* 발견 확인 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <BugIcon className="size-3" /> 발견 확인
            </div>
            <div className="text-2xl font-semibold tabular-nums">{findings.length}</div>
          </CardHeader>
          <CardContent className="flex flex-wrap gap-2.5 text-[10px]">
            <span className="text-rose-500">심각 {findingsBySev.critical}</span>
            <span className="text-red-400">높음 {findingsBySev.high}</span>
            <span className="text-amber-400">중간 {findingsBySev.medium}</span>
            <span className="text-slate-400">낮음 {findingsBySev.low}</span>
          </CardContent>
        </Card>

        {/* 자산 노드 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <NetworkIcon className="size-3" /> 자산 노드
            </div>
            <div className="text-2xl font-semibold tabular-nums">{totalAssets}</div>
          </CardHeader>
          <CardContent className="text-[10px] text-muted-foreground">작업 간 공유</CardContent>
        </Card>

        {/* 트래픽 주고받음 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <ActivityIcon className="size-3" /> 트래픽 상호작용
            </div>
            <div className="text-2xl font-semibold tabular-nums">{traffic.length}</div>
          </CardHeader>
          <CardContent className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
            {settings?.traffic_capture ? (
              <>
                <LiveDot />
                <span>기록 중</span>
              </>
            ) : (
              <span>캡처가 꺼져 있음</span>
            )}
          </CardContent>
        </Card>

        {/* LLM 사용량 */}
        <Card className="gap-1">
          <CardHeader className="pb-0">
            <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
              <ZapIcon className="size-3" /> 토큰 사용량
            </div>
            <div className="text-2xl font-semibold tabular-nums">
              {fmtTokens(displayedTokens.input + displayedTokens.output) || "—"}
            </div>
          </CardHeader>
          <CardContent className="text-[10px] text-muted-foreground">
            입력 {fmtTokens(displayedTokens.input)}(캐시 포함 {fmtTokens(displayedTokens.cacheRead)})· 출력{" "}
            {fmtTokens(displayedTokens.output)}
          </CardContent>
        </Card>
      </div>

      {/* ── Row 2: LLM Token 소모 ── */}
      <Card className="p-4">
        {/* 머리 */}
        <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-1.5 text-xs font-semibold">
            <ZapIcon className="size-3.5 text-muted-foreground" />
            LLM 토큰 사용량
            {/* 데이터 출처 스위치: 예전=activity 통계(지난 작업 포함), 새 버전=llm_usage 계량 장부(더 정확, 켠 뒤만 포함) */}
            <div className="ml-1 flex gap-0.5 rounded-md border bg-muted/30 p-0.5">
              {(
                [
                  { v: "old", label: "이전 버전" },
                  { v: "new", label: "새 버전" },
                ] as const
              ).map(({ v, label }) => (
                <button
                  type="button"
                  key={v}
                  onClick={() => setTokenVersion(v)}
                  title={
                    v === "new"
                      ? "새 버전: 호출 계량 기록 측정 원장에서 옵니다. 호출마다 정확하고 중단 소모를 포함하며, 켠 뒤의 데이터만 다룹니다"
                      : "이전 버전: 활동 기록 통계에서 옵니다(과거 작업 포함). 중단 소모는 집계하지 않고, 모델까지 정확히 나누지 못합니다"
                  }
                  className={cn(
                    "rounded px-2 py-0.5 text-[9px] font-medium transition-colors",
                    tokenVersion === v
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
            <span className="text-[10px] font-normal text-muted-foreground">
              {tokenVersion === "new" ? "호출 계량 기록" : "활동 기록"}
            </span>
          </div>
          {/* 프로필 탭 */}
          <div className="flex flex-wrap items-center gap-1">
            <button
              type="button"
              onClick={() => setTokenTab("all")}
              className={cn(
                "rounded-md px-2.5 py-1 text-[10px] font-medium transition-colors",
                tokenTab === "all"
                  ? "bg-foreground text-background"
                  : "bg-muted/30 text-muted-foreground hover:text-foreground",
              )}
            >
              전체
            </button>
            {llmProfiles.map((p) => {
              const key = Number(p.id);
              const hasData = tokenByProfile.has(key) || (p.is_default && tokenByProfile.has(defaultProfileId));
              const resolvedKey = tokenByProfile.has(key) ? key : p.is_default ? defaultProfileId : key;
              return (
                <button
                  type="button"
                  key={p.id}
                  onClick={() => setTokenTab(resolvedKey)}
                  className={cn(
                    "flex items-center gap-1 rounded-md px-2.5 py-1 text-[10px] font-medium transition-colors",
                    tokenTab === resolvedKey
                      ? "bg-foreground text-background"
                      : "bg-muted/30 text-muted-foreground hover:text-foreground",
                    !hasData && "opacity-40",
                  )}
                >
                  {p.name}
                  {p.is_default && (
                    <span
                      className={cn(
                        "rounded px-1 py-0 text-[8px]",
                        tokenTab === resolvedKey
                          ? "bg-background/20 text-background"
                          : "bg-emerald-500/20 text-emerald-400",
                      )}
                    >
                      기본값
                    </span>
                  )}
                </button>
              );
            })}
          </div>
        </div>

        {/* 본문: 왼쪽 지표 + 오른쪽 막대 차트 */}
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[220px_1fr]">
          {/* 왼쪽: 주요 지표 */}
          <div className="flex flex-col gap-4">
            {/* 합계 */}
            <div>
              <div className="text-[10px] text-muted-foreground">합계(입력+출력)</div>
              <div className="mt-0.5 text-3xl font-bold tabular-nums tracking-tight">
                {fmtTokens(displayedTokens.input + displayedTokens.output) || "—"}
              </div>
              <div className="mt-0.5 text-[10px] text-muted-foreground">{displayedTokens.taskCount} 개 작업</div>
            </div>

            {/* 종류별 막대 */}
            <div className="space-y-3">
              {(() => {
                // input에 이미 캐시가 포함됨. 겹치지 않는 세 조각: 못 맞춘 입력 + 캐시 적중 + 출력 = 총량.
                const total = displayedTokens.input + displayedTokens.output;
                return [
                  {
                    label: "입력(미적중)",
                    value: displayedTokens.input - displayedTokens.cacheRead,
                    barColor: dailyTrendConfig.input.color!,
                    text: "text-blue-400",
                  },
                  {
                    label: "캐시 적중",
                    value: displayedTokens.cacheRead,
                    barColor: dailyTrendConfig.cacheRead.color!,
                    text: "text-emerald-400",
                  },
                  {
                    label: "출력",
                    value: displayedTokens.output,
                    barColor: dailyTrendConfig.output.color!,
                    text: "text-violet-400",
                  },
                ].map(({ label, value, barColor, text }) => {
                  const pct = total > 0 ? (value / total) * 100 : 0;
                  return (
                    <div key={label}>
                      <div className="mb-1 flex items-center justify-between text-[10px]">
                        <span className="text-muted-foreground">{label}</span>
                        <span className={cn("font-mono font-semibold tabular-nums", text)}>{fmtTokens(value)}</span>
                      </div>
                      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                        <div
                          className="h-full rounded-full transition-all duration-500"
                          style={{ width: `${pct}%`, background: barColor }}
                        />
                      </div>
                    </div>
                  );
                });
              })()}
            </div>

            {/* 캐시 적중률 */}
            {(() => {
              // input에 이미 캐시가 포함됨 → 적중률 = 캐시 적중 / 전체 입력.
              const denominator = displayedTokens.input;
              const hitPct = denominator > 0 ? Math.round((displayedTokens.cacheRead / denominator) * 100) : 0;
              return (
                <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2 text-[10px]">
                  <span className="text-muted-foreground">캐시 적중률</span>
                  <span
                    className={cn("font-semibold tabular-nums", hitPct > 50 ? "text-emerald-400" : "text-amber-400")}
                  >
                    {hitPct}%
                  </span>
                </div>
              );
            })()}
          </div>

          {/* 오른쪽: 일별 막대 차트 */}
          <div className="flex min-h-0 flex-col">
            <div className="mb-2 flex items-center justify-between">
              <div className="flex gap-3 text-[9px] text-muted-foreground">
                {(["input", "output", "cacheRead"] as const).map((k) => (
                  <span key={k} className="flex items-center gap-1">
                    <span
                      className="inline-block size-2 rounded-sm"
                      style={{ background: dailyTrendConfig[k].color }}
                    />
                    {dailyTrendConfig[k].label}
                  </span>
                ))}
              </div>
              <div className="flex gap-0.5 rounded-md border bg-muted/30 p-0.5">
                {(
                  [
                    { days: 7, label: "7일" },
                    { days: 30, label: "30일" },
                    { days: 90, label: "3개월" },
                    { days: 180, label: "6개월" },
                    { days: 365, label: "1년" },
                  ] as const
                ).map(({ days, label }) => (
                  <button
                    type="button"
                    key={days}
                    onClick={() => setTokenDays(days)}
                    className={cn(
                      "rounded px-2.5 py-0.5 text-[9px] font-medium transition-colors",
                      tokenDays === days
                        ? "bg-background text-foreground shadow-sm"
                        : "text-muted-foreground hover:text-foreground",
                    )}
                  >
                    {label}
                  </button>
                ))}
              </div>
            </div>

            {dailyTokenData.length === 0 ? (
              <div
                className="flex flex-1 items-center justify-center rounded-lg border bg-muted/10 text-xs text-muted-foreground"
                style={{ minHeight: 180 }}
              >
                데이터 없음
              </div>
            ) : (
              <ChartContainer config={dailyTrendConfig} className="h-[200px] w-full">
                <BarChart data={dailyTokenData} margin={{ top: 4, right: 4, bottom: 0, left: 0 }} maxBarSize={40}>
                  <CartesianGrid vertical={false} />
                  <XAxis
                    dataKey="date"
                    tickLine={false}
                    axisLine={false}
                    tick={{ fontSize: 10 }}
                    tickMargin={6}
                    interval="preserveStartEnd"
                  />
                  <YAxis
                    tickFormatter={(v) => fmtTokens(Number(v))}
                    tickLine={false}
                    axisLine={false}
                    tick={{ fontSize: 10 }}
                    width={44}
                  />
                  <ChartTooltip
                    cursor={false}
                    content={
                      <ChartTooltipContent
                        indicator="line"
                        formatter={(value, name) => (
                          <div className="flex w-full items-center justify-between gap-4">
                            <span>
                              {(dailyTrendConfig as Record<string, { label: string }>)[String(name)]?.label ??
                                String(name)}
                            </span>
                            <span className="font-mono font-semibold tabular-nums">{fmtTokens(Number(value))}</span>
                          </div>
                        )}
                      />
                    }
                  />
                  <Bar dataKey="input" stackId="1" fill="var(--color-input)" radius={[0, 0, 0, 0]} />
                  <Bar dataKey="output" stackId="1" fill="var(--color-output)" radius={[0, 0, 0, 0]} />
                  <Bar dataKey="cacheRead" stackId="1" fill="var(--color-cacheRead)" radius={[4, 4, 0, 0]} />
                </BarChart>
              </ChartContainer>
            )}
          </div>
        </div>
      </Card>

      {/* ── Row 3: 활동 흐름 | 발견 ── */}
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
        {/* 활동 흐름 */}
        <Card className="p-4">
          <div className="mb-3 flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs font-semibold">
              <ActivityIcon className="size-3.5 text-muted-foreground" />
              활동 흐름
            </div>
            <div className="flex items-center gap-2">
              <span className="text-[10px] text-muted-foreground">
                {activity.filter((a) => a.kind !== "usage").length} 건의 이벤트
              </span>
              <Link
                href="/function/tasks"
                className="flex items-center gap-0.5 text-[10px] text-muted-foreground hover:text-foreground"
              >
                작업 보기 <ArrowUpRightIcon className="size-3" />
              </Link>
            </div>
          </div>

          <div className="mb-2.5 flex flex-wrap gap-1.5">
            {(["tool_use", "tool_result", "text", "thinking", "result"] as const).map((kind) => {
              const count = activity.filter((a) => a.kind === kind).length;
              if (count === 0) return null;
              return (
                <span key={kind} className="rounded border bg-muted/20 px-1.5 py-0.5 text-[9px] text-muted-foreground">
                  {({ tool_use: "도구 호출", tool_result: "도구 결과", text: "설명", thinking: "추론", result: "요약" })[kind]} <strong className="text-foreground/70">{count}</strong>
                </span>
              );
            })}
          </div>

          <div className="divide-y">
            {recentActivity.length === 0 ? (
              <div className="py-4 text-center text-xs text-muted-foreground">활동 기록 없음</div>
            ) : (
              recentActivity.map((a) => (
                <div key={a.seq} className="flex gap-2.5 py-2">
                  <span
                    className={cn(
                      "shrink-0 self-start rounded border px-1.5 py-0.5 font-mono text-[9px]",
                      workerBg(a.worker),
                    )}
                  >
                    {({ planner: "플래너", mainagent: "메인 에이전트", worker: "워커" } as Record<string, string>)[a.worker] ?? a.worker}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="text-[11px] font-medium">{kindLabel(a)}</div>
                    <div className="mt-0.5 truncate text-[10px] text-muted-foreground">{a.summary}</div>
                  </div>
                  <div className="shrink-0 text-right">
                    <Badge variant="outline" className="px-1.5 py-0 text-[9px] text-muted-foreground">
                      {({ tool_use: "도구 호출", tool_result: "도구 결과", text: "설명", thinking: "추론", result: "요약" } as Record<string, string>)[a.kind] ?? a.kind}
                    </Badge>
                    <div className="mt-0.5 text-[9px] tabular-nums text-muted-foreground">{fmtRel(a.ts)}</div>
                  </div>
                </div>
              ))
            )}
          </div>
        </Card>

        {/* 발견 */}
        <Card className="p-4">
          <div className="mb-3 flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs font-semibold">
              <BugIcon className="size-3.5 text-muted-foreground" />
              발견
            </div>
            <Link
              href="/function/findings"
              className="flex items-center gap-0.5 text-[10px] text-muted-foreground hover:text-foreground"
            >
              전체 <ArrowUpRightIcon className="size-3" />
            </Link>
          </div>

          <div className="divide-y">
            {recentFindings.length === 0 ? (
              <div className="py-4 text-center text-xs text-muted-foreground">발견 없음</div>
            ) : (
              recentFindings.map((f) => (
                <div key={f.id} className="flex items-start gap-2 py-2">
                  <StatusBadge
                    domain="severity"
                    value={f.severity}
                    className="mt-0.5 shrink-0 px-1.5 py-0 text-[10px]"
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="text-[11px] font-semibold">{f.vulnclass}</span>
                      {f.task_description && (
                        <span className="truncate text-[10px] text-muted-foreground">{f.task_description}</span>
                      )}
                    </div>
                    <div className="mt-0.5 line-clamp-2 text-[10px] leading-snug text-muted-foreground">
                      {f.summary}
                    </div>
                  </div>
                  <span className="shrink-0 text-[9px] tabular-nums text-muted-foreground">{fmtRel(f.ts)}</span>
                </div>
              ))
            )}
          </div>
        </Card>
      </div>

      {/* ── Row 4: 작업 표 ── */}
      <Card className="overflow-hidden p-0">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <div className="flex items-center gap-1.5 text-xs font-semibold">
            <ClockIcon className="size-3.5 text-muted-foreground" />
            작업
          </div>
          <div className="flex items-center gap-2">
            <span className="text-[10px] text-muted-foreground">{tasks.length} 개 작업</span>
            <Link
              href="/function/tasks"
              className="flex items-center gap-0.5 text-[10px] text-muted-foreground hover:text-foreground"
            >
              전체 <ArrowUpRightIcon className="size-3" />
            </Link>
          </div>
        </div>
        <table className="w-full border-collapse text-xs">
          <thead>
            <tr className="border-b">
              {["작업", "상태", "엔진", "목표 진행", "진행 중", "최근 활동"].map((h) => (
                <th
                  key={h}
                  className="px-4 py-2 text-left text-[9px] font-semibold uppercase tracking-widest text-muted-foreground first:pl-4"
                >
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {sortedTasks.length === 0 ? (
              <tr>
                <td colSpan={6} className="px-4 py-6 text-center text-xs text-muted-foreground">
                  작업 없음
                </td>
              </tr>
            ) : (
              sortedTasks.map((t) => {
                const goalsPct = t.goals_total ? Math.round(((t.goals_met ?? 0) / t.goals_total) * 100) : null;
                return (
                  <tr key={t.id} className="border-b last:border-0 transition-colors hover:bg-muted/30">
                    <td className="max-w-xs px-4 py-3">
                      <Link href={`/function/tasks/detail?id=${t.id}`} className="group flex flex-col">
                        <span className="truncate font-medium group-hover:underline">{t.description}</span>
                        <span className="mt-0.5 truncate font-mono text-[10px] text-muted-foreground">{t.id}</span>
                      </Link>
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge domain="task" value={t.status} dot className="px-1.5 py-0 text-[10px]" />
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge
                        domain="engine"
                        value={t.engine_mode ?? "idle"}
                        className="px-1.5 py-0 text-[10px]"
                      />
                    </td>
                    <td className="px-4 py-3">
                      {goalsPct !== null ? (
                        <div className="flex items-center gap-2">
                          <Progress value={goalsPct} className="h-1 w-14" />
                          <span className="tabular-nums text-muted-foreground">
                            {t.goals_met ?? 0}/{t.goals_total}
                          </span>
                        </div>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3 tabular-nums">
                      {(t.in_flight ?? 0) > 0 ? (
                        <span className="font-semibold">{t.in_flight}</span>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3 tabular-nums text-muted-foreground">
                      {fmtRel(t.last_activity_unix ?? t.created_unix)}
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </Card>

      {/* ── Row 5: 자산 분포 | 트래픽 상태 코드 | 가로채기 & 승인 대기 ── */}
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
        {/* 자산 분포 */}
        <Card className="p-4">
          <SectionTitle icon={NetworkIcon} sub="유형별">
            자산 분포
          </SectionTitle>

          {assetByType.length === 0 ? (
            <div className="py-6 text-center text-xs text-muted-foreground">자산 데이터 없음</div>
          ) : (
            <div className="flex flex-col gap-2">
              {assetByType.map(([type, count]) => (
                <div key={type} className="flex items-center gap-2 text-xs">
                  <span className="w-8 shrink-0 text-right text-[10px] text-muted-foreground">
                    {ASSET_TYPE_LABELS[type] ?? type}
                  </span>
                  <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                    <div
                      className={cn("h-full rounded-full", ASSET_COLORS[type] ?? "bg-slate-400")}
                      style={{ width: `${(count / assetMax) * 100}%` }}
                    />
                  </div>
                  <span className="w-4 text-right tabular-nums">{count}</span>
                </div>
              ))}
            </div>
          )}

          <div className="mt-3 border-t pt-3 text-[10px] text-muted-foreground">총 {totalAssets} 노드</div>
        </Card>

        {/* 트래픽 상태 코드 */}
        <Card className="p-4">
          <SectionTitle icon={ActivityIcon} sub={`${traffic.length} 회 요청`}>
            트래픽 상태 코드
          </SectionTitle>

          {/* 막대 차트 */}
          {trafficByCodes.length === 0 ? (
            <div className="py-6 text-center text-xs text-muted-foreground">트래픽 데이터 없음</div>
          ) : (
            <>
              <div className="mb-3 flex items-end gap-2" style={{ height: 52 }}>
                {trafficByCodes.map(({ code, n }) => (
                  <div key={code} className="flex flex-1 flex-col items-center gap-1">
                    <span className="text-[9px] tabular-nums text-muted-foreground">{n}</span>
                    <div
                      className={cn("w-full min-h-1 rounded-sm", statusBg(code), "opacity-80")}
                      style={{ height: Math.max(4, (n / trafficMax) * 36) }}
                    />
                    <span className={cn("text-[9px] font-mono", statusColor(code))}>{code}</span>
                  </div>
                ))}
              </div>

              <div className="border-t pt-2.5">
                <div className="mb-1.5 text-[10px] text-muted-foreground">최근 요청</div>
                <div className="flex flex-col gap-1.5">
                  {recentTraffic.map((e) => (
                    <div key={e.id} className="flex items-center gap-1.5 text-[10px]">
                      <span
                        className={cn(
                          "shrink-0 rounded border px-1 py-0 font-mono text-[9px]",
                          e.method === "GET"
                            ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-400"
                            : "border-blue-500/30 bg-blue-500/10 text-blue-400",
                        )}
                      >
                        {e.method}
                      </span>
                      <span className="min-w-0 flex-1 truncate text-muted-foreground">
                        {e.host}
                        {e.url.replace(/^https?:\/\/[^/]+/, "").substring(0, 30)}
                      </span>
                      <span className={cn("shrink-0 font-mono text-[9px]", statusColor(e.status))}>{e.status}</span>
                    </div>
                  ))}
                </div>
              </div>
            </>
          )}
        </Card>

        {/* 시스템 상태 & 승인 대기 */}
        <Card className="p-4">
          <SectionTitle icon={ShieldCheckIcon}>시스템 상태</SectionTitle>

          <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2">
              <div className="text-[10px] text-muted-foreground">LLM 설정</div>
              <Badge
                variant="outline"
                className={cn(
                  "text-[10px]",
                  stats?.llm_configured
                    ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-400"
                    : "border-red-500/30 bg-red-500/10 text-red-400",
                )}
              >
                {stats?.llm_configured ? "설정됨" : "설정되지 않음"}
              </Badge>
            </div>

            <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2">
              <div className="text-[10px] text-muted-foreground">트래픽 캡처</div>
              <Badge
                variant="outline"
                className={cn(
                  "text-[10px]",
                  settings?.traffic_capture
                    ? "border-violet-500/30 bg-violet-500/10 text-violet-400"
                    : "text-muted-foreground",
                )}
              >
                {settings?.traffic_capture ? "켜짐" : "꺼짐"}
              </Badge>
            </div>

            {activeProfile && (
              <div className="flex items-center justify-between rounded-lg border bg-muted/20 px-3 py-2">
                <div className="text-[10px] text-muted-foreground">활성 모델</div>
                <span className="font-mono text-[10px]">{activeProfile.model}</span>
              </div>
            )}
          </div>

          {/* 대기 중 승인 */}
          {pendingCount > 0 && (
            <div className="mt-3">
              <div className="mb-1.5 text-[10px] font-medium text-amber-400">승인 대기({pendingCount})</div>
              <div className="flex flex-col gap-1.5">
                {pending.slice(0, 3).map((p) => (
                  <Link
                    key={p.id}
                    href="/system/intercept/approvals"
                    className="flex items-center justify-between rounded-lg border border-amber-500/20 bg-amber-500/5 px-2.5 py-2 hover:bg-amber-500/10"
                  >
                    <div className="min-w-0">
                      <div className="text-[10px] font-medium">{p.tool_name}</div>
                      <div className="text-[9px] text-muted-foreground">{p.agent_name}</div>
                    </div>
                    <ArrowUpRightIcon className="size-3 shrink-0 text-amber-400" />
                  </Link>
                ))}
                {pendingCount > 3 && (
                  <Link
                    href="/system/intercept/approvals"
                    className="text-center text-[10px] text-muted-foreground hover:text-foreground"
                  >
                    추가로 {pendingCount - 3} 건…
                  </Link>
                )}
              </div>
            </div>
          )}

          {pendingCount === 0 && (
            <div className="mt-3 rounded-lg border bg-muted/10 px-3 py-3 text-center text-[10px] text-muted-foreground">
              <ShieldCheckIcon className="mx-auto mb-1 size-4 text-emerald-500/50" />
              승인 대기 중인 가로채기 없음
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
