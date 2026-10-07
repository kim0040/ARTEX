"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 새 버전이 올라오기를 기다리는 최대 시간. 업그레이드 한 번은 프로세스를 세 번 시작합니다(잠시 보관 → 교체 → 새 버전).
 *  매번 초 단위이고, 3분이면 느린 디스크와 Docker 컨테이너 다시 만들기를 덮습니다. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress와 분리: 잠시 보관이 끝나면 프로세스가 사라지고 SSE가 끊깁니다. 그때 /api/health 주기 조회로 바꿉니다.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet는 백엔드 캐시를 우회할지도 정합니다. 페이지에 들어올 때의 자동 확인은 캐시를 씁니다(윗줄이 방금 조회함),
  // 사용자가 직접 「업데이트 확인」을 누르면 원본으로 강제합니다. 그렇지 않으면 방금 나온 버전은 캐시가 지나야 보입니다.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("업데이트 확인 실패: " + r.error);
          else if (r.has_update) toast.success(`새 버전 발견 ${r.latest}`);
          else if (r.comparable) toast.success("지금이 최신 버전입니다");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("업데이트 확인 실패: " + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // /api/health를 주기 조회하여 버전 번호가 바뀔 때까지.
  //
  // 판정 기준은 "연결됨"이 아니라 "버전이 바뀜"이어야 합니다. 교체하는 동안 이전 버전이 잠깐 다시 한 번 올라오고
  // (그 한 번은 artex.new로 교체하고 바로 종료할 뿐). 연결만 보면 성공으로 잘못 판단합니다.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`다음으로 업데이트됨 ${j.version}, 페이지를 다시 불러오는 중`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 재시작 창 안에 연결이 안 되는 것은 예상입니다. 주기 조회를 계속합니다.
      }
    }
    setRestarting(false);
    toast.error("서비스 재시작 대기가 시간 초과되었습니다. 백엔드 로그를 확인하거나, artex가 start.sh / start.bat으로 시작되었는지 확인하세요.");
  }, []);

  // 업데이트 진행을 구독. SSE는 Next의 /api 재작성을 타지 않습니다(그 층은 버퍼링되어 이벤트가 안 나감).
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("업데이트 실패: " + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // 프로세스가 끝날 때 SSE는 반드시 끊깁니다. 이미 재시작 대기에 들어갔다면 이것은 정상입니다.
        // /api/health 주기 조회가 이어서 판단하면 됩니다.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `다음으로 업데이트 확인 ${info.latest}？\n\n` +
        "업데이트하면 프로그램이 다시 시작되고, 실행 중인 작업은 중단됩니다.\n" +
        (info.mode === "docker"
          ? "\n주의: 컨테이너 안 업데이트는 프로그램만 바꾸고, 이미지 안의 playwright / nmap 같은 도구 모음은 업데이트하지 않습니다." +
            "새 버전이 새 도구에 의존하면 docker compose pull을 사용하세요."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "준비 중…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("업데이트 시작 실패:" + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "이전 버전으로 되돌릴까요?\n\n프로그램이 다시 시작되고, 실행 중인 작업은 중단됩니다.\n주의: 데이터베이스 구조는 되돌리지 않으며, 이전 버전은 새 버전이 쓴 데이터를 알아보지 못할 수 있습니다.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("이전 버전으로 바꿨습니다. 다시 시작하는 중…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("되돌리기 실패:" + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 다운로드 단계에서만 진짜 백분율을 얻을 수 있습니다(Content-Length로 계산). 검사/압축 풀기/재시작 대기는 모두
  // 시간을 알 수 없는 단계. 진행 막대를 채우고 맥박 애니메이션을 넣어 "바쁘지만 얼마나 더 걸릴지 모름"을 나타냅니다.
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 설정 페이지는 여러 열 폭포 배치입니다. 카드가 줄 간격을 스스로 맡고 열을 가로질러 끊기지 않습니다(page.tsx 주석 참고).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          버전과 업데이트
        </CardTitle>
        <CardDescription>GitHub에서 새 버전을 확인하고 설치합니다. 업데이트하면 프로그램이 다시 시작되고, 실행 중인 작업은 중단됩니다.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">현재 버전</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "독립 프로그램"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">최신 버전</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              업데이트 기록 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            GitHub에 연결할 수 없습니다:{info.error}
            {"　"}위에서 전역 프록시를 설정한 뒤 다시 시도할 수 있습니다.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} 제공하지 않음 {info.os}/{info.arch} 의 배포 패키지(없음 {info.asset}), 자동으로 업데이트할 수 없습니다.
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            다운로드합니다 <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}, SHA256을 검사하고 스모크 테스트를 통과한 뒤에만 교체하며, 실패하면 현재 버전을 자동으로 유지합니다.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            현재 최신 버전입니다.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker에서의 업데이트는 프로그램만 바꿉니다. 이미지 안의 playwright / nmap 같은 도구 묶음은 업데이트하지 않으며,
            <span className="font-mono"> docker compose up -d </span>
            컨테이너를 다시 만들면 이미지에 들어 있는 버전으로 돌아갑니다. 이미지까지 함께 올리려면 다음을 실행하세요
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "다시 시작하고 새 버전을 적용하는 중입니다. 잠시 기다리세요(페이지가 자동으로 새로고침됩니다)…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            업데이트 확인
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `다음으로 업데이트 ${info.latest}` : "지금 업데이트"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              이전 버전으로 되돌리기
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          원클릭 업데이트는 보호 스크립트가 프로그램을 다시 시작합니다. 다음을 통해 진행하세요 <span className="font-mono">start.sh</span>(Windows는
          <span className="font-mono"> start.bat</span>)로 ARTEX를 시작합니다. artex 본체를 직접 실행하면, 프로그램이 끝난 뒤 자동으로 다시 켜지지 않습니다.
        </p>
      </CardContent>
    </Card>
  );
}
