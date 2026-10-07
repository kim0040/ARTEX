import { APP_CONFIG } from "@/config/app-config";
import { LEGAL_NOTICE_DATE, LEGAL_SECTIONS } from "@/config/legal-notice";

export function LegalNotice() {
  return (
    <div className="space-y-5 text-sm leading-relaxed">
      <p className="rounded-lg border bg-muted/40 p-3">
        이 글은 한국어 학습판의 출처와 이용 안내입니다. 라이선스 원문을 대체하지 않습니다.
        <span className="mt-1 block text-xs text-muted-foreground">안내 갱신일: {LEGAL_NOTICE_DATE}</span>
      </p>
      {LEGAL_SECTIONS.map((section) => (
        <section key={section.title} className="space-y-2">
          <h2 className="font-semibold text-foreground">{section.title}</h2>
          <p className="text-muted-foreground">{section.body}</p>
        </section>
      ))}
      <nav aria-label="출처와 라이선스 자료" className="flex flex-wrap gap-x-4 gap-y-2 border-t pt-4">
        <a className="underline underline-offset-4" href={APP_CONFIG.sourceUrl}>
          한국어 포크 소스
        </a>
        <a className="underline underline-offset-4" href={APP_CONFIG.upstreamUrl}>
          원 프로젝트
        </a>
        <a className="underline underline-offset-4" href="/legal/LICENSE.txt">
          AGPL 라이선스 원문
        </a>
        <a className="underline underline-offset-4" href="/legal/THIRD-PARTY-LICENSE.txt">
          화면 기반 코드 MIT 고지
        </a>
        <a className="underline underline-offset-4" href="/legal/DEPENDENCY-NOTICES.txt">
          의존성 라이선스·저작권 고지
        </a>
        <a className="underline underline-offset-4" href="/legal/UPSTREAM-NOTICE.txt">
          원 작성자의 이용 안내 원문
        </a>
        <a
          className="underline underline-offset-4"
          href="https://www.law.go.kr/lsLawLinkInfo.do?chrClsCd=010202&lsJoLnkSeq=900629439"
        >
          정보통신망법 제48조
        </a>
        <a className="underline underline-offset-4" href="https://www.law.go.kr/법령/개인정보보호법">
          개인정보 보호법
        </a>
      </nav>
    </div>
  );
}
