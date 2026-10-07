import type { Activity } from "./types";

// 기록과 실시간 응답은 겹치거나 순서가 어긋날 수 있습니다. 저장된
// 활동의 seq가 신원입니다. 다시 그릴 때 줄이나 토큰 합계가 중복되면 안 됩니다.
export function mergeActivities(current: Activity[], incoming: Activity[]): Activity[] {
  const bySeq = new Map<number, Activity>();
  for (const activity of current) bySeq.set(activity.seq, activity);
  for (const activity of incoming) bySeq.set(activity.seq, activity);
  return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
}
