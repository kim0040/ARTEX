import Link from "next/link";

import { LegalNotice } from "@/components/legal-notice";
import { APP_CONFIG } from "@/config/app-config";

export default function AboutPage() {
  return (
    <main className="mx-auto max-w-3xl space-y-8 px-5 py-10 sm:px-8">
      <header className="space-y-3">
        <p className="text-sm text-muted-foreground">{APP_CONFIG.forkLabel}</p>
        <h1 className="text-2xl font-semibold">{APP_CONFIG.name}</h1>
        <p className="text-muted-foreground">출처·라이선스·이용 안내</p>
        <Link href="/function/tasks" className="inline-block underline underline-offset-4">
          작업 목록으로 돌아가기
        </Link>
      </header>
      <LegalNotice />
    </main>
  );
}
