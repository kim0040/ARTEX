"use client";

import { useEffect, useRef, useState } from "react";

import { useRouter } from "next/navigation";

import { AlertTriangle, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogClose, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  const [agreed, setAgreed] = useState(false);
  const [termsOpen, setTermsOpen] = useState(false);
  const [readToEnd, setReadToEnd] = useState(false);
  const termsBodyRef = useRef<HTMLDivElement>(null);

  // 약관 맨 아래까지 스크롤해야(스크롤 없이 전체가 보이는 경우 포함) 「동의」를 누를 수 있습니다.
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // 열 때 초기화하고, 내용이 원래 한 화면에 들어가 스크롤이 안 생기는 경우도 처리합니다.
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // 로그인되어 있으면 바로 메인 화면으로(정적 내보내기에는 middleware가 이 이동을 대신하지 않음).
    const token = auth.getToken();
    if (token) {
      // localStorage에는 아직 자격이 있는데 cookie는 없을 수 있습니다. 먼저 맞춘 다음, 새 요청을 보냅니다,
      // 서버 가드나 라우트 캐시가 이동을 아직 checking 상태인 로그인 페이지로 돌려보내지 않게.
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("백엔드 서비스에 연결할 수 없습니다"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("먼저 「사용 안내」를 읽고 동의하세요");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("ARTEX", password);
      auth.setToken(token);
      window.location.replace("/function/tasks");
    } catch {
      setError("사용자 이름 또는 비밀번호가 틀렸습니다");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        로그인 상태를 확인하는 중…
      </div>
    );
  }

  return (
    <div className="flex h-dvh">
      {/* Left panel */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={160} height={160} className="relative brightness-0 invert" />
        </div>
      </div>

      {/* Right panel */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <h2 className="text-2xl font-medium tracking-tight">로그인</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">다시 오셨습니다. ARTEX를 계속 쓰려면 비밀번호를 입력하세요</p>
          </div>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">사용자 이름</Label>
              <Input id="username" value="ARTEX" readOnly className="bg-muted text-muted-foreground" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">비밀번호</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="비밀번호를 입력하세요"
                autoFocus
                autoComplete="current-password"
              />
            </div>
            <div className="flex items-start gap-2">
              <Checkbox
                id="agree-terms"
                checked={agreed}
                onCheckedChange={(v) => setAgreed(v === true)}
                className="mt-0.5"
              />
              <Label htmlFor="agree-terms" className="text-sm font-normal leading-relaxed text-muted-foreground">
                읽었으며 동의합니다
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  "사용 안내"
                </button>
              </Label>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
              {loading ? "로그인 중..." : "로그인"}
            </Button>
          </form>
        </div>
      </div>

      <Dialog open={termsOpen} onOpenChange={setTermsOpen}>
        <DialogContent className="gap-0 p-0 sm:max-w-2xl">
          <DialogHeader className="flex-row items-center gap-3 border-b px-6 py-4">
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-0.5">
              <DialogTitle className="text-base">ARTEX 사용 안내와 면책 조항</DialogTitle>
              <p className="text-xs text-muted-foreground">
                버전 v1.0 · 적용일 2026-09-18 · 로그인 전에 아래 조항을 모두 읽으세요
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              이 "사용 안내와 면책 조항"(이하 "본 안내")은 귀하와 ARTEX 프로젝트 작성자 및 기여자 사이의 사용 약속입니다. 사용 전에 각 조항을 신중히 읽고 충분히 이해하세요. 특히 굵게 표시되거나 색으로 표시된 면책, 책임 제한, 금지 조항을 보세요.
              <span className="font-medium text-foreground">
                {" "}
                이 소프트웨어를 다운로드, 설치, 접속하거나 어떤 방식으로든 사용하면, 이 안내를 읽고 이해했으며 모든 제약을 받는 데 동의한 것으로 봅니다.
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                제1조 · 정의와 오픈소스 라이선스
              </h4>
              <p className="pl-7">
                이 소프트웨어(ARTEX)는 GNU Affero General Public License v3.0(AGPL-3.0)으로 공개된 오픈소스 프로그램입니다. 그 라이선스에 따라 자유롭게 사용, 복제, 수정, 배포할 수 있습니다. 다만 파생 저작물(네트워크로 제3자에게 제공하는 온라인 서비스 포함)도 같은 AGPL-3.0으로 공개하고, 사용자에게 해당하는 전체 소스 코드를 공개해야 합니다. AGPL-3.0 전문은 함께 제공되는 LICENSE 파일을 기준으로 합니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                제2조 · 허용된 사용 범위
              </h4>
              <p className="pl-7">
                이 소프트웨어는 개인 학습, 코드 연구, 보안 기술 원리 토론, 그리고 직접 만든 로컬 격리 환경에서의 기술 확인용입니다. 학습, 학술 연구, 코드 검토처럼 공격이나 파괴가 아닌 용도에 맞습니다. 이 조항이 분명히 허용한 경우 외에는 다른 목적으로 쓸 수 없습니다.
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />
                제3조 · 금지 행위
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  어떤 웹사이트, 온라인 서비스, 다른 사람이나 제3자가 소유한 연결 시스템에 스캔, 탐사, 악용 또는 공격을 해서는 안 됩니다(허가를 받았는지, 자신의 자산인지와 관계없습니다);
                </li>
                <li>이 소프트웨어를 실제 모의 침투, 공방 대결, 레드팀/블루팀 훈련 또는 운영 환경에 사용해서는 안 됩니다;</li>
                <li>이 소프트웨어를 불법 침입, 데이터 탈취, 랜섬, 서비스 거부(DoS/DDoS) 또는 어떤 파괴적이거나 범죄적인 활동에도 사용해서는 안 됩니다;</li>
                <li>이 소프트웨어와 그 출력에 있는 저작권, 라이선스, 안전 안내를 지우거나 바꾸거나 피해서는 안 됩니다;</li>
                <li>본인이 있는 국가나 지역의 법률, 법규, 규제에 어긋나는 어떤 행위도 해서는 안 됩니다.</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                제4조 · 지식재산권
              </h4>
              <p className="pl-7">
                이 소프트웨어의 저작권과 관련 지식재산권은 프로젝트 작성자와 기여자에게 있습니다. AGPL-3.0 라이선스가 정한 범위에서 해당 권리를 부여합니다. 그 라이선스가 분명히 준 권리 외에, 이 안내는 다른 권리를 명시적이거나 묵시적으로 주지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                제5조 · 데이터와 개인정보
              </h4>
              <p className="pl-7">
                이 소프트웨어는 직접 설치하는 오픈소스 프로그램입니다. 작성자는 중앙 서비스를 운영하지 않으며, 사용 데이터를 모으거나 올리지 않습니다. 사용 중 만들거나 다루거나 접하는 모든 데이터는 본인이 관리하며, 그 합법성과 안전도 본인 책임입니다. 데이터를 잘못 다뤄 생기는 결과는 본인이 집니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                제6조 · 준수와 법적 책임
              </h4>
              <p className="pl-7">
                본인이 있는 국가나 지역의 네트워크 안전, 데이터 안전, 개인정보 보호, 컴퓨터 범죄에 관한 모든 법령을 스스로 지켜야 합니다(중국 본토에서는 네트워크 안전법, 데이터 안전법, 개인정보 보호법 및 관련 해석을 포함하되 이에 한정되지 않음).
                <span className="font-medium text-foreground">
                  {" "}
                  위 법령이나 이 안내를 어겨 생기는 모든 법적 책임과 결과는 본인이 혼자 집니다. 이 소프트웨어의 작성자와 기여자와는 관계가 없습니다.
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                제7조 · 면책과 책임 제한
              </h4>
              <p className="pl-7">
                이 소프트웨어는 "있는 그대로(AS IS)"와 "현재 상태(AS AVAILABLE)"로 제공됩니다. 상품성, 특정 목적 적합성, 정확성, 비침해를 포함해 명시적이거나 묵시적인 어떤 보증도 하지 않습니다. 법이 허용하는 최대 범위에서, 작성자와 기여자는 이 소프트웨어를 쓰거나 쓰지 못해 생기는 직접, 간접, 우발, 특별, 결과적 손해에 책임을 지지 않습니다. 사용 방법이 적절한지와 관계없습니다. 데이터 손실, 시스템 손상, 업무 중단, 이익 손실, 법적 분쟁을 포함하되 이에 한정되지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                제8조 · 조항 변경과 최종 해석
              </h4>
              <p className="pl-7">
                작성자는 법령이나 프로젝트 필요에 따라 이 안내를 때때로 바꿀 수 있습니다. 바뀐 버전은 프로젝트와 함께 공개되며 공개한 날부터 적용됩니다. 계속 사용하면 바뀐 조항에 동의한 것으로 봅니다. 법이 허용하는 범위에서 이 안내의 최종 해석 권한은 프로젝트 작성자에게 있습니다. 어떤 조항이 무효가 되어도 나머지 조항의 효력에는 영향이 없습니다.
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "모든 조항을 살펴보았습니다" : "조항을 맨 아래까지 스크롤한 뒤 확인하세요"}
            </p>
            <DialogClose asChild>
              <Button
                type="button"
                disabled={!readToEnd}
                onClick={() => {
                  setAgreed(true);
                  setError("");
                }}
              >
                모든 조항을 읽었으며 동의합니다
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
