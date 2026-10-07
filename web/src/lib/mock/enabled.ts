// Mock 스위치. 빌드 때 주입되는 공개 변수(NEXT_PUBLIC_ 접두사만 브라우저에서 읽을 수 있음).
// Vercel에서 NEXT_PUBLIC_MOCK=1을 주면 사이트 전체가 mock으로 가고 백엔드가 필요 없습니다.
// 켜지면 화면은 엔진 대신 이 목업으로 자산 그래프와 탐색 그래프를 보여 줍니다.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
