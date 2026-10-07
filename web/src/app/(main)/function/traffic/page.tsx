"use client";

// 기록 프록시가 잡아 둔 HTTP 왕래를 표로 봅니다.

import * as React from "react";

import {
  ArrowDownWideNarrowIcon,
  ArrowUpNarrowWideIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  EraserIcon,
  FilterXIcon,
  ListChecksIcon,
  Loader2Icon,
  RadioTowerIcon,
  SearchIcon,
  Trash2Icon,
} from "lucide-react";
import { toast } from "sonner";

import { HttpCodeBlock } from "@/components/http-code-block";
import { LinkTrafficDialog } from "@/components/link-traffic-dialog";
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
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { SortableHead } from "@/components/ui/sortable-head";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api } from "@/lib/api";
import { useStoredSortPreference } from "@/lib/sort-preference";
import type { TrafficDetail, TrafficExchange, TrafficHost, TrafficResp } from "@/lib/types";
import { cn } from "@/lib/utils";

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function fmtBytes(n: number) {
  if (n <= 0) return "0 B";
  // 전체 삭제가 보고하는 되찾은 공간에는 GB가 중요합니다. 캡처가 많은
  // 경우 몇 GB를 돌려줄 수 있습니다.
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1);
  const v = n / 1024 ** i;
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

function statusTone(status: number) {
  if (status >= 500) return "text-red-500";
  if (status >= 400) return "text-amber-500";
  if (status >= 300) return "text-blue-500";
  if (status >= 200) return "text-emerald-500";
  return "text-muted-foreground";
}

function MethodBadge({ method }: { method: string }) {
  return <Badge className="shrink-0 font-mono">{method}</Badge>;
}

// 예전 캡처는 Host를 저장하기 전일 수 있습니다. net/http는 Host를
// Request.Header 밖에 둡니다. 보여 줄 때는 채우고, 새로 기록되는 트래픽은
// 기록 층에서도 같이 고칩니다.
function requestWithHost(raw: string, exchange: TrafficExchange): string {
  if (!raw.trim() || /^host\s*:/im.test(raw)) return raw;
  let host = exchange.host;
  try {
    host = new URL(exchange.url).host || host;
  } catch {
    // 상대 주소나 예전 주소는 색인된 호스트로 돌아갑니다.
  }
  const newline = raw.includes("\r\n") ? "\r\n" : "\n";
  const firstLineEnd = raw.indexOf(newline);
  if (firstLineEnd < 0) return `${raw}${newline}Host: ${host}`;
  return `${raw.slice(0, firstLineEnd + newline.length)}Host: ${host}${newline}${raw.slice(firstLineEnd + newline.length)}`;
}

// 고정된 메서드 목록(서버는 정확히 일치하는 것만 거름). 한 페이지에서
// 선택지를 만들면 그 페이지의 메서드만 나오므로 그렇게 하지 않습니다.
const METHODS = ["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"];
const PAGE_SIZES = [25, 50, 100, 200];
type HostCountSortDirection = "asc" | "desc";

// 서버가 정렬하는 열. 목록은 `sort`로 백엔드에 그대로 보내지고,
// 백엔드가 같은 이름만 허용하므로 traffic.Page와 맞추세요.
const SORT_FIELDS = ["ts", "status", "resp_len"] as const;
type SortField = (typeof SORT_FIELDS)[number];
const SORT_STORAGE_KEY = "traffic-sort";

// 필터 드롭다운의 상태 등급 묶음. 값은 `status`로 보내고,
// 백엔드는 정확한 코드 또는 "Nxx" 등급으로 읽습니다.
const STATUS_BUCKETS = ["2xx", "3xx", "4xx", "5xx"];

