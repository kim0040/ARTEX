"use client";

import * as React from "react";

import { PlusIcon, Trash2Icon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import type { AssetInterceptKind, AssetInterceptRuleInput } from "@/lib/types";

// NativeSelect(네이티브 <select>)를 shadcn Select 대신 씀: 이 편집기는 Sheet 서랍 안에서 쓰이고,
// shadcn Select의 드롭다운은 body로 portal 되고, 바깥 클릭이 서랍의 「바깥 클릭으로 닫기」를 잘못 켭니다. 네이티브 드롭다운에는 이 문제가 없습니다.
export const ASSET_INTERCEPT_KIND_OPTIONS: {
  value: AssetInterceptKind;
  label: string;
  placeholder: string;
}[] = [
  { value: "exact_domain", label: "도메인(완전 일치)", placeholder: "example.gov.cn" },
  { value: "exact_ip", label: "IP(완전 일치)", placeholder: "203.0.113.10" },
  { value: "exact_url", label: "URL(완전 일치)", placeholder: "https://example.com/login" },
  { value: "fuzzy_domain", label: "도메인(부분 일치)", placeholder: ".gov.cn" },
  { value: "fuzzy_ip", label: "IP(부분 일치)", placeholder: "203.0.113." },
  { value: "fuzzy_url", label: "URL(부분 일치)", placeholder: "/admin" },
  { value: "cidr", label: "CIDR 대역", placeholder: "192.168.0.0/16" },
];

// AssetInterceptRulesEditor는 「가로채기/허용 규칙」의 제어되는 여러 줄 편집 영역입니다(가로채기 block/허용 allow +
// 유형 + 일치 내용 + 메모). 스스로 저장하지 않음. 부모가 언제 보낼지 정합니다.
export function AssetInterceptRulesEditor({
  value,
  onChange,
}: {
  value: AssetInterceptRuleInput[];
  onChange: (v: AssetInterceptRuleInput[]) => void;
}) {
  function update(i: number, patch: Partial<AssetInterceptRuleInput>) {
    onChange(value.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  }
  function remove(i: number) {
    onChange(value.filter((_, idx) => idx !== i));
  }
  function add() {
    onChange([...value, { action: "block", kind: "fuzzy_domain", pattern: "", note: "", enabled: true }]);
  }
  return (
    <div className="grid gap-2">
      {value.map((r, i) => {
        const ph = ASSET_INTERCEPT_KIND_OPTIONS.find((o) => o.value === r.kind)?.placeholder ?? "";
        return (
          // biome-ignore lint/suspicious/noArrayIndexKey: 행에 안정적인 id가 없어, 인덱스로 제어하면 됩니다
          <div key={i} className="flex items-center gap-2">
            <NativeSelect
              size="sm"
              className="w-[84px] shrink-0"
              value={r.action}
              onChange={(e) => update(i, { action: e.target.value as "block" | "allow" })}
            >
              <NativeSelectOption value="block">가로채기</NativeSelectOption>
              <NativeSelectOption value="allow">허용</NativeSelectOption>
            </NativeSelect>
            <NativeSelect
              size="sm"
              className="w-[120px] shrink-0"
              value={r.kind}
              onChange={(e) => update(i, { kind: e.target.value as AssetInterceptKind })}
            >
              {ASSET_INTERCEPT_KIND_OPTIONS.map((o) => (
                <NativeSelectOption key={o.value} value={o.value}>
                  {o.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
            <Input
              className="flex-1"
              placeholder={ph}
              value={r.pattern}
              onChange={(e) => update(i, { pattern: e.target.value })}
            />
            <Input
              className="w-[120px] shrink-0"
              placeholder="메모(선택)"
              value={r.note}
              onChange={(e) => update(i, { note: e.target.value })}
            />
            <Button
              type="button"
              size="icon"
              variant="ghost"
              className="text-destructive hover:text-destructive size-8 shrink-0"
              onClick={() => remove(i)}
            >
              <Trash2Icon className="size-4" />
            </Button>
          </div>
        );
      })}
      <Button type="button" size="sm" variant="outline" className="w-fit" onClick={add}>
        <PlusIcon className="size-4" /> 하나 추가
      </Button>
    </div>
  );
}
