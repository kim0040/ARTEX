import assert from "node:assert/strict";
import test from "node:test";
import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";

test("mention trigger supports Korean labels and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("선택 @[발견#1 X]", 12), null);
  assert.deepEqual(activeMention("보기@발견 뒤의 글", 5), { start: 2, end: 5, query: "발견" });
  assert.equal(activeMention("@발견\n다음 줄", 9), null);
});

test("categories, Korean aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("발").categories[0].kind, "finding");
  assert.equal(mentionSearch("발견").kind, "finding");
  assert.equal(mentionSearch("발견SQL주입").query, "SQL주입");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("엔드포인트 GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "제목[1]\n설명" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  const value = `분석 ${first} 그리고 ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].label, "발견 #12 · 제목（1） 설명");
  // 예전에 저장된 대화는 중국어 종류 이름을 그대로 갖고 있다. 화면 문구는 발견이지만 파서는 둘 다 읽는다.
  assert.equal(selectedMentions("@[漏洞#3 옛]")[0].label, "漏洞 #3 · 옛"); // han-allow 저장된 멘션
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});
