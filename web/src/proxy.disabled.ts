// 프록시를 꺼 둔 파일입니다.
// 켜려면 이 파일 이름을 `proxy.ts`로 바꾸세요.
import { type NextRequest, NextResponse } from "next/server";

/**
 * 요청이 끝나기 전에 실행됩니다.
 * 주소 바꾸기, 리다이렉트, 헤더 변경에 씁니다.
 * 더 많은 예는 Next.js 프록시 문서를 보세요.
 */
export function proxy(_req: NextRequest) {
  // 예시: 로그인되어 있으면 대시보드로 보냅니다
  // const token = req.cookies.get("session_token")?.value; // 세션 쿠키 예시
  // if (token && req.nextUrl.pathname === "/auth/login") // 로그인 화면이면
  //   return NextResponse.redirect(new URL("/dashboard", req.url)); // 대시보드로 이동

  return NextResponse.next();
}

/**
 * 매처는 모든 경로에서 실행됩니다.
 * 자산이나 API를 건너뛰려면 문서의 부정 매처를 쓰세요.
 */
export const config = {
  matcher: "/:path*",
};
