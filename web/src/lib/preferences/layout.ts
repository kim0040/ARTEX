// 사이드바 형태
export const SIDEBAR_VARIANT_OPTIONS = [
  { label: "기본 메뉴", value: "sidebar" },
  { label: "안쪽 여백", value: "inset" },
  { label: "떠 있는 메뉴", value: "floating" },
] as const;
export const SIDEBAR_VARIANT_VALUES = SIDEBAR_VARIANT_OPTIONS.map((v) => v.value);
export type SidebarVariant = (typeof SIDEBAR_VARIANT_VALUES)[number];

// 사이드바 접기
export const SIDEBAR_COLLAPSIBLE_OPTIONS = [
  { label: "아이콘만 표시", value: "icon" },
  { label: "완전히 숨김", value: "offcanvas" },
] as const;
export const SIDEBAR_COLLAPSIBLE_VALUES = SIDEBAR_COLLAPSIBLE_OPTIONS.map((v) => v.value);
export type SidebarCollapsible = (typeof SIDEBAR_COLLAPSIBLE_VALUES)[number];

// 본문 배치
export const CONTENT_LAYOUT_OPTIONS = [
  { label: "가운데 정렬", value: "centered" },
  { label: "전체 너비", value: "full-width" },
] as const;
export const CONTENT_LAYOUT_VALUES = CONTENT_LAYOUT_OPTIONS.map((v) => v.value);
export type ContentLayout = (typeof CONTENT_LAYOUT_VALUES)[number];

// 위 막대 스타일
export const NAVBAR_STYLE_OPTIONS = [
  { label: "상단 고정", value: "sticky" },
  { label: "화면과 함께 스크롤", value: "scroll" },
] as const;
export const NAVBAR_STYLE_VALUES = NAVBAR_STYLE_OPTIONS.map((v) => v.value);
export type NavbarStyle = (typeof NAVBAR_STYLE_VALUES)[number];
