"use client";

// 루트 화면 자체가 실패하면 레이아웃을 쓸 수 없어 언어와 본문을 직접 제공합니다.
export default function GlobalError({ reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <html lang="ko">
      <body style={{ fontFamily: "system-ui", textAlign: "center", padding: "4rem 1rem" }}>
        <h1>화면을 불러오지 못했습니다</h1>
        <p>다시 시도해 주세요. 문제가 계속되면 서버 연결과 시스템 로그를 확인하세요.</p>
        <button type="button" onClick={reset}>
          다시 시도
        </button>
      </body>
    </html>
  );
}
