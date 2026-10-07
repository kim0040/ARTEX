"use client";

import * as React from "react";
import {
  RadioIcon,
  SearchIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  Loader2Icon,
  XIcon,
  Trash2Icon,
  CopyIcon,
  CheckIcon,
} from "lucide-react";

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

import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { api } from "@/lib/api";
import type { LLMRecordItem, LLMRecordDetail, LLMTask } from "@/lib/types";

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("ko-KR", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function fmtLatency(ms: number) {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function fmtTokens(n: number) {
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

function tryFormatJSON(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// 지금 칸 안 텍스트를 복사하는 작은 버튼. 복사에 성공하면 잠시 체크를 보여 줍니다. text가 비었거나 자리표시자뿐이면 끕니다.
function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = React.useState(false);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  React.useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);

  const disabled = !text;
  const copy = async () => {
    if (disabled) return;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // navigator.clipboard는 비보안 맥락(예: http 내부망)에서 쓸 수 없습니다. execCommand로 되돌립니다.
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch { /* 무시: 지원하지 않으면 조용히 넘김 */ }
      document.body.removeChild(ta);
    }
    setCopied(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1500);
  };

  return (
    <Button
      variant="ghost"
      size="icon"
      className="size-5 shrink-0"
      disabled={disabled}
      title={copied ? "복사했습니다" : "내용 복사"}
      onClick={copy}
    >
      {copied ? <CheckIcon className="size-3 text-emerald-600" /> : <CopyIcon className="size-3" />}
    </Button>
  );
}

const PAGE_SIZES = [25, 50, 100];

export default function LLMRecordsPage() {
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(50);
  const [session, setSession] = React.useState("");
  const [sessionQ, setSessionQ] = React.useState("");
  const [model, setModel] = React.useState("");

  const [records, setRecords] = React.useState<LLMRecordItem[]>([]);
  const [total, setTotal] = React.useState(0);
  const [loading, setLoading] = React.useState(false);

  // 기록 켜기/끄기(settings.llm_record, 기본은 끔). 끄면
  // 백엔드는 아무것도 기록하지 않습니다.
  const [recEnabled, setRecEnabled] = React.useState(false);
  const [recBusy, setRecBusy] = React.useState(false);

  // 그 자리 상세 패널(버프처럼 화면을 나누고, 대화창은 아님)
  const [selected, setSelected] = React.useState<LLMRecordItem | null>(null);
  const [detail, setDetail] = React.useState<LLMRecordDetail | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);
  // 정규화 보기 / HTTP 원문 보기. 원문은 provider 쪽 문제를 확인하는 유일한 근거입니다. 정규화 보기는
  // 도구 schema가 들어 있지 않고, 응답에도 tool_use 블록이 없습니다.
  const [rawView, setRawView] = React.useState(false);

  const hasRaw = !!(detail?.raw_request || detail?.raw_response);
  // 스위치는 사용자 선택을 유지하지만, 원문이 없는 예전 기록으로 바꾸면 빈 화면 대신 해석 보기로 자동으로 돌아갑니다.
  const showRaw = rawView && hasRaw;
  // 원문 요청 본문은 JSON입니다. pretty-print는 배치만 바꾸고 의미는 그대로라 읽기 쉽습니다. 원문 응답은 SSE
  // 프레임. tryFormatJSON 해석이 실패하면 그대로 돌려주므로, 양쪽이 함수 하나를 같이 쓰면 됩니다.
  const reqText = showRaw
    ? detail?.raw_request && tryFormatJSON(detail.raw_request)
    : detail?.request_body && tryFormatJSON(detail.request_body);
  const respText = showRaw
    ? detail?.raw_response
    : detail?.response_body && tryFormatJSON(detail.response_body);

  // 작업별 삭제(작업 고르기 + 확인 대화)
  const [tasks, setTasks] = React.useState<LLMTask[]>([]);
  const [pickedTask, setPickedTask] = React.useState("");
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [reloadTick, setReloadTick] = React.useState(0); // 사람이 누른 다시 불러오기 신호

  // 마운트 때 기록 스위치 상태를 불러옵니다.
  React.useEffect(() => {
    let alive = true;
    api
      .settings()
      .then((s) => { if (alive) setRecEnabled(!!s.llm_record); })
      .catch(() => {});
    return () => { alive = false; };
  }, []);

  const toggleRecording = async (on: boolean) => {
    setRecBusy(true);
    setRecEnabled(on); // 먼저 화면에 반영
    try {
      const s = await api.setSettings({ llm_record: on });
      setRecEnabled(!!s.llm_record);
    } catch {
      setRecEnabled(!on); // 실패하면 되돌림
    } finally {
      setRecBusy(false);
    }
  };

  // 세션 필터는 잠깐 기다렸다가 적용합니다.
  React.useEffect(() => {
    const t = setTimeout(() => setSessionQ(session.trim()), 300);
    return () => clearTimeout(t);
  }, [session]);

  // 필터가 바뀌면 페이지를 처음으로 되돌립니다.
  React.useEffect(() => {
    setPage(0);
  }, [sessionQ, model, size, pickedTask]);

  // 목록을 불러옵니다.
  React.useEffect(() => {
    let alive = true;
    setLoading(true);
    api
      .llmRecords({ model: model || undefined, session: sessionQ || undefined, task: pickedTask || undefined, page, size })
      .then((r) => {
        if (!alive) return;
        setRecords(r.records ?? []);
        setTotal(r.total ?? 0);
      })
      .catch(() => {})
      .finally(() => alive && setLoading(false));
    api
      .llmTasks()
      .then((r) => { if (alive) setTasks(r.tasks ?? []); })
      .catch(() => {});
    return () => { alive = false; };
  }, [page, size, sessionQ, model, pickedTask, reloadTick]);

  // 고른 작업의 LLM 기록을 모두 지운 뒤 다시 불러옵니다.
  const confirmDelete = () => {
    setDeleting(true);
    api
      .llmRecordsDeleteTask(pickedTask)
      .then(() => {
        setDeleteOpen(false);
        setSelected(null);
        setDetail(null);
        setPickedTask("");
        setPage(0);
        setReloadTick((t) => t + 1);
      })
      .catch(() => {})
      .finally(() => setDeleting(false));
  };

  // 줄을 고르면 전체 요청/응답을 나중에 불러옵니다.
  React.useEffect(() => {
    if (!selected) {
      setDetail(null);
      return;
    }
    let alive = true;
    setDetailLoading(true);
    setDetail(null);
    api
      .llmRecordDetail(selected.id)
      .then((d) => { if (alive) setDetail(d); })
      .catch(() => {})
      .finally(() => { if (alive) setDetailLoading(false); });
    return () => { alive = false; };
  }, [selected]);

  const totalPages = Math.max(1, Math.ceil(total / size));
  const rangeStart = total === 0 ? 0 : page * size + 1;
  const rangeEnd = page * size + records.length;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {/* 머리 */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-2">
          <RadioIcon className="h-5 w-5 text-muted-foreground" />
          <h1 className="text-xl font-semibold tracking-tight">LLM 기록</h1>
          <Badge variant="secondary">{total}</Badge>
        </div>
      </div>

      {/* 도구 막대 */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative max-w-sm flex-1">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="세션 ID 검색..."
            value={session}
            onChange={(e) => setSession(e.target.value)}
            className="h-8 pl-8"
          />
        </div>
        <Input
          placeholder="모델"
          className="h-8 w-48"
          value={model}
          onChange={(e) => setModel(e.target.value)}
        />
        <Select value={pickedTask} onValueChange={setPickedTask}>
          <SelectTrigger size="sm" className="w-56">
            <SelectValue placeholder="작업 선택…" />
          </SelectTrigger>
          <SelectContent>
            {tasks.length === 0 ? (
              <SelectItem value="__none__" disabled>
                작업 기록 없음
              </SelectItem>
            ) : (
              tasks.map((t) => (
                <SelectItem key={t.task_id} value={t.task_id}>
                  <span className="font-mono">#{t.task_id}</span>
                  <span className="ml-2 text-muted-foreground">（{t.count}）</span>
                </SelectItem>
              ))
            )}
          </SelectContent>
        </Select>
        <Button
          variant="destructive"
          size="sm"
          className="h-8"
          disabled={!pickedTask || deleting}
          title={pickedTask ? undefined : "먼저 위에서 작업을 선택하세요"}
          onClick={() => setDeleteOpen(true)}
        >
          <Trash2Icon className="size-3.5" />
          작업 대화 삭제
        </Button>
        <Select value={String(size)} onValueChange={(v) => setSize(Number(v))}>
          <SelectTrigger size="sm" className="w-28">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZES.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n} / 페이지
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        {/* 기록 켜기/끄기. 끄면 LLM 호출을 기록하지 않습니다 */}
        <div className="flex items-center gap-2 rounded-md border px-2.5 py-1">
          <Switch
            id="llm-rec-toggle"
            size="sm"
            checked={recEnabled}
            onCheckedChange={toggleRecording}
            disabled={recBusy}
          />
          <label
            htmlFor="llm-rec-toggle"
            className={cn(
              "cursor-pointer text-xs font-medium select-none",
              recEnabled ? "text-foreground" : "text-muted-foreground",
            )}
          >
            {recEnabled ? "녹화 중" : "꺼짐"}
          </label>
        </div>

        <div className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
          <span className="tabular-nums">
            {rangeStart}–{rangeEnd} / {total}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page <= 0}
            onClick={() => setPage((p) => Math.max(0, p - 1))}
          >
            <ChevronLeftIcon />
          </Button>
          <span className="tabular-nums">
            {page + 1} / {totalPages}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page + 1 >= totalPages}
            onClick={() => setPage((p) => Math.min(totalPages - 1, p + 1))}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </div>

      {/* 기록 표와 그 자리 상세(버프처럼 화면을 나눔) */}
      <div className="flex h-[calc(100vh-13rem)] min-h-0 flex-col gap-3">
        <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
          <div className="min-h-0 flex-1 overflow-auto">
            <Table>
              <TableHeader className="sticky top-0 z-10 bg-card">
                <TableRow>
                  <TableHead className="w-[130px]">시간</TableHead>
                  <TableHead className="w-[60px]">작업</TableHead>
                  <TableHead className="w-[90px]">워커</TableHead>
                  <TableHead className="w-[100px]">프로필</TableHead>
                  <TableHead className="w-[140px]">모델</TableHead>
                  <TableHead className="w-[70px]">지연</TableHead>
                  <TableHead className="w-[90px]">토큰</TableHead>
                  <TableHead className="w-[60px]">상태</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {loading && records.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center">
                      <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
                    </TableCell>
                  </TableRow>
                ) : records.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                      LLM 호출 기록이 아직 없습니다
                    </TableCell>
                  </TableRow>
                ) : (
                  records.map((rec) => (
                    <TableRow
                      key={rec.id}
                      className={cn(
                        "cursor-pointer",
                        selected?.id === rec.id && "bg-accent hover:bg-accent",
                      )}
                      onClick={() => setSelected(rec)}
                    >
                      <TableCell className="text-xs text-muted-foreground tabular-nums">
                        {fmtTime(rec.ts)}
                      </TableCell>
                      <TableCell className="text-xs font-mono text-muted-foreground">
                        {rec.task_id ? `#${rec.task_id}` : "-"}
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline" className="text-xs font-mono">
                          {rec.worker || "-"}
                        </Badge>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs">{rec.profile_name || "-"}</span>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs font-mono">{rec.model || "-"}</span>
                      </TableCell>
                      <TableCell>
                        <span className={cn("text-xs", rec.latency_ms > 30000 && "text-amber-500")}>
                          {fmtLatency(rec.latency_ms)}
                        </span>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs">
                          {fmtTokens(rec.input_tokens)} / {fmtTokens(rec.output_tokens)}
                        </span>
                      </TableCell>
                      <TableCell>
                        {rec.status === "ok" ? (
                          <Badge variant="secondary" className="text-xs text-emerald-600">정상</Badge>
                        ) : (
                          <Badge variant="destructive" className="text-xs">오류</Badge>
                        )}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        </Card>

        {/* 그 자리 상세 패널 */}
        {selected && (
          <Card className="flex h-[42%] min-h-0 flex-col overflow-hidden py-0">
            {/* 상세 머리 */}
            <div className="flex items-center gap-2 border-b px-3 py-2">
              <Badge variant="outline" className="text-xs font-mono">
                #{selected.id}
              </Badge>
              <Badge variant="outline" className="text-xs font-mono">
                {selected.profile_name || "-"}
              </Badge>
              <Badge variant="outline" className="text-xs font-mono">
                {selected.model || "-"}
              </Badge>
              {selected.task_id && (
                <Badge variant="outline" className="text-xs font-mono">
                  작업 #{selected.task_id}
                </Badge>
              )}
              <span className="text-xs text-muted-foreground">
                {fmtTime(selected.ts)}
              </span>
              <span className={cn("text-xs", selected.latency_ms > 30000 && "text-amber-500")}>
                {fmtLatency(selected.latency_ms)}
              </span>
              {selected.status === "ok" ? (
                <Badge variant="secondary" className="text-xs text-emerald-600">정상</Badge>
              ) : (
                <Badge variant="destructive" className="text-xs">오류</Badge>
              )}
              {/* 원문 보기 스위치. 예전 기록에는 원문이 없습니다. 그때는 조용히 되돌리지 않고 끄어둡니다.
                  「원문과 해석이 같다」처럼 보이지 않게 하기 위해서입니다. */}
              <Button
                variant={showRaw ? "secondary" : "ghost"}
                size="sm"
                className="ml-auto h-7 shrink-0 text-xs"
                disabled={!hasRaw}
                title={hasRaw ? "제공자(provider)와 실제로 주고받은 HTTP 원문 보기" : "이 기록은 이 기능이 생기기 전에 녹화되어 원문이 없습니다"}
                onClick={() => setRawView((v) => !v)}
              >
                원문
              </Button>
              <Button
                variant="ghost"
                size="icon"
                className="size-7 shrink-0"
                onClick={() => setSelected(null)}
              >
                <XIcon />
              </Button>
            </div>
            {/* 요청 / 응답 나눔 */}
            <div className="grid min-h-0 flex-1 grid-cols-2 divide-x">
              <div className="flex min-h-0 min-w-0 flex-col">
                <div className="flex items-center gap-2 border-b py-0.5 pr-1.5 pl-3 text-[11px] font-medium text-muted-foreground">
                  <span>요청{showRaw && " · 원문"}</span>
                  <CopyButton text={reqText || ""} />
                </div>
                <div className="min-h-0 flex-1 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-3 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      불러오는 중…
                    </div>
                  ) : (
                    <pre className="p-3 font-mono text-xs break-all whitespace-pre-wrap">
                      {reqText || "(비어 있음)"}
                    </pre>
                  )}
                </div>
              </div>
              <div className="flex min-h-0 min-w-0 flex-col">
                <div className="flex items-center gap-2 border-b py-0.5 pr-1.5 pl-3 text-[11px] font-medium text-muted-foreground">
                  <span>응답{showRaw && " · 원문(SSE)"}</span>
                  <CopyButton text={respText || ""} />
                </div>
                <div className="min-h-0 flex-1 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-3 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      불러오는 중…
                    </div>
                  ) : (
                    <pre className={cn(
                      "p-3 font-mono text-xs break-all whitespace-pre-wrap",
                      selected.status !== "ok" && "text-red-600 dark:text-red-400",
                    )}>
                      {respText || "(비어 있음)"}
                    </pre>
                  )}
                </div>
              </div>
            </div>
          </Card>
        )}
      </div>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>「{pickedTask}」 작업의 모든 LLM 대화를 삭제할까요?</AlertDialogTitle>
            <AlertDialogDescription>
              이 작업의 모든 LLM 호출 기록(요청/응답 원문 포함)을 영구 삭제합니다. 이 작업은 되돌릴 수 없습니다.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>취소</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
              disabled={deleting}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting ? "삭제 중…" : "삭제 확인"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
