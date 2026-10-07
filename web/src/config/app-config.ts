import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "ARTEX",
  version: packageJson.version,
  copyright: `© ${currentYear}, ARTEX.`,
  meta: {
    title: "ARTEX — 자율 침투 테스트 콘솔",
    description: "LLM이 이끄는 자율 침투 테스트 시스템 콘솔",
  },
};
