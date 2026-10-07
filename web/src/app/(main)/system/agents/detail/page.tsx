"use client";

import * as React from "react";

import Link from "next/link";
import { useSearchParams } from "next/navigation";

import { ArrowLeftIcon } from "lucide-react";

import { AgentEditor } from "@/components/agent-editor";
import { Button } from "@/components/ui/button";

// 에이전트 하나를 바로 여는 편집 페이지입니다. 기본 흐름은
// /system/agents의 서랍이고, 이 페이지는 같은 AgentEditor를 넓게 써서
// 주소만 있어도(외부 링크 포함) 편집기가 열리게 합니다.
function AgentDetailInner() {
  const searchParams = useSearchParams();
  const key = searchParams.get("key") ?? "";

  return (
    <div className="flex flex-1 flex-col gap-4">
      <div className="flex items-center gap-2">
        <Button asChild variant="ghost" size="icon-sm">
          <Link href="/system/agents">
            <ArrowLeftIcon className="size-4" />
          </Link>
        </Button>
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Agent</h1>
          <p className="text-muted-foreground font-mono text-xs">{key}</p>
        </div>
      </div>
      <div className="flex min-h-0 flex-1 flex-col rounded-lg border">
        <AgentEditor agentKey={key} />
      </div>
    </div>
  );
}

// useSearchParams는 정적 내보내기를 위해 Suspense 안에 있어야 합니다.
export default function AgentDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <AgentDetailInner />
    </React.Suspense>
  );
}
