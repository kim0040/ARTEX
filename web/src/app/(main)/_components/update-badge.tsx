"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * 윗줄의 "새 버전이 있음" 알림: 페이지를 통째로 불러올 때 한 번 확인하고, 업데이트가 있으면 버전 번호 옆을 밝힙니다.
 * 누르면 시스템 설정 페이지의 「버전과 업데이트」 카드로 바로 갑니다.
 *
 * 백엔드의 GitHub 조회 결과는 30분 캐시가 있으므로, 마운트할 때마다 한 번 조회해도 안전합니다.
 * 인증 없는 GitHub API는 시간당 IP당 60회뿐입니다. 그 캐시가 없으면 탭을 여러 개 열었을 때
 * 할당량을 다 써 버려, 나중에 정말 업데이트하려 해도 조회가 안 됩니다.
 *
 * 조회 실패는 모두 조용히 넘깁니다. 윗줄은 오류를 알리는 곳이 아닙니다. 설정 페이지에서 「업데이트 확인」을 누르면 이유를 봅니다.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update는 이미 "버전 번호를 비교할 수 있음" 판단을 포함합니다. 개발 빌드는 이 알림이 켜지지 않습니다.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 조용히: 망이 없거나 GitHub가 제한해도 윗줄에 오류를 띄우면 안 됩니다.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`새 버전 발견 ${latest}, 클릭하면 업데이트로 이동`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 숨 쉬는 점: 윗줄 요소가 많아 글자만으로는 놓치기 쉽습니다. 움직임으로 한눈에 보이게 합니다. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">새 버전 {latest}</span>
      <span className="sm:hidden">새 버전</span>
    </Link>
  );
}
