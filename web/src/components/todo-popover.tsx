"use client";

import * as React from "react";
import { ListTodo } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

// TodoPopover는 세션의 최신 TodoWrite 상태를 보여 줍니다(대화 또는
// 작업의 워커/플래너 재생). 할 일 전체는 활동 목록에 없고
// 상세는 나중에 불러오므로, 열 때 가장 최근 TodoWrite
// 호출의 상세를 seq로 가져와 {todos:[…]} JSON을 읽습니다. 브라우저에서만 하고, 호출하는 쪽이
// seq와 상세 조회 함수(대화 또는 탐색 주소)를 넘깁니다.
export function TodoPopover({
  seq,
  fetchDetail,
}: {
  seq: number | null;
  fetchDetail: (seq: number) => Promise<string>;
}) {
  const [open, setOpen] = React.useState(false);
  const [todos, setTodos] = React.useState<{ content: string; status: string }[] | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [err, setErr] = React.useState("");

  const load = React.useCallback(async () => {
    if (seq == null) return;
    setLoading(true);
    setErr("");
    try {
      const detail = await fetchDetail(seq);
      const start = detail.indexOf("{"); // detail에 "TodoWrite " 접두사가 있을 수 있음
      const parsed = JSON.parse(start >= 0 ? detail.slice(start) : detail);
      setTodos(Array.isArray(parsed?.todos) ? parsed.todos : []);
    } catch {
      setErr("Todo 해석 실패");
      setTodos(null);
    } finally {
      setLoading(false);
    }
  }, [seq, fetchDetail]);

  // 열 때마다 다시 읽습니다. 실행이 진행되면 할 일이 바뀝니다.
  React.useEffect(() => {
    if (open) load();
  }, [open, load]);

  const disabled = seq == null;
  const MARK: Record<string, string> = { pending: "☐", in_progress: "▶", completed: "✔" };
  return (
    <Popover open={open} onOpenChange={disabled ? undefined : setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          title={disabled ? "이 세션에는 아직 Todo가 없습니다" : "최근 Todo 보기"}
          className="text-muted-foreground/70 hover:text-primary flex items-center gap-0.5 text-xs disabled:pointer-events-none disabled:opacity-40"
        >
          <ListTodo className="size-3" />
          Todo
        </button>
      </PopoverTrigger>
      <PopoverContent align="end" className="max-h-80 w-80 overflow-auto p-2">
        <p className="text-muted-foreground px-1 pb-1 text-[11px] font-medium">
          최근 Todo{loading ? " · 불러오는 중…" : ""}
        </p>
        {err && <p className="text-destructive px-1 text-xs">{err}</p>}
        {todos && todos.length === 0 && !loading && (
          <p className="text-muted-foreground px-1 text-xs">(비어 있음)</p>
        )}
        <ul className="space-y-0.5">
          {(todos ?? []).map((t, i) => (
            <li
              key={`${i}:${t.content}`}
              className={cn("flex gap-1.5 px-1 text-xs", t.status === "completed" && "text-muted-foreground line-through")}
            >
              <span className="shrink-0">{MARK[t.status] ?? "☐"}</span>
              <span className="break-words">{t.content}</span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