export default function TrafficPage() {
  const [selectedFlows, setSelectedFlows] = React.useState<Set<string>>(() => new Set());
  const [linking, setLinking] = React.useState(false);
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(50);
  const [host, setHost] = React.useState(""); // 호스트 원문 입력
  const [hostQ, setHostQ] = React.useState(""); // 잠깐 기다린 뒤 서버로
  const [query, setQuery] = React.useState(""); // 자유 검색 원문 입력
  const [queryQ, setQueryQ] = React.useState(""); // 잠깐 기다린 뒤 서버로
  const [method, setMethod] = React.useState("all");

  // 고급 필터(이슈 177): 응답 본문, 경로, 상태 등급,
  // 응답 크기 범위. 글 입력은 호스트/검색처럼 잠깐 기다리고, 상태
  // 선택은 바로 적용됩니다.
  const [body, setBody] = React.useState("");
  const [bodyQ, setBodyQ] = React.useState("");
  const [path, setPath] = React.useState("");
  const [pathQ, setPathQ] = React.useState("");
  const [statusFilter, setStatusFilter] = React.useState("all");
  const [respMin, setRespMin] = React.useState("");
  const [respMinQ, setRespMinQ] = React.useState("");
  const [respMax, setRespMax] = React.useState("");
  const [respMaxQ, setRespMaxQ] = React.useState("");
  const [sort, setSort] = useStoredSortPreference<SortField>(SORT_STORAGE_KEY, SORT_FIELDS, "ts", "desc");

  const [traffic, setTraffic] = React.useState<TrafficResp | null>(null);
  const [selected, setSelected] = React.useState<TrafficExchange | null>(null);
  const [detail, setDetail] = React.useState<TrafficDetail | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);

  const [hosts, setHosts] = React.useState<TrafficHost[]>([]); // 대상 고르기
  const [selectedHosts, setSelectedHosts] = React.useState<string[]>([]); // 고르기에서 체크한 항목
  const [pickerOpen, setPickerOpen] = React.useState(false);
  const [hostCountSortDirection, setHostCountSortDirection] = React.useState<HostCountSortDirection>("desc");

  const [deleteMode, setDeleteMode] = React.useState<"filter" | "selected" | "all" | null>(null); // null이면 대화창이 닫힘
  const [deleting, setDeleting] = React.useState(false);
  const [reloadTick, setReloadTick] = React.useState(0); // 사람이 누른 다시 불러오기 신호

  // 두 필터 모두 잠깐 기다려, 글자마다 다시 불러오지 않습니다.
  React.useEffect(() => {
    const t = setTimeout(() => setHostQ(host.trim()), 300);
    return () => clearTimeout(t);
  }, [host]);
  React.useEffect(() => {
    const t = setTimeout(() => setQueryQ(query.trim()), 300);
    return () => clearTimeout(t);
  }, [query]);
  React.useEffect(() => {
    const t = setTimeout(() => setBodyQ(body.trim()), 300);
    return () => clearTimeout(t);
  }, [body]);
  React.useEffect(() => {
    const t = setTimeout(() => setPathQ(path.trim()), 300);
    return () => clearTimeout(t);
  }, [path]);
  React.useEffect(() => {
    const t = setTimeout(() => setRespMinQ(respMin.trim()), 300);
    return () => clearTimeout(t);
  }, [respMin]);
  React.useEffect(() => {
    const t = setTimeout(() => setRespMaxQ(respMax.trim()), 300);
    return () => clearTimeout(t);
  }, [respMax]);

  const hasAdvancedFilter = Boolean(bodyQ || pathQ || respMinQ || respMaxQ) || statusFilter !== "all";
  const resetAdvancedFilters = () => {
    setBody("");
    setBodyQ("");
    setPath("");
    setPathQ("");
    setStatusFilter("all");
    setRespMin("");
    setRespMinQ("");
    setRespMax("");
    setRespMaxQ("");
  };

  // 필터, 크기, 정렬이 바뀌면 첫 페이지로 돌아갑니다.
  // biome-ignore lint/correctness/useExhaustiveDependencies: 이 값들은 페이지를 일부러 처음으로 되돌립니다.
  React.useEffect(() => {
    setPage(0);
  }, [hostQ, queryQ, method, size, bodyQ, pathQ, statusFilter, respMinQ, respMaxQ, sort]);

  // 현재 페이지를 불러옵니다. 자동 새로고침은 0페이지(최신)만 해서, 이전
  // 기록을 보는 중에 화면이 튀지 않게 합니다.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reloadTick은 사람이 누른 다시 불러오기 신호입니다.
  React.useEffect(() => {
    let alive = true;
    const load = () => {
      api
        .traffic(page, size, hostQ, method, queryQ, {
          body: bodyQ,
          path: pathQ,
          status: statusFilter,
          respMin: respMinQ,
          respMax: respMaxQ,
          sort: sort.field,
          order: sort.direction,
        })
        .then((r) => {
          if (alive) setTraffic(r);
        })
        .catch(() => {
          // 잠깐 새로고침이 실패해도 마지막으로 성공한 데이터를 유지합니다.
        });
      api
        .trafficHosts()
        .then((r) => {
          if (alive) setHosts(r.hosts ?? []);
        })
        .catch(() => {
          // 잠깐 새로고침이 실패해도 마지막으로 성공한 호스트 목록을 유지합니다.
        });
    };
    load();
    const t = setInterval(() => {
      if (page !== 0) return; // 최신 페이지만 자동으로 새로고칩니다
      load();
    }, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [page, size, hostQ, method, queryQ, bodyQ, pathQ, statusFilter, respMinQ, respMaxQ, sort, reloadTick]);

  // 지금 호스트 필터(부분 일치) 또는 체크한
  // 호스트(정확히 여러 개)의 트래픽을 지운 뒤 다시 불러옵니다.
  const allSelected = hosts.length > 0 && hosts.every((h) => selectedHosts.includes(h.host));
  const sortedHosts = React.useMemo(
    () =>
      [...hosts].sort((a, b) => {
        const countOrder = hostCountSortDirection === "asc" ? a.count - b.count : b.count - a.count;
        return countOrder || a.host.localeCompare(b.host);
      }),
    [hosts, hostCountSortDirection],
  );

  // 필터 없는 전체 비우기는 "비우기", 호스트 범위는 "삭제" — 대화상자의
  // 제목과 확인 버튼은 지금 어떤 삭제인지에 따라 바뀝니다.
  const deleteVerb = deleteMode === "all" ? "비우기" : "삭제";
  const deleteTitle = deleteMode
    ? {
        all: "모든 트래픽 기록을 비울까요?",
        selected: `선택한 항목 삭제 ${selectedHosts.length} 개 대상의 모든 트래픽?`,
        filter: "이 대상의 트래픽을 모두 삭제할까요?",
      }[deleteMode]
    : "";

  // `reclaimed`는 전체 삭제에서만 옵니다. 호스트만 지울 때는
  // 줄 개수만 보고합니다.
  const requestDelete = (mode: "filter" | "selected" | "all"): Promise<{ deleted: number; reclaimed?: number }> => {
    if (mode === "all") return api.trafficDeleteAll();
    if (mode === "selected") return api.trafficDeleteHosts(selectedHosts);
    return api.trafficDeleteHost(hostQ);
  };

  const confirmDelete = () => {
    if (!deleteMode) return;
    setDeleting(true);
    const mode = deleteMode;
    requestDelete(mode)
      .then((r) => {
        setDeleteMode(null);
        setSelected(null);
        setDetail(null);
        if (mode !== "filter") {
          setSelectedHosts([]);
          setPickerOpen(false);
        }
        if (mode === "all") {
          // 빈 인덱스를 압축하는 이유는 되찾은 공간이므로, 그 점을 말합니다.
          const reclaimed = r.reclaimed ?? 0;
          const freed = reclaimed > 0 ? `, 해제 ${fmtBytes(reclaimed)} 저장` : "";
          toast.success(`비움 ${r.deleted} 건의 트래픽${freed}`);
        }
        setPage(0);
        setReloadTick((t) => t + 1);
      })
      .catch((e) => {
        // 확인창을 열어 두어, 실패한 삭제를 다시 시도할 수 있게 합니다.
        if (mode === "all") toast.error(`비우기 실패:${(e as Error).message}`);
      })
      .finally(() => setDeleting(false));
  };

  // 고른 왕래의 원문 요청/응답을 나중에 불러옵니다.
  React.useEffect(() => {
    if (!selected) {
      setDetail(null);
      return;
    }
    let alive = true;
    setDetailLoading(true);
    setDetail(null);
    api
      .trafficExchange(selected.id)
      .then((d) => {
        if (alive) setDetail(d);
      })
      .catch(() => {
        if (alive) setDetail({ req: "(패킷을 불러올 수 없음)", resp: "" });
      })
      .finally(() => {
        if (alive) setDetailLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [selected]);

  // 활성 열을 다시 누르면 방향을 바꾸고, 아니면 새 열을
  // 최신/큰 것부터 정렬합니다.
  const toggleSort = (field: SortField) =>
    setSort((prev) =>
      prev.field === field
        ? { field, direction: prev.direction === "asc" ? "desc" : "asc" }
        : { field, direction: "desc" },
    );

  const exchanges = React.useMemo(() => traffic?.exchanges ?? [], [traffic]);
  const total = traffic?.total ?? exchanges.length;
  const pageCount = Math.max(1, Math.ceil(total / size));
  const rangeStart = total === 0 ? 0 : page * size + 1;
  const rangeEnd = page * size + exchanges.length;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">트래픽</h1>
          <p className="text-muted-foreground text-sm">전역 기록 프록시 · 모든 HTTP 왕래</p>
        </div>
        <div className="flex items-center gap-4 text-sm">
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium",
              traffic?.enabled
                ? "border-emerald-500/20 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
                : "border-transparent bg-muted text-muted-foreground",
            )}
          >
            <RadioTowerIcon className="size-3.5" />
            {traffic?.enabled ? "녹화 중" : "사용 중지"}
          </span>
          {traffic?.proxy && <span className="font-mono text-xs text-muted-foreground">{traffic.proxy}</span>}
          <span className="text-xs text-muted-foreground">
            총 <span className="tabular-nums">{traffic?.count ?? 0}</span> 건
          </span>
        </div>
      </div>

      {/* 도구 막대 */}
      <div className="flex flex-wrap items-center gap-2">
        <Popover open={pickerOpen} onOpenChange={setPickerOpen}>
          <PopoverTrigger asChild>
            <Button variant="outline" size="sm" className="h-8">
              <ListChecksIcon className="size-3.5" />
              {selectedHosts.length > 0 ? `대상 선택(${selectedHosts.length}）` : "대상 선택…"}
            </Button>
          </PopoverTrigger>
          <PopoverContent
            className="w-[calc(100vw-2rem)] p-0 data-open:animate-none data-closed:animate-none sm:w-80"
            align="start"
            collisionPadding={16}
          >
            <div className="flex items-center justify-between border-b px-3 py-2">
              <span className="text-xs font-medium text-muted-foreground">목표별로 일괄 삭제</span>
              <div className="flex items-center gap-1">
                {hosts.length > 0 && (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        onClick={() => setHostCountSortDirection((current) => (current === "desc" ? "asc" : "desc"))}
                        aria-label={
                          hostCountSortDirection === "desc"
                            ? "데이터 패킷 수가 현재 내림차순입니다. 클릭하면 오름차순으로 바뀝니다"
                            : "데이터 패킷 수가 현재 오름차순입니다. 클릭하면 내림차순으로 바뀝니다"
                        }
                      >
                        {hostCountSortDirection === "desc" ? <ArrowDownWideNarrowIcon /> : <ArrowUpNarrowWideIcon />}
                      </Button>
                    </TooltipTrigger>
                    <TooltipContent>패킷 수 기준{hostCountSortDirection === "desc" ? "내림차순" : "오름차순"}</TooltipContent>
                  </Tooltip>
                )}
                {hosts.length > 0 && (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-6 px-2 text-xs"
                    onClick={() => setSelectedHosts(allSelected ? [] : hosts.map((h) => h.host))}
                  >
                    {allSelected ? "모두 선택 해제" : "모두 선택"}
                  </Button>
                )}
              </div>
            </div>
            <div className="max-h-64 overflow-y-auto">
              {hosts.length === 0 ? (
                <div className="px-3 py-6 text-center text-xs text-muted-foreground">트래픽 기록 없음</div>
              ) : (
                sortedHosts.map((h, index) => (
                  <label
                    key={h.host}
                    htmlFor={`traffic-host-${index}`}
                    className="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs hover:bg-accent"
                  >
                    <Checkbox
                      id={`traffic-host-${index}`}
                      checked={selectedHosts.includes(h.host)}
                      onCheckedChange={() =>
                        setSelectedHosts((prev) =>
                          prev.includes(h.host) ? prev.filter((x) => x !== h.host) : [...prev, h.host],
                        )
                      }
                    />
                    <span className="truncate font-mono">{h.host}</span>
                    <span className="ml-auto shrink-0 tabular-nums text-muted-foreground">{h.count}</span>
                  </label>
                ))
              )}
            </div>
            <div className="border-t p-2">
              <Button
                variant="destructive"
                size="sm"
                className="w-full"
                disabled={selectedHosts.length === 0}
                onClick={() => {
                  setDeleteMode("selected");
                  setPickerOpen(false);
                }}
              >
                선택 삭제({selectedHosts.length}）
              </Button>
            </div>
          </PopoverContent>
        </Popover>
        <div className="relative w-48">
          <Input placeholder="host…" value={host} onChange={(e) => setHost(e.target.value)} className="h-8" />
        </div>
        <Button
          variant="destructive"
          size="sm"
          className="h-8"
          disabled={!hostQ || deleting}
          title={hostQ ? undefined : "먼저 왼쪽에서 대상을 고르거나 host를 입력하세요"}
          onClick={() => setDeleteMode("filter")}
        >
          <Trash2Icon className="size-3.5" />
          이 목표 삭제
        </Button>
        {/* 두 번째 파괴 버튼처럼 채우지 않고 테두리만 씀: 이 버튼은 모든
            필터를 무시하므로, 한 번만 잘못 눌러도 "이 목표 삭제"가 되는 것처럼 보이면 안 됩니다. */}
        <Button
          variant="outline"
          size="sm"
          className="h-8 text-destructive hover:bg-destructive/10 hover:text-destructive"
          disabled={!traffic?.count || deleting}
          title={traffic?.count ? "트래픽을 모두 삭제하고 저장소를 압축" : "현재 트래픽 기록이 없습니다"}
          onClick={() => setDeleteMode("all")}
        >
          <EraserIcon className="size-3.5" />
          모두 비우기
        </Button>
        <div className="relative max-w-sm flex-1">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="전체 검색(URL / 메서드 / 유형 / 상태 코드…)"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="h-8 pl-8"
          />
        </div>
        <Select value={method} onValueChange={setMethod}>
          <SelectTrigger size="sm" className="w-32">
            <SelectValue placeholder="메서드" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">모든 메서드</SelectItem>
            {METHODS.map((m) => (
              <SelectItem key={m} value={m}>
                {m}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
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
            {page + 1} / {pageCount}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page + 1 >= pageCount}
            onClick={() => setPage((p) => Math.min(pageCount - 1, p + 1))}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </div>

      {/* 고급 필터(이슈 177): 수십만 왕래 중에서 패킷 하나로 좁힙니다. */}
      <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/30 px-2 py-1.5">
        <span className="pl-1 text-xs font-medium text-muted-foreground">고급 필터</span>
        <div className="relative w-56">
          <Input
            placeholder="응답 내용(본문 키워드, 3자 이상)"
            value={body}
            onChange={(e) => setBody(e.target.value)}
            className="h-8"
          />
        </div>
        <div className="relative w-52">
          <Input
            placeholder="경로(예: /api/user/…)"
            value={path}
            onChange={(e) => setPath(e.target.value)}
            className="h-8"
          />
        </div>
        <Select value={statusFilter} onValueChange={setStatusFilter}>
          <SelectTrigger size="sm" className="w-28">
            <SelectValue placeholder="상태 코드" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">모든 상태 코드</SelectItem>
            {STATUS_BUCKETS.map((s) => (
              <SelectItem key={s} value={s}>
                {s}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="flex items-center gap-1 text-xs text-muted-foreground">
          <span>응답 길이</span>
          <Input
            type="number"
            min={0}
            placeholder="최소(B)"
            value={respMin}
            onChange={(e) => setRespMin(e.target.value)}
            className="h-8 w-24"
          />
          <span>–</span>
          <Input
            type="number"
            min={0}
            placeholder="최대(B)"
            value={respMax}
            onChange={(e) => setRespMax(e.target.value)}
            className="h-8 w-24"
          />
        </div>
        {hasAdvancedFilter ? (
          <Button variant="ghost" size="sm" className="h-8" onClick={resetAdvancedFilters}>
            <FilterXIcon className="size-3.5" />
            필터 지우기
          </Button>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm text-muted-foreground">선택됨 {selectedFlows.size} 건의 트래픽</span>
        <Button variant="outline" size="sm" disabled={selectedFlows.size === 0} onClick={() => setLinking(true)}>
          발견에 연결
        </Button>
        {selectedFlows.size > 0 ? (
          <Button variant="ghost" size="sm" onClick={() => setSelectedFlows(new Set())}>
            선택 비우기
          </Button>
        ) : null}
      </div>
      {/* 기록 표 */}
      <div className="flex h-[calc(100vh-15rem)] min-h-0 flex-col">
        <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
          <div className="min-h-0 flex-1 overflow-auto">
            <Table>
              <TableHeader className="sticky top-0 z-10 bg-card">
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      aria-label="이 페이지의 트래픽 선택"
                      checked={exchanges.length > 0 && exchanges.every((e) => selectedFlows.has(e.id))}
                      onCheckedChange={(checked) =>
                        setSelectedFlows((previous) => {
                          const next = new Set(previous);
                          for (const e of exchanges) {
                            if (checked === true) next.add(e.id);
                            else next.delete(e.id);
                          }
                          return next;
                        })
                      }
                    />
                  </TableHead>
                  <SortableHead
                    field="ts"
                    label="시간"
                    activeField={sort.field}
                    direction={sort.direction}
                    onSort={toggleSort}
                    className="w-36"
                  />
                  <TableHead className="w-44">host</TableHead>
                  <TableHead className="w-20">메서드</TableHead>
                  <TableHead>URL</TableHead>
                  <SortableHead
                    field="status"
                    label="상태 코드"
                    activeField={sort.field}
                    direction={sort.direction}
                    onSort={toggleSort}
                    className="w-20"
                  />
                  <TableHead className="w-36">content-type</TableHead>
                  <SortableHead
                    field="resp_len"
                    label="응답 길이"
                    activeField={sort.field}
                    direction={sort.direction}
                    onSort={toggleSort}
                    align="right"
                    className="w-24 text-right"
                  />
                </TableRow>
              </TableHeader>
              <TableBody>
                {exchanges.map((e) => (
                  <TableRow
                    key={e.id}
                    className={cn("cursor-pointer", selected?.id === e.id && "bg-accent hover:bg-accent")}
                    onClick={() => setSelected(e)}
                  >
                    <TableCell>
                      <Checkbox
                        aria-label={`트래픽 선택 ${e.id}`}
                        checked={selectedFlows.has(e.id)}
                        onClick={(event) => event.stopPropagation()}
                        onCheckedChange={(checked) =>
                          setSelectedFlows((previous) => {
                            const next = new Set(previous);
                            if (checked === true) next.add(e.id);
                            else next.delete(e.id);
                            return next;
                          })
                        }
                      />
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground tabular-nums">{fmtTime(e.ts)}</TableCell>
                    <TableCell className="font-mono text-xs">{e.host}</TableCell>
                    <TableCell>
                      <MethodBadge method={e.method} />
                    </TableCell>
                    <TableCell className="max-w-0">
                      <span className="block truncate font-mono text-xs">{e.url}</span>
                    </TableCell>
                    <TableCell>
                      <span className={cn("font-mono text-xs font-semibold tabular-nums", statusTone(e.status))}>
                        {e.status}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{e.content_type}</TableCell>
                    <TableCell className="text-right text-xs tabular-nums">{fmtBytes(e.resp_len)}</TableCell>
                  </TableRow>
                ))}
                {exchanges.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                      {traffic === null ? "불러오는 중…" : "일치하는 트래픽이 없습니다."}
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
        </Card>
      </div>

      {linking ? (
        <LinkTrafficDialog
          trafficIds={[...selectedFlows]}
          onClose={() => setLinking(false)}
          onBound={() => setSelectedFlows(new Set())}
        />
      ) : null}
      <Sheet open={selected !== null} onOpenChange={(open) => !open && setSelected(null)}>
        <SheetContent className="w-full! max-w-none! gap-0 p-0 sm:w-[48rem]! sm:max-w-[48rem]!">
          {selected && (
            <>
              <SheetHeader className="border-b px-5 py-4">
                <div className="flex items-center gap-2 pr-8">
                  <MethodBadge method={selected.method} />
                  <Badge variant="secondary" className={cn("font-mono tabular-nums", statusTone(selected.status))}>
                    {selected.status}
                  </Badge>
                  <span className="ml-auto text-xs text-muted-foreground tabular-nums">{fmtTime(selected.ts)}</span>
                </div>
                <SheetTitle className="break-all font-mono">{selected.host}</SheetTitle>
                <SheetDescription className="break-all font-mono">{selected.url}</SheetDescription>
              </SheetHeader>
              <Tabs defaultValue="request" className="min-h-0 flex-1 gap-0">
                <TabsList className="mx-5 mt-4 grid w-auto grid-cols-2">
                  <TabsTrigger value="request">요청 Request</TabsTrigger>
                  <TabsTrigger value="response">응답 Response</TabsTrigger>
                </TabsList>
                <TabsContent value="request" className="min-h-0 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-5 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      패킷 불러오는 중…
                    </div>
                  ) : (
                    <HttpCodeBlock raw={requestWithHost(detail?.req ?? "", selected)} />
                  )}
                </TabsContent>
                <TabsContent value="response" className="min-h-0 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-5 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      패킷 불러오는 중…
                    </div>
                  ) : (
                    <HttpCodeBlock raw={detail?.resp ?? ""} />
                  )}
                </TabsContent>
              </Tabs>
            </>
          )}
        </SheetContent>
      </Sheet>

      <AlertDialog
        open={deleteMode !== null}
        onOpenChange={(o) => {
          if (!o) setDeleteMode(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{deleteTitle}</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteMode === "all" && (
                <>
                  전부를 영구 삭제 <span className="font-semibold tabular-nums">{traffic?.count ?? 0}</span>{" "}
                  건의 트래픽 기록(요청/응답 원문 포함). 현재 필터는 무시합니다. 이 작업은 되돌릴 수 없습니다. 발견에 이미 묶인 트래픽 증거는 별도의 증거 보관함에 있어 영향을 받지 않습니다.
                  <br />
                  <span className="text-muted-foreground">
                    비우면 저장 공간도 함께 압축해, 인덱스가 쓰던 디스크를 시스템에 돌려줍니다. 그동안 트래픽 기록은 잠시 멈춥니다.
                  </span>
                </>
              )}
              {deleteMode === "selected" && (
                <>
                  다음을 영구 삭제 <span className="font-semibold tabular-nums">{selectedHosts.length}</span> 개 목표(
                  <span className="font-mono">
                    {selectedHosts.slice(0, 3).join("、")}
                    {selectedHosts.length > 3 ? "…" : ""}
                  </span>
                  )의 모든 트래픽 기록(요청/응답 원문 포함). 이 작업은 되돌릴 수 없습니다.
                </>
              )}
              {deleteMode === "filter" && (
                <>
                  host에 다음이 포함된 항목을 영구 삭제 <span className="font-mono font-semibold">{hostQ}</span>{" "}
                  의 모든 트래픽 기록(요청/응답 원문 포함). 이 작업은 되돌릴 수 없습니다.
                </>
              )}
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
              {deleting ? `${deleteVerb}중…` : `확인${deleteVerb}`}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
