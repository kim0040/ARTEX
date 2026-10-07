"use client";

import { Button } from "@/components/ui/button";

// 화면을 그리는 중 생긴 오류도 한국어로 안내합니다. 원문 오류는 제품 화면에 노출하지 않습니다.
export default function ErrorPage({ reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-4 px-6 text-center">
      <h1 className="text-2xl font-semibold">화면을 불러오지 못했습니다</h1>
      <p className="text-muted-foreground">다시 시도해 주세요. 문제가 계속되면 서버 연결과 시스템 로그를 확인하세요.</p>
      <Button onClick={reset}>다시 시도</Button>
    </main>
  );
}
