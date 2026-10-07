"use client";

import * as React from "react";

import { BellIcon, PlusIcon, SendIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { NotificationChannel, NotificationFilter, NotificationMeta } from "@/lib/types";

import {
  CHANNEL_FIELDS,
  type ChannelForm,
  emptyForm,
  KIND_LABEL,
  parseIDs,
  parseKeywords,
  parseKV,
  SEVERITY_OPTIONS,
} from "./_components/channel-fields";
import { ConfigField, FilterSummary } from "./_components/channel-form";
import { DeliveryList } from "./_components/delivery-list";
import { formatBacklog, StatTile } from "./_components/stat-tile";

// 이 페이지는 엮기만 합니다. 데이터를 불러오고, 양식 상태를 유지하고, API를 호출합니다.
// 필드 정의와 해석은 _components/channel-fields.ts에, 컨트롤과 필터 요약은
// _components/channel-form.tsx, 전달 기록은 _components/delivery-list.tsx——
// 나눈 이유: 각자 따로 읽을 수 있는데, 한 파일에 몰아 넣으면 이 페이지가 1100줄에 가깝기 때문입니다.
export default function NotifyPage() {
  const [meta, setMeta] = React.useState<NotificationMeta | null>(null);
  const [channels, setChannels] = React.useState<NotificationChannel[]>([]);
  const [tab, setTab] = React.useState<"channels" | "deliveries">("channels");

  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<NotificationChannel | null>(null);
  const [form, setForm] = React.useState<ChannelForm>(emptyForm("dingtalk"));
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  const [globalSaving, setGlobalSaving] = React.useState(false);
  const [baseURL, setBaseURL] = React.useState("");
  const [digestMin, setDigestMin] = React.useState("");

  const load = React.useCallback(() => {
    api
      .notifyMeta()
      .then((m) => {
        setMeta(m);
        setBaseURL(m.public_base_url);
        setDigestMin(m.digest_interval_min);
      })
      .catch((e) => toast.error("푸시 설정 읽기 실패: " + (e as Error).message));
    // 채널 목록 불러오기 실패는 알려야 합니다. 조용히 실패하면 「채널이 하나도 없음」처럼 보이고,
    // 사용자는 설정이 사라진 줄 압니다. 바로 오류를 내는 것보다 더 당황스럽습니다.
    api
      .notifyChannels()
      .then(setChannels)
      .catch((e) => toast.error("채널 목록 읽기 실패: " + (e as Error).message));
  }, []);
  React.useEffect(() => {
    load();
  }, [load]);

  function setF(patch: Partial<ChannelForm>) {
    setForm((f) => ({ ...f, ...patch }));
  }
  function setCfg(key: string, value: unknown) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function openAdd() {
    setEditing(null);
    setForm(emptyForm(meta?.kinds[0]?.kind ?? "dingtalk"));
    setOpen(true);
  }

  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    // filter는 백엔드에서 Go 구조체라 항상 객체로 직렬화됩니다(null이 아님). 그래서 대비가 필요 없습니다.
    const f = ch.filter;
    setForm({
      name: ch.name,
      kind: ch.kind,
      mode: ch.mode,
      enabled: ch.enabled,
      ratePerMin: String(ch.rate_per_min),
      // 백엔드가 다시 보여 주는 config의 자격은 가림 값입니다. 양식에 그대로 넣고, 보낼 때도 그대로 돌려보내며,
      // 백엔드는 이것으로 저장소의 원래 값을 유지합니다.
      config: { ...ch.config },
      minSeverity: f.min_severity ?? "",
      includeText: (f.vulnclass_include ?? []).join("\n"),
      excludeText: (f.vulnclass_exclude ?? []).join("\n"),
      taskIDsText: (f.task_ids ?? []).join(","),
      assetIDsText: (f.asset_ids ?? []).join(","),
      onStatusChange: f.on_status_change ?? false,
    });
    setOpen(true);
  }

  // buildConfig는 양식 상태를 채널 config로 바꿉니다.
  //
  // 유일한 규칙, 두 종류의 값:
  //   - 가림 값("__masked__...")은 그대로 돌려보냄 → 백엔드는 「이 필드는 안 바뀜, 저장소의 원래 값 유지」로 읽습니다
  //   - 나머지는 모두 사용자가 입력한 대로 보냅니다. 빈 문자열은 「그 필드를 비움」입니다
  //
  // 자격 필드를 특별히 봐 주지 않는 이유(예: 「자격은 비워 두면 건너뜀」)는, 그러면 사용자가 **지울 수 없기** 때문입니다
  // 잘못 넣은 비밀 키. 화면의 어떤 동작도 「이것을 지우겠다」를 표현할 수 없었습니다. 지금의 규칙에서는,
  // 입력 칸을 비우면 그 필드를 비우는 것입니다. 의미가 하나이고 사용자가 제어할 수 있습니다.
  // 가림 값은 입력 칸에 나타나지 않습니다(ConfigField 참고). 그래서 「칸에 글자가 있음」은 언제나
  // 「사용자가 직접 적은 것」.
  function buildConfig(): Record<string, unknown> {
    const defs = CHANNEL_FIELDS[form.kind] ?? [];
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const raw = form.config[d.key];
      if (d.kind === "switch") {
        out[d.key] = raw === true;
        continue;
      }
      if (typeof raw === "string" && raw.startsWith("__masked__")) {
        out[d.key] = raw;
        continue;
      }
      if (d.kind === "number") {
        const n = Number(raw);
        out[d.key] = Number.isFinite(n) && n > 0 ? n : 0;
        continue;
      }
      if (d.kind === "kv") {
        out[d.key] = parseKV(String(raw ?? ""));
        continue;
      }
      if (d.kind === "list") {
        out[d.key] = String(raw ?? "")
          .split(/[\s,，]+/)
          .map((s) => s.trim())
          .filter(Boolean);
        continue;
      }
      out[d.key] = String(raw ?? "").trim();
    }
    return out;
  }

  function buildFilter(): NotificationFilter {
    return {
      min_severity: form.minSeverity || undefined,
      vulnclass_include: parseKeywords(form.includeText),
      vulnclass_exclude: parseKeywords(form.excludeText),
      task_ids: parseIDs(form.taskIDsText),
      asset_ids: parseIDs(form.assetIDsText),
      on_status_change: form.onStatusChange,
    };
  }

  async function saveForm() {
    if (!form.name.trim()) {
      toast.error("채널 이름을 입력하세요");
      return;
    }
    setSaving(true);
    try {
      const payload = {
        name: form.name.trim(),
        kind: form.kind,
        mode: form.mode,
        enabled: form.enabled,
        config: buildConfig(),
        filter: buildFilter(),
        rate_per_min: form.ratePerMin.trim() === "" ? undefined : Number(form.ratePerMin),
      };
      if (editing) {
        await api.notifyUpdateChannel(editing.id, payload);
        toast.success("저장했습니다");
        setOpen(false);
      } else {
        await api.notifyCreateChannel(payload);
        toast.success("채널을 추가했습니다");
        setOpen(false);
      }
      load();
    } catch (e) {
      toast.error("저장 실패:" + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function testChannel() {
    if (!editing) return;
    setTesting(true);
    try {
      const r = await api.notifyTestChannel(editing.id);
      toast.success(`테스트 메시지를 보냄(${r.latency_ms} ms). 그룹에서 확인하세요`);
    } catch (e) {
      // 백엔드는 채널이 돌려준 원래 오류를 그대로 전달합니다. 설정 확인의 유일한 단서이므로 그대로 보여 줍니다.
      toast.error("테스트 실패: " + (e as Error).message, { duration: 12000 });
    } finally {
      setTesting(false);
    }
  }

  async function removeChannel(ch: NotificationChannel) {
    try {
      await api.notifyDeleteChannel(ch.id);
      toast.success(`삭제됨:${ch.name}`);
      setOpen(false);
      load();
    } catch (e) {
      toast.error("삭제 실패:" + (e as Error).message);
    }
  }

  async function toggleEnabled(ch: NotificationChannel) {
    try {
      await api.notifyUpdateChannel(ch.id, { enabled: !ch.enabled });
      load();
    } catch (e) {
      toast.error("동작 실패: " + (e as Error).message);
    }
  }

  async function toggleGlobal(on: boolean) {
    setGlobalSaving(true);
    try {
      await api.setSettings({ notify_enabled: on });
      setMeta((m) => (m ? { ...m, enabled: on } : m));
      toast.success(on ? "푸시가 켜져 있습니다" : "푸시가 일시정지되었습니다");
    } catch (e) {
      toast.error("동작 실패: " + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  async function saveGlobal() {
    setGlobalSaving(true);
    try {
      const patch: Record<string, unknown> = { notify_public_base_url: baseURL.trim() };
      const n = Number(digestMin);
      if (Number.isFinite(n) && n > 0) patch.notify_digest_interval_min = n;
      await api.setSettings(patch);
      toast.success("저장했습니다");
      load();
    } catch (e) {
      toast.error("저장 실패:" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  const fields = CHANNEL_FIELDS[form.kind] ?? [];
  const secretKeys = new Set(meta?.kinds.find((k) => k.kind === form.kind)?.secret_keys ?? []);
  const defaultRate = meta?.kinds.find((k) => k.kind === form.kind)?.default_rate_per_min ?? 0;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">알림</h1>
          <p className="text-muted-foreground text-sm">
            발견이 나오면 딩톡 / 페이사 / 기업 위챗 같은 채널로 보냅니다 · 채널마다 보내는 시점과 필터를 따로 정할 수 있습니다
          </p>
        </div>
        {meta && (
          // div를 쓰고 label은 쓰지 않음: Switch에 aria-label이 있고, 밖에 label을 한 겹 더 씌우면
          // 어떤 네이티브 컨트롤에도 연결되지 않으면서, 글자를 누르면 전환될 것처럼 보이게 합니다.
          <div className="flex shrink-0 items-center gap-2 text-sm">
            <span className="text-muted-foreground">전체 스위치</span>
            <Switch
              checked={meta.enabled}
              disabled={globalSaving}
              onCheckedChange={toggleGlobal}
              aria-label="푸시 전체 스위치"
            />
          </div>
        )}
      </div>

      {meta && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile label="채널" value={`${meta.stats.channels_on} / ${meta.stats.channels}`} hint="사용 / 총수" />
          <StatTile label="오늘 전달" value={String(meta.stats.sent_today)} />
          <StatTile label="전송 대기" value={String(meta.stats.pending)} />
          <StatTile label="실패" value={String(meta.stats.failed)} tone={meta.stats.failed > 0 ? "red" : undefined} />
          <StatTile
            label="가장 오래 적체"
            value={formatBacklog(meta.stats.backlog_age_ms)}
            // 밀린 나이가 밀린 건수보다 훨씬 유용합니다. 3건이 밀린 것은 3초일 수도, 3시간일 수도 있습니다.
            hint={meta.stats.backlog_age_ms > 5 * 60_000 ? "푸시가 멈춘 것 같습니다" : undefined}
            tone={meta.stats.backlog_age_ms > 5 * 60_000 ? "red" : undefined}
          />
        </div>
      )}

      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">전역 설정</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="n-base">되돌아갈 주소</Label>
            <Input
              id="n-base"
              placeholder="https://artex.example.com"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">메시지의 "상세 보기" 버튼이 가리키는 주소입니다. 비워 두면 버튼이 없습니다.</p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="n-digest">모음 주기(분)</Label>
            <Input
              id="n-digest"
              type="number"
              min={1}
              max={1440}
              placeholder="30"
              value={digestMin}
              onChange={(e) => setDigestMin(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">"모음" 모드 채널에만 적용됩니다.</p>
          </div>
          <div className="sm:col-span-2">
            <Button onClick={saveGlobal} disabled={globalSaving}>
              전역 설정 저장
            </Button>
          </div>
        </CardContent>
      </Card>

      <Tabs value={tab} onValueChange={(v) => setTab(v as "channels" | "deliveries")} className="flex flex-col gap-4">
        <TabsList>
          <TabsTrigger value="channels">채널</TabsTrigger>
          <TabsTrigger value="deliveries">전달 기록</TabsTrigger>
        </TabsList>

        <TabsContent value="channels">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <button
              type="button"
              onClick={openAdd}
              className="text-foreground/70 border-foreground/70 hover:bg-muted/60 hover:shadow-sm flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition"
            >
              <PlusIcon className="size-6" />
              <span className="text-sm">채널 추가</span>
            </button>

            {channels.map((ch) => (
              <Card
                key={ch.id}
                onClick={() => openEdit(ch)}
                className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
              >
                <CardHeader>
                  <div className="flex items-center gap-2">
                    <BellIcon className="text-muted-foreground size-4 shrink-0" />
                    <CardTitle className="truncate text-base">{ch.name}</CardTitle>
                    {/* 카드 전체를 누르면 편집으로 들어가므로, 이 두 컨트롤은 각자 버블을 삼켜야 합니다.
                        그렇지 않으면 스위치나 삭제가 편집까지 같이 켭니다. stopPropagation을 컨트롤 자체에
                        겁니다. div로 한 번 더 감싸지 않습니다. div로 감싸면 「눌러지는 것 같지만 역할이
                        없는」 정적 요소가 되어 a11y 경고가 나고, 의미도 맞지 않습니다. */}
                    <div className="ml-auto flex items-center gap-2">
                      <Switch
                        checked={ch.enabled}
                        onCheckedChange={() => toggleEnabled(ch)}
                        onClick={(e) => e.stopPropagation()}
                        aria-label="사용"
                      />
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="삭제"
                        onClick={(e) => {
                          e.stopPropagation();
                          // void로 Promise를 명시적으로 버림: removeChannel이 스스로 catch하고 toast하며,
                          // 여기서는 await가 필요 없습니다(onClick이 async가 아님).
                          void removeChannel(ch);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="grid gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{KIND_LABEL[ch.kind] ?? ch.kind}</Badge>
                    <Badge variant="outline">{ch.mode === "digest" ? "집계" : "실시간"}</Badge>
                    {!ch.enabled && <Badge variant="outline">사용 중지됨</Badge>}
                  </div>
                  <FilterSummary filter={ch.filter} />
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="deliveries">
          <DeliveryList channels={channels} />
        </TabsContent>
      </Tabs>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? editing.name : "알림 채널 추가"}</SheetTitle>
            <SheetDescription>
              {KIND_LABEL[form.kind] ?? form.kind}
              {defaultRate > 0 ? ` · 기본 속도 제한 ${defaultRate} 건/분` : " · 속도 제한 없음"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label>채널 유형</Label>
                <Select
                  value={form.kind}
                  onValueChange={(v) => {
                    // 유형을 바꾸면 자격 필드 세트가 바뀌므로, 이전 설정을 합쳐 넣을 수 없습니다.
                    setF({ kind: v, config: {} });
                  }}
                  disabled={!!editing}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(meta?.kinds ?? []).map((k) => (
                      <SelectItem key={k.kind} value={k.kind}>
                        {KIND_LABEL[k.kind] ?? k.kind}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {editing && (
                  <p className="text-muted-foreground text-xs">
                    채널 유형은 바꿀 수 없습니다. 유형을 바꾸면 자격 증명 세트를 바꾸는 것과 같습니다. 새 채널을 만드세요.
                  </p>
                )}
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-name">채널 이름</Label>
                <Input
                  id="n-name"
                  placeholder="비상 대응 방 / 일상 안내 방"
                  value={form.name}
                  onChange={(e) => setF({ name: e.target.value })}
                />
              </div>

              {fields.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  이 채널의 입력 화면이 아직 없습니다(프론트엔드에 CHANNEL_FIELDS 항목이 없음). 채운 뒤 다시 시도하세요.
                </p>
              ) : (
                fields.map((d) => (
                  <ConfigField
                    key={d.key}
                    def={d}
                    value={form.config[d.key]}
                    isSecret={secretKeys.has(d.key)}
                    onChange={(v) => setCfg(d.key, v)}
                  />
                ))
              )}

              <div className="grid gap-2">
                <Label>푸시 시점</Label>
                <Select value={form.mode} onValueChange={(v) => setF({ mode: v as "realtime" | "digest" })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="realtime">실시간 · 발견마다 하나씩 보냄</SelectItem>
                    <SelectItem value="digest">모음 · 주기마다 하나로 합침</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">
                  "높음은 실시간, 나머지는 모음"으로 하려면 채널을 두 개 만드세요. 하나는 실시간 + 기준 높음, 하나는 모음 + 등급 제한 없음입니다.
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-rate">속도 제한(건/분)</Label>
                <Input
                  id="n-rate"
                  type="number"
                  min={0}
                  placeholder={defaultRate > 0 ? String(defaultRate) : "0 = 제한 없음"}
                  value={form.ratePerMin}
                  onChange={(e) => setF({ ratePerMin: e.target.value })}
                />
                <p className="text-muted-foreground text-xs">
                  비워 두면 채널 기본값을 씁니다. 0은 속도 제한 없음입니다. 한도를 넘어도 메시지를 버리지 않고 보내기만 미룹니다.
                </p>
              </div>

              <div className="border-t pt-4">
                <p className="mb-3 text-sm font-medium">필터 규칙(비우면 필터하지 않음)</p>
                <div className="grid gap-4">
                  <div className="grid gap-2">
                    <Label>최저 등급</Label>
                    <Select
                      value={form.minSeverity || "all"}
                      onValueChange={(v) => setF({ minSeverity: v === "all" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITY_OPTIONS.map((o) => (
                          <SelectItem key={o.value || "all"} value={o.value || "all"}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-inc">이 발견 유형만 보냄</Label>
                    <Textarea
                      id="n-inc"
                      placeholder={"SQL 인젝션\n명령 실행"}
                      value={form.includeText}
                      onChange={(e) => setF({ includeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      한 줄에 키워드 하나. 대소문자를 가리지 않는 부분 문자열 일치. 비워 둠=모든 유형.
                    </p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-exc">이 발견 유형 제외</Label>
                    <Textarea
                      id="n-exc"
                      placeholder={"정보 유출"}
                      value={form.excludeText}
                      onChange={(e) => setF({ excludeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">제외가 포함보다 우선합니다. 둘 다 맞으면 제외됩니다.</p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-tasks">작업 ID 제한</Label>
                    <Input
                      id="n-tasks"
                      placeholder="1, 2, 3"
                      value={form.taskIDsText}
                      onChange={(e) => setF({ taskIDsText: e.target.value })}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-assets">자산 ID 제한</Label>
                    <Input
                      id="n-assets"
                      placeholder="10, 11"
                      value={form.assetIDsText}
                      onChange={(e) => setF({ assetIDsText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">작업/자산을 비우면 제한 없음. 입력하면 발견과 겹쳐야 합니다.</p>
                  </div>
                  <div className="flex items-center gap-2 text-sm">
                    <Switch
                      checked={form.onStatusChange}
                      onCheckedChange={(v) => setF({ onStatusChange: v })}
                      aria-label="상태 변경 수신"
                    />
                    발견 처리 상태가 바뀔 때도 보냅니다(실시간 모드만)
                  </div>
                </div>
              </div>

              <div className="flex items-center gap-2 text-sm">
                <Switch checked={form.enabled} onCheckedChange={(v) => setF({ enabled: v })} aria-label="사용" />
                이 채널 사용
              </div>
            </div>

            <div className="flex gap-2 pt-2 pb-6">
              <Button onClick={saveForm} disabled={saving}>
                {editing ? "저장" : "추가"}
              </Button>
              {editing && (
                <Button variant="outline" onClick={testChannel} disabled={testing}>
                  <SendIcon /> 테스트 메시지 보내기
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
