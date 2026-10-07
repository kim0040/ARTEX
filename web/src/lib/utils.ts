import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 서랍/대화상자(Sheet/Dialog)의 onInteractOutside 닫기 판단 보조.
//
// 배경: 서랍 안의 Radix 팝업 층(Select 드롭다운, DropdownMenu, Popover 등)은 서랍으로 portal 되고
// 밖. 팝업 층이 열린 채 딤 배경/서랍 밖을 눌러 접으려 하면, 이번 pointerdown은 Select와 Sheet 두
// DismissableLayer가 같이 처리함. Select가 먼저 닫히고 discrete 이벤트이며 React가 동기 flush하므로,
// Sheet 처리기 차례가 오면 팝업 층의 data-state는 이미 closed로 뒤집혀 있음 —— "지금" 팝업 층이 열려 있는지 검사하면
// 본래 믿을 수 없음(실측으로 이미 확인).
//
// 올바른 방법: Radix의 pointerdown 감시는 버블 단계에 있습니다. 우리는 capture 단계(그보다 앞)에 먼저
// "지금 팝업 층이 열려 있는지"를 기록해 두고, onInteractOutside가 그 기록으로 닫기를 허용할지 정합니다.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: Radix 버블 단계의 pointerdown 처리기보다 먼저 기록
  );
}

// radixOverlayWasOpenAtPointerDown은 "가장 최근 pointerdown 때 Radix 팝업 층이 있었는지"를 돌려줍니다
// 열려 있음". 서랍/대화상자는 이것으로: 팝업 층이 열린 채 딤 배경을 누르면 → 팝업 층만 접고, 자신은 닫지 않음.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText는 텍스트를 클립보드에 쓰고, 성공 여부를 돌려줍니다.
// 배경: navigator.clipboard는 보안 맥락(HTTPS / localhost)에서만 쓸 수 있습니다. IP + HTTP로
// 접근할 때 undefined이면, 그때 execCommand("copy")로 한 단계 내립니다.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 이어서 한 단계 낮은 방안으로
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
