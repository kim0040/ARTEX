// 채널 필드 표와 설정 값을 해석하는 도구.
//
// 페이지와 나눈 이유: 이것은 보기가 아니라 **데이터**입니다. 채널마다 어떤 필드가 있는지,
// 각자 어떤 컨트롤을 쓸지, 그리고 양식 텍스트와 설정 값(JSON)의 양방향 변환.
// 파일 하나에 따로 두면, 채널을 추가할 때 여기만 고치면 되고 페이지 자체는 안 고쳐도 됩니다.
// 채널 유형의 표시 이름과 짧은 소개. 프론트에 두는 이유: 문구에만 영향을 주고 백엔드는 알 필요가 없습니다.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "딩톡",
  feishu: "페이사",
  wecom: "기업 위챗",
  webhook: "일반 Webhook",
  telegram: "Telegram",
  email: "이메일",
};

// 채널별 설정 필드 정의.
//
// 여기서는 프론트 필드 표를 일부러 한 부 유지합니다. 백엔드가 schema를 내려 주지 않게: 백엔드는
// Validate(필수/형식). UI에 필요한 것은 배치와 컨트롤 유형이며, 둘의 관심사는 같지 않습니다.
// 유일한 결합점은 secret_keys입니다. 어떤 필드를 비밀번호 칸으로 그릴지는 백엔드가 주고,
// 어떤 값이 자격인지는 채널 구현만 알기 때문입니다(기업 위챗의 Webhook 전체가 자격이고,
// 딩톡의 것은 secret 중 하나일 뿐입니다). 채널을 추가할 때 여기 항목이 하나 빠지면 양식만 비고,
// 조용히 오류 나지 않음(아래 hasFields가 알려 줌).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "서명 비밀키",
      kind: "password",
      help: "봇 보안 설정에서 「서명 추가」를 고르면 입력하세요. 「사용자 정의 키워드」를 고르거나 보안 설정을 켜지 않았으면 비워 두세요",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "서명 검증 키", kind: "password", help: "봇에서 「서명 검증」을 켜면 입력하고, 아니면 비워 두세요" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "대상 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "요청 메서드",
      kind: "select",
      options: [
        { value: "POST", label: "POST(요청 본문 있음)" },
        { value: "PUT", label: "PUT(요청 본문 있음)" },
        { value: "PATCH", label: "PATCH(요청 본문 있음)" },
        { value: "GET", label: "GET(요청 본문 없음)" },
      ],
    },
    { key: "headers", label: "사용자 정의 요청 헤더", kind: "kv", help: "한 줄에 KEY=VALUE, 예: Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "요청 본문 템플릿",
      kind: "textarea",
      help:
        "비워 두면 내장 기본 템플릿을 사용합니다. 변수: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}," +
        "그리고 range .Items 아래의 .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel." +
        "문자열을 넣을 때는 {{.Xxx}}가 아니라 {{json .Xxx}}를 쓰세요. 그렇지 않으면 제목의 따옴표가 JSON을 깨뜨립니다.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API 주소",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "비워 두면 공식 주소를 사용합니다. 자체 Bot API 역방향 프록시를 쓸 때 입력하세요",
    },
  ],
  email: [
    { key: "host", label: "SMTP 서버", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "포트",
      kind: "number",
      placeholder: "587",
      help: "587은 STARTTLS를 탑니다. 465는 「암시적 TLS」를 켜세요",
    },
    { key: "username", label: "계정", kind: "text" },
    { key: "password", label: "비밀번호 / 인가 코드", kind: "password" },
    { key: "from", label: "보낸 사람", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "수신자", kind: "list", help: "주소가 여러 개면 쉼표로 구분" },
    { key: "tls", label: "암시적 TLS", kind: "switch", help: "465 포트가 열려 있습니다. 587은 계속 닫혀 있습니다(자동으로 STARTTLS)." },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "제한 없음" },
  { value: "low", label: "낮음 이상" },
  { value: "medium", label: "중간 이상" },
  { value: "high", label: "높음 이상" },
  { value: "critical", label: "심각만" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV는 「한 줄에 KEY=VALUE」 텍스트 영역을 해석합니다.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs는 쉼표/공백으로 나뉜 id 목록을 해석합니다.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords는 줄/쉼표로 나뉜 키워드 목록을 해석합니다(발견 유형 이름에 공백이 있을 수 있어 줄 또는 쉼표로 자름).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
