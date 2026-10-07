import test from "node:test";
import assert from "node:assert/strict";

import { ALLOWLIST_REASONS, scanSource } from "./check-korean.mjs";

function values(violations, kind) {
  return violations.filter((violation) => !kind || violation.kind === kind).map((violation) => violation.value);
}

test("JSX 고정 문구와 속성, label/text 표현식의 영어를 찾는다", () => {
  const source = `
    const choices = [{ label: condition ? "Open" : "Close", text: "Save" }];
    export function Fixture({ condition }) {
      return (
        <section>
          <button aria-label="Open settings" title={condition ? "Share" : "Delete"}>Documents</button>
          <input placeholder={"Search users"} alt={\`Profile image\`} />
        </section>
      );
    }
  `;

  const violations = scanSource(source, "fixture.tsx");
  const english = values(violations, "english-ui");
  for (const expected of ["Open", "Close", "Save", "Open settings", "Share", "Delete", "Documents", "Search users", "Profile image"]) {
    assert.ok(english.includes(expected), `영어 UI 문구가 누락됨: ${expected}`);
  }
  assert.ok(english.every((value) => !value.includes("condition")), "식별자는 UI 문구로 보고하지 않아야 함");
});

test("영어/중국어 로케일과 JSX 한자를 찾고 한국어는 통과시킨다", () => {
  const source = `
    <html lang="en">
      <body><p>中文说明</p><p>정상 한국어</p></body>
    </html>;
    const locale = "zh-CN";
    new Intl.DateTimeFormat("en-US");
    date.toLocaleString("en-US");
  `;

  const violations = scanSource(source, "locale.tsx");
  const localeValues = values(violations, "locale");
  assert.ok(localeValues.includes("en"), "html lang=en을 찾아야 함");
  assert.equal(localeValues.filter((value) => value === "zh-CN").length, 1);
  assert.equal(localeValues.filter((value) => value === "en-US").length, 2);
  assert.deepEqual(values(violations, "han-ui"), ["中文说明"]);
  assert.equal(values(violations, "english-ui").length, 0, "한국어 문구는 영어 위반이 아님");
});

test("code/pre, 기술 토큰, URL, 브랜드, 사용자 원문은 허용한다", () => {
  const source = `
    export function Fixture() {
      return <>
        <div aria-label="메뉴">한국어</div>
        <pre>Delete file --force</pre>
        <code>{"https://example.com/HTTP.json"}</code>
        <p>HTTP JSON LLM MCP Skill SKILL.md</p>
        <p>OpenAI API</p>
      </>;
    }
  `;
  const mockSource = `
    export const finding = {
      prompt: "Delete this original prompt",
      evidence: "中文 evidence from the user",
      text: "Prompt from a mock response",
    };
  `;
  const fontSource = `export const fontRegistry = { sans: { label: "Geist Mono" } };`;

  assert.equal(scanSource(source, "exceptions.tsx").length, 0);
  assert.equal(scanSource(mockSource, "src/lib/mock/data.ts").length, 0);
  assert.equal(scanSource(fontSource, "src/lib/fonts/registry.ts").length, 0);
  assert.equal(ALLOWLIST_REASONS.mockOriginal, "lib/mock/data.ts의 목업 원문·증거");
  assert.equal(ALLOWLIST_REASONS.fontRegistry, "lib/fonts/registry.ts의 글꼴 고유명");
});

test("사용자 표시 속성의 위치와 종류를 반환한다", () => {
  const violations = scanSource(`<input aria-label="Search" />`, "line-fixture.tsx");
  assert.equal(violations.length, 1);
  assert.deepEqual(violations[0], {
    path: "line-fixture.tsx",
    line: 1,
    column: 19,
    kind: "english-ui",
    value: "Search",
    message: "사용자 UI의 영어 자연어 문구",
  });
});
