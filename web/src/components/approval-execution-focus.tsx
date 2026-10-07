"use client";

import * as React from "react";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { Activity, InterceptExecution } from "@/lib/types";

// 페이지와 상관없이, 저장된 호출 하나를 정확히 찾습니다.
export function useApprovalFocus({ taskId, conversationId }: { taskId?: string; conversationId?: number }) {
  const [state, setState] = React.useState<{
    id: number;
    source?: InterceptExecution;
    error?: string;
    loading: boolean;
  } | null>(null);
  const [revision, setRevision] = React.useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: revision은 실패 뒤 같은 요청을 다시 시도하는 신호입니다.
  React.useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const id = Number(q.get("approval"));
    const matches = conversationId ? q.get("c") === String(conversationId) : q.get("id") === taskId;
    if (!matches || !Number.isSafeInteger(id) || id <= 0) {
      setState(null);
      return;
    }
    let cancelled = false;
    setState({ id, loading: true });
    api
      .interceptExecution(id, conversationId)
      .then((source) => {
        if (cancelled) return;
        if (
          conversationId
            ? source.conversation_id !== conversationId
            : source.task_id !== taskId || source.conversation_id != null
        ) {
          throw new Error("승인 출처가 현재 세션과 다릅니다");
        }
        setState({ id, source, loading: false });
      })
      .catch((e) => {
        if (!cancelled) setState({ id, error: (e as Error).message || "원래 실행을 찾을 수 없습니다", loading: false });
      });
    return () => {
      cancelled = true;
    };
  }, [taskId, conversationId, revision]);
  const close = React.useCallback(() => {
    const url = new URL(window.location.href);
    url.searchParams.delete("approval");
    window.history.replaceState(null, "", url);
    setState(null);
  }, []);
  return { state, close, retry: () => setRevision((v) => v + 1) };
}

type HistoryPage = { items: Activity[]; hasMore: boolean };

// 그 호출까지 기존 대화 기록을 빈틈없이 채우고, 그 사이의 메시지는
// 모두 남깁니다. 페이지 기록 한가운데 호출 하나만 끼워 넣지 않습니다.
export function useApprovalHistory(
  source: InterceptExecution | undefined,
  loaded: boolean,
  items: Activity[],
  loadPage: (before: number) => Promise<HistoryPage>,
  mergePage: (page: HistoryPage) => void,
) {
  const itemsRef = React.useRef(items);
  itemsRef.current = items;
  const [result, setResult] = React.useState<{ source: InterceptExecution; error?: string }>();
  React.useEffect(() => {
    if (!source || !loaded) return;
    let cancelled = false;
    void (async () => {
      let current = itemsRef.current;
      let before = current[0]?.seq ?? 0;
      while (!current.some((a) => a.seq === source.seq && a.kind === "tool_use")) {
        const page = await loadPage(before);
        if (cancelled) return;
        if (!page.items.length || (before > 0 && page.items[0].seq >= before)) {
          throw new Error("세션에서 해당 도구 호출을 찾지 못했습니다. 기록이 삭제되었을 수 있습니다");
        }
        mergePage(page);
        current = page.items;
        before = current[0].seq;
        if (!page.hasMore && !current.some((a) => a.seq === source.seq)) {
          throw new Error("세션에서 해당 도구 호출을 찾지 못했습니다");
        }
      }
      if (!cancelled) setResult({ source });
    })().catch((error) => {
      if (!cancelled) setResult({ source, error: (error as Error).message });
    });
    return () => {
      cancelled = true;
    };
  }, [source, loaded, loadPage, mergePage]);
  return {
    ready: !!source && result?.source === source && !result.error,
    error: result?.source === source ? result?.error : undefined,
  };
}

export function ApprovalExecutionFocus({
  focus,
  history,
}: {
  focus: ReturnType<typeof useApprovalFocus>;
  history: ReturnType<typeof useApprovalHistory>;
}) {
  const { state } = focus;
  if (!state) return null;
  const error = state.error || history.error;
  return (
    <div
      role="status"
      className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b px-4 py-2 text-xs"
    >
      <span className={error ? "text-destructive" : "text-muted-foreground"}>
        {error
          ? `찾을 수 없음:${error}`
          : history.ready
            ? `승인 펼침 #${state.id} 에 해당하는 도구 호출`
            : `승인 불러오는 중 #${state.id} 이(가) 있는 대화 위치…`}
      </span>
      <div className="flex gap-2">
        {error ? (
          <Button size="sm" variant="outline" onClick={focus.retry}>
            위치 다시 시도
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" onClick={focus.close}>
          위치 표시 취소
        </Button>
      </div>
    </div>
  );
}
