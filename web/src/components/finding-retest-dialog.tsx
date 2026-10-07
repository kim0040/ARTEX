"use client";

import * as React from "react";

import { RotateCcwIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { FindingRetest } from "@/lib/types";

interface FindingRetestDialogProps {
  findingId: string;
  findingName?: string;
  onClose: () => void;
  onStarted?: (retest: FindingRetest) => void;
}

// 열릴 때만 마운트하고, 닫으면 설명을 비웁니다. 목록과 상세가 제출 잠금과 오류 처리를 함께 쓰고, 시작한 뒤 현재 페이지에 남습니다.
export function FindingRetestDialog({ findingId, findingName, onClose, onStarted }: FindingRetestDialogProps) {
  const notesId = React.useId();
  const [notes, setNotes] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  const submitLock = React.useRef(false);

  async function start() {
    if (submitLock.current) return;
    submitLock.current = true;
    setSubmitting(true);
    try {
      const result = await api.startFindingRetest(findingId, notes.trim());
      onStarted?.(result.retest);
      onClose();
      toast.success(result.created ? "재검사를 시작했습니다. 「재검사 중」을 눌러 세션을 보세요" : "이 발견은 재검사 중이며, 기존 세션을 볼 수 있습니다");
    } catch (e) {
      toast.error(`재검사 시작 실패:${(e as Error).message}`);
    } finally {
      submitLock.current = false;
      setSubmitting(false);
    }
  }

  return (
    <Dialog open onOpenChange={(open) => !open && !submitLock.current && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>재검사 발견 #{findingId}</DialogTitle>
          <DialogDescription className="break-words">
            {findingName ? <span className="mb-2 block">{findingName}</span> : null}
            재검사 Agent는 원래 증거와 테스트 제한을 읽고, 별도 세션에서 맞춰 확인합니다. 재검사가 끝나고 수정이 확인되면 발견 상태가 자동으로 "수정됨"이 됩니다. 다른 결론은 원래 상태를 유지합니다.
          </DialogDescription>
        </DialogHeader>
        <FieldGroup>
          <Field data-disabled={submitting}>
            <FieldLabel htmlFor={notesId}>추가 설명(선택)</FieldLabel>
            <Textarea
              id={notesId}
              value={notes}
              maxLength={4000}
              rows={4}
              disabled={submitting}
              onChange={(e) => setNotes(e.target.value)}
              placeholder="예: 원래 테스트 계정으로 원래 인터페이스를 검증. 고친 버전은 v2."
            />
            <FieldDescription>수정 버전, 테스트 조건, 이번 제한을 덧붙일 수 있습니다.</FieldDescription>
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button variant="outline" disabled={submitting} onClick={onClose}>
            취소
          </Button>
          <Button disabled={submitting} onClick={() => void start()}>
            {submitting ? <Spinner data-icon="inline-start" /> : <RotateCcwIcon data-icon="inline-start" />}
            {submitting ? "만드는 중…" : "재검사 시작"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
