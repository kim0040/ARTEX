"use client";

import Link from "next/link";

import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <div className="flex h-dvh flex-col items-center justify-center space-y-2 text-center">
      <h1 className="font-semibold text-2xl">페이지를 찾을 수 없습니다</h1>
      <p className="text-muted-foreground">주소가 올바른지 확인하거나 작업 목록으로 돌아가세요.</p>
      <Link prefetch={false} replace href="/function/tasks">
        <Button variant="outline">작업 목록으로 돌아가기</Button>
      </Link>
    </div>
  );
}
