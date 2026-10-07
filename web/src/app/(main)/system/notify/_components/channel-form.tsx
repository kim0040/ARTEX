"use client";

import { CheckIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType은 이 파일 안의 값 보조입니다(컨트롤 렌더와 강하게 묶임). channel-fields에 두지 않습니다.
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText는 아무 설정 값이나 입력 칸에 넣을 수 있는 문자열로 그립니다.
// config는 JSON에서 옵니다. 값은 string / number / boolean / array / null일 수 있고,
// 여기서는 「텍스트 칸에 넣을 수 있는지」만 봅니다. 구체적 직렬화는 buildConfig가 맡습니다.
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType은 필드 유형을 input의 type 속성에 대응시킵니다.
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField는 필드 정의에 맞춰 해당 컨트롤을 그립니다.
//
// 가림 필드 처리가 여기서 유일한 신경 쓸 점입니다. 입력 칸은 가림 값 자체를 **보여 주지 않고**, 한 줄만 보여 줍니다
// 「저장됨」 안내. 이렇게 하면 화면의 규칙은 하나입니다. 칸에 글자가 있으면 사용자가 적은 것이고,
// 빈 칸은 빈 값입니다. "__masked__:…abc123"을 입력 칸에 넣으면, 사용자는 그것을 직접
// 지워 버린 자리표시 글. 오히려 자격을 잘못 지우기 쉽습니다.
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // 백엔드가 다시 보여 주는 가림 값: "__masked__:…abc123" 형태. 끝은 원래 값의 알아볼 수 있는 조각입니다.
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={def.label} />
        {def.label}
        {def.help && <span className="text-muted-foreground">（{def.help}）</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{def.label}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // 컨트롤은 필드 유형별로 나눕니다. 중첩 삼항 대신 if 사슬을 쓰는 이유: 여기서 컨트롤 네 가지를 구분해야 하고,
  // 세 층 삼항을 읽으려면 이미 멈춰 괄호를 세야 합니다.
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      저장됨{maskedTail ? `(끝자리 ${maskedTail}）` : ""} · 새 값을 넣으면 덮어쓰고, 비우면 그 항목을 삭제
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{def.help}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{def.label}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary는 필터 조건을 한 줄로 줄여, 카드를 펼치지 않아도 이 채널이 무엇을 푸시하는지 알게 합니다.
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const parts: string[] = [];
  if (filter.min_severity) {
    parts.push(SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity)?.label ?? filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(`유형 포함 ${filter.vulnclass_include.length} 개 단어`);
  if (filter.vulnclass_exclude?.length) parts.push(`제외 ${filter.vulnclass_exclude.length} 개 단어`);
  if (filter.task_ids?.length) parts.push(`${filter.task_ids.length} 개 작업`);
  if (filter.asset_ids?.length) parts.push(`${filter.asset_ids.length} 개 자산`);
  if (filter.on_status_change) parts.push("상태 변경 포함");
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">모든 발견</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
