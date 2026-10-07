"use client";

import type { ReactNode } from "react";
import * as React from "react";

import { AppSidebar } from "@/app/(main)/_components/sidebar/app-sidebar";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { auth } from "@/lib/auth";
import { MOCK } from "@/lib/mock/enabled";
import { getClientCookie } from "@/lib/cookie.client";
import {
  SIDEBAR_COLLAPSIBLE_VALUES,
  SIDEBAR_VARIANT_VALUES,
  type SidebarCollapsible,
  type SidebarVariant,
} from "@/lib/preferences/layout";
import { PREFERENCE_DEFAULTS } from "@/lib/preferences/preferences-config";
import { cn } from "@/lib/utils";

import { MainContent } from "./_components/main-content";

// 화면 배치에 꼭 필요한 설정을 브라우저 쿠키에서 읽습니다(정적 내보내기 사전 렌더에는
// document가 없어서 기본값으로 돌아갑니다). 앱을 정적으로 보낼 수 있게
// 전부 브라우저에서만 처리합니다. Server Actions나
// next/headers는 쓰지 않습니다.
function readPref<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  if (typeof document === "undefined") return fallback;
  const value = getClientCookie(key);
  return value && (allowed as readonly string[]).includes(value) ? (value as T) : fallback;
}

export default function Layout({ children }: Readonly<{ children: ReactNode }>) {
  // 브라우저 쪽 로그인 문입니다. 정적 내보내기가 끄는 Next 프록시/미들웨어를
  // 대신합니다. 토큰이 없으면 /login으로 보내고, 확인 전에는 아무것도 그리지 않아
  // 로그아웃한 사람에게 보호된 화면이나 API 호출이 깜빡이지 않습니다.
  const [authed, setAuthed] = React.useState(false);
  React.useEffect(() => {
    if (auth.getToken()) {
      setAuthed(true);
    } else {
      // 클라이언트 가드가 로그인 안 됨으로 보면, cookie도 같이 지워야 합니다. 그렇지 않으면 proxy.ts는
      // "cookie가 있음"만으로 /login에서 다시 메인 화면으로 돌려보내, 이 가드와 서로 튕깁니다
      // 무한 리다이렉트가 되어 → 흰 화면(cookie와 localStorage가 어긋날 때 발생).
      auth.clearToken();
      window.location.href = "/login";
    }
  }, []);

  const defaultOpen = typeof document === "undefined" ? true : getClientCookie("sidebar_state") !== "false";
  const variant = readPref<SidebarVariant>(
    "sidebar_variant",
    SIDEBAR_VARIANT_VALUES,
    PREFERENCE_DEFAULTS.sidebar_variant,
  );
  const collapsible = readPref<SidebarCollapsible>(
    "sidebar_collapsible",
    SIDEBAR_COLLAPSIBLE_VALUES,
    PREFERENCE_DEFAULTS.sidebar_collapsible,
  );

  if (!authed) return null;

  return (
    <SidebarProvider
      defaultOpen={defaultOpen}
      style={
        {
          "--sidebar-width": "calc(var(--spacing) * 68)",
        } as React.CSSProperties
      }
    >
      <AppSidebar variant={variant} collapsible={collapsible} />
      <SidebarInset
        className={cn(
          "[html[data-content-layout=centered]_&>*]:mx-auto",
          "[html[data-content-layout=centered]_&>*]:w-full",
          "[html[data-content-layout=centered]_&>*]:max-w-screen-2xl",
          "peer-data-[variant=inset]:border",
          "[--dashboard-header-height:--spacing(12)]",
          "min-w-0 overflow-x-hidden",
        )}
      >
        {MOCK && (
          <div role="note" className="border-b bg-muted/50 px-4 py-2 text-sm text-muted-foreground">
            샘플 화면 · 표시된 데이터는 예시입니다. 실제 검사·알림 전송을 실행하지 않습니다.
          </div>
        )}
        <MainContent>{children}</MainContent>
      </SidebarInset>
    </SidebarProvider>
  );
}
