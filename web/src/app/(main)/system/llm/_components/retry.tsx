"use client";

// LLM 재시도 설정의 공통 부품: 다섯 층 재시도 각각의 「횟수 + 간격」.
//
// 다섯 층, 안에서 밖으로: 연결(SDK) → 빈 응답(SDK) → 같은 provider 안전 창 → 주기 조회 차단기 → 의도 다시 실행.
// 앞의 세 층은 엔드포인트를 따라가므로, 모델 설정마다 전역 기본값을 덮을 수 있습니다. 뒤의 두 층은 프로세스 단위라 전역에 하나만 있습니다.
//
// 모든 입력은 같은 「비워 둠 = 설정 안 함」 의미를 따릅니다. 백엔드 db.RetryRule과 같습니다:
//   횟수   비움/0 = 내장 기본값 사용 | -1 = 이 층 재시도 끄기 | >0 = 이 횟수 사용
//   간격   비움/0 = 이 층 원래의 지수 백오프 사용 | >0 = 이 고정 밀리초 간격으로 바꿈

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** 이 층 재시도가 어디서 일어나고, 누가 실행하는지 */
  where: string;
  /** 어떤 오류가 이 층으로 오는지. 상태 코드까지 적어, 추측하지 않게 합니다 */
  trigger: string;
  /** 비슷해 보이지만 이 층을 【타지 않는】 오류. 값을 넣어도 반응이 없어 bug로 오해하지 않게 합니다 */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 횟수를 비웠을 때의 기본값. 자리표시자에 씁니다 */
  defAttempts: number;
  /** 간격을 비웠을 때의 기본 방식. 자리표시자에 씁니다 */
  defInterval: string;
  /** 횟수에 -1을 넣는 뜻 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "연결 재시도",
    where: "SDK · 200을 받기 전",
    trigger:
      "연결되지 않거나 아직 200을 받지 못함: 연결 재설정 / 읽기·쓰기 시간 초과 / DNS 실패 같은 네트워크 계층 오류, 그리고 HTTP 408, 429, 500, 502, 503, 504.",
    skips: "나머지 상태 코드(400 / 401 / 403 / 404 / 413 / 422 등)는 확정 거부라, 다시 보내도 같이 실패합니다. 바로 위로 전달하세요.",
    desc: "같은 요청을 그대로 다시 보냅니다. 스트림이 시작되면(이미 200을 받음) 중간에 끊겨도 이 층이 맡지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 지수(상한 8s)",
    offHint: "-1 = 한 번도 재시도하지 않고, 실패하면 바로 위로 전달",
  },
  empty: {
    title: "빈 응답 재시도",
    where: "SDK · openai 형식만",
    trigger:
      "HTTP 200이고 finish_reason은 정상 stop인데, 응답 전체에 내용 블록이 하나도 없습니다. 게이트웨이 빈 프레임, 생각 필드 프레임 유실, 샘플링 끊김이 이렇게 보입니다.",
    skips: "max_tokens로 잘려 내용이 없는 경우는 해당하지 않습니다(출력 상한을 높여야 하며, 다시 보내도 한 번 더 부딪힐 뿐입니다).",
    desc: "prompt 전체를 다시 보내므로 긴 컨텍스트에서는 비용이 크고, 횟수를 크게 주지 않는 것이 좋습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 지수(상한 8s)",
    offHint: "-1 = 빈 응답은 그대로 넘김",
  },
  stream: {
    title: "같은 provider의 안전 구간에서 재시도",
    where: "이 프로젝트 · 출력을 넘기기 전",
    trigger:
      "스트림이 성립한 뒤(200을 받은 뒤)에야 문제가 납니다. 연결이 중간에 끊기거나, 공급자가 overloaded이거나, 스트림 안의 429 / 5xx 오류 이벤트이며, 호출자에게는 token이 하나도 전달되지 않았습니다.",
    skips:
      "한도 소진(402 / insufficient_quota, 폴링이 설정을 바꿈), 컨텍스트가 너무 김(413 / context length, 압축에 넘김), 400 / 401 / 403 / 404 / 422 확정 거부는 모두 재시도하지 않습니다.",
    desc: "같은 설정에서 같은 요청을 다시 재생합니다. 아직 어떤 출력도 넘기지 않았으므로, 다시 재생해도 모델 출력이나 도구 실행이 반복되지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s 지수(상한 4s)",
    offHint: "-1 = 스트림이 끊기면 바로 바깥의 의도 재실행에 넘김",
  },
  breaker: {
    title: "폴링 서킷 브레이크",
    where: "이 프로젝트 · 프로세스 단위, 전역으로 하나",
    trigger:
      "순간 실패(429, 5xx, 네트워크 오류)가 연속으로 임계값에 닿으면 서킷 브레이크가 됩니다. 잔액 부족(402), 키 무효(401 / 403), 모델 없음(404) 같은 확정 실패는 임계값을 보지 않고 한 번에 서킷 브레이크가 됩니다.",
    skips: "한 번 성공하면 바로 0으로 돌아가므로, 가끔 불안정한 설정이 조금씩 쌓여 서킷 브레이크까지 가지 않습니다.",
    desc: "서킷 브레이크 뒤 쿨다운에 들어가며, 쿨다운 동안 폴링은 이 설정을 건너뜁니다. 상태는 저장되어 재시작해도 사라지지 않습니다.",
    attemptsLabel: "연속 실패 몇 회에 서킷 브레이크",
    defAttempts: 3,
    defInterval: "1분→5분→30분 단계",
    offHint: "-1 = 일시적 실패는 회로를 끊지 않음(확정 실패는 여전히 회로를 끊음)",
  },
  intent: {
    title: "의도 다시 실행",
    where: "이 프로젝트 · 프로세스 단위, 전역으로 하나",
    trigger:
      "앞 층이 모두 막지 못함: 워커가 model_error로 끝남. 안쪽 재시도를 모두 썼거나, 스트림이 출력을 넘기기 시작한 뒤에야 끊김(그때 다시 재생하면 안전하지 않아, 통째로 다시 할 수밖에 없음).",
    skips: "한도 소진은 이미 폴링이 설정을 바꿔 처리하므로 여기서 다시 실행하지 않습니다. 작업이 일시정지 / 종료 / 마무리에 들어가면 바로 양보하며, 백오프 시간을 차지하지 않습니다.",
    desc: "의도 전체를 처음부터 다시 실행합니다. 가장 바깥층이라, 한 번 다시 실행하면 안쪽 여러 층의 횟수가 다시 곱해집니다.",
    attemptsLabel: "다시 실행 횟수",
    defAttempts: 2,
    defInterval: "고정 3초",
    offHint: "-1 = 다시 실행하지 않고, 그 의도는 바로 blocked로 판정",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** 밀리초를 사람 말로. 입력 칸 옆에만 다시 보여, 0을 세지 않게 합니다. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 제어되는 숫자 입력: 빈 문자열 ↔ 0. 중간 상태("-", "1e")는 로컬에 그대로 두고 부모를 건드리지 않습니다. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 부모가 값 세트 전체를 바꿀 때(방식을 읽어 옴, 설정 전환) 따라갑니다. 자신이 타이핑할 때는 여기로 오지 않고,
  // 그때 value는 이미 로컬 텍스트를 parse한 결과와 같기 때문입니다.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 한 층 재시도의 두 조절값. idPrefix는 같은 페이지에 여러 번 나올 때 label의 htmlFor를 지키기 위해 씁니다. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 설정 서랍 안의 짧은 판: 펼침 설명을 빼고, 「어떤 오류가 이 층으로 오는지」 한 문장만 남김 */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* 어떤 오류가 이 층으로 오는지, 상태 코드까지. 값을 넣어도 효과가 안 보이면, 대부분 오류가 애초에 이 층에 떨어지지 않습니다. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">트리거</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">이 단계는 거치지 않음</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`기본 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            간격 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="기본 백오프"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `고정 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">비우면 = 기본값;{meta.offHint}。</p>}
    </div>
  );
}

