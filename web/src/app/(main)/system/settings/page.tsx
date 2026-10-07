"use client";

import * as React from "react";

import { CpuIcon, FlaskConicalIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // 동작 제약을 넣는 범위(기본은 모두 켬).
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // 실험 기능: noa 맥락 압축(기본 끔).
  const [noaCompaction, setNoaCompaction] = React.useState(false);
  // 순수 프론트 선호: /api/settings를 타지 않고 localStorage를 직접 읽고 씁니다.
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
    setNoaCompaction(!!s.noa_compaction);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("동시 실행 수는 0보다 큰 정수여야 합니다");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("동시 작업 에이전트 수를 저장했습니다(이후 시작하는 작업부터 적용)");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("Python 인터프리터 설정을 저장했습니다");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // 먼저 화면에 반영
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // 실패하면 되돌림
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // 먼저 화면에 반영
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // 실패하면 되돌림
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "에이전트 자동 트래픽 연결을 켰습니다" : "에이전트 자동 트래픽 연결을 껐습니다");
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`저장 실패: ${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // 먼저 화면에 반영
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // 실패하면 되돌림
  };

  const toggleNoaCompaction = (v: boolean) => {
    setNoaCompaction(v); // 먼저 화면에 반영
    api
      .setSettings({ noa_compaction: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "noa 맥락 압축을 켰습니다(이후에 시작하는 실행부터 적용)" : "noa 맥락 압축을 껐습니다(내장 압축으로 복귀)");
      })
      .catch((e) => {
        setNoaCompaction(!v); // 실패하면 되돌림
        toast.error(`저장 실패: ${(e as Error).message}`);
      });
  };

  // 웹 검색 설정(켜기 또는 백엔드)을 저장합니다. 먼저 화면에 반영한 뒤 다시 읽습니다.
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("웹 검색 설정을 저장했습니다");
      })
      .catch((e) => {
        toast.error("저장 실패: " + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("Brave API Key를 저장했습니다");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("Tavily API Key를 저장했습니다");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(proxyInput.trim() ? "출구 프록시를 저장했습니다" : "출구 프록시를 지웠습니다(직접 연결로 변경)");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(globalProxyInput.trim() ? "전역 프록시를 저장했습니다" : "전역 프록시를 지웠습니다(직접 연결로 변경)");
      })
      .catch((e) => toast.error("저장 실패: " + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // 지금 폼에 있는 값(백엔드와
  // 프록시, 입력한 키)으로 "test" 검색을 실제로 해 봅니다. 비어 있으면 서버에 저장된 값을 씁니다. 결과는 토스트로 알립니다.
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`검색 테스트 성공 · ${r.backend} 반환 ${r.count} 건의 결과`);
        else toast.error("검색 테스트 실패: " + (r.error || "알 수 없는 오류"));
      })
      .catch((e) => toast.error("검색 테스트 실패: " + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free를 골랐는데 저장된 키도 입력 중인 키도 없으면 도구는 꺼진 채로 둡니다.
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">시스템 설정</h1>
        <p className="text-muted-foreground text-sm">전역 실행 스위치</p>
      </div>

      {/* grid 대신 여러 열: 웹 검색 카드가 나머지보다 몇 배 높고, 높이는 고른 백엔드에 따라 바뀝니다(brave/tavily
          의 key 입력은 조건에 따라 그려짐). grid는 가장 높은 카드에 맞춰 한 줄을 다 채우고 옆에 빈칸을 남깁니다.
          여러 열은 내용 높이에 맞춰 스스로 채워집니다. 카드 간격은 gap이 아니라 mb입니다. 여러 열 배치에서
          column-gap은 열 간격만 맡고, 줄 간격은 자식이 직접 줘야 합니다. */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              트래픽 캡처
            </CardTitle>
            <CardDescription>
              켜면 모든 에이전트의 HTTP 트래픽을 기록 프록시를 통해 저장합니다. 에이전트에 traffic_search / traffic_get 도구와 프록시 설정을 전달하고, 프롬프트에 프록시 설명을 포함합니다.
              <br />
              끄면(기본값) 트래픽을 기록하지 않습니다. 에이전트에 프록시 설정과 트래픽 도구를 전달하지 않고, 프롬프트에도 프록시 관련 내용을 포함하지 않습니다. 설정을 바꾸면 에이전트를 즉시 다시 만들어 적용합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "켜짐 · 트래픽을 기록하고 프록시를 넣는 중" : "꺼짐 · 기록하지 않고, 프록시를 넣지 않음"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              에이전트 자동 트래픽 묶기
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              기본값은 꺼짐입니다. 켜면 발견이 저장될 때 실행되는 보고서 에이전트가 기존 HTTP 요청/응답을 대조하고, 해당 트래픽을 연결한 뒤 보고서를 작성합니다.
              <b>패킷 확인과 추가 도구 호출로 토큰 사용량이 늘어납니다.</b>
              <br />
              TCP이거나 패킷을 잡지 못했거나 일치하는 트래픽이 없어도 정상적으로 보고할 수 있습니다. 이 스위치는 트래픽 캡처, 수동 연결, 저장된 증거 보기에 영향을 주지 않습니다. 다음 라운드의 에이전트부터 적용됩니다. 끄면 새로운 자동 연결을 바로 거절합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "켜짐 · 토큰 소비가 늘어남" : "꺼짐 · 수동으로 계속 연결할 수 있음"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              전역 프록시
            </CardTitle>
            <CardDescription>
              모든 에이전트의 <b>대상 트래픽</b>이 프록시를 통해 외부로 나갑니다(출발 IP 숨김 / 경유 호스트). <b>http / https / socks5</b>를 지원하며,{' '}
              <code>user:pass</code> 인증도 사용할 수 있습니다. 비워 두면 직접 연결됩니다.
              <br />
              <b>트래픽 캡처</b>를 켜면 이 프록시는 기록 프록시의 <b>업스트림</b>으로 사용됩니다(트래픽을 모두 저장한 뒤 이 프록시를 통해 외부로 전송합니다). 캡처를 끄면 에이전트의 bash / WebFetch 외부 통신에 바로 사용합니다. 웹 검색 프록시와 LLM 프록시와는 독립적입니다.
              <br />
              <b>안내</b>: 캡처를 끄면 socks5는 각 명령줄 도구가 <code>ALL_PROXY</code>를 지원하는지에 따라 동작합니다(curl은 사용할 수 있지만 일부 도구는 무시할 수 있습니다). socks5를 주로 쓰면 트래픽 캡처를 켜는 것이 좋습니다. 이 경로는 MITM이 직접 연결하므로 도구가 알지 못해도 안정적으로 적용됩니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              프록시 주소
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="socks5://user:pass@host:1080 또는 http://host:port(비우면 직접 연결)"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                저장
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim() ? "설정됨 · 모든 대상 트래픽이 이 프록시를 통해 외부로 나감" : "설정되지 않음 · 대상 트래픽이 직접 연결됨"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              동작 제한 넣기
            </CardTitle>
            <CardDescription>
              켜면 각 작업의 <b>동작 제한</b>(작업 개요의 「조작 제약」에서 관리하는 allow/deny 항목)을 해당 에이전트의 시스템 프롬프트에 넣어 탐색 범위를 정합니다(예: 「현재 포트만 검사」, 「무차별 대입 금지」).
              <br />
              <b>플래너(planner)</b>와 <b>워커(worker)</b>에 제한을 전달할지 각각 설정할 수 있습니다. 기본값은 모두 켜짐입니다. 변경 사항은 다음 라운드부터 적용되며 에이전트를 다시 만들 필요는 없습니다. 끄면 해당 에이전트에는 더 이상 제한을 전달하지 않습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                플래너(planner)에 적용{injectPlanner ? " · 켜짐" : " · 꺼짐"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                워커(worker)에 적용{injectWorker ? " · 켜짐" : " · 꺼짐"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <FlaskConicalIcon className="size-4" />
              실험 기능
            </CardTitle>
            <CardDescription>
              아직 검증 중인 기능이며 기본값은 꺼짐입니다. 에이전트 동작을 바꾸거나 안정성에 영향을 줄 수 있으므로, 영향을 이해한 뒤 사용하세요.
              <br />
              <b>noa 컨텍스트 압축</b>: 모델이 긴 대화 기록을 스스로 압축합니다(norma v0.4.0). 켜면 플랫폼에 연결된 네 종류의 에이전트(
              <b>플래너 / 워커 / 메인 에이전트 / 대화</b>)가 noa로 맥락을 관리하고 내장 압축을 대신합니다.
              압축 전 원문은 되짚어 볼 수 있도록 해당 작업의 작업 디렉터리에 보관됩니다. 전환은 이후 시작하는 실행부터 적용되며 에이전트를 다시 만들 필요는 없습니다.
              끄면 곧바로 내장 압축으로 돌아갑니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="noa-compaction" className="text-sm font-normal text-muted-foreground">
              noa 컨텍스트 압축{noaCompaction ? " · 켜짐" : " · 꺼짐"}
            </Label>
            <Switch
              id="noa-compaction"
              checked={noaCompaction}
              disabled={!loaded}
              onCheckedChange={toggleNoaCompaction}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              웹 검색
            </CardTitle>
            <CardDescription>
              웹 검색의 <b>전체 스위치와 출처 설정</b>입니다. 전체 스위치를 켜면 각 에이전트 설정에서 웹 검색 사용 여부를 따로 선택할 수 있습니다.
              <b>web_search</b>는 제목·링크·요약만 반환하며 본문은 가져오지 않습니다. 본문 가져오기는 WebFetch가 담당합니다. 웹 검색은 기록 프록시를 거치지 않으며 트래픽 캡처와도 별개입니다.
              <br />
              출처는 <b>DuckDuckGo(ddgs)</b>(Key 불필요), <b>Brave(무료 버전)</b>(Brave API Key 필요), <b>Tavily</b>(Tavily API Key 필요),{' '}
              <b>DeepSeek</b>(현재 LLM 설정 재사용) 중에서 선택합니다. 전체 스위치를 끄면 각 에이전트의 웹 검색 스위치를 사용할 수 없습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch ? "전체 스위치가 켜져 있습니다 · 각 에이전트 설정에서 따로 켤 수 있습니다" : "꺼짐 · 각 에이전트가 웹 검색을 켤 수 없음"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // 먼저 화면에 반영
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">검색 출처</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // 먼저 화면에 반영
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="출처 선택" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo(ddgs · 무료, Key 없음)</SelectItem>
                    <SelectItem value="brave-free">Brave(무료 버전 · Key 필요)</SelectItem>
                    <SelectItem value="tavily">Tavily(Key 필요)</SelectItem>
                    <SelectItem value="deepseek">DeepSeek(공식)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek 공식 웹 검색</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  이 출처는 현재 활성 LLM 설정을 그대로 사용합니다. 따라서 <b>DeepSeek 공식 모델만 지원</b>하며, 이 설정은
                  <b>anthropic 프로토콜을 사용해야 합니다</b>. DeepSeek의 OpenAI 프로토콜 주소는 서버 검색을 지원하지 않습니다. LLM 설정을 바꾸면 이 출처가 동작하지 않을 수 있습니다.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  다른 출처와 달리 검색은 <b>DeepSeek 서버에서 실행</b>되므로 검색할 때마다 모델 호출이 한 번 더 발생합니다(토큰 비용이 발생합니다).
                  검색 요청은 <b>위의 출구 프록시를 거치지 않고</b> <b>트래픽 기록에도 포함되지 않습니다</b>. 반환 결과에는 <b>제목과 링크만</b>
                  포함되며 요약은 제공되지 않습니다. 본문이 필요하면 WebFetch가 가져옵니다.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  위 조건을 만족하는지는 직접 확인하세요. 시스템은 가로채지 않습니다. 아래 "검색 테스트" 버튼으로 실제로 한 번 실행해 확인할 수 있습니다.
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">설정됨</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "설정됨(비우면 그대로)" : "Brave API Key 입력"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    저장
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    Brave를 선택했지만 아직 Key가 없습니다. Key를 저장하기 전에는 검색 도구가 켜지지 않습니다.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  무료 한도는 약 2,000회/월입니다. https://brave.com/search/api/ 에서 Key를 받으세요.
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">설정됨</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "설정됨(비우면 그대로)" : "Tavily API Key 입력(tvly-…)"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    저장
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    Tavily를 선택했지만 아직 Key가 없습니다. Key를 저장하기 전에는 검색 도구가 켜지지 않습니다.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">https://tavily.com 에서 등록하고 API Key를 받으세요.</p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  출구 프록시(선택)
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port 또는 socks5://host:port(비우면 직접 연결)"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    저장
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  검색 주소에만 쓰는 별도의 출구 프록시입니다(VPN/SOCKS 등). 트래픽을 기록하는 MITM 프록시와는 관계없습니다. 네트워크가 안 될 때 이 프록시로 접속합니다.
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  현재 설정(출처 + 프록시 + Key)으로 "test"를 실제로 한 번 검색해 쓸 수 있는지 확인합니다.
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "테스트 중…" : "테스트 검색"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              사용자 지정 스크립트 · Python 인터프리터
            </CardTitle>
            <CardDescription>
              사용자 지정 <b>script</b> 유형 도구는 이 Python 인터프리터로 실행합니다. 경로를 비워 두면 실행할 때 자동으로 찾으며, python3을 우선합니다. venv나 특정 버전의 절대 경로를 직접 지정할 수도 있습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3(비우면 자동 감지)"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                다시 검사
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                저장
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              작업 동시 실행 · 워커 수
            </CardTitle>
            <CardDescription>
              각 작업에서 동시에 실행하는 워커 수입니다(기본 3). 숫자가 클수록 동시에 확인하는 작업이 많아지고 사용량도 늘어납니다. 변경 사항은
              <b>이후 시작하는 작업부터 적용</b>되며 실행 중인 작업에는 영향을 주지 않습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                저장
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              세션 입력창 전송 키
            </CardTitle>
            <CardDescription>
              대화 페이지와 작업 상세의 메인 에이전트 세션 입력창이 이 설정을 함께 사용합니다. 선택하면 즉시 적용되므로 저장할 필요가 없습니다.
              <br />
              이 설정은 <b>이 브라우저에만 저장</b>되며 계정과 동기화되지 않습니다. 브라우저를 바꾸거나 사이트 데이터를 삭제하면 다시 설정해야 합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              보내는 방식
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
