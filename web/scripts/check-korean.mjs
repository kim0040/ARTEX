import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath, pathToFileURL } from "node:url";

import * as ts from "typescript";

/**
 * 허용값은 규칙별 이유를 한 곳에서 관리한다. 이 목록은 번역을 건너뛰기
 * 위한 포괄적인 파일 제외 목록이 아니라, 코드에서 자주 쓰이는 값의
 * 의미를 기록한 것이다.
 */
export const ALLOWLIST_REASONS = Object.freeze({
  technicalToken: "영어 식별자·API 값·HTTP/JSON/LLM/MCP/Skill 기술 토큰",
  codeLiteral: "URL·경로·명령어·코드 파일명",
  brandName: "브랜드·서비스·제품 고유명",
  fontRegistry: "lib/fonts/registry.ts의 글꼴 고유명",
  userContent: "사용자 원문·증거·모델 프롬프트",
  mockOriginal: "lib/mock/data.ts의 목업 원문·증거",
  koreanLocale: "한국어 로케일(ko, ko-KR)",
});

const UI_ATTRIBUTE_NAMES = new Set(["aria-label", "title", "placeholder", "alt", "label", "text"]);
const CODE_TAGS = new Set(["code", "pre", "script", "style", "codeblock", "httpcodeblock", "syntaxhighlighter"]);
const ORIGINAL_CONTAINER_KEYS = new Set([
  "prompt",
  "system",
  "user",
  "user_message",
  "evidence",
  "summary",
  "report",
  "raw",
  "output",
  "input",
  "tool_input",
  "arguments",
  "request",
  "response",
  "original",
  "content",
]);

// 이 토큰들은 UI 번역 대상이 아니라 API/프로토콜/파일명/코드 표현으로 취급한다.
const TECHNICAL_TOKENS = new Set([
  "api",
  "apis",
  "appid",
  "cli",
  "code",
  "css",
  "csv",
  "dom",
  "dns",
  "ftp",
  "get",
  "ctrl",
  "enter",
  "esc",
  "html",
  "http",
  "https",
  "id",
  "ids",
  "ip",
  "ipv4",
  "ipv6",
  "json",
  "jwt",
  "llm",
  "markdown",
  "mcp",
  "npm",
  "npx",
  "patch",
  "pdf",
  "png",
  "post",
  "put",
  "react",
  "regex",
  "rpc",
  "sse",
  "sql",
  "ssh",
  "svg",
  "shift",
  "tab",
  "tcp",
  "tls",
  "token",
  "tokens",
  "tsx",
  "ts",
  "uri",
  "url",
  "uuid",
  "web",
  "xml",
  "yaml",
  "yml",
  "zip",
]);

// 실행 상태/모델 설정에서만 쓰이는 소문자 값이다. 대문자 UI 표기(예: Worker,
// Model)는 아래 자연어 검사에 남겨 두어 실제 버튼·표 제목을 찾게 한다.
const LOWERCASE_CODE_TOKENS = new Set([
  "agent",
  "assistant",
  "backend",
  "bash",
  "cache",
  "command",
  "debug",
  "error",
  "curl",
  "disabled",
  "enabled",
  "finding",
  "frontend",
  "info",
  "input",
  "mainagent",
  "model",
  "output",
  "planner",
  "profile",
  "read",
  "role",
  "script",
  "shell",
  "socks5",
  "state",
  "status",
  "stdio",
  "streaming",
  "system",
  "test",
  "thinking",
  "warn",
  "worker",
  "workers",
  "write",
  "mitm",
]);

const PROVIDER_TERMS = new Set([
  "api",
  "chat",
  "completion",
  "completions",
  "function",
  "functions",
  "input",
  "key",
  "messages",
  "output",
  "reasoning",
  "responses",
  "search",
  "streaming",
  "thinking",
  "tools",
  "work",
]);

const BRAND_NAMES = new Set([
  "acme",
  "anthropic",
  "artex",
  "bash",
  "brave",
  "cloudflare",
  "deepseek",
  "ddgs",
  "duckduckgo",
  "ffuf",
  "geist",
  "github",
  "google",
  "jenkins",
  "microsoft",
  "minimax",
  "nmap",
  "next",
  "nginx",
  "openai",
  "playwright",
  "python",
  "scopesentry",
  "simple-icons",
  "sqlmap",
  "tavily",
  "norma",
  "x",
]);

