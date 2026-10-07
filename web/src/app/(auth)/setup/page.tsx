"use client";

import { useEffect, useState } from "react";

import { useRouter } from "next/navigation";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { APP_CONFIG } from "@/config/app-config";
import { auth } from "@/lib/auth";

export default function SetupPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);

  useEffect(() => {
    api.authStatus()
      .then(({ initialized }) => {
        if (initialized) router.replace("/login");
      })
      .catch(() => setError("백엔드 서비스에 연결할 수 없습니다"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setError("두 번 입력한 비밀번호가 다릅니다");
      return;
    }
    if (password.length < 8) {
      setError("비밀번호는 8자 이상으로 입력하세요");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.initPassword(password);
      auth.setToken(token);
      router.replace("/function/tasks");
    } catch (err) {
      setError(err instanceof Error ? err.message : "초기화 실패");
    } finally {
      setLoading(false);
    }
  }

  if (checking) return null;

  return (
    <div className="flex h-dvh">
      {/* 왼쪽 패널 */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element -- 일반 이미지 태그 예외 */}
          <img
            src="/logo.png"
            alt="ARTEX"
            width={160}
            height={160}
            className="relative brightness-0 invert"
          />
        </div>
      </div>

      {/* 오른쪽 패널 */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <p className="text-sm text-muted-foreground">{APP_CONFIG.name} · {APP_CONFIG.forkLabel}</p>
            <h2 className="text-2xl font-medium tracking-tight">초기 비밀번호</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">처음 사용할 관리자 비밀번호를 정하세요. 8자 이상으로 입력해야 합니다.</p>
          </div>
          <p className="text-sm text-center"><a href="/about" className="underline underline-offset-4">출처·라이선스·이용 안내</a></p>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="password">새 비밀번호</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="8자 이상"
                autoFocus
                autoComplete="new-password"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="confirm">비밀번호 확인</Label>
              <Input
                id="confirm"
                type="password"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                placeholder="비밀번호를 다시 입력"
                autoComplete="new-password"
              />
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !confirm}>
              {loading ? "저장 중..." : "비밀번호를 설정하고 로그인"}
            </Button>
          </form>
        </div>
      </div>
    </div>
  );
}
