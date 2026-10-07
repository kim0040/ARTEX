"use client";

import * as React from "react";

import { ArrowDownIcon, ArrowUpIcon, PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { TrafficEvidenceViewer } from "@/components/traffic-evidence-viewer";
import { TrafficPickerDialog } from "@/components/traffic-picker-dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { FindingTraffic, FindingTrafficBinding, TrafficEvidenceRole } from "@/lib/types";

const ROLES: Record<TrafficEvidenceRole, string> = {
  baseline: "정상 대조",
  proof: "발견 증명",
  verification: "추가 검증",
  supporting: "보조 증거",
};

export function FindingTrafficPanel({
  findingId,
  contextTask,
  readOnly,
  onChanged,
}: {
  findingId: string;
  contextTask?: string;
  readOnly?: boolean;
  onChanged: () => void;
}) {
  const [data, setData] = React.useState<FindingTraffic | null>(null);
  const [error, setError] = React.useState("");
  const [adding, setAdding] = React.useState(false);
  const [preview, setPreview] = React.useState<string | null>(null);
  const [editing, setEditing] = React.useState<FindingTrafficBinding | null>(null);
  const [role, setRole] = React.useState<TrafficEvidenceRole>("supporting");
  const [note, setNote] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [reload, setReload] = React.useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: 다시 시도 토큰이 바뀌면 이 요청을 일부러 다시 불러옵니다.
  React.useEffect(() => {
    let active = true;
    setData(null);
    setError("");
    api
      .findingTraffic(findingId, contextTask)
      .then((value) => {
        if (active) setData(value);
      })
      .catch((e: Error) => {
        if (active) setError(e.message);
      });
    return () => {
      active = false;
    };
  }, [findingId, contextTask, reload]);

  async function mutate(action: () => Promise<FindingTraffic>) {
    setBusy(true);
    try {
      setData(await action());
      setEditing(null);
      onChanged();
      toast.success("트래픽 증거가 업데이트되었습니다");
    } catch (e) {
      toast.error((e as Error).message);
      setReload((n) => n + 1);
    } finally {
      setBusy(false);
    }
  }

  function move(index: number, delta: number) {
    if (!data) return;
    const ids = data.bindings.map((b) => b.id);
    [ids[index], ids[index + delta]] = [ids[index + delta], ids[index]];
    void mutate(() => api.orderFindingTraffic(findingId, ids, data.version, contextTask));
  }

  return (
    <>
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <CardTitle>연결 트래픽 {data ? `(${data.bindings.length})` : ""}</CardTitle>
            {!readOnly ? (
              <Button variant="outline" size="sm" disabled={!data || busy} onClick={() => setAdding(true)}>
                <PlusIcon data-icon="inline-start" />
                트래픽 묶기
              </Button>
            ) : (
              <Badge variant="outline">상속된 증거 · 읽기 전용</Badge>
            )}
          </div>
          <CardDescription>재현 순서대로 요청과 응답을 정리합니다. 원본 트래픽을 지워도 이미 묶인 증거는 남습니다.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {error ? (
            <Alert variant="destructive">
              <AlertDescription>
                {error}
                <Button variant="link" onClick={() => setReload((n) => n + 1)}>
                  다시 시도
                </Button>
              </AlertDescription>
            </Alert>
          ) : null}
          {!data ? (
            <Skeleton className="h-28 w-full" />
          ) : data.bindings.length === 0 ? (
            <Empty>
              <EmptyHeader>
                <EmptyTitle>연결 트래픽 없음</EmptyTitle>
                <EmptyDescription>정상 비교, 발견 증명, 추가 확인 요청을 묶을 수 있습니다.</EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            data.bindings.map((b, index) => (
              <div key={b.id} className="flex min-w-0 flex-col gap-2 rounded-md border p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <Badge variant="outline">
                    {index + 1}. {ROLES[b.role]}
                  </Badge>
                  <Badge variant="secondary">
                    {b.snapshot.method} · {b.snapshot.status}
                  </Badge>
                  <span className="text-xs text-muted-foreground">증거 #{b.id}</span>
                </div>
                <Button
                  variant="link"
                  className="h-auto justify-start p-0 text-left whitespace-normal"
                  onClick={() => setPreview(b.id)}
                >
                  <span className="break-all font-mono text-xs">{b.snapshot.url}</span>
                </Button>
                {b.note ? <p className="text-sm whitespace-pre-wrap">{b.note}</p> : null}
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <span className="text-xs text-muted-foreground">
                    {new Date(b.snapshot.captured_at * 1000).toLocaleString("ko-KR")}
                  </span>
                  <div className="flex flex-wrap gap-1">
                    <Button variant="outline" size="sm" onClick={() => setPreview(b.id)}>
                      패킷 보기
                    </Button>
                    {!readOnly ? (
                      <>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={busy}
                          onClick={() => {
                            setEditing(b);
                            setRole(b.role);
                            setNote(b.note);
                          }}
                        >
                          설명 편집
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`증거 위로 ${b.id}`}
                          disabled={busy || index === 0}
                          onClick={() => move(index, -1)}
                        >
                          <ArrowUpIcon />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`증거 아래로 ${b.id}`}
                          disabled={busy || index === data.bindings.length - 1}
                          onClick={() => move(index, 1)}
                        >
                          <ArrowDownIcon />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={busy}
                          onClick={() =>
                            void mutate(() => api.removeFindingTraffic(findingId, b.id, data.version, contextTask))
                          }
                        >
                          연결 해제
                        </Button>
                      </>
                    ) : null}
                  </div>
                </div>
              </div>
            ))
          )}
        </CardContent>
      </Card>
      {adding && data ? (
        <TrafficPickerDialog
          findingId={findingId}
          contextTask={contextTask}
          bound={data.bindings}
          onClose={() => setAdding(false)}
          onBound={() => {
            setReload((n) => n + 1);
            onChanged();
          }}
        />
      ) : null}
      <TrafficEvidenceViewer
        findingId={findingId}
        bindingId={preview}
        contextTask={contextTask}
        onClose={() => setPreview(null)}
      />
      <Dialog
        open={editing !== null}
        onOpenChange={(open) => {
          if (!open && !busy) setEditing(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>트래픽 증거 편집</DialogTitle>
            <DialogDescription>이 요청/응답 묶음이 발견 결론을 어떻게 뒷받침하는지 설명합니다.</DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="evidence-role">용도</FieldLabel>
              <Select value={role} onValueChange={(v) => setRole(v as TrafficEvidenceRole)}>
                <SelectTrigger id="evidence-role">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    {Object.entries(ROLES).map(([value, label]) => (
                      <SelectItem key={value} value={value}>
                        {label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
            </Field>
            <Field>
              <FieldLabel htmlFor="evidence-note">증거 설명</FieldLabel>
              <Textarea id="evidence-note" value={note} onChange={(e) => setNote(e.target.value)} />
            </Field>
          </FieldGroup>
          <DialogFooter>
            <Button variant="outline" disabled={busy} onClick={() => setEditing(null)}>
              취소
            </Button>
            <Button
              disabled={busy || !editing || !data}
              onClick={() => {
                if (editing && data)
                  void mutate(() =>
                    api.editFindingTraffic(findingId, editing.id, data.version, { role, note }, contextTask),
                  );
              }}
            >
              설명 저장
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
