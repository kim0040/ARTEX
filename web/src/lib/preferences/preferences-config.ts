/**
 * 각 설정을 어디에 저장하는지입니다.
 *
 * "client-cookie"  → 브라우저에서만 쿠키를 씁니다.
 * "server-cookie"  → 서버 액션으로 쿠키를 씁니다.
 * "localStorage"   → 브라우저에만 저장합니다(배치와 무관한 값).
 * "none"           → 저장하지 않고, 새로고침하면 초기화됩니다.
 *
 * 배치에 꼭 필요한 설정(sidebar_variant / sidebar_collapsible)은
 * 서버 렌더와 같아야 해서 localStorage를 쓸 수 없습니다.
 * 나머지는 어떤 저장 방식이든 괜찮습니다.
 */

import type { FontKey } from "@/lib/fonts/registry";

import type { ContentLayout, NavbarStyle, SidebarCollapsible, SidebarVariant } from "./layout";
import type { ThemeMode, ThemePreset } from "./theme";

export type PreferencePersistence = "none" | "client-cookie" | "server-cookie" | "localStorage";

/**
 * 쓸 수 있는 설정 키와 값의 타입입니다.
 */
export type PreferenceValueMap = {
  theme_mode: ThemeMode;
  theme_preset: ThemePreset;
  font: FontKey;
  content_layout: ContentLayout;
  navbar_style: NavbarStyle;
  sidebar_variant: SidebarVariant;
  sidebar_collapsible: SidebarCollapsible;
};

export type PreferenceKey = keyof PreferenceValueMap;

/**
 * 배치에 꼭 필요한 키 → 서버 렌더 화면(사이드바 모양)에 영향을 주므로
 * 서버에서도 읽을 수 있어야 합니다.
 */
export const LAYOUT_CRITICAL_KEYS = ["sidebar_variant", "sidebar_collapsible"] as const;
export type LayoutCriticalKey = (typeof LAYOUT_CRITICAL_KEYS)[number];

/**
 * 나머지는 필수가 아니라 브라우저에서 읽어도 됩니다.
 */
export type NonCriticalKey = Exclude<PreferenceKey, LayoutCriticalKey>;

/**
 * 배치에 꼭 필요한 값은 서버 렌더가 알아야 해서 "localStorage"를 쓸 수 없습니다.
 * 그래서 그 키의 허용 저장 방식에서 뺍니다.
 */
type LayoutCriticalPersistence = Exclude<PreferencePersistence, "localStorage">;

/**
 * 최종 설정:
 * - 배치에 꼭 필요한 키 → 저장 방식이 제한됨
 * - 나머지 키 → 어떤 저장 방식이든 가능
 */
type PreferencePersistenceConfig = {
  [K in LayoutCriticalKey]: LayoutCriticalPersistence;
} & {
  [K in NonCriticalKey]: PreferencePersistence;
};

/**
 * 처음 불러올 때의 기본 설정값입니다.
 */
export const PREFERENCE_DEFAULTS: PreferenceValueMap = {
  theme_mode: "light",
  theme_preset: "default",
  font: "geist",
  content_layout: "full-width",
  navbar_style: "sticky",
  sidebar_variant: "floating",
  sidebar_collapsible: "icon",
};

/**
 * 각 설정을 어떻게 저장하는지입니다.
 * 키마다 바꿀 수 있습니다.
 */
export const PREFERENCE_PERSISTENCE: PreferencePersistenceConfig = {
  theme_mode: "client-cookie",
  theme_preset: "client-cookie",
  font: "client-cookie",
  content_layout: "client-cookie",
  navbar_style: "client-cookie",
  sidebar_variant: "client-cookie", // 배치에 꼭 필요함 → "localStorage"일 수 없습니다
  sidebar_collapsible: "client-cookie", // 배치에 꼭 필요함 → "localStorage"일 수 없습니다
};
