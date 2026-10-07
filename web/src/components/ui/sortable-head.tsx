"use client";

import { ArrowDownIcon, ArrowUpDownIcon, ArrowUpIcon } from "lucide-react";
import type * as React from "react";

import { TableHead } from "@/components/ui/table";
import type { SortDirection } from "@/lib/sort-preference";
import { cn } from "@/lib/utils";

// SortableHead는 누르면 그 열의 정렬을 바꾸는 표 머리 칸입니다.
// 정렬 중이 아니면 위아래 중립 표시를, 그 필드가 정렬 중이면
// 방향 화살표를 그립니다. 작업 표에서 먼저 쓴 방식과 같아서
// 정렬되는 열이 앱 전체에서 같게 보이고 같게 동작합니다.
export function SortableHead<Field extends string>({
  field,
  label,
  activeField,
  direction,
  align = "left",
  className,
  onSort,
}: {
  field: Field;
  label: string;
  activeField: Field | null;
  direction: SortDirection;
  align?: "left" | "right";
  className?: string;
  onSort: (field: Field) => void;
}) {
  const active = activeField === field;
  let ariaSort: React.AriaAttributes["aria-sort"] = "none";
  if (active) ariaSort = direction === "asc" ? "ascending" : "descending";

  let actionLabel = `기준${label}내림차순 정렬`;
  if (active) actionLabel = `${label}현재${direction === "asc" ? "오름차순" : "내림차순"}, 클릭하면 정렬 방향이 바뀝니다`;

  let icon = <ArrowUpDownIcon className="size-3.5 opacity-40 transition-opacity group-hover/sort:opacity-100" />;
  if (active) icon = direction === "asc" ? <ArrowUpIcon className="size-3.5" /> : <ArrowDownIcon className="size-3.5" />;

  return (
    <TableHead className={className} aria-sort={ariaSort}>
      <button
        type="button"
        className={cn(
          "group/sort inline-flex h-full w-full items-center gap-1 outline-none focus-visible:underline",
          align === "right" && "justify-end",
        )}
        aria-label={actionLabel}
        onClick={() => onSort(field)}
      >
        <span>{label}</span>
        {icon}
      </button>
    </TableHead>
  );
}
