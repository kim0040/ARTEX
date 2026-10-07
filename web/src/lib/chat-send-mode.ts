"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// 세션 입력 칸의 전송/줄바꿈 키. 순수 프론트 선호: localStorage에만 두고, 저장소에 넣지 않으며 계정과 동기화하지 않고,
// 그래서 브라우저를 바꾸면 다시 설정해야 합니다. issue #39 참고. 0.3.2는 Ctrl+Enter 전송을 Enter 전송으로 바꿨고,
// 여기서 예전 키 위치를 선택 항목으로 돌려줍니다.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter 보내기, Shift+Enter 줄바꿈" },
  { value: "ctrl-enter", label: "Ctrl+Enter 보내기, Enter 줄바꿈" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 같은 탭 안의 구독자 집합. localStorage의 storage 이벤트는 「다른」 탭에서만 일어나고,
// 이 페이지에서 설정을 바꾼 뒤 emit으로 같은 페이지의 입력 칸에 알려야 합니다. 그렇지 않으면 새로고침해야 적용됩니다.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 돌려주는 것은 문자열 리터럴입니다. Object.is는 값으로 비교하므로 useSyncExternalStore가 순환에 빠지지 않습니다.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// 서버에는 localStorage가 없습니다. 먼저 기본값을 그리고, hydrate 뒤 getSnapshot이 바로잡습니다.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey는 한 번의 키 입력이 전송이어야 하는지 판단합니다.
// isComposing / keyCode 229는 한글 등 입력기가 글자를 고르는 중입니다. 반드시 통과시켜야 하며, 그렇지 않으면 엔터로 단어를 고를 때 잘못 전송됩니다.
// enter 모드는 Shift만 제외합니다. 0.3.2의 동작과 글자 그대로 같습니다. 설정을 안 바꾼 사용자의 손맛은 그대로입니다.
// ctrl-enter 모드는 Ctrl과 Cmd(macOS)를 모두 받습니다.
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