// 한 단어인 UI 문구도 잡기 위한 최소 어휘다. 긴 문장은 별도 목록 없이
// 자연어 토큰이 두 개 이상이면 위반으로 판단한다.
const COMMON_UI_WORDS = new Set([
  "agent",
  "app",
  "cancel",
  "centered",
  "close",
  "collapse",
  "compatibility",
  "content",
  "copy",
  "customize",
  "dashboard",
  "dark",
  "delete",
  "documents",
  "error",
  "found",
  "font",
  "fonts",
  "full",
  "icon",
  "key",
  "license",
  "light",
  "loading",
  "looking",
  "manage",
  "method",
  "model",
  "models",
  "more",
  "navbar",
  "no",
  "offcanvas",
  "open",
  "page",
  "preferences",
  "preset",
  "profile",
  "read",
  "results",
  "save",
  "scroll",
  "search",
  "select",
  "send",
  "session",
  "settings",
  "share",
  "sidebar",
  "soon",
  "something",
  "sticky",
  "style",
  "system",
  "theme",
  "title",
  "toggle",
  "view",
  "width",
  "worker",
  "workers",
  "write",
  "your",
]);

const LOCALE_RE = /^(?:en|en[-_]us|en[-_]gb|zh|zh[-_]cn|zh[-_]tw|zh[-_]hans|zh[-_]hant)$/i;
const HAN_RE = /[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]/u;
const KOREAN_RE = /[\uac00-\ud7a3]/u;
const LATIN_WORD_RE = /[A-Za-z][A-Za-z'-]*/g;
const SOURCE_EXTENSIONS = new Set([".ts", ".tsx", ".js", ".jsx"]);
const FONT_NAME_RE = /^(?:Geist|Inter|Noto|Sans|Nunito|Figtree|Roboto|Raleway|DM|Public|Outfit|Mono|Pixel|JetBrains|Merriweather|Lora|Playfair|Slab)(?:\b|\s)/i;

/** @typedef {"english-ui" | "han-ui" | "locale" | "syntax"} ViolationKind */

/**
 * @typedef {Object} Violation
 * @property {string} path
 * @property {number} line
 * @property {number} column
 * @property {ViolationKind} kind
 * @property {string} value
 * @property {string} message
 */

function normalizePath(filePath) {
  return String(filePath).replaceAll("\\", "/");
}

function isFontRegistryPath(filePath) {
  return /(?:^|\/)lib\/fonts\/registry\.ts$/u.test(normalizePath(filePath));
}

function isMockDataPath(filePath) {
  return /(?:^|\/)lib\/mock\/data\.(?:ts|tsx)$/u.test(normalizePath(filePath));
}

function getNodeText(sourceFile, node) {
  return node.getText(sourceFile);
}

function getPropertyName(sourceFile, name) {
  if (ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name)) {
    return getNodeText(sourceFile, name).replace(/^['"]|['"]$/gu, "");
  }
  return "";
}

function getTagName(sourceFile, tagName) {
  return getNodeText(sourceFile, tagName).toLowerCase();
}

function trimUiText(value) {
  return value.replace(/\s+/gu, " ").trim();
}

function isLocaleLiteral(value) {
  return LOCALE_RE.test(value.trim());
}

function isLanguageAttribute(name) {
  return name.toLowerCase() === "lang";
}

function isAllCapsToken(token) {
  return token.length > 1 && token === token.toUpperCase() && /[A-Z]/u.test(token);
}

function isTechnicalToken(token) {
  const lower = token.toLowerCase();
  if (BRAND_NAMES.has(lower)) {
    return true;
  }
  if (LOWERCASE_CODE_TOKENS.has(lower)) {
    return token === lower || isAllCapsToken(token);
  }
  if (!TECHNICAL_TOKENS.has(lower)) {
    return false;
  }
  // 명시된 프로토콜 토큰은 대소문자 표기와 관계없이 코드값으로 허용한다.
  return true;
}

function isCodeLiteral(value) {
  const text = trimUiText(value);
  if (!text) {
    return true;
  }
  if (/^(?:https?|ftp|socks5):\/\//iu.test(text) || /^mailto:/iu.test(text)) {
    return true;
  }
  // 예시 도메인/주소가 한국어 설명 안에 섞인 경우도 원문 코드값으로 본다.
  if (/(?:https?|ftp|socks5):\/\/\S+|\b(?:[a-z0-9-]+\.)+(?:com|net|org|io|dev|cn|kr)(?:\/\S*)?/iu.test(text)) {
    return true;
  }
  if (/(?:^|\s)\/[A-Za-z0-9_-]+/u.test(text) || /(?:^|\s)[~./][^\s)]+/u.test(text)) {
    return true;
  }
  if (/\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_-]+\b/u.test(text)) {
    return true;
  }
  if (/\b(?:allow|deny)\/(?:allow|deny)\b/iu.test(text)) {
    return true;
  }
  if (/\b(?:X|[A-Z]{2,})-[A-Za-z0-9_-]+\b/u.test(text) || /\b[A-Z][A-Z0-9_]{2,}\b/u.test(text)) {
    return true;
  }
  if (/\b(?:[A-Za-z]{2,}|api|sk|pk)[-_…][A-Za-z0-9…-]*\b/iu.test(text)) {
    return true;
  }
  if (/\bv?\d+(?:\.\d+)+\b/u.test(text)) {
    return true;
  }
  if (/^(?:[~./]|[A-Za-z]:[\\/])[^\s]*$/u.test(text)) {
    return true;
  }
  if (/^(?:[A-Za-z0-9_$-]+\.)+[A-Za-z0-9_$-]+$/u.test(text)) {
    return true;
  }
  if (/(?:^|\s)(?:npm|npx|pnpm|yarn|docker|curl|git|go|python(?:3)?|pytest|playwright|sqlmap|rm|bash|sh|zsh|nmap|ffuf)\b/iu.test(text)) {
    return /(?:--?[A-Za-z]|\/|\\|\||&&|\$\{|\binstall\b|\brun\b|\btest\b|\b-f\b)/u.test(text);
  }
  if (/\b[A-Za-z][A-Za-z0-9_-]*\s*(?:>=|<=|!=|=)\s*[^\s,)]/u.test(text)) {
    return true;
  }
  if (/^[-@A-Za-z0-9_./]+\s+[-@A-Za-z0-9_./]+/u.test(text) && /[/=@]/u.test(text)) {
    return true;
  }
  if (/^[{[]|[}\]]$/u.test(text) || /[{}[\]]/.test(text) && /[":=]/u.test(text)) {
    return true;
  }
  if (/\b[A-Za-z0-9_-]+\.(?:md|json|ya?ml|tsx?|jsx?|py|sh|sql|env|woff2?|png|svg)\b/iu.test(text)) {
    return true;
  }
  // Tailwind 클래스/데이터 속성처럼 공백으로 이어진 코드 토큰은 자연어가 아니다.
  if (/^(?:[A-Za-z0-9_:[\]#./-]+\s+)+[A-Za-z0-9_:[\]#./-]+$/u.test(text) && /[-:]/u.test(text)) {
    return true;
  }
  if (/^[A-Za-z_$][A-Za-z0-9_$]*$/u.test(text) && /[_$]|[a-z][A-Z]|\d/u.test(text)) {
    return true;
  }
  return false;
}

function isLikelyFontName(value) {
  return FONT_NAME_RE.test(trimUiText(value));
}

function hasMeaningfulEnglish(value) {
  const tokens = value.match(LATIN_WORD_RE) ?? [];
  if (tokens.length === 0) {
    return false;
  }
  const naturalTokens = tokens.filter((token) => {
    const lower = token.toLowerCase();
    if (isTechnicalToken(token) || BRAND_NAMES.has(lower)) {
      return false;
    }
    // 한 글자 약어와 제품의 X 링크는 자연어로 세지 않는다.
    return token.length > 1 && !isAllCapsToken(token);
  });
  // 알려진 제품명이 포함된 괄호 설명은 provider/API의 고유 표기다.
  if (tokens.some((token) => BRAND_NAMES.has(token.toLowerCase()))) {
    const remaining = tokens.filter((token) => {
      const lower = token.toLowerCase();
      return !BRAND_NAMES.has(lower) && !isTechnicalToken(token) && !PROVIDER_TERMS.has(lower);
    });
    if (remaining.length === 0) {
      return false;
    }
  }
  if (tokens.some((token) => isTechnicalToken(token))) {
    const remaining = tokens.filter((token) => {
      const lower = token.toLowerCase();
      return !isTechnicalToken(token) && !PROVIDER_TERMS.has(lower);
    });
    if (remaining.length === 0) {
      return false;
    }
  }
  if (naturalTokens.length >= 2) {
    return true;
  }
  // 한국어 설명 안에 단독으로 섞인 Agent/API 같은 용어는 자연어 문장으로
  // 오인하지 않는다. 순수 영어 버튼/제목의 한 단어는 계속 검사한다.
  return naturalTokens.length === 1 && !KOREAN_RE.test(value) && COMMON_UI_WORDS.has(naturalTokens[0].toLowerCase());
}

function addViolation(violations, seen, sourceFile, node, kind, value, message) {
  const start = node.getStart(sourceFile);
  const key = `${start}:${kind}:${value}`;
  if (seen.has(key)) {
    return;
  }
  seen.add(key);
  const position = sourceFile.getLineAndCharacterOfPosition(start);
  violations.push({
    path: sourceFile.fileName,
    line: position.line + 1,
    column: position.character + 1,
    kind,
    value,
    message,
  });
}

function checkStringValue(sourceFile, node, rawValue, state, violations, seen) {
  const value = trimUiText(rawValue);
  if (!value || state.inCode) {
    return;
  }

  // 로케일 잔여값은 UI 문구가 아니어도 검사한다. ko/ko-KR은 허용 목록의 값이다.
  if (isLocaleLiteral(value)) {
    addViolation(violations, seen, sourceFile, node, "locale", value, "한국어 UI에 남은 영어/중국어 로케일");
    return;
  }

  if (!state.surface || state.inOriginal) {
    return;
  }

  // 목업 데이터의 원문·증거는 번역 대상인 화면 문구로 간주하지 않는다.
  if ((isMockDataPath(sourceFile.fileName) || state.inMockData) && !state.jsxSurface) {
    return;
  }

  // fonts/registry.ts의 label은 글꼴 고유명이다. 다른 UI 파일까지 제외하지 않는다.
  if (isFontRegistryPath(sourceFile.fileName) && (state.propertyName === "label" || isLikelyFontName(value))) {
    return;
  }

  if (isCodeLiteral(value)) {
    return;
  }

  if (HAN_RE.test(value)) {
    addViolation(violations, seen, sourceFile, node, "han-ui", value, "사용자 UI에 한자/중국어 문자가 남아 있음");
    return;
  }

  if (hasMeaningfulEnglish(value)) {
    addViolation(violations, seen, sourceFile, node, "english-ui", value, "사용자 UI의 영어 자연어 문구");
  }
}

function visitTemplateExpression(sourceFile, node, state, violations, seen) {
  checkStringValue(sourceFile, node, node.head.text, state, violations, seen);
  for (const span of node.templateSpans) {
    checkStringValue(sourceFile, span.literal, span.literal.text, state, violations, seen);
    visitNode(sourceFile, span.expression, state, violations, seen);
  }
}

function visitExpression(sourceFile, node, state, violations, seen) {
  if (!node) {
    return;
  }
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
    checkStringValue(sourceFile, node, node.text, state, violations, seen);
    return;
  }
  if (ts.isTemplateExpression(node)) {
    visitTemplateExpression(sourceFile, node, state, violations, seen);
    return;
  }
  visitNode(sourceFile, node, state, violations, seen);
}

function visitJsxAttribute(sourceFile, node, state, violations, seen) {
  const attributeName = getNodeText(sourceFile, node.name).toLowerCase();
  if (!node.initializer) {
    return;
  }
  const attributeState = {
    ...state,
    surface: UI_ATTRIBUTE_NAMES.has(attributeName) || isLanguageAttribute(attributeName),
    jsxSurface: true,
    propertyName: attributeName,
  };
  if (ts.isStringLiteral(node.initializer)) {
    checkStringValue(sourceFile, node.initializer, node.initializer.text, attributeState, violations, seen);
    return;
  }
  if (ts.isJsxExpression(node.initializer)) {
    visitExpression(sourceFile, node.initializer.expression, attributeState, violations, seen);
  }
}

function visitJsxOpeningElement(sourceFile, node, state, violations, seen) {
  const tagName = getTagName(sourceFile, node.tagName);
  const elementState = {
    ...state,
    inCode: state.inCode || CODE_TAGS.has(tagName),
  };
  for (const attribute of node.attributes.properties) {
    if (ts.isJsxAttribute(attribute)) {
      visitJsxAttribute(sourceFile, attribute, elementState, violations, seen);
    }
  }
}

function visitJsxElement(sourceFile, node, state, violations, seen) {
  const tagName = getTagName(sourceFile, node.openingElement.tagName);
  const elementState = {
    ...state,
    inCode: state.inCode || CODE_TAGS.has(tagName),
  };
  for (const attribute of node.openingElement.attributes.properties) {
    if (ts.isJsxAttribute(attribute)) {
      visitJsxAttribute(sourceFile, attribute, elementState, violations, seen);
    }
  }
  for (const child of node.children) {
    visitNode(sourceFile, child, { ...elementState, surface: true }, violations, seen);
  }
}

function isMockVariableName(sourceFile, name) {
  const text = getNodeText(sourceFile, name);
  return /^(?:mock|fixture)(?:_|$)/iu.test(text) || /_mock(?:_|$)/iu.test(text);
}

function isCodeContextString(node) {
  const parent = node.parent;
  if (!parent) {
    return false;
  }
  if (ts.isElementAccessExpression(parent) && parent.argumentExpression === node) {
    return true;
  }
  if (ts.isCallExpression(parent) && parent.arguments.some((argument) => argument === node)) {
    return true;
  }
  if (ts.isNewExpression(parent) && parent.arguments?.some((argument) => argument === node)) {
    return true;
  }
  if (ts.isBinaryExpression(parent)) {
    const operator = parent.operatorToken.kind;
    if (
      operator === ts.SyntaxKind.EqualsEqualsToken ||
      operator === ts.SyntaxKind.EqualsEqualsEqualsToken ||
      operator === ts.SyntaxKind.ExclamationEqualsToken ||
      operator === ts.SyntaxKind.ExclamationEqualsEqualsToken ||
      operator === ts.SyntaxKind.LessThanToken ||
      operator === ts.SyntaxKind.GreaterThanToken ||
      operator === ts.SyntaxKind.LessThanEqualsToken ||
      operator === ts.SyntaxKind.GreaterThanEqualsToken
    ) {
      // status === "loading" 같은 런타임 코드값이다.
      return true;
    }
  }
  if (ts.isCaseClause(parent) || ts.isDefaultClause(parent)) {
    return true;
  }
  return false;
}

function visitPropertyAssignment(sourceFile, node, state, violations, seen) {
  const propertyName = getPropertyName(sourceFile, node.name).toLowerCase();
  const isOriginal = state.inOriginal || ORIGINAL_CONTAINER_KEYS.has(propertyName);
  const propertyState = {
    ...state,
    inOriginal: isOriginal,
    surface: state.surface || propertyName === "label" || propertyName === "text",
    propertyName,
  };
  visitExpression(sourceFile, node.initializer, propertyState, violations, seen);
}

function visitNode(sourceFile, node, state, violations, seen) {
  if (!node) {
    return;
  }
  if (ts.isJsxText(node)) {
    checkStringValue(sourceFile, node, node.getText(sourceFile), { ...state, surface: true, jsxSurface: true }, violations, seen);
    return;
  }
  if (ts.isJsxElement(node)) {
    visitJsxElement(sourceFile, node, state, violations, seen);
    return;
  }
  if (ts.isJsxSelfClosingElement(node)) {
    const tagName = getTagName(sourceFile, node.tagName);
    const elementState = { ...state, inCode: state.inCode || CODE_TAGS.has(tagName) };
    for (const attribute of node.attributes.properties) {
      if (ts.isJsxAttribute(attribute)) {
        visitJsxAttribute(sourceFile, attribute, elementState, violations, seen);
      }
    }
    return;
  }
  if (ts.isJsxExpression(node)) {
    visitExpression(sourceFile, node.expression, state, violations, seen);
    return;
  }
  if (ts.isPropertyAssignment(node)) {
    visitPropertyAssignment(sourceFile, node, state, violations, seen);
    return;
  }
  if (ts.isVariableDeclaration(node)) {
    const variableState = {
      ...state,
      inMockData: state.inMockData || isMockVariableName(sourceFile, node.name),
    };
    visitNode(sourceFile, node.initializer, variableState, violations, seen);
    return;
  }
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
    // lang/en-US처럼 로케일은 사용자 표시 여부와 관계없이 점검한다.
    const stringState = isCodeContextString(node) ? { ...state, surface: false } : state;
    checkStringValue(sourceFile, node, node.text, stringState, violations, seen);
    return;
  }
  if (ts.isTemplateExpression(node)) {
    visitTemplateExpression(sourceFile, node, state, violations, seen);
    return;
  }
  ts.forEachChild(node, (child) => visitNode(sourceFile, child, state, violations, seen));
}

/**
 * 하나의 TypeScript/TSX 소스를 검사한다.
 * @param {string} text
 * @param {string} filePath
 * @returns {Violation[]}
 */
export function scanSource(text, filePath = "inline.tsx") {
  const normalizedPath = normalizePath(filePath);
  const scriptKind = /\.(?:tsx|jsx)$/iu.test(normalizedPath) ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const sourceFile = ts.createSourceFile(normalizedPath, text, ts.ScriptTarget.Latest, true, scriptKind);
  const violations = [];
  const seen = new Set();

  for (const diagnostic of sourceFile.parseDiagnostics ?? []) {
    const start = diagnostic.start ?? 0;
    const node = start < sourceFile.end ? sourceFile : sourceFile;
    const message = ts.flattenDiagnosticMessageText(diagnostic.messageText, " ");
    addViolation(violations, seen, sourceFile, node, "syntax", message, "TypeScript 구문 오류");
  }

  visitNode(
    sourceFile,
    sourceFile,
    { inCode: false, inOriginal: false, inMockData: false, inJsxExpression: false, surface: false, jsxSurface: false, propertyName: "" },
    violations,
    seen,
  );
  return violations.sort((left, right) => left.line - right.line || left.column - right.column || left.kind.localeCompare(right.kind));
}

function collectSourceFiles(rootPath) {
  const files = [];
  const stack = [rootPath];
  while (stack.length > 0) {
    const currentPath = stack.pop();
    const stat = fs.statSync(currentPath);
    if (stat.isDirectory()) {
      for (const entry of fs.readdirSync(currentPath, { withFileTypes: true })) {
        if (entry.name === "node_modules" || entry.name === ".next" || entry.name === "out") {
          continue;
        }
        stack.push(path.join(currentPath, entry.name));
      }
      continue;
    }
    if (SOURCE_EXTENSIONS.has(path.extname(currentPath).toLowerCase())) {
      files.push(currentPath);
    }
  }
  return files.sort();
}

function resolveScanRoots(argumentsList) {
  if (argumentsList.length > 0) {
    return argumentsList.map((entry) => path.resolve(process.cwd(), entry));
  }
  return [path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../src")];
}

/**
 * CLI 진입점. 지정한 파일/디렉터리 또는 기본 web/src를 검사한다.
 */
export function main(argumentsList = process.argv.slice(2)) {
  const roots = resolveScanRoots(argumentsList);
  const files = roots.flatMap((root) => {
    if (!fs.existsSync(root)) {
      console.error(`검사 대상이 없습니다: ${root}`);
      return [];
    }
    return collectSourceFiles(root);
  });
  const violations = [];
  for (const filePath of files) {
    const source = fs.readFileSync(filePath, "utf8");
    violations.push(...scanSource(source, filePath));
  }
  violations.sort((left, right) => left.path.localeCompare(right.path) || left.line - right.line || left.column - right.column);

  for (const violation of violations) {
    console.error(`${violation.path}:${violation.line}:${violation.column} [${violation.kind}] ${violation.message}: ${JSON.stringify(violation.value)}`);
  }
  if (violations.length > 0) {
    console.error(`한국어 UI 검사 실패: ${violations.length}건`);
    process.exitCode = 1;
  } else {
    console.log(`한국어 UI 검사 통과: ${files.length}개 파일`);
  }
  return violations;
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (invokedPath === import.meta.url) {
  main();
}
