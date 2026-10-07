import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "ARTEX 한국어 학습판",
  forkLabel: "비공식 한국어 포크",
  sourceUrl: "https://github.com/kim0040/ARTEX",
  upstreamUrl: "https://github.com/Autumn-27/ARTEX",
  version: packageJson.version,
  copyright: `© ${currentYear}, ARTEX 원 작성자·기여자 및 한국어 포크 기여자`,
  meta: {
    title: "ARTEX 한국어 학습판 — 비공식 한국어 포크",
    description: "ARTEX의 구조와 동작을 한국어로 읽고 배우는 학습용 포크입니다.",
  },
};
