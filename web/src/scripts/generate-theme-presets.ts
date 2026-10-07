/**
 * 스크립트: generate-theme-presets.ts
 *
 * /styles/presets 안의 CSS에서 테마 정의를 찾습니다.
 * `label:`, `value:`와 라이트/다크의 기본색(`--primary`)을 꺼냅니다.
 * 이 기본색은 화면에서 테마를 색 점이나 미리보기로 보여 줄 때 씁니다.
 * 기본 테마 색은 /app/globals.css에서 가져옵니다.
 * 꺼낸 정보는 /lib/preferences/theme.ts의 표시된 구간에 넣습니다.
 *
 * 사용법:
 * - 로컬에서 테마 프리셋을 추가한 뒤 직접 실행합니다:
 *     npm run generate:presets  테마 프리셋을 만듭니다
 * - 새 CSS 프리셋에는 `label:`과 `value:` 주석이 있어야 합니다.
 * - 지금은 Husky의 push 전 훅이 이 생성을 자동으로 실행합니다.
 * - 원하면 빌드 단계에 직접 넣어도 됩니다.
 */

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

const presetDir = path.resolve(__dirname, "../styles/presets");

if (!fs.existsSync(presetDir)) {
  console.error(`❌ Preset directory not found at: ${presetDir}`);
  process.exit(1);
}

const outputPath = path.resolve(__dirname, "../lib/preferences/theme.ts");

const files = fs.readdirSync(presetDir).filter((file) => file.endsWith(".css"));

if (files.length === 0) {
  console.warn("⚠️ No preset CSS files found. Only default preset will be included.");
}

const presets = files.map((file) => {
  const filePath = path.join(presetDir, file);
  const content = fs.readFileSync(filePath, "utf8");

  const labelMatch = content.match(/label:\s*(.+)/);
  const valueMatch = content.match(/value:\s*(.+)/);

  if (!labelMatch) {
    console.warn(`⚠️ No 'label:' found in ${file}, using filename as fallback.`);
  }
  if (!valueMatch) {
    console.warn(`⚠️ No 'value:' found in ${file}, using filename as fallback.`);
  }

  const label = labelMatch?.[1]?.trim() ?? file.replace(".css", "");
  const value = valueMatch?.[1]?.trim() ?? file.replace(".css", "");

  const lightPrimaryMatch = content.match(/:root\[data-theme-preset="[^"]*"\][\s\S]*?--primary:\s*([^;]+);/);
  const darkPrimaryMatch = content.match(/\.dark:root\[data-theme-preset="[^"]*"\][\s\S]*?--primary:\s*([^;]+);/);

  const primary = {
    light: lightPrimaryMatch?.[1]?.trim() ?? "",
    dark: darkPrimaryMatch?.[1]?.trim() ?? "",
  };

  if (!lightPrimaryMatch || !darkPrimaryMatch) {
    console.warn(`⚠️ Missing --primary for ${file} (light or dark). Check CSS syntax.`);
  }

  return { label, value, primary };
});

const globalStylesPath = path.resolve(__dirname, "../app/globals.css");

let globalContent = "";
try {
  globalContent = fs.readFileSync(globalStylesPath, "utf8");
} catch (err) {
  console.error(`❌ Could not read globals.css at ${globalStylesPath}`);
  console.error(err);
  process.exit(1);
}

const defaultLightPrimaryRegex = /:root\s*{[^}]*--primary:\s*([^;]+);/;
const defaultDarkPrimaryRegex = /\.dark\s*{[^}]*--primary:\s*([^;]+);/;

const defaultLightPrimaryMatch = defaultLightPrimaryRegex.exec(globalContent);
const defaultDarkPrimaryMatch = defaultDarkPrimaryRegex.exec(globalContent);

const defaultPrimary = {
  light: defaultLightPrimaryMatch?.[1]?.trim() ?? "",
  dark: defaultDarkPrimaryMatch?.[1]?.trim() ?? "",
};

presets.unshift({ label: "기본", value: "default", primary: defaultPrimary });

const generatedBlock = `// --- generated:themePresets:start ---

export const THEME_PRESET_OPTIONS = ${JSON.stringify(presets, null, 2)} as const;

export const THEME_PRESET_VALUES = THEME_PRESET_OPTIONS.map((p) => p.value);

export type ThemePreset = (typeof THEME_PRESET_OPTIONS)[number]["value"];

// --- generated:themePresets:end ---`;

const fileContent = fs.readFileSync(outputPath, "utf8");

const updated = fileContent.replace(
  /\/\/ --- generated:themePresets:start ---[\s\S]*?\/\/ --- generated:themePresets:end ---/,
  generatedBlock,
);

function main() {
  const biomeBin = require.resolve("@biomejs/biome/bin/biome");
  const formatted = execFileSync(process.execPath, [biomeBin, "format", "--stdin-file-path", outputPath], {
    input: updated,
    encoding: "utf8",
  });

  if (formatted === fileContent) {
    console.log("ℹ️  No changes in theme.ts");
    return;
  }

  fs.writeFileSync(outputPath, formatted);
  console.log("✅ theme.ts updated with new theme presets");
}

try {
  main();
} catch (err) {
  console.error("❌ Unexpected error while generating theme presets:", err);
  process.exit(1);
}
