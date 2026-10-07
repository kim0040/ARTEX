"use client";

import * as React from "react";

import { SearchIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// ── DSL 자동완성 ──────────────────────────────────────────────────────────
// 전역 자산 보기(/function/assets)와 작업별 테스트 자산이 함께 씀
// 검색이라, 두 검색창이 똑같이 동작하고 똑같이 보입니다.

const DSL_FIELDS: { name: string; desc: string; ops: { op: string; desc: string }[] }[] = [
  {
    name: "domain",
    desc: "도메인(루트 도메인/서브도메인/서비스 도메인)",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "ip",
    desc: "IPv4/IPv6 주소",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "url",
    desc: "전체 URL(서비스/인터페이스)",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "root_domain",
    desc: "루트 도메인",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "page_title",
    desc: "페이지 제목(HTTP 서비스)",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "icp",
    desc: "ICP 등록 번호",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "service_name",
    desc: "서비스 이름(HTTP가 아닌 서비스)",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "app_name",
    desc: "앱 이름",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "bundle_id",
    desc: "애플리케이션 Bundle ID",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "category",
    desc: "애플리케이션 분류",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "app_icp",
    desc: "애플리케이션 ICP 등록",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "method",
    desc: "HTTP 메서드 GET/POST/PUT/…",
    ops: [
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "service_type",
    desc: "서비스 유형: http | other",
    ops: [
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "record_type",
    desc: "DNS 조회 유형 A/CNAME/MX/…",
    ops: [
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "technology",
    desc: "기술 핑거프린트(배열 필드)",
    ops: [
      { op: "=", desc: "유사 일치" },
      { op: "==", desc: "정확히 일치" },
      { op: "!=", desc: "제외" },
    ],
  },
  {
    name: "port",
    desc: "포트 번호(정수)",
    ops: [
      { op: "==", desc: "같음" },
      { op: "!=", desc: "같지 않음" },
      { op: ">", desc: "보다 큼" },
      { op: ">=", desc: "보다 크거나 같음" },
      { op: "<", desc: "보다 작음" },
      { op: "<=", desc: "보다 작거나 같음" },
    ],
  },
  {
    name: "status_code",
    desc: "HTTP 상태 코드(정수)",
    ops: [
      { op: "==", desc: "같음" },
      { op: "!=", desc: "같지 않음" },
      { op: ">", desc: "보다 큼" },
      { op: ">=", desc: "보다 크거나 같음" },
      { op: "<", desc: "보다 작음" },
      { op: "<=", desc: "보다 작거나 같음" },
    ],
  },
  { name: "company_id", desc: "소속 기업 ID(정수)", ops: [{ op: "==", desc: "같음" }] },
  { name: "task_id", desc: "출처 작업 ID(정수)", ops: [{ op: "==", desc: "같음" }] },
];

const LOGIC_OPS = [
  { label: "AND", desc: "그리고(두 조건 모두 충족)" },
  { label: "OR", desc: "또는(하나만 충족)" },
];

interface DslSuggestion {
  kind: "field" | "operator" | "logic";
  label: string;
  desc: string;
  replaceStart: number;
  replaceEnd: number;
  insertText: string;
}

function getDslSuggestions(text: string, cursor: number): DslSuggestion[] {
  const before = text.slice(0, cursor);
  // 지금 토큰: 커서에서 끝나는, 공백과 괄호가 아닌 구간
  const tokenMatch = before.match(/([^\s()]*$)/);
  const currentToken = tokenMatch?.[1] ?? "";
  const tokenStart = cursor - currentToken.length;

  // 토큰에 필드와 연산자가 이미 있으면 값을 치는 중이니 제안하지 않습니다
  if (/^[a-z_]+(==|!=|>=|<=|=|>|<)/.test(currentToken)) return [];

  // 아는 필드 이름이 완성되면 그 필드의 연산자를 제안합니다
  const exactField = DSL_FIELDS.find((f) => f.name === currentToken.toLowerCase());
  if (exactField) {
    return exactField.ops.map(({ op, desc }) => ({
      kind: "operator",
      label: `${exactField.name}${op}`,
      desc,
      replaceStart: tokenStart,
      replaceEnd: cursor,
      insertText: `${exactField.name}${op}`,
    }));
  }

  // 지금 토큰 앞의 모든 것(앞뒤 공백 제거)
  const beforeToken = before.slice(0, tokenStart).trimEnd();
  const afterExpression = beforeToken.length > 0 && !/\b(AND|OR)\s*$/i.test(beforeToken) && !beforeToken.endsWith("(");

  // 지금 토큰이 AND/OR의 앞부분이고, 앞에 완성된 식이 있습니다
  if (/^(a|an|and|o|or)$/i.test(currentToken) && afterExpression) {
    return LOGIC_OPS.filter((l) => l.label.startsWith(currentToken.toUpperCase())).map(({ label, desc }) => ({
      kind: "logic",
      label,
      desc,
      replaceStart: tokenStart,
      replaceEnd: cursor,
      insertText: `${label} `,
    }));
  }

  // 지금 토큰이 없고 완성된 식 다음이면 AND/OR를 제안합니다
  if (!currentToken && afterExpression) {
    return LOGIC_OPS.map(({ label, desc }) => ({
      kind: "logic",
      label,
      desc,
      replaceStart: cursor,
      replaceEnd: cursor,
      insertText: `${label} `,
    }));
  }

  // 기본: 앞글자로 걸러 필드를 제안합니다
  const prefix = currentToken.toLowerCase();
  return DSL_FIELDS.filter((f) => f.name.startsWith(prefix)).map((f) => ({
    kind: "field",
    label: f.name,
    desc: f.desc,
    replaceStart: tokenStart,
    replaceEnd: cursor,
    insertText: f.name,
  }));
}

function applyDslSuggestion(text: string, s: DslSuggestion): { text: string; cursor: number } {
  const newText = text.slice(0, s.replaceStart) + s.insertText + text.slice(s.replaceEnd);
  return { text: newText, cursor: s.replaceStart + s.insertText.length };
}

const KIND_STYLE: Record<string, string> = {
  field: "text-blue-500 dark:text-blue-400",
  operator: "text-amber-500 dark:text-amber-400",
  logic: "text-emerald-500 dark:text-emerald-400",
};

// AssetDslSearch는 같이 쓰는 DSL 검색창입니다. 고정폭 입력과
// field/operator/logic 자동완성 popover와 상태 줄("N건 찾음" /
// 오류나 불러오는 중을 보여 줍니다. 전체 자산 화면과 작업별 화면이 같이 씁니다.
export function AssetDslSearch({
  query,
  onChange,
  loading,
  error,
  count,
}: {
  query: string;
  onChange: (v: string) => void;
  loading: boolean;
  error: string;
  count?: number;
}) {
  const inputRef = React.useRef<HTMLInputElement>(null);
  const [suggestions, setSuggestions] = React.useState<DslSuggestion[]>([]);
  const [selIdx, setSelIdx] = React.useState(0);
  const [open, setOpen] = React.useState(false);

  const refresh = React.useCallback((val: string, pos: number) => {
    const suggs = getDslSuggestions(val, pos);
    setSuggestions(suggs);
    setSelIdx(0);
    setOpen(suggs.length > 0);
  }, []);

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const val = e.target.value;
    onChange(val);
    refresh(val, e.target.selectionStart ?? val.length);
  };

  const apply = React.useCallback(
    (s: DslSuggestion) => {
      const cursor = inputRef.current?.selectionStart ?? query.length;
      // logic 종류는 커서 위치에 넣고, 나머지는 replaceStart/End 구간을 바꿉니다
      const adjusted: DslSuggestion =
        s.kind === "logic" && !query.slice(s.replaceStart, s.replaceEnd)
          ? { ...s, replaceStart: cursor, replaceEnd: cursor }
          : s;
      const { text: newText, cursor: newCursor } = applyDslSuggestion(query, adjusted);
      onChange(newText);
      requestAnimationFrame(() => {
        if (!inputRef.current) return;
        inputRef.current.setSelectionRange(newCursor, newCursor);
        inputRef.current.focus();
        refresh(newText, newCursor);
      });
    },
    [query, onChange, refresh],
  );

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open || suggestions.length === 0) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSelIdx((i) => Math.min(i + 1, suggestions.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSelIdx((i) => Math.max(i - 1, 0));
    } else if (e.key === "Tab" || e.key === "Enter") {
      const s = suggestions[selIdx];
      if (s) {
        e.preventDefault();
        apply(s);
      }
    } else if (e.key === "Escape") {
      setOpen(false);
    }
  };

  const cursorPos = () => inputRef.current?.selectionStart ?? query.length;

  return (
    <div className="flex flex-col gap-1">
      <div className="relative max-w-lg">
        <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          ref={inputRef}
          placeholder="DSL 검색: domain=example AND status_code>=400"
          value={query}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onFocus={() => refresh(query, cursorPos())}
          onClick={() => refresh(query, cursorPos())}
          onBlur={() => setTimeout(() => setOpen(false), 120)}
          className="h-8 pl-8 font-mono text-xs"
        />
        {open && suggestions.length > 0 && (
          <div className="absolute top-full left-0 z-50 mt-1 w-max min-w-full max-w-sm rounded-md border bg-popover py-1 shadow-md">
            {suggestions.map((s, i) => (
              <button
                type="button"
                key={i}
                className={cn(
                  "flex w-full cursor-pointer items-center gap-3 px-3 py-1.5 text-left",
                  i === selIdx ? "bg-accent" : "hover:bg-accent/50",
                )}
                onMouseEnter={() => setSelIdx(i)}
                onMouseDown={(e) => {
                  e.preventDefault();
                  apply(s);
                }}
              >
                <span className={cn("shrink-0 font-mono text-xs font-semibold", KIND_STYLE[s.kind])}>{s.label}</span>
                <span className="text-xs text-muted-foreground">{s.desc}</span>
              </button>
            ))}
          </div>
        )}
      </div>
      {query.trim() && !open && (
        <p className="pl-1 text-[11px] text-muted-foreground">
          {loading ? "검색 중…" : error ? <span className="text-destructive">{error}</span> : `찾음 ${count ?? 0} 건`}
        </p>
      )}
    </div>
  );
}
