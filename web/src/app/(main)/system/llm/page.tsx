"use client";

import * as React from "react";

import {
  Loader2Icon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SaveIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { LLMPoolMember, LLMPoolStatus, LLMProfile, LLMRetryOverride } from "@/lib/types";
import { cn } from "@/lib/utils";

import { ProfileRetryFields, RetryPolicyPanel, ZERO_OVERRIDE } from "./_components/retry";

// 생각 스위치(thinking.type)와 생각 강도(reasoning_effort)는 【서로 독립】인 두 필드입니다.
// 각자 따로 설정. 어떤 인터페이스는 thinking 필드가 없고 강도 매개변수만으로 생각을 켜므로, 떼어 놓아야 합니다.
// 저장소의 빈 문자열 = 그 필드를 【보내지 않음】. Radix Select는 빈 value를 받지 않으므로 UI는 "none"을 씀
// 보초 값은 보내지 않음을 뜻합니다. 저장하고 읽을 때 ""와 서로 바꿉니다(NONE / fromStore / toStore).
const NONE = "none";
const fromStore = (v?: string) => (v ? v : NONE);
const toStore = (v: string) => (v === NONE ? "" : v);
const THINKING_TYPES: { value: string; label: string }[] = [
  { value: NONE, label: "보내지 않음(기본)" },
  { value: "disabled", label: "꺼짐" },
  { value: "enabled", label: "켜짐" },
];
// 출력 상한에 어떤 요청 필드 이름을 쓸지(openai 형식에만 의미 있음). NONE ↔ ""는 같은 보초 변환을 탑니다.
const MAX_TOKENS_FIELDS: { value: string; label: string }[] = [
  { value: NONE, label: "max_tokens(기본)" },
  { value: "max_completion_tokens", label: "max_completion_tokens" },
];
// 다른 두 형식은 필드 이름이 각자 고정이라 옵션이 의미 없습니다. 안내 문구에서 바로 분명히 말합니다.
const MAX_TOKENS_FIELD_HINTS: Record<string, string> = {
  openai:
    "상한을 어느 키로 보낼지. max_tokens가 기본이며, 대부분의 호환 게이트웨이는 이것만 알아봅니다. OpenAI 공식 추론 모델(o 시리즈 / GPT-5)은 반대로 max_completion_tokens만 알아보며, max_tokens를 받으면 unsupported_parameter를 바로 보고합니다.",
  anthropic: "openai 형식만 고를 수 있습니다. Anthropic의 필드 이름은 max_tokens로 고정입니다.",
  "openai-responses": "openai 형식만 고를 수 있습니다. Responses API의 필드 이름은 max_output_tokens로 고정입니다.",
};
const EFFORT_LEVELS: { value: string; label: string }[] = [
  { value: NONE, label: "보내지 않음(기본)" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return `${secs}s`;
  return `${Math.ceil(secs / 60)}min`;
}

// 설정 하나가 카드에 보이는 「정상인지」. Key를 안 넣은 설정은 애초에 요청을 보낼 수 없어, 차단보다 먼저 말해야 하고,
// 나머지 상태는 주기 조회의 차단 기록에서 옵니다(주기 조회가 꺼져 있으면 새 기록이 생기지 않음. 이때 「정상」= 알려진 고장이 없음).
type Health = { label: string; cls: string; hint?: string };
function healthOf(p: LLMProfile, m?: LLMPoolMember): Health {
  if (!p.api_key_hint) {
    return {
      label: "키가 설정되지 않음",
      cls: "border-muted-foreground/40 text-muted-foreground",
      hint: "API 키를 입력하지 않아 호출할 수 없습니다",
    };
  }
  if (m?.state === "tripped") {
    return {
      label: m.cooldown_secs > 0 ? `서킷 브레이크됨 · ${cooldownText(m.cooldown_secs)}` : "회로를 끊음",
      cls: "border-destructive/50 text-destructive",
      hint: m.last_error,
    };
  }
  if (m?.state === "degraded") {
    return {
      label: `이상 · 실패 ${m.fails} 회`,
      cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      hint: m.last_error,
    };
  }
  return { label: "정상", cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };
}

// ─────────────────────────────────────────────────────────────────────────────
// 주기 조회 설정 서랍
// ─────────────────────────────────────────────────────────────────────────────

function PoolSheet({
  open,
  onOpenChange,
  pool,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
}) {
  const [busy, setBusy] = React.useState(false);

  // 냉각 카운트다운은 백엔드가 계산한 남은 초입니다. 서랍이 열려 있고 정상이 아닌 설정이 있을 때만 주기적으로 가져와, 흐르게 합니다.
  React.useEffect(() => {
    if (!open || !pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    const t = setInterval(() => void onReload(), 10_000);
    return () => clearInterval(t);
  }, [open, pool, onReload]);

  async function toggle(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? "LLM 순회를 켰습니다" : "LLM 순회를 껐습니다");
      } else {
        toast.success("예비 설정을 업데이트했습니다");
      }
    } catch (e) {
      toast.error(`설정 실패:${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? "이 설정을 복원했습니다" : "모든 설정을 복원했습니다");
    } catch (e) {
      toast.error(`복구 실패:${(e as Error).message}`);
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  // 주기 조회에 참여하는 구성원(「주기 조회에 참여하지 않음」으로 표시된 것은 제외). 순서가 백엔드의 실제 시도 순서입니다.
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex flex-col gap-0 p-0 data-[side=right]:sm:max-w-lg">
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <ZapIcon className="size-4" /> LLM 순환 · 장애 조치
          </SheetTitle>
          <SheetDescription>
            켜면,<b>모델을 지정하지 않음</b>의 에이전트가 현재 설정에서 쓸 수 없으면(잔액 부족 / 키 무효 / 속도 제한 /
            서비스 오류) 다음 설정으로 자동 전환합니다.
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-6">
          <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="grid gap-0.5">
              <Label className="text-sm">순환 사용</Label>
              <p className="text-muted-foreground text-xs">기본은 꺼짐. 꺼져 있으면 항상 활성 설정만 쓰며, 실패하면 그대로 실패입니다.</p>
            </div>
            <Switch
              checked={enabled}
              disabled={busy}
              onCheckedChange={(v) => void toggle({ llm_pool_enabled: v })}
              aria-label="LLM 순회 스위치"
            />
          </div>

          {enabled && (
            <>
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">지정한 모델이 실패할 때도 폴백</Label>
                  <p className="text-muted-foreground text-xs">
                    기본값은 꺼짐: 에이전트나 작업이 설정을 지정하면 그것만 씁니다. 실패하면 그대로 실패합니다(다른 모델로 조용히 바꾸지 않습니다). 켜면 지정한 설정이 실패할 때도 아래 순환 목록으로 넘어갑니다.
                  </p>
                </div>
                <Switch
                  checked={pool?.bind_fallback ?? false}
                  disabled={busy}
                  onCheckedChange={(v) => void toggle({ llm_pool_bind_fallback: v })}
                  aria-label="바인딩 설정 실패 시 폴백 스위치"
                />
              </div>

              <Separator />

              <div className="grid gap-2">
                <div className="flex items-center justify-between">
                  <Label className="text-sm">순회 순서</Label>
                  {tripped.length > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => void recover()}>
                      <RotateCcwIcon /> 모두 복원
                    </Button>
                  )}
                </div>
                {inChain.length < 2 && (
                  <p className="text-muted-foreground text-xs">
                    사용 가능한 설정은 현재 {inChain.length} 개뿐입니다. 순환이 적용되지 않습니다. API 키를 입력했고 순환에 참여하는 설정이 최소 2개 필요합니다.
                  </p>
                )}
                {chain.map((m) => {
                  const excluded = m.excluded && !m.active;
                  const order = excluded ? null : inChain.findIndex((x) => x.profile_id === m.profile_id) + 1;
                  return (
                    <div
                      key={m.profile_id}
                      className={cn(
                        "grid gap-1 rounded-lg border p-2.5 text-sm",
                        excluded && "opacity-55",
                        m.state === "tripped" && "border-destructive/40",
                      )}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="w-5 shrink-0 text-center font-mono text-muted-foreground text-xs">
                          {order ?? "—"}
                        </span>
                        <span className="font-medium">{m.name}</span>
                        {m.active && (
                          <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                            활성
                          </Badge>
                        )}
                        {excluded && <Badge variant="outline">순환에 참여하지 않음</Badge>}
                        <div className="ml-auto flex items-center gap-2">
                          {m.state === "tripped" && m.cooldown_secs > 0 && (
                            <span className="text-muted-foreground text-xs">쿨다운 {cooldownText(m.cooldown_secs)}</span>
                          )}
                          {m.state === "degraded" && (
                            <span className="text-muted-foreground text-xs">연속 실패 {m.fails} 회</span>
                          )}
                          {m.state !== "ok" && (
                            <Button
                              size="icon"
                              variant="ghost"
                              className="size-7"
                              aria-label="즉시 복구"
                              title="즉시 복구: 서킷 브레이크를 지우고, 다음 호출에서 이 설정을 다시 시도"
                              onClick={() => void recover(m.profile_id)}
                            >
                              <RotateCcwIcon className="size-3.5" />
                            </Button>
                          )}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 pl-7 text-muted-foreground text-xs">
                        <code className="truncate font-mono">{m.model}</code>
                        {!m.active && <span>우선순위 {m.priority}</span>}
                      </div>
                      {m.last_error && (
                        <p className="truncate pl-7 font-mono text-muted-foreground text-xs" title={m.last_error}>
                          {m.last_error}
                        </p>
                      )}
                    </div>
                  );
                })}
                {chain.length === 0 && (
                  <div className="rounded-lg border border-dashed p-4 text-center text-muted-foreground text-sm">
                    설정 없음
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs leading-relaxed">
                활성 설정은 항상 1순위입니다. 나머지는 우선순위가 높은 순입니다(각 설정에서 지정). 설정이 실패하면 쿨다운에 들어갑니다(60초 → 5분 → 30분). 쿨다운 동안은 건너뛰고, 회복되면 자동으로 돌아옵니다. 컨텍스트 창에 현재 요청이 들어가지 않는 설정은 건너뜁니다. 모델을 지정한 에이전트와 작업은 기본적으로 순환에 참여하지 않습니다.
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// 모델 설정 서랍(만들기 / 편집이 같은 양식을 씀)
// ─────────────────────────────────────────────────────────────────────────────

function ProfileSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null; // null = 새로 만들기
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (id: string) => void;
}) {
  const isNew = !profile;
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<"anthropic" | "openai" | "openai-responses">("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0"); // 맥락 창(K tokens). 0=기본 200K
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0"); // 주기 조회 순번. 클수록 먼저
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true); // true=스트리밍(기본). false=비스트리밍
  const [maxTokens, setMaxTokens] = React.useState("0"); // 답 한 번의 출력 상한. 0=보내지 않음
  const [maxTokensField, setMaxTokensField] = React.useState(NONE); // 상한에 어떤 필드 이름을 쓰는지. NONE=max_tokens
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState(""); // 사용자 지정 세션 헤더 이름. 비어 있음=보내지 않음
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE); // 이 설정의 재시도 덮어쓰기. 모두 0=전역을 따라감
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<string[]>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);

  // 열 때마다 넘긴 profile로 양식을 한 번 채웁니다(새로 만들면 기본값으로 초기화). 서랍을 닫았다가 다시 열면
  // 깨끗한 시작 한 번이 되어, 이전 설정의 잔상이 남지 않습니다.
  React.useEffect(() => {
    if (!open) return;
    setName(profile?.name ?? "");
    setFormat(profile?.format === "openai" || profile?.format === "openai-responses" ? profile.format : "anthropic");
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(String(profile?.context_window_k ?? 0));
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelsOpen(false);
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : undefined;

  async function loadModels() {
    if (loadingModels) return;
    setLoadingModels(true);
    setModels([]);
    try {
      const r = await api.fetchLLMModels(format, baseUrl, apiKey, proxy, profileId);
      if (r.ok && r.models && r.models.length > 0) {
        setModels(r.models);
        setModelsOpen(true);
        toast.success(`불러옴 ${r.models.length} 개 모델`);
      } else {
        toast.error(`모델 불러오기 실패:${r.error ?? "모델을 가져오지 못했습니다"}`);
      }
    } catch (e) {
      toast.error(`모델 불러오기 오류:${(e as Error).message}`);
    } finally {
      setLoadingModels(false);
    }
  }

  async function testConnection() {
    if (testing) return;
    setTesting(true);
    try {
      // 설정이 실제로 쓸 생각 매개변수로 테스트합니다. 그 필드를 지원하지 않는 모델은 여기서 실패하고,
      // 작업을 돌릴 때까지 기다렸다가 터지지 않게. profile id를 넘김: Key 입력 칸이 비면 이미 저장된 Key를 씀.
      const r = await api.testLLM(
        format,
        model,
        baseUrl,
        apiKey,
        proxy,
        toStore(thinkingType),
        toStore(effort),
        profileId,
        streaming,
        sessionHeaderKey.trim(),
      );
      // 답 내용도 함께 보여 줍니다. 모델이 정말 말했는지 보여야, 세션에서 통한 것과 같은 일입니다.
      if (r.ok)
        toast.success(`연결 성공 · ${r.latency_ms ?? "?"}ms · ${r.model ?? model}`, {
          description: r.reply ? `답장:${r.reply}` : undefined,
        });
      else toast.error(`연결 실패:${r.error ?? "알 수 없음"}`);
    } catch (e) {
      toast.error(`테스트 오류:${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!name.trim() || !model.trim()) {
      toast.error("이름과 모델을 입력하세요");
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      const { id } = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        name: name.trim(),
        format,
        model: model.trim(),
        base_url: baseUrl.trim(),
        proxy: proxy.trim(),
        api_key: apiKey,
        rate_per_second: Number(rps) || 0,
        rate_per_minute: Number(rpm) || 0,
        context_window_k: Number(cw) || 0,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        streaming,
        max_tokens: Math.max(0, Number(maxTokens) || 0),
        // 필드 이름 스위치는 openai(Chat Completions)에만 의미가 있습니다. 다른 형식은 모두 기본으로 되돌리고,
        // 백엔드도 같은 정규화를 한 번 더 합니다. 여기서는 UI가 서로 모순된 값을 보내지 않게만 합니다.
        max_tokens_field: format === "openai" ? toStore(maxTokensField) : "",
        session_header_key: sessionHeaderKey.trim(),
        retry,
      });
      if (isNew) toast.success(`새로 만듦:${name.trim()}(카드에서 「활성화로 설정」을 눌러 사용)`);
      else toast.success(profile?.is_default ? "저장했습니다. 활성 설정은 바로 적용되며, 다시 시작할 필요가 없습니다" : "저장했습니다");
      onSaved(String(id));
      onOpenChange(false);
    } catch (e) {
      toast.error(`저장 실패:${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? "새 모델 설정" : `편집:${profile?.name}`}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                활성화 중
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            {isNew
              ? "만든 뒤에는 자동으로 활성화되지 않습니다. 카드에서 「활성화로 설정」을 눌러 켜세요."
              : "수정한 뒤 저장을 누르세요. 활성 설정은 저장 후 모든 에이전트에 바로 적용됩니다."}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-name">이름</Label>
              <Input
                id="p-name"
                placeholder="예: OpenAI 운영"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label>형식</Label>
              <Select value={format} onValueChange={(v) => setFormat(v as "anthropic" | "openai" | "openai-responses")}>
                <SelectTrigger>
                  <SelectValue placeholder="형식 선택" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">Anthropic</SelectItem>
                  <SelectItem value="openai">OpenAI (Chat Completions)</SelectItem>
                  <SelectItem value="openai-responses">OpenAI (Responses API)</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-model">모델</Label>
            <div className="flex gap-2">
              <Input
                id="p-model"
                className="font-mono"
                placeholder="claude-opus-4-8"
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
              {/* modal: 이 Popover의 내용은 <body>로 portal 되어 Sheet의 스크롤 잠금 밖에 있습니다.
                  modal을 주지 않으면 목록은 그려지지만 스크롤되지 않습니다. modal이 있으면 맨 위 스크롤 잠금을 스스로 가집니다. */}
              <Popover open={modelsOpen} onOpenChange={setModelsOpen} modal>
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    className="shrink-0"
                    disabled={loadingModels}
                    onClick={loadModels}
                    title="API에서 사용 가능한 모델 불러오기"
                  >
                    {loadingModels ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                  </Button>
                </PopoverTrigger>
                {models.length > 0 && (
                  <PopoverContent className="max-h-72 w-72 gap-0 overflow-y-auto overscroll-contain p-1" align="end">
                    {models.map((m) => (
                      <button
                        key={m}
                        type="button"
                        className="w-full shrink-0 rounded-md px-2 py-1.5 text-left font-mono text-xs hover:bg-accent hover:text-accent-foreground"
                        onClick={() => {
                          setModel(m);
                          setModelsOpen(false);
                        }}
                      >
                        {m}
                      </button>
                    ))}
                  </PopoverContent>
                )}
              </Popover>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-base-url">Base URL(선택)</Label>
            <Input
              id="p-base-url"
              className="font-mono"
              placeholder="https://api.openai.com/v1"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-proxy">프록시(선택)</Label>
            <Input
              id="p-proxy"
              className="font-mono"
              placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              LLM이 밖으로 보내는 요청만 이 프록시를 탑니다. http/https/socks5를 지원하고 계정과 비밀번호를 넣을 수 있습니다(예: socks5://user:pass@host:port, 비밀번호의 특수문자는 URL 인코딩). 비워 두면 프록시를 쓰지 않습니다(직접 연결).
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-session-header">사용자 지정 세션 헤더(선택)</Label>
            <Input
              id="p-session-header"
              className="font-mono"
              placeholder="예: x-session-id(비우면 보내지 않음)"
              value={sessionHeaderKey}
              onChange={(e) => setSessionHeaderKey(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              헤더 이름을 입력하면 요청마다 이 HTTP 헤더가 붙고, 헤더 값은 자동으로 다음이 됩니다 <b>현재 세션의 session id</b>(chat 세션 예:
              conv-12, worker 예: exp3-worker-i87). session-id 헤더로 프롬프트 캐시 /
              고정 라우팅을 하는 게이트웨이용입니다. 같은 세션의 여러 턴은 안정적이고, 세션마다 값이 다릅니다. 비우면 보내지 않습니다.
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-api-key">API 키</Label>
            <Input
              id="p-api-key"
              type="password"
              placeholder={keyHint ? `설정됨(${keyHint}), 비워 두면 유지` : "sk-…"}
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-3">
            <div className="grid gap-2">
              <Label htmlFor="p-rps">초당 속도 제한</Label>
              <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-rpm">분당 속도 제한</Label>
              <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-cw">컨텍스트 창(K)</Label>
              <Input
                id="p-cw"
                type="number"
                min={0}
                max={1000}
                value={cw}
                onChange={(e) => setCw(e.target.value)}
                placeholder="200"
              />
            </div>
          </div>
          <p className="-mt-2 text-muted-foreground text-xs">
            속도 제한 0은 제한 없음이며 모든 에이전트가 함께 씁니다. 컨텍스트 창 단위는 K(천 token)입니다. 0은 기본 200K, 상한은 1000(즉 1M)입니다. 너무 높게 두면 압축이 시작되지 않습니다.
          </p>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-priority" className="text-sm">
                  순환 우선순위
                </Label>
                <p className="text-muted-foreground text-xs">
                  숫자가 클수록 먼저 선택됩니다. 활성 설정은 항상 1순위이며 이 값과 관계없습니다. 우선순위가 같은 설정은 번갈아 앞에 서서 한도를 나누게 됩니다.
                </p>
              </div>
              <Input
                id="p-priority"
                type="number"
                className="w-24 shrink-0"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">순환에 참여하지 않음</Label>
                <p className="text-muted-foreground text-xs">
                  켜면 장애 조치 대상으로 쓰이지 않습니다(에이전트나 작업이 직접 지정하면 여전히 쓸 수 있습니다). 특정 에이전트만 쓰고, 다른 곳이 실패할 때 소모되지 않게 하려는 비싼 설정에 맞습니다.
                </p>
              </div>
              <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label="순회에 넣지 않음" />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">스트리밍 출력 · streaming</Label>
                <p className="text-muted-foreground text-xs">
                  켜면(기본값) 스트리밍 SSE를 사용합니다. 실행 중 실시간 진행과 실시간 token 수를 볼 수 있습니다. 끄면 진짜 비스트리밍(stream:false, 완성된 응답을 한 번에 반환)입니다. 일부 게이트웨이의 좋지 않은 SSE 처리(빈 프레임 / 추론 항목 누락)를 피할 수 있지만, 실행 중 실시간 진행은 볼 수 없습니다.
                </p>
              </div>
              <Switch checked={streaming} onCheckedChange={setStreaming} aria-label="스트리밍 출력" />
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-max-tokens" className="text-sm">
                  출력 상한 · max tokens
                </Label>
                <p className="text-muted-foreground text-xs">
                  한 번의 답변에서 만들 최대 token 수입니다. 요청마다 함께 보냅니다. 0(기본값)은 이 항목을 보내지 않고 서버 기본값을 따릅니다. 위의 "컨텍스트 창"과는 다릅니다. 그것은 모델의 전체 용량이고, 로컬에서 압축 기준을 계산할 때만 씁니다. 너무 작게 두면 추론 모델이 생각 단계에서 잘려 답을 한 글자도 내지 못할 수 있습니다.
                </p>
              </div>
              <Input
                id="p-max-tokens"
                type="number"
                min={0}
                className="w-28 shrink-0"
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
                placeholder="0"
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">상한 항목 이름</Label>
                <p className="text-muted-foreground text-xs">{MAX_TOKENS_FIELD_HINTS[format]}</p>
              </div>
              <Select
                value={format === "openai" ? maxTokensField : NONE}
                onValueChange={setMaxTokensField}
                disabled={format !== "openai"}
              >
                <SelectTrigger className="w-56 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MAX_TOKENS_FIELDS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label className="text-sm">추론 스위치 · thinking.type</Label>
                <p className="text-muted-foreground text-xs">
                  thinking 항목을 보낼지 정합니다. 보내지 않음은 이 항목을 빼는 것입니다(MiniMax처럼 지원하지 않는 모델과 호환). 끄면 disabled를 보내고, 켜면 enabled를 보냅니다. 아래 강도와는 서로 독립입니다.
                </p>
              </div>
              <Select value={thinkingType} onValueChange={setThinkingType}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {THINKING_TYPES.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">추론 강도 · reasoning_effort</Label>
                <p className="text-muted-foreground text-xs">
                  별도의 강도 단계입니다(OpenAI reasoning_effort / Anthropic output_config.effort). 어떤 인터페이스는 thinking 항목이 없고 강도만으로 추론을 켭니다. 그래서 따로 둘 수 있고, 추론 스위치를 보내지 않을 수 있습니다.
                </p>
              </div>
              <Select value={effort} onValueChange={setEffort}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {EFFORT_LEVELS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <ProfileRetryFields value={retry} onChange={setRetry} />
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? "테스트 중…" : "연결 테스트"}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? "새로 만들기" : "저장"}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────

export default function LLMPage() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [poolOpen, setPoolOpen] = React.useState(false);
  // 서랍의 스위치와 내용은 따로 저장합니다. 닫을 때 editing은 그대로 둡니다. 그렇지 않으면 닫히는 애니메이션 동안 제목이
  // 「X 편집」이 「새로 만들기」로 번쩍임. editing = null이면 새로 만들기.
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* 무시 */
    }
  }, []);

  const load = React.useCallback(async () => {
    try {
      setProfiles(await api.llmProfiles());
    } catch {
      /* 무시 */
    }
    await loadPool();
  }, [loadPool]);

  React.useEffect(() => {
    void load();
  }, [load]);

  // 카드의 건강 배지는 profile id로 주기 조회 상태를 가져옵니다.
  const health = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(`활성화됨:${name}`);
      await load();
    } catch (e) {
      toast.error(`활성화 실패:${(e as Error).message}`);
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error("현재 활성화된 설정은 삭제할 수 없습니다");
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(`삭제됨:${p.name}`);
      await load();
    } catch (e) {
      toast.error(`삭제 실패:${(e as Error).message}`);
    }
  }

  const poolOn = pool?.enabled ?? false;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">LLM</h1>
          <p className="text-muted-foreground text-sm">
            모든 에이전트가 함께 쓰는 형식 / 모델 / 속도 제한 설정입니다. 카드를 눌러 편집하세요. 별표가 현재 활성 설정입니다.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => setPoolOpen(true)}>
            <ZapIcon /> 순환 설정
            {poolOn && (
              <Badge variant="outline" className="ml-1 border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                켜짐
              </Badge>
            )}
          </Button>
          <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
            <PlusIcon /> 새로 만들기
          </Button>
        </div>
      </div>

      <Tabs defaultValue="profiles" className="flex-1">
        <TabsList>
          <TabsTrigger value="profiles">모델 설정</TabsTrigger>
          <TabsTrigger value="retry">재시도와 백오프</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4">
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {profiles.map((p) => {
              const h = healthOf(p, health.get(p.id));
              return (
                // biome-ignore lint/a11y/useSemanticElements: 카드 안에 자체 동작 버튼이 있어, 네이티브 <button>을 쓰면 버튼 중첩이 됩니다(잘못된 HTML)
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30",
                    p.is_default && "border-amber-400/50 bg-amber-400/5",
                  )}
                >
                  <CardContent className="grid gap-2 px-4">
                    <div className="flex items-start gap-2">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className="uppercase">
                            {p.format}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto", h.cls)} title={h.hint}>
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">{p.model}</code>
                      </div>
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6 text-muted-foreground text-xs">
                      {p.api_key_hint && <span>{p.api_key_hint}</span>}
                      <span>
                        {p.rate_per_second}/s · {p.rate_per_minute}/min
                      </span>
                      {p.proxy && <span className="truncate">프록시 {p.proxy}</span>}
                      {p.reasoning_effort && (
                        <span>추론 {p.reasoning_effort === "off" ? "꺼짐" : p.reasoning_effort}</span>
                      )}
                      {/* 주기 조회에 묶인 두 필드는 주기 조회가 켜져 있을 때만 의미가 있습니다. 꺼져 있으면 자리를 차지하지 않습니다 */}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? <span>순환에 참여하지 않음</span> : <span>우선순위 {p.priority ?? 0}</span>)}
                    </div>

                    <div className="mt-1 flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? "활성화함" : "활성화로 설정"}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="설정 삭제"
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
            {profiles.length === 0 && (
              <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
                아직 모델 설정이 없습니다. 오른쪽 위의 "새로 만들기"를 눌러 첫 설정을 만드세요.
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileSheet profile={editing} open={editOpen} onOpenChange={setEditOpen} onSaved={() => void load()} />
      <PoolSheet open={poolOpen} onOpenChange={setPoolOpen} pool={pool} onReload={loadPool} />
    </div>
  );
}
