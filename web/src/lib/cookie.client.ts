// 브라우저에서만 쓰는 쿠키 도구입니다.
// 이 함수들은 브라우저 쿠키만 다룹니다.
// 서버 쪽 쿠키 변경은 서버 액션이 맡습니다.

function writeClientCookie(serializedCookie: string) {
  // biome-ignore lint/suspicious/noDocumentCookie: 이 프로젝트는 넓은 브라우저 지원을 위해 아직 document.cookie를 씁니다.
  document.cookie = serializedCookie;
}

export function setClientCookie(key: string, value: string, days = 7) {
  const expires = new Date(Date.now() + days * 864e5).toUTCString();
  writeClientCookie(`${key}=${value}; expires=${expires}; path=/`);
}

export function getClientCookie(key: string) {
  return document.cookie
    .split("; ")
    .find((row) => row.startsWith(`${key}=`))
    ?.split("=")[1];
}

export function deleteClientCookie(key: string) {
  writeClientCookie(`${key}=; expires=Thu, 01 Jan 1970 00:00:00 UTC; path=/`);
}
