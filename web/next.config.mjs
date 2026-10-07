import { fileURLToPath } from "node:url";

// 정적 내보내기: `NEXT_EXPORT=1 next build` 는 web/out 에 정적 파일만 만든다.
// nginx 문서 루트에 그대로 넣을 수 있다. 개발(next dev)에서는 이 변수를 두지 않아
// /api 프록시와 핫 리로드가 남는다.
// 초보: 화면은 여기서 빌드되고, 자산 그래프·탐색 그래프 데이터는 Go 프로세스(:8787)의 /api 로 읽는다.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel 데모: 사이트 전체가 mock 이고 백엔드가 없어 /api 프록시가 필요 없다.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 상위 폴더의 lockfile 이 프로젝트 루트 추정과 자원 경로를 바꾸지 않게 한다.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // 개발 중 같은 망의 IP 에서 HMR 자원에 접속할 수 있게 한다. 필요하면 목록을 고친다.
  // dev 에서는 /_next/* 와 HMR 을 아무 IPv4 에서나 연다. 망 주소가 바뀌어도 된다.
  // Next 는 보안 때문에 맨 "*" 를 막는다. 조각을 나눈 "*.*.*.*" 가 아무 IPv4 와 맞는다.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 순수 정적 내보내기: Node 런타임 없음. 이미지는 최적화하지 않음. 경로마다 <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock 데모: 백엔드가 없어 /api 프록시가 필요 없다.
          images: { unoptimized: true },
        }
      : {
          // 개발: /api/* 를 Go 백엔드(기본 :8787, AUTOPENTEST_API 로 바꿀 수 있음)로 프록시한다.
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