/** 모델 설정 서랍 안의 세 층 덮어쓰기(엔드포인트를 따라가는 그 세 층). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">재시도 재정의</Label>
        <p className="text-muted-foreground text-xs">
          이 설정에만 적용되며 "재시도와 백오프"의 전역 기본값을 덮어씁니다. 칸을 비우면 전역을 따릅니다. 횟수에 -1을 넣으면 그 단계의 재시도를 끕니다. 간격을 넣으면 지수 백오프 대신 고정 간격을 씁니다. 서킷 브레이크와 의도 다시 실행은 프로세스 단위라 전역 페이지에서만 바꿀 수 있습니다.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「재시도와 대기」 tab: 다섯 층의 전역 기본값. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`재시도 정책 읽기 실패:${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // 백엔드는 범위를 벗어난 값을 구간 안으로 집어 넣고 돌려줍니다. 돌려준 값으로 바로 새로고침합니다. 보는 것이 저장된 것입니다.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("저장했습니다. 바로 적용됩니다(지금 도는 이 호출은 이전 매개변수를 그대로 씀)");
    } catch (e) {
      toast.error(`저장 실패:${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 재시도 정책을 읽는 중…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        모델 호출이 한 번 실패하면 다섯 단계의 재시도를 안쪽에서 바깥쪽으로 거칩니다:
        <span className="text-foreground"> 연결 → 빈 응답 → 같은 provider 안전 구간 → 순환 서킷 브레이크 → 의도 다시 실행</span>
        . 안쪽을 모두 써야 바깥 차례가 되므로, 횟수는
        <span className="text-foreground">곱하기</span>
        입니다. 각 단계를 최대로 두면 한 번의 흔들림에 요청이 수십 번 나갈 수 있습니다. 모두 비워 두면 현재 기본값과 같고, 이 페이지가 없을 때와 완전히 같습니다. 앞의 세 단계는 각 모델 설정에서 따로 덮어쓸 수 있습니다.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          저장
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          모두 기본값으로
        </Button>
      </div>
    </div>
  );
}
