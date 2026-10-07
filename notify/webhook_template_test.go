package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 이 파일은 범용 Webhook 템플릿의 **능력 경계**를 고정합니다.
//
// 패키지에서 유일하게 「사용자가 준 문자열이 코드처럼 평가되는」 곳입니다.
// 무엇을 할 수 있고 없는지 분명히 하고 테스트로 고정합니다. 그렇지 않으면
// 나중에 템플릿 문맥에 메서드를 추가하거나 FuncMap에 readFile을 넣는 일이
// 조용히 능력 면을 넓힙니다. diff는 무해한 작은 함수처럼 보입니다.

// TestTemplateContextHasNoMethods 가 가장 중요합니다.
//
// text/template 은 내보낸 메서드를 호출합니다({{.Foo}}는 필드도 메서드도 됩니다).
// 템플릿 문맥이 내보낸 메서드가 있는 **어떤** 타입이든 닿으면, 그 메서드를 템플릿 작성자에게 여는 것입니다.
// 이 기능의 문맥은 일부러 순수 데이터입니다(내보낸 필드만, 메서드 없음).
//
// 이 테스트가 실패하면 webhookTemplateData / webhookItem에 메서드를 추가한 것입니다.
// 통과시키기 전에, 그 메서드가 드러내고 싶지 않은 것을 템플릿이 읽을 수 있는지 먼저 생각하세요.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s 이(가) 메서드 %d개를 노출했습니다(%s). text/template이 호출할 수 있어 "+
				"그 메서드의 능력을 템플릿 작성자에게 여는 것과 같습니다", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal 은 템플릿에 열린 함수 집합을 고정합니다.
//
// FuncMap에 함수가 하나 늘면 능력이 하나 늘니다. 지금은 json / jsons뿐이고,
// 값을 JSON 조각으로 직렬화합니다. 파일을 읽거나, 요청을 보내거나, 명령을 실행할 수 없습니다.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("템플릿 함수 집합이 바뀌었습니다. 결과 %v, 기대 %v. 함수를 추가하기 전에 능력 면이 넓어지지 않는지 확인하세요"+
			"(파일 읽기/쓰기, 네트워크 요청, 명령 실행 불가)", got, want)
	}
}

// TestTemplateCannotReachUnknownData 는 템플릿의 범위 밖 접근을 덮습니다.
// 없는 것에 접근하면 실패해야 하고, 무언가를 다시 보여 주면 안 됩니다. 실패 정보에 내부 데이터가 나오면 안 됩니다.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("없는 필드에 접근하면 오류여야 합니다")
	}
	// 오류에 템플릿 문맥의 실제 내용(발견 제목/요약)이 나오면 안 됩니다.
	for _, leak := range []string{"SQL 주입", "매개변수 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("템플릿 오류가 메시지 내용 %q 을(를) 노출했습니다: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently 는 템플릿 오타가 설정 오류라 재시도해도 스스로 낫지 않음을 고정합니다.
// 재시도 가능으로 보면 나쁜 템플릿 하나가 전달마다 백오프를 세 바퀴 헛돌게 합니다.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("템플릿 문법 오류는 저장할 때 막혀야 합니다")
	}
	// 검사를 우회해 바로 보내도 영구 실패여야 하고, 반복 재시도가 아니어야 합니다.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("나쁜 템플릿은 영구 실패여야 합니다. 결과 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON 은 「템플릿 결과는 올바른 JSON이어야 한다」는 제약을 덮습니다.
// 템플릿으로 평문을 만들어 다른 프로토콜을 치는 용도도 같이 막습니다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 올바른 템플릿은 통과합니다.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("올바른 템플릿은 검사를 통과해야 합니다: %v", err)
	}
	// JSON이 아니면 거절해야 합니다(그대로 내보내지 않음).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON이 아닌 렌더 결과는 영구 실패여야 합니다. 결과 %v", err)
	}
	if !strings.Contains(err.Error(), "올바른 JSON") {
		t.Errorf("오류 정보가 JSON 문제임을 밝혀야 합니다. 결과 %v", err)
	}
}
