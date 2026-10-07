"use client";

import * as React from "react";

import {
  ArrowUpIcon,
  Bot,
  ChevronDownIcon,
  ChevronRightIcon,
  ListChecksIcon,
  Loader2Icon,
  MoreHorizontalIcon,
  PaperclipIcon,
  PencilIcon,
  PinIcon,
  PinOffIcon,
  PlusIcon,
  Square,
  Trash2Icon,
  XIcon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { MentionTextarea } from "@/components/mention-textarea";
import { SideQuestionButton, SideQuestionWorkspace } from "@/components/side-question-workspace";
import { TodoPopover } from "@/components/todo-popover";
import { ApprovalExecutionFocus, useApprovalFocus, useApprovalHistory } from "@/components/approval-execution-focus";
import { Transcript } from "@/components/transcript";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { mergeActivities } from "@/lib/activity-merge";
import { api } from "@/lib/api";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import { isBtwCommand } from "@/lib/side-questions";
import type { Activity, Agent, ChatAttachment, Conversation, LLMProfile } from "@/lib/types";
import { cn } from "@/lib/utils";

// fmtBytes는 첨부 칩에 사람이 읽기 쉬운 파일 크기를 그립니다(transcript.tsx와 같음).
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// fmtTokens는 토큰 수를 짧게 그립니다(1234 → 1.2k, 2_000_000 → 2M).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

// fmtDuration은 지난 밀리초를 짧게 그립니다(90초 → 1분 30초).
function fmtDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h${String(m % 60).padStart(2, "0")}m`;
}

// HISTORY_PAGE는 기록 한 페이지에 담는 단계 수입니다. 열면 최신 페이지,
// 맨 위로 스크롤할 때마다 한 페이지를 더 불러옵니다. 길게 잡아 긴
// 대화도 가볍게 둡니다(위로 올리기 전에는 한 페이지 정도만 화면에 있습니다).
const HISTORY_PAGE = 200;
const CONVERSATION_LIST_PAGE = 100;

// 왼쪽 목록에서 사용자가 접어 둔 에이전트 묶음입니다. 저장해서
// 새로고침 뒤에도 목록이 같게 보입니다. 모르는 키는 해롭지 않습니다(지운 에이전트는
// 다시 묶음을 그리지 않을 뿐입니다).
const COLLAPSED_AGENTS_KEY = "artex.chat.collapsed-agents";

function conversationIsPinned(conversation: Conversation): boolean {
  return conversation.pinned ?? Boolean(conversation.pinned_at);
}

// AgentGroup은 왼쪽 목록의 접히는 구역 하나입니다. 고정되지 않은
// 한 에이전트의 대화를, 최근 활동이 먼저 오게 모읍니다.
interface AgentGroup {
  key: string;
  name: string;
  conversations: Conversation[];
  runningCount: number;
}

// groupByAgent는 대화를 에이전트별로 나눕니다. 들어온 순서는
// 묶음 안과 묶음 사이 모두 유지합니다. 서버가 이미 updated_at
// 내림차순이라, 처음 나타난 순서 = 가장 최근에 움직인 묶음이 먼저입니다.
function groupByAgent(conversations: Conversation[], agentByKey: Map<string, Agent>): AgentGroup[] {
  const groups = new Map<string, AgentGroup>();
  for (const conversation of conversations) {
    let group = groups.get(conversation.agent_key);
    if (!group) {
      group = {
        key: conversation.agent_key,
        name: agentByKey.get(conversation.agent_key)?.name || conversation.agent_key,
        conversations: [],
        runningCount: 0,
      };
      groups.set(conversation.agent_key, group);
    }
    group.conversations.push(conversation);
    if (conversation.running) group.runningCount++;
  }
  return [...groups.values()];
}

// LiveBadge는 작업의 메인 에이전트에서 재사용한, 작게 맥박 치는 "실시간" 칩입니다
// 콘솔. 차례가 흐르는 동안 보입니다.
function LiveBadge() {
  return (
    <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
      <span className="size-1 animate-pulse rounded-full bg-blue-500" />
      실시간
    </span>
  );
}

// Composer는 아래쪽 공통 입력칸입니다(입력칸은 상한까지 자라고, Enter는 보내기,
// Shift+Enter는 줄바꿈). DraftChat과 ChatView가 같은 방식입니다.
function Composer({
  value,
  onChange,
  onSend,
  disabled,
  placeholder,
  leftSlot,
  running,
  onStop,
  stopDisabled,
  attachments,
  onPickFiles,
  onRemoveAttachment,
  uploading,
  allowBtw,
}: {
  value: string;
  onChange: (v: string) => void;
  onSend: () => void;
  disabled: boolean;
  placeholder: string;
  leftSlot?: React.ReactNode;
  running?: boolean;
  onStop?: () => void;
  stopDisabled?: boolean;
  // 방법 1 파일 업로드: onPickFiles를 넘긴 경우에만 클립 버튼 + 첨부 chip 미리보기를 보여 줍니다.
  attachments?: ChatAttachment[];
  onPickFiles?: (files: File[]) => void;
  onRemoveAttachment?: (path: string) => void;
  uploading?: boolean;
  allowBtw?: boolean;
}) {
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const atts = attachments ?? [];
  // 전송 키는 시스템 설정이 정합니다(localStorage). 기본은 Enter로 전송.
  const sendMode = useChatSendMode();
  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (!shouldSubmitOnKey(e, sendMode)) return;
    e.preventDefault();
    onSend();
  }
  return (
    <div className="border-t p-3">
      {atts.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {atts.map((a) => (
            <div
              key={a.path}
              className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
              title={a.path}
            >
              <PaperclipIcon className="size-3 shrink-0 text-primary" />
              <span className="max-w-[160px] truncate">{a.name}</span>
              <span className="text-muted-foreground">{fmtBytes(a.size)}</span>
              {onRemoveAttachment && (
                <button
                  type="button"
                  className="ml-0.5 text-muted-foreground hover:text-foreground"
                  onClick={() => onRemoveAttachment(a.path)}
                  title="제거"
                >
                  <XIcon className="size-3" />
                </button>
              )}
            </div>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {leftSlot ? <div className="w-full sm:w-auto">{leftSlot}</div> : null}
        {onPickFiles && (
          <>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              onChange={(e) => {
                // FileList는 input 요소와 살아 있는 연결입니다. 먼저 배열로 스냅샷한 다음 value를 비워야 하고,
                // 그렇지 않으면 비동기 onPickFiles(예: 초안 상태에서 먼저 세션을 만듦)가 다시 실행될 때 빈 목록을 받습니다.
                const picked = Array.from(e.target.files ?? []);
                e.target.value = ""; // 같은 파일을 다시 고를 수 있게 비웁니다
                if (picked.length > 0) onPickFiles(picked);
              }}
            />
            <Button
              size="icon"
              variant="ghost"
              onClick={() => fileInputRef.current?.click()}
              disabled={disabled || uploading}
              title="파일 업로드"
            >
              {uploading ? <Loader2Icon className="size-4 animate-spin" /> : <PaperclipIcon className="size-4" />}
            </Button>
          </>
        )}
        <MentionTextarea
          className="max-h-40 min-h-10 min-w-0 flex-1 resize-none"
          rows={1}
          placeholder={placeholder}
          value={value}
          disabled={disabled && !(running && allowBtw)}
          onValueChange={onChange}
          onKeyDown={onKeyDown}
        />
        {running && allowBtw && isBtwCommand(value) && (
          <Button size="icon" onClick={onSend} aria-label="옆길 질문 보내기" title="옆길 질문 보내기">
            <ArrowUpIcon />
          </Button>
        )}
        {running ? (
          // 실행 중에는 보내기 버튼이 멈추기 버튼이 됩니다.
          // 이 세션만 중단합니다(트리거 대기열은 계속 갑니다).
          <Button size="icon" variant="destructive" onClick={onStop} disabled={stopDisabled} title="이번 실행 중지">
            <Square className="size-3.5 fill-current" />
          </Button>
        ) : (
          <Button
            size="icon"
            onClick={onSend}
            disabled={disabled || (!value.trim() && atts.length === 0)}
            title="메시지 보내기"
            aria-label="메시지 보내기"
          >
            <ArrowUpIcon />
          </Button>
        )}
      </div>
    </div>
  );
}

// LLMProfileRow는 입력칸 아래에 활성 LLM 설정을 보여주고
// 팝오버로 바꿉니다. `selected`는 프로필 id이고, null이면 기본값입니다.
function LLMProfileRow({
  profiles,
  selected,
  onChange,
  disabled,
  rightSlot,
}: {
  profiles: LLMProfile[];
  selected: number | null;
  onChange: (id: number | null) => void;
  disabled?: boolean;
  rightSlot?: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  const activeDefault = profiles.find((p) => p.is_default);
  const current = selected != null ? profiles.find((p) => Number(p.id) === selected) : null;
  const label = current ? current.name : `기본${activeDefault ? `（${activeDefault.name}）` : ""}`;

  return (
    <div className="flex min-w-0 shrink-0 items-center gap-1 px-1 pt-0.5 pb-1">
      <ZapIcon className="text-muted-foreground/50 size-3 shrink-0" />
      <span className="truncate text-muted-foreground/70 text-xs" title={label}>
        {label}
      </span>
      <Popover open={open} onOpenChange={disabled ? undefined : setOpen}>
        <PopoverTrigger asChild>
          <button
            type="button"
            disabled={disabled}
            className="flex shrink-0 items-center gap-0.5 text-primary text-xs hover:underline disabled:pointer-events-none disabled:opacity-40"
          >
            바꾸기
            <ChevronDownIcon className="size-3" />
          </button>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-64 p-1">
          <p className="text-muted-foreground px-2 py-1 text-[11px] font-medium">LLM 설정 선택</p>
          {/* 기본 선택 */}
          <button
            type="button"
            onClick={() => {
              onChange(null);
              setOpen(false);
            }}
            className={cn(
              "flex w-full flex-col rounded px-2 py-1.5 text-left hover:bg-accent",
              selected == null && "bg-accent",
            )}
          >
            <span className="text-sm">기본{activeDefault ? `（${activeDefault.name}）` : ""}</span>
            {activeDefault && (
              <span className="text-muted-foreground text-[11px]">
                {activeDefault.format} · {activeDefault.model}
              </span>
            )}
          </button>
          {profiles.map((p) => (
            <button
              key={p.id}
              type="button"
              onClick={() => {
                onChange(Number(p.id));
                setOpen(false);
              }}
              className={cn(
                "flex w-full flex-col rounded px-2 py-1.5 text-left hover:bg-accent",
                selected === Number(p.id) && "bg-accent",
              )}
            >
              <span className="text-sm">{p.name}</span>
              <span className="text-muted-foreground text-[11px]">
                {p.format} · {p.model}
              </span>
            </button>
          ))}
        </PopoverContent>
      </Popover>
      {rightSlot}
    </div>
  );
}

// DraftChat은 오른쪽의 기본 화면입니다. 새 채팅(머리에 에이전트 선택,
// 가운데 빈 상태, 입력칸)이고 대화는 아직 없습니다.
// 대화는 처음 보낼 때 만들어집니다(ChatGPT처럼). 그다음
// 부모가 진짜 ChatView로 바꿉니다.
function DraftChat({
  agents,
  profiles,
  onStarted,
}: {
  agents: Agent[];
  profiles: LLMProfile[];
  onStarted: (c: Conversation, pending?: { input?: string; attachments?: ChatAttachment[] }) => void;
}) {
  const [agentKey, setAgentKey] = React.useState("");
  const [llmProfileId, setLlmProfileId] = React.useState<number | null>(null);
  const [input, setInput] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [uploading, setUploading] = React.useState(false);

  // 에이전트가 로드되면 기본값을 Auto로 둡니다.
  React.useEffect(() => {
    if (!agentKey && agents.some((a) => a.key === "auto")) setAgentKey("auto");
  }, [agentKey, agents]);

  const agent = agents.find((a) => a.key === agentKey);

  async function send() {
    const msg = input.trim();
    if (!msg || !agentKey || sending) return;
    setSending(true);
    try {
      const c = await api.createConversation(agentKey, "", llmProfileId);
      await api.sendConversationMessage(c.id, msg);
      onStarted(c);
    } catch (e) {
      toast.error("보내기 실패:" + (e as Error).message);
      setSending(false);
    }
  }

  // 첨부는 대화를 소유자로 하는 업로드 폴더(sessions/conv-<id>/)가 필요합니다.
  // 초안에는 아직 없어서, 파일을 고르면 대화를 만들고 그 안에
  // 올린 뒤, 친 글과 첨부를 ChatView로 넘깁니다.
  // 사용자는 거기서 보냅니다). 작업 메인 에이전트 콘솔의 업로드를
  // 처음 보낼 때 대화를 만드는 흐름에 맞춘 것입니다.
  async function pickFiles(files: File[]) {
    if (files.length === 0 || !agentKey || uploading || sending) return;
    setUploading(true);
    try {
      const c = await api.createConversation(agentKey, "", llmProfileId);
      const r = await api.chatUpload("session", `conv-${c.id}`, files);
      onStarted(c, { input, attachments: r.attachments });
    } catch (e) {
      toast.error("업로드 실패:" + (e as Error).message);
      setUploading(false);
    }
  }

  const agentPicker = (
    <Select value={agentKey} onValueChange={setAgentKey}>
      <SelectTrigger className="w-full sm:w-40">
        <SelectValue placeholder="에이전트 선택…" />
      </SelectTrigger>
      <SelectContent>
        <SelectGroup>
          {agents.map((a) => (
            <SelectItem key={a.key} value={a.key}>
              <span className="flex items-center gap-2">
                <Bot className="size-3.5" />
                {a.name}
                {!a.builtin && (
                  <Badge variant="outline" className="px-1 py-0 text-[9px]">
                    사용자 지정
                  </Badge>
                )}
              </span>
            </SelectItem>
          ))}
        </SelectGroup>
      </SelectContent>
    </Select>
  );

  return (
    <>
      {/* 빈 시작 상태가 패널을 채웁니다 */}
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 px-4 text-center">
        <div className="bg-primary/10 flex size-12 items-center justify-center rounded-full">
          <Bot className="text-primary size-6" />
        </div>
        <div className="text-sm font-medium">시작과 "{agent?.name ?? "Agent"}" 대화</div>
        {agent?.description && <p className="text-muted-foreground max-w-md text-xs">{agent.description}</p>}
      </div>

      <Composer
        value={input}
        onChange={setInput}
        onSend={send}
        disabled={sending || uploading || !agentKey}
        placeholder="메시지를 입력하고, @로 기록을 인용하며, Enter로 보냅니다"
        leftSlot={agentPicker}
        onPickFiles={pickFiles}
        uploading={uploading}
      />
      <LLMProfileRow
        profiles={profiles}
        selected={llmProfileId}
        onChange={setLlmProfileId}
        disabled={sending || uploading}
      />
    </>
  );
}

// ChatView는 대화 하나의 오른쪽 화면입니다. 작업 상세의
// 메인 에이전트 콘솔과 같습니다. 에이전트와 실시간 배지, 토큰/시간,
// 맨 아래에 붙는 대화 기록(채팅 모드), 입력칸이 있습니다.
function ChatView({
  conv,
  agents,
  profiles,
  initial,
  onTitleMaybeChanged,
  onConvUpdated,
}: {
  conv: Conversation;
  agents: Agent[];
  profiles: LLMProfile[];
  // 초안이 클립으로 이 대화를 만들며 넘긴, 아직 보내지 않은 글과
  // 이미 올린 첨부입니다(마운트 때 한 번만 씁니다).
  initial?: { input?: string; attachments?: ChatAttachment[] };
  onTitleMaybeChanged: () => void;
  onConvUpdated: () => void;
}) {
  const approvalFocus = useApprovalFocus({ conversationId: conv.id });
  const [messages, setMessages] = React.useState<Activity[]>([]);
  const [running, setRunning] = React.useState(false);
  const [input, setInput] = React.useState(initial?.input ?? "");
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  // 방법 1 파일 업로드: 올린 첨부(sessions/conv-<id>/uploads/에 놓임). 다음 메시지와 함께 보냅니다.
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>(initial?.attachments ?? []);
  const [uploading, setUploading] = React.useState(false);
  const cursorRef = React.useRef(0); // 불러온 최신 id. 뒤를 이어 붙일 기준 번호
  const earliestRef = React.useRef(0); // 불러온 가장 오래된 id. 위로 넘어갈 기준 번호
  const hasMoreRef = React.useRef(false); // 불러온 창 위에 더 오래된 기록이 있습니다
  const loadingMoreRef = React.useRef(false); // 가드: 위로 스크롤 불러오기는 한 번에 하나만
  const [historyLoaded, setHistoryLoaded] = React.useState(false);
  const [hasMore, setHasMore] = React.useState(false); // "이전 기록 불러오기" 안내를 켜는 값
  const agent = agents.find((a) => a.key === conv.agent_key);
  const currentProfileId = conv.llm_profile_id ?? null;
  const side = useSideQuestions(`/api/conversations/${conv.id}`);

  async function changeProfile(id: number | null) {
    try {
      await api.updateConversationProfile(conv.id, id);
      onConvUpdated();
    } catch (e) {
      toast.error("LLM 전환 실패:" + (e as Error).message);
    }
  }

  // 재사용하는 Transcript가 이 대화의 상세를 가져오는 함수입니다.
  const fetchDetail = React.useCallback((seq: number) => api.conversationMsgDetail(conv.id, seq), [conv.id]);

  // 가장 최근 TodoWrite 도구 호출의 seq(할 일 팝오버용). 없으면 null.
  const latestTodoSeq = React.useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      const a = messages[i];
      if (a.kind === "tool_use" && a.tool === "TodoWrite") return a.seq;
    }
    return null;
  }, [messages]);

  // 고른 대화가 바뀌면 초기화하고 다시 불러옵니다. 열 때는 최신
  // 페이지만 불러옵니다. 긴 대화의 최종 답은 맨 끝에 있으므로 최신
  // 페이지에 바로 보입니다(이슈 3: 열기/새로고침이 가장 오래된
  // 페이지만 읽어, 다른 메시지가 기준 번호를 밀기 전에는 완료 결과가 없었습니다).
  // 더 오래된 기록은 위로 스크롤할 때 들어옵니다(아래 loadEarlier).
  React.useEffect(() => {
    cursorRef.current = 0;
    earliestRef.current = 0;
    hasMoreRef.current = false;
    setHasMore(false);
    setMessages([]);
    setHistoryLoaded(false);
    setRunning(false);
    let live = true;
    api
      .conversationHistory(conv.id, 0, HISTORY_PAGE)
      .then((r) => {
        if (!live) return;
        setMessages((current) => mergeActivities(r.items, current));
        cursorRef.current = Math.max(cursorRef.current, r.cursor);
        earliestRef.current = r.items.length ? r.items[0].seq : 0;
        hasMoreRef.current = r.hasMore;
        setHasMore(r.hasMore);
        setRunning((current) => current || r.running);
        setHistoryLoaded(true);
      })
      .catch(() => {
        // 출처 링크는 위치 정보로 기록을 다시 시도할 수 있습니다. 일반 채팅은
        // 잠깐 실패해도 지금 쓸 수 있는 빈 상태를 유지합니다.
        if (live) setHistoryLoaded(true);
      });
    return () => {
      live = false;
    };
  }, [conv.id]);

  // 이 대화가 열려 있는 동안 트리거나 다른 탭이 차례를 시작할 수 있습니다.
  // 잘못된 목록 스냅샷이, 마지막 메시지가 오기 전에 꼬리 읽기를 멈추면 안 됩니다.
  React.useEffect(() => {
    if (conv.running) setRunning(true);
  }, [conv.running]);

  // 차례가 도는 동안 주기 조회합니다. 기준 번호 뒤의 새 단계를 가져옵니다.
  React.useEffect(() => {
    if (!running) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      let keepPolling = true;
      try {
        const r = await api.conversationMessages(conv.id, cursorRef.current);
        if (!live) return;
        if (r.items.length) {
          setMessages((prev) => mergeActivities(prev, r.items));
        }
        cursorRef.current = Math.max(cursorRef.current, r.cursor);
        keepPolling = r.running;
        setRunning(r.running);
        if (!r.running) onTitleMaybeChanged(); // 첫 차례의 자동 제목이 반영됨
      } catch {
        /* 잠깐 오류. 주기 조회는 계속 */
      } finally {
        // 느린 응답이 같은 기준 번호로 다른 주기 조회와 겹치면 안 됩니다.
        if (live && keepPolling) timer = setTimeout(() => void tick(), 1000);
      }
    };
    void tick();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [running, conv.id, onTitleMaybeChanged]);

  const loadFocusPage = React.useCallback((before: number) => api.conversationHistory(conv.id, before, HISTORY_PAGE), [conv.id]);
  const mergeFocusPage = React.useCallback((page: { items: Activity[]; hasMore: boolean }) => {
    setMessages((prev) => mergeActivities(page.items, prev));
    earliestRef.current = page.items[0]?.seq ?? earliestRef.current;
    hasMoreRef.current = page.hasMore;
    setHasMore(page.hasMore);
  }, []);
  const focusHistory = useApprovalHistory(approvalFocus.state?.source, historyLoaded, messages, loadFocusPage, mergeFocusPage);

  // ---- 대화 기록 자동 스크롤(열면 맨 아래, 올려 보면 맨 아래에 붙이지 않음) ----
  const contentRef = React.useRef<HTMLDivElement | null>(null);
  const atBottomRef = React.useRef(true);
  const viewport = React.useCallback(
    () => (contentRef.current?.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null) ?? null,
    [],
  );
  // 위로 스크롤하면 더 오래된 페이지를 앞에 붙이고, 보이는 위치는
  // 유지합니다(붙이기 전 높이와 오프셋을 재고, 차이만큼 되돌림).
  const loadEarlier = React.useCallback(async () => {
    if (loadingMoreRef.current || !hasMoreRef.current) return;
    const vp = viewport();
    if (!vp) return;
    loadingMoreRef.current = true;
    const prevH = vp.scrollHeight;
    const prevTop = vp.scrollTop;
    try {
      const r = await api.conversationHistory(conv.id, earliestRef.current, HISTORY_PAGE);
      if (r.items.length) {
        setMessages((prev) => mergeActivities(r.items, prev));
        earliestRef.current = r.items[0].seq;
      }
      hasMoreRef.current = r.hasMore;
      setHasMore(r.hasMore);
      requestAnimationFrame(() => {
        const v = viewport();
        if (v) v.scrollTop = prevTop + (v.scrollHeight - prevH);
      });
    } catch {
      /* 잠깐 오류. 다음 스크롤이 다시 시도 */
    } finally {
      loadingMoreRef.current = false;
    }
  }, [conv.id, viewport]);
  React.useEffect(() => {
    const vp = viewport();
    if (!vp) return;
    const onScroll = () => {
      if (approvalFocus.state && !focusHistory.ready) return;
      atBottomRef.current = vp.scrollTop + vp.clientHeight >= vp.scrollHeight - 60;
      if (vp.scrollTop <= 80) void loadEarlier(); // 맨 위 근처 → 더 오래된 페이지
    };
    vp.addEventListener("scroll", onScroll, { passive: true });
    return () => vp.removeEventListener("scroll", onScroll);
  }, [viewport, loadEarlier, approvalFocus.state, focusHistory.ready]);
  // 대화를 열거나 바꾸면 최신(맨 아래)으로 점프
  // biome-ignore lint/correctness/useExhaustiveDependencies: 대화가 바뀌면 스크롤 초기화를 일부러 다시 실행합니다.
  React.useLayoutEffect(() => {
    const vp = viewport();
    if (vp) {
      vp.scrollTop = vp.scrollHeight;
      atBottomRef.current = true;
    }
  }, [conv.id, viewport]);
  // 새 활동은 사용자가 이미 맨 아래에 붙어 있을 때만 맨 아래에 붙입니다
  // biome-ignore lint/correctness/useExhaustiveDependencies: 메시지와 실행 상태가 바뀌면 맨 아래 고정을 일부러 다시 실행합니다.
  React.useLayoutEffect(() => {
    if (approvalFocus.state || !atBottomRef.current) return;
    const vp = viewport();
    if (vp) vp.scrollTop = vp.scrollHeight;
  }, [messages, running, viewport, approvalFocus.state]);

  // 대화별 토큰 합계(실시간). 메인 에이전트
  // 콘솔과 같은 계산입니다. 끝난 실행의 `result` 합 + 진행 중 실행의 최신 `usage`.
  const tokenTotal = React.useMemo(() => {
    let i = 0,
      o = 0,
      cr = 0;
    let li = 0,
      lo = 0,
      lcr = 0;
    let turns = 0; // agent 루프 회전 수 = 모델 호출 횟수(매번 kind='usage' 한 줄)
    for (const a of messages) {
      if (a.kind === "result") {
        i += a.input_tokens ?? 0;
        o += a.output_tokens ?? 0;
        cr += a.cache_read_tokens ?? 0;
        li = lo = lcr = 0;
      } else if (a.kind === "usage") {
        turns += 1;
        li = a.input_tokens ?? 0;
        lo = a.output_tokens ?? 0;
        lcr = a.cache_read_tokens ?? 0;
      }
    }
    const I = i + li,
      O = o + lo,
      CR = cr + lcr;
    return { i: I, o: O, cr: CR, turns, any: I + O + CR > 0 };
  }, [messages]);

  // pickFiles는 이 대화의 세션 폴더(sessions/conv-<id>/
  // uploads/)에 올리고, 다음 메시지와 함께 보낼 정보를 대기열에 넣습니다.
  async function pickFiles(files: File[]) {
    if (files.length === 0) return;
    setUploading(true);
    try {
      const r = await api.chatUpload("session", `conv-${conv.id}`, files);
      setAttachments((prev) => [...prev, ...r.attachments]);
    } catch (e) {
      toast.error("업로드 실패:" + (e as Error).message);
    } finally {
      setUploading(false);
    }
  }

  async function send() {
    const msg = input.trim();
    const atts = attachments;
    if (side.handleCommand(msg, () => setInput(""))) return;
    if ((!msg && atts.length === 0) || sending || running) return;
    setSending(true);
    setInput("");
    setAttachments([]);
    try {
      await api.sendConversationMessage(conv.id, msg, atts.length ? atts : undefined);
      // 실시간 루프가 저장된 사람 차례를 바로 가져옵니다. 그
      // 조회를 같이 써서, 보낸 뒤 별도 요청이 주기 조회와 경주하지 않게 합니다.
      setRunning(true);
    } catch (e) {
      toast.error("보내기 실패:" + (e as Error).message);
      setInput(msg); // 사용자가 글을 잃지 않게 되돌립니다
      setAttachments(atts); // 첨부도 되돌립니다
    } finally {
      setSending(false);
    }
  }

  // stop은 이 대화의 진행 중 실행을 중단합니다. 백엔드가 에이전트를 풀면
  // 다음 1초 주기 조회에서 running이 거짓이 됩니다. 트리거 대기열은
  // 영향받지 않습니다. 대기 중인 다음 실행은 그대로 시작됩니다.
  async function stop() {
    if (stopping) return;
    setStopping(true);
    try {
      await api.stopConversation(conv.id);
    } catch (e) {
      toast.error("중지 실패:" + (e as Error).message);
    } finally {
      setStopping(false);
    }
  }

  return (
    <SideQuestionWorkspace side={side} label={agent?.name ?? conv.agent_key} composerLayout="inline">
      {/* 머리: 어떤 에이전트인지, 실시간, 토큰 */}
      <div className="flex min-w-0 flex-wrap items-center gap-2 border-b px-4 py-2.5">
        <Bot className="text-muted-foreground size-4 shrink-0" />
        <span className="min-w-0 max-w-48 truncate text-sm font-medium">{agent?.name ?? conv.agent_key}</span>
        <span className="text-muted-foreground hidden shrink-0 font-mono text-xs sm:inline">{conv.agent_key}</span>
        {agent && !agent.builtin && (
          <Badge variant="outline" className="shrink-0 px-1.5 py-0 text-[10px]">
            사용자 지정
          </Badge>
        )}
        {agent?.description && (
          <span className="text-muted-foreground min-w-0 truncate text-xs">{agent.description}</span>
        )}
        {running && <LiveBadge />}
        <SideQuestionButton side={side} />
        <div className="text-muted-foreground ml-auto flex min-w-0 max-w-full items-center justify-end gap-x-3 gap-y-1 text-xs max-sm:w-full max-sm:flex-wrap">
          {tokenTotal.turns > 0 && (
            <span title="agent 루프 횟수(모델 호출 횟수)" className="tabular-nums">
              {tokenTotal.turns} 라운드
            </span>
          )}
          {tokenTotal.any && (
            <span title="input / cache(read) / output tokens" className="min-w-0 truncate tabular-nums">
              input {fmtTokens(tokenTotal.i)} · cache {fmtTokens(tokenTotal.cr)} · output {fmtTokens(tokenTotal.o)}
            </span>
          )}
        </div>
      </div>

      <ApprovalExecutionFocus focus={approvalFocus} history={focusHistory} />

      {/* 메시지 */}
      <ScrollArea type="auto" className="min-h-0 min-w-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!">
        <div className="min-w-0 max-w-full px-4 py-3" ref={contentRef}>
          {messages.length === 0 && !running ? (
            <div className="text-muted-foreground py-10 text-center text-sm">
              시작과 "{agent?.name ?? conv.agent_key}" 대화
            </div>
          ) : (
            <>
              {hasMore && (
                <div className="text-muted-foreground/70 pb-2 text-center text-[11px]">위로 스크롤하면 이전 메시지를 불러옵니다…</div>
              )}
              <Transcript activity={messages} live={running} chat fetchDetail={fetchDetail} focusedSeq={focusHistory.ready ? approvalFocus.state?.source?.seq : undefined} />
            </>
          )}
        </div>
      </ScrollArea>

      <Composer
        value={input}
        onChange={setInput}
        onSend={send}
        disabled={running || sending}
        allowBtw
        placeholder={running ? "Agent가 답하는 중입니다. /btw로 질문할 수 있습니다…" : "메시지를 입력하고, @로 기록을 인용하며, Enter로 보냅니다"}
        running={running}
        onStop={stop}
        stopDisabled={stopping}
        attachments={attachments}
        onPickFiles={pickFiles}
        onRemoveAttachment={(path) => setAttachments((p) => p.filter((x) => x.path !== path))}
        uploading={uploading}
      />
      <LLMProfileRow
        profiles={profiles}
        selected={currentProfileId}
        onChange={changeProfile}
        disabled={running || sending}
        rightSlot={<TodoPopover seq={latestTodoSeq} fetchDetail={fetchDetail} />}
      />
    </SideQuestionWorkspace>
  );
}

// ConversationItem은 왼쪽 목록의 한 줄입니다. 제목, 에이전트 부제, 그 자리
// 이름 바꾸기, 고정 표시, 작은 동작 메뉴가 있습니다. 에이전트 묶음 아래 줄은
// 에이전트 부제를 뺍니다(showAgent=false). 머리가 이미 말합니다.
const ConversationItem = React.memo(function ConversationItem({
  conv,
  agent,
  showAgent = true,
  active,
  renaming,
  renameText,
  onSelect,
  onStartRename,
  onRenameText,
  onCommitRename,
  onCancelRename,
  onTogglePinned,
  onDelete,
  selectionMode,
  selectedForDelete,
  onSelectedForDeleteChange,
}: {
  conv: Conversation;
  agent?: Agent;
  showAgent?: boolean;
  active: boolean;
  renaming: boolean;
  renameText: string;
  onSelect: (id: number) => void;
  onStartRename: (conversation: Conversation) => void;
  onRenameText: (v: string) => void;
  onCommitRename: (id: number, title: string) => void;
  onCancelRename: () => void;
  onTogglePinned: (conversation: Conversation) => void;
  onDelete: (id: number) => void;
  selectionMode: boolean;
  selectedForDelete: boolean;
  onSelectedForDeleteChange: (id: number, checked: boolean) => void;
}) {
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const renameInputRef = React.useRef<HTMLInputElement>(null);
  const cancelRenameRef = React.useRef(false);
  const pinned = conversationIsPinned(conv);

  React.useEffect(() => {
    if (!renaming) return;
    cancelRenameRef.current = false;
    renameInputRef.current?.focus();
    renameInputRef.current?.select();
  }, [renaming]);

  return (
    <div
      className={cn(
        "group flex min-w-0 items-center gap-1 rounded-md pr-1 transition-colors",
        active ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
      )}
    >
      {selectionMode && (
        <Checkbox
          checked={selectedForDelete}
          onCheckedChange={(checked) => onSelectedForDeleteChange(conv.id, checked === true)}
          aria-label={`대화 선택 「${conv.title || "새 대화"}」`}
          className="ml-1 shrink-0"
        />
      )}
      {renaming ? (
        <input
          ref={renameInputRef}
          value={renameText}
          onChange={(e) => onRenameText(e.target.value)}
          onBlur={() => {
            if (!cancelRenameRef.current) onCommitRename(conv.id, renameText);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") onCommitRename(conv.id, renameText);
            if (e.key === "Escape") {
              e.preventDefault();
              cancelRenameRef.current = true;
              onCancelRename();
            }
          }}
          className="border-input bg-background min-w-0 flex-1 rounded-md border px-2 py-1 text-sm"
        />
      ) : (
        <button
          type="button"
          onClick={() => onSelect(conv.id)}
          onDoubleClick={() => onStartRename(conv)}
          title="두 번 눌러 이름 바꾸기"
          className="min-w-0 flex-1 rounded-md px-2 py-1.5 text-left"
        >
          <div className="flex min-w-0 items-center gap-1.5">
            {pinned && <PinIcon className="text-primary size-3 shrink-0" aria-label="맨 위에 고정함" />}
            <div className="truncate text-sm">{conv.title || "새 대화"}</div>
            {conv.running ? (
              <Badge variant="secondary" className="shrink-0 gap-1" title="Agent 실행 중">
                <Spinner className="size-3" aria-hidden="true" />
                실행 중
              </Badge>
            ) : null}
          </div>
          <div className="text-muted-foreground flex min-w-0 items-center gap-1 text-[11px]">
            {showAgent && (
              <>
                <Bot className="size-3 shrink-0" />
                <span className="min-w-0 truncate">{agent?.name ?? conv.agent_key}</span>
                <span className="shrink-0">·</span>
              </>
            )}
            <span className="shrink-0">
              {new Date(conv.created_at).toLocaleDateString("zh-CN", {
                month: "numeric",
                day: "numeric",
                hour: "2-digit",
                minute: "2-digit",
              })}
            </span>
            <span className="shrink-0 opacity-60">#{conv.id}</span>
          </div>
        </button>
      )}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            className="text-muted-foreground shrink-0"
            aria-label={`대화 관리 「${conv.title || "새 대화"}」`}
          >
            <MoreHorizontalIcon />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuGroup>
            <DropdownMenuItem onSelect={() => onStartRename(conv)}>
              <PencilIcon />
              이름 바꾸기
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onTogglePinned(conv)}>
              {pinned ? <PinOffIcon /> : <PinIcon />}
              {pinned ? "맨 위 고정 해제" : "상단 고정"}
            </DropdownMenuItem>
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuGroup>
            <DropdownMenuItem variant="destructive" onSelect={() => setDeleteOpen(true)}>
              <Trash2Icon />
              삭제
            </DropdownMenuItem>
          </DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>대화 삭제 "{conv.title || "새 대화"}」？</AlertDialogTitle>
            <AlertDialogDescription>이 작업은 되돌릴 수 없습니다.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>취소</AlertDialogCancel>
            <AlertDialogAction onClick={() => onDelete(conv.id)}>삭제</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
});

// AgentGroupHeader는 한 에이전트 줄 위에 붙는, 누를 수 있는 구분선입니다.
// 접기 화살표, 에이전트 이름, 실행 중 표시, 줄 개수가 있습니다.
function AgentGroupHeader({
  group,
  collapsed,
  hasActive,
  onToggle,
}: {
  group: AgentGroup;
  collapsed: boolean;
  hasActive: boolean;
  onToggle: (key: string) => void;
}) {
  return (
    <button
      type="button"
      onClick={() => onToggle(group.key)}
      aria-expanded={!collapsed}
      title={collapsed ? `펼치기 「${group.name}」` : `접기 「${group.name}」`}
      className={cn(
        "sticky top-0 z-10 flex min-w-0 items-center gap-1.5 rounded-md bg-card px-1.5 py-1 text-left font-medium text-[11px] transition-colors hover:bg-accent/50",
        collapsed && hasActive ? "text-foreground" : "text-muted-foreground",
      )}
    >
      <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", !collapsed && "rotate-90")} />
      <Bot className="size-3 shrink-0" />
      <span className="min-w-0 flex-1 truncate">{group.name}</span>
      {collapsed && hasActive && (
        <span className="size-1.5 shrink-0 rounded-full bg-primary" title="현재 대화가 이 그룹 안에 있음" />
      )}
      {group.runningCount > 0 && (
        <Spinner className="size-3 shrink-0" aria-label={`${group.runningCount} 개 대화 실행 중`} />
      )}
      <span className="shrink-0 tabular-nums opacity-60">{group.conversations.length}</span>
    </button>
  );
}

export default function ChatPage() {
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [convs, setConvs] = React.useState<Conversation[]>([]);
  const [agentFilter, setAgentFilter] = React.useState<string | null>(null);
  const [selectedId, setSelectedId] = React.useState<number | null>(null);
  const [sourceRequested, setSourceRequested] = React.useState(false);
  const [convsLoaded, setConvsLoaded] = React.useState(false);
  const selectConversation = React.useCallback((id: number | null) => {
    if (id !== selectedId) {
      const url = new URL(window.location.href);
      url.searchParams.delete("approval");
      setSourceRequested(false);
      window.history.replaceState(null, "", url);
    }
    setSelectedId(id);
  }, [selectedId]);

  const [renamingId, setRenamingId] = React.useState<number | null>(null);
  const [renameText, setRenameText] = React.useState("");
  const [selectedConversationIds, setSelectedConversationIds] = React.useState<Set<number>>(() => new Set());
  // selectionMode는 여러 선택 화면을 여닫습니다. 기본은 꺼짐(깨끗한 목록,
  // 체크 상자). 머리의 "여러 선택" 버튼이 켜고, "완료"가 끄며
  // 선택을 비웁니다.
  const [selectionMode, setSelectionMode] = React.useState(false);
  const [bulkDeleteOpen, setBulkDeleteOpen] = React.useState(false);
  const [bulkDeleting, setBulkDeleting] = React.useState(false);
  const [visibleConversationCount, setVisibleConversationCount] = React.useState(CONVERSATION_LIST_PAGE);
  // 접힌 에이전트 묶음. 마운트 뒤에 localStorage에서 채웁니다(지연
  // useState 초기화가 아님). 서버와 브라우저의 첫 화면이 같게 합니다.
  const [collapsedAgents, setCollapsedAgents] = React.useState<Set<string>>(() => new Set());
  // 초안이 클립으로 대화를 만들며 넘긴, 아직 보내지 않은 글과 올린 첨부입니다.
  // 새 대화 id가 키입니다(ChatView가 마운트 때 한 번 씁니다. id는 반복되지 않아
  // 남은 항목은 해롭지 않습니다).
  const [pendingByConv, setPendingByConv] = React.useState<
    Record<number, { input?: string; attachments?: ChatAttachment[] }>
  >({});

  const conversationListSeq = React.useRef(0);
  const reloadConvs = React.useCallback(async () => {
    const seq = ++conversationListSeq.current;
    try {
      const items = await api.conversations();
      if (seq === conversationListSeq.current) { setConvs(items); setConvsLoaded(true); }
    } catch {
      // 주기 조회가 잠깐 실패해도 고른 대화 기록과 목록을 유지합니다.
    }
  }, []);
  React.useEffect(() => {
    api
      .agents()
      .then(setAgents)
      .catch(() => {
        /* 이미 있는 대화를 그리는 데 에이전트 정보는 없어도 됩니다. */
      });
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => {
        /* 프로필 이름이 없어도 대화는 쓸 수 있습니다. */
      });
  }, []);

  // 목록 주기 조회 한 번이 모든 사이드바 줄의 실행 상태를 줍니다. 선택하지 않은
  // 줄도 포함합니다. 끝날 때까지 기다려 느린 요청이 겹치지 않게 합니다.
  React.useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      await reloadConvs();
      if (!disposed) timer = setTimeout(() => void poll(), 2000);
    }
    void poll();
    return () => {
      disposed = true;
      conversationListSeq.current++;
      clearTimeout(timer);
    };
  }, [reloadConvs]);

  React.useEffect(() => {
    setSelectedConversationIds((current) => {
      if (current.size === 0) return current;
      const live = new Set(convs.map((conversation) => conversation.id));
      const next = new Set([...current].filter((id) => live.has(id)));
      return next.size === current.size ? current : next;
    });
  }, [convs]);

  React.useEffect(() => {
    const raw = getLocalStorageValue(COLLAPSED_AGENTS_KEY);
    if (!raw) return;
    try {
      const keys = JSON.parse(raw);
      if (Array.isArray(keys))
        setCollapsedAgents(new Set(keys.filter((key): key is string => typeof key === "string")));
    } catch {
      // 깨진 저장값이면 모두 펼친 채로 시작합니다.
    }
  }, []);

  const toggleAgentCollapsed = React.useCallback((key: string) => {
    setCollapsedAgents((current) => {
      const next = new Set(current);
      if (!next.delete(key)) next.add(key);
      setLocalStorageValue(COLLAPSED_AGENTS_KEY, JSON.stringify([...next]));
      return next;
    });
  }, []);

  // 마운트 때 주소의 열린 대화(?c=<id>)를 복원해서, 새로고침해도
  // 빈 초안이 아니라 같은 대화로 돌아옵니다. 수화 뒤에 실행합니다(지연
  // useState 초기화가 아님). 서버와 브라우저가 어긋나지 않게 합니다.
  React.useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    setSourceRequested(params.has("approval"));
    const c = params.get("c");
    const id = c ? Number(c) : NaN;
    if (Number.isFinite(id)) setSelectedId(id);
  }, []);
  // 지금 선택을 주소에 반영합니다(replaceState라 방문 기록이 늘지 않음).
  // 대화가 없는 selectedId(오래된 ?c= 또는 방금 만든
  // 대화가 reloadConvs 전에 있는 경우)는 초안 화면이 될 뿐이라 해롭지 않습니다. 그래서
  // 여기서 자동으로 지우지 않습니다(새 대화 만들기와 경주했습니다).
  React.useEffect(() => {
    const url = new URL(window.location.href);
    if (selectedId != null) url.searchParams.set("c", String(selectedId));
    else url.searchParams.delete("c");
    window.history.replaceState(null, "", url);
  }, [selectedId]);

  const selected = React.useMemo(() => convs.find((c) => c.id === selectedId) ?? null, [convs, selectedId]);
  const agentByKey = React.useMemo(() => new Map(agents.map((agent) => [agent.key, agent])), [agents]);
  const filteredConversations = React.useMemo(
    () => (agentFilter === null ? convs : convs.filter((conversation) => conversation.agent_key === agentFilter)),
    [convs, agentFilter],
  );
  const visibleConversations = React.useMemo(
    () => filteredConversations.slice(0, visibleConversationCount),
    [filteredConversations, visibleConversationCount],
  );
  // 고정된 줄은 묶음 위에 평평하게 둡니다(서버 순서 = pinned_at 내림차순).
  // 나머지는 에이전트별로 나누고, 가장 최근에 움직인 에이전트가 먼저입니다.
  const pinnedConversations = React.useMemo(
    () => visibleConversations.filter(conversationIsPinned),
    [visibleConversations],
  );
  const agentGroups = React.useMemo(
    () =>
      groupByAgent(
        visibleConversations.filter((c) => !conversationIsPinned(c)),
        agentByKey,
      ),
    [visibleConversations, agentByKey],
  );
  // 대화에 쓰는 에이전트: 사용자 에이전트와 대화형 내장(role=assistant,
  // 예: Auto / 침투 테스트). 오케스트레이션 내장(goals/planner/mainagent/worker)
  // 작업 전용이라 채팅 화면에는 숨깁니다.
  const chatAgents = React.useMemo(() => agents.filter((a) => !a.builtin || a.role === "assistant"), [agents]);
  const agentFilterOptions = React.useMemo(() => {
    const counts = new Map<string, number>();
    for (const conversation of convs) {
      counts.set(conversation.agent_key, (counts.get(conversation.agent_key) ?? 0) + 1);
    }
    // 그 에이전트가 나중에 꺼지거나 지워져도, 과거 출처는 포함합니다.
    const keys = new Set([...chatAgents.map((agent) => agent.key), ...counts.keys()]);
    if (agentFilter !== null) keys.add(agentFilter);
    return [...keys]
      .map((key) => ({ key, name: agentByKey.get(key)?.name || key, count: counts.get(key) ?? 0 }))
      .sort((a, b) => a.name.localeCompare(b.name, "zh-CN"));
  }, [convs, chatAgents, agentByKey, agentFilter]);
  const conversationCountLabel =
    agentFilter === null ? `총 ${convs.length} 개` : `${filteredConversations.length} / ${convs.length} 개`;

  function changeAgentFilter(key: string | null) {
    setAgentFilter(key);
    setVisibleConversationCount(CONVERSATION_LIST_PAGE);
    setSelectedConversationIds(new Set());
    setBulkDeleteOpen(false);
    setRenamingId(null);
  }

  const selectedConversationCount = selectedConversationIds.size;
  const allConversationsSelected =
    filteredConversations.length > 0 && selectedConversationCount === filteredConversations.length;
  const someConversationsSelected = selectedConversationCount > 0 && !allConversationsSelected;
  let conversationHeaderChecked: boolean | "indeterminate" = false;
  if (allConversationsSelected) conversationHeaderChecked = true;
  else if (someConversationsSelected) conversationHeaderChecked = "indeterminate";

  const toggleConversationSelected = React.useCallback((id: number, checked: boolean) => {
    setSelectedConversationIds((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  function toggleAllConversations(checked: boolean) {
    setSelectedConversationIds(
      checked ? new Set(filteredConversations.map((conversation) => conversation.id)) : new Set(),
    );
  }

  function exitSelectionMode() {
    setSelectionMode(false);
    setSelectedConversationIds(new Set());
  }

  const del = React.useCallback(
    async (id: number) => {
      try {
        await api.deleteConversation(id);
        setSelectedId((current) => (current === id ? null : current));
        setSelectedConversationIds((current) => {
          if (!current.has(id)) return current;
          const next = new Set(current);
          next.delete(id);
          return next;
        });
        void reloadConvs();
      } catch (e) {
        toast.error("삭제 실패:" + (e as Error).message);
      }
    },
    [reloadConvs],
  );

  async function deleteSelectedConversations() {
    const ids = [...selectedConversationIds];
    if (ids.length === 0 || bulkDeleting) return;
    setBulkDeleting(true);
    const deleted = new Set<number>();
    const failed: { id: number; error: string }[] = [];
    try {
      for (let offset = 0; offset < ids.length; offset += 100) {
        const result = await api.deleteConversations(ids.slice(offset, offset + 100));
        for (const item of result.items) {
          if (item.ok) deleted.add(item.id);
          else failed.push({ id: item.id, error: item.error ?? "대화가 없습니다" });
        }
      }
      if (deleted.has(selectedId ?? -1)) selectConversation(null);
      setSelectedConversationIds((current) => {
        const next = new Set(current);
        for (const id of deleted) next.delete(id);
        return next;
      });
      if (deleted.size > 0) toast.success(`삭제됨 ${deleted.size} 개 대화`);
      if (failed.length > 0) {
        const details = failed
          .slice(0, 3)
          .map((item) => `#${item.id}（${item.error}）`)
          .join("；");
        toast.error(`${failed.length} 개 대화 삭제 실패:${details}${failed.length > 3 ? " 등" : ""}`);
      }
      setBulkDeleteOpen(false);
      // 전부 성공하면 깨끗한 목록으로 돌아갑니다. 일부가 실패하면 선택 모드를 유지해
      // 남은 것을 다시 시도할 수 있게 합니다.
      if (failed.length === 0) setSelectionMode(false);
      void reloadConvs();
    } catch (error) {
      toast.error(`일괄 삭제 실패:${(error as Error).message}`);
      void reloadConvs();
    } finally {
      setBulkDeleting(false);
    }
  }

  const togglePinned = React.useCallback(
    async (conversation: Conversation) => {
      const pinned = conversationIsPinned(conversation);
      try {
        await api.pinConversation(conversation.id, !pinned);
        void reloadConvs();
      } catch (e) {
        toast.error(`${pinned ? "맨 위 고정 해제" : "상단 고정"}실패:${(e as Error).message}`);
      }
    },
    [reloadConvs],
  );

  const startRename = React.useCallback((c: Conversation) => {
    setRenamingId(c.id);
    setRenameText(c.title || "");
  }, []);
  const commitRename = React.useCallback(
    async (id: number, value: string) => {
      const title = value.trim();
      setRenamingId(null);
      if (!title) return;
      try {
        await api.renameConversation(id, title);
        void reloadConvs();
      } catch (e) {
        toast.error("이름 바꾸기 실패: " + (e as Error).message);
      }
    },
    [reloadConvs],
  );
  const cancelRename = React.useCallback(() => setRenamingId(null), []);

  return (
    <div
      data-content-padding="false"
      className="flex h-[calc(100svh-3rem)] min-w-0 flex-col overflow-hidden p-3 sm:p-4 md:h-[calc(100svh-4rem)] md:p-6"
    >
      <div className="grid min-h-0 min-w-0 flex-1 grid-cols-1 grid-rows-[minmax(10rem,15rem)_minmax(0,1fr)] gap-3 md:grid-cols-[18rem_minmax(0,1fr)] md:grid-rows-[minmax(0,1fr)] md:gap-4">
        {/* 왼쪽: 대화 목록 */}
        <div className="bg-card flex flex-col overflow-hidden rounded-lg border">
          <div className="flex flex-col gap-2 border-b p-2">
            <Button size="sm" className="w-full" onClick={() => selectConversation(null)}>
              <PlusIcon /> 대화 만들기
            </Button>
            <Select
              value={agentFilter === null ? "all" : `agent:${agentFilter}`}
              onValueChange={(value) => changeAgentFilter(value === "all" ? null : value.slice(6))}
              disabled={bulkDeleting}
            >
              <SelectTrigger size="sm" className="w-full min-w-0" aria-label="에이전트별 대화 필터">
                <Bot />
                <SelectValue placeholder="모든 Agent" />
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  <SelectItem value="all">모든 Agent</SelectItem>
                  {agentFilterOptions.map((agent) => (
                    <SelectItem key={agent.key} value={`agent:${agent.key}`}>
                      {agent.name}（{agent.count}）
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
            {convs.length > 0 &&
              (selectionMode ? (
                <div className="flex items-center gap-2 px-1">
                  <Checkbox
                    checked={conversationHeaderChecked}
                    onCheckedChange={(checked) => toggleAllConversations(checked === true)}
                    aria-label="현재 필터의 모든 대화 선택"
                    disabled={filteredConversations.length === 0 || bulkDeleting}
                  />
                  <span className="text-muted-foreground min-w-0 flex-1 text-xs tabular-nums">
                    {selectedConversationCount > 0 ? `선택됨 ${selectedConversationCount} 개` : conversationCountLabel}
                  </span>
                  {selectedConversationCount > 0 && (
                    <Button
                      size="sm"
                      variant="destructive"
                      disabled={bulkDeleting}
                      onClick={() => setBulkDeleteOpen(true)}
                    >
                      <Trash2Icon data-icon="inline-start" />
                      삭제
                    </Button>
                  )}
                  <Button size="sm" variant="ghost" onClick={exitSelectionMode}>
                    완료
                  </Button>
                </div>
              ) : (
                <div className="flex items-center gap-2 px-1">
                  <span className="text-muted-foreground min-w-0 flex-1 text-xs tabular-nums">
                    {conversationCountLabel}
                  </span>
                  <Button
                    size="sm"
                    variant="ghost"
                    className="text-muted-foreground"
                    onClick={() => setSelectionMode(true)}
                    disabled={filteredConversations.length === 0}
                  >
                    <ListChecksIcon data-icon="inline-start" />
                    여러 개 선택
                  </Button>
                </div>
              ))}
          </div>
          <ScrollArea
            key={agentFilter === null ? "all" : `agent:${agentFilter}`}
            type="auto"
            className="min-h-0 min-w-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!"
          >
            <div className="flex min-w-0 flex-col gap-0.5 p-2">
              {filteredConversations.length === 0 && (
                <p className="text-muted-foreground px-2 py-6 text-center text-xs">
                  {agentFilter === null ? "대화가 아직 없습니다" : "이 에이전트에는 아직 대화가 없습니다"}
                </p>
              )}
              {pinnedConversations.map((c) => (
                <ConversationItem
                  key={c.id}
                  conv={c}
                  agent={agentByKey.get(c.agent_key)}
                  active={selectedId === c.id}
                  renaming={renamingId === c.id}
                  renameText={renamingId === c.id ? renameText : ""}
                  onSelect={selectConversation}
                  onStartRename={startRename}
                  onRenameText={setRenameText}
                  onCommitRename={commitRename}
                  onCancelRename={cancelRename}
                  onTogglePinned={togglePinned}
                  onDelete={del}
                  selectionMode={selectionMode}
                  selectedForDelete={selectedConversationIds.has(c.id)}
                  onSelectedForDeleteChange={toggleConversationSelected}
                />
              ))}
              {agentGroups.map((group) => {
                const collapsed = collapsedAgents.has(group.key);
                return (
                  <React.Fragment key={group.key}>
                    <AgentGroupHeader
                      group={group}
                      collapsed={collapsed}
                      hasActive={group.conversations.some((c) => c.id === selectedId)}
                      onToggle={toggleAgentCollapsed}
                    />
                    {!collapsed &&
                      group.conversations.map((c) => (
                        <ConversationItem
                          key={c.id}
                          conv={c}
                          agent={agentByKey.get(c.agent_key)}
                          showAgent={false}
                          active={selectedId === c.id}
                          renaming={renamingId === c.id}
                          renameText={renamingId === c.id ? renameText : ""}
                          onSelect={selectConversation}
                          onStartRename={startRename}
                          onRenameText={setRenameText}
                          onCommitRename={commitRename}
                          onCancelRename={cancelRename}
                          onTogglePinned={togglePinned}
                          onDelete={del}
                          selectionMode={selectionMode}
                          selectedForDelete={selectedConversationIds.has(c.id)}
                          onSelectedForDeleteChange={toggleConversationSelected}
                        />
                      ))}
                  </React.Fragment>
                );
              })}
              {visibleConversationCount < filteredConversations.length && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="mt-1 w-full"
                  onClick={() => setVisibleConversationCount((count) => count + CONVERSATION_LIST_PAGE)}
                >
                  더 불러오기
                </Button>
              )}
            </div>
          </ScrollArea>
        </div>

        {/* 오른쪽: 채팅 */}
        <div className="bg-card flex min-w-0 flex-col overflow-hidden rounded-lg border">
          {selected ? (
            <ChatView
              key={selected.id}
              conv={selected}
              agents={agents}
              profiles={profiles}
              initial={pendingByConv[selected.id]}
              onTitleMaybeChanged={reloadConvs}
              onConvUpdated={reloadConvs}
            />
          ) : sourceRequested ? (
            <div role="status" className="p-6 text-sm text-muted-foreground">
              {convsLoaded ? "대화가 삭제되었습니다" : "해당 대화를 불러오는 중…"}
            </div>
          ) : (
            <DraftChat
              agents={chatAgents}
              profiles={profiles}
              onStarted={(c, pending) => {
                if (agentFilter !== null && agentFilter !== c.agent_key) changeAgentFilter(null);
                // 새 대화를 바로 넣어, 이 렌더에서 `selected`가 그것을
                // 가리키게 합니다(reloadConvs가 끝나기 전에 ChatView로 전환).
                // 그다음 reloadConvs가 제목 등을 맞춥니다.
                if (pending) setPendingByConv((p) => ({ ...p, [c.id]: pending }));
                setConvs((prev) => {
                  if (prev.some((item) => item.id === c.id)) return prev;
                  const firstUnpinned = prev.findIndex((item) => !conversationIsPinned(item));
                  const insertAt = firstUnpinned < 0 ? prev.length : firstUnpinned;
                  return [...prev.slice(0, insertAt), c, ...prev.slice(insertAt)];
                });
                setSelectedId(c.id);
                void reloadConvs();
              }}
            />
          )}
        </div>
      </div>
      <AlertDialog open={bulkDeleteOpen} onOpenChange={setBulkDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>선택한 항목 삭제 {selectedConversationCount} 개 대화?</AlertDialogTitle>
            <AlertDialogDescription>대화 메시지와 실행 기록이 함께 삭제됩니다. 이 작업은 되돌릴 수 없습니다.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={bulkDeleting}>취소</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={bulkDeleting || selectedConversationCount === 0}
              onClick={(event) => {
                event.preventDefault();
                void deleteSelectedConversations();
              }}
            >
              {bulkDeleting && <Loader2Icon data-icon="inline-start" className="animate-spin" />}
              {bulkDeleting ? "삭제 중" : "삭제 확인"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
