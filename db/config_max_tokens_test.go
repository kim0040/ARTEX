package db

import "testing"

// TestProfileMaxTokensRoundTrip은 출력 상한 열이
// 읽고 쓰는 면 전체에 있는지 고정한다. 두 열 목록(목록 조회와 키 조회)이 모두 실어야 하고,
// 수정이 그 열을 빼면 안 된다. 열 개수를 잘못 세면
// 뒤의 Scan 대상이 조용히 밀린다. 이 테스트가 그걸 잡는다.
func TestProfileMaxTokensRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	// Cleanup으로 닫는다. 먼저 등록해 마지막에 돈다. cleanup은 LIFO라
	// 평범한 `defer d.Close()`는 그보다 먼저 실행된다. 행을 지우는 cleanup이
	// 닫힌 풀에 대고 돌면 테스트 행이 조용히 남는다.
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveProfile(&LLMProfile{
		Name: "t-maxtok", Format: "openai", Model: "m", APIKey: "k",
		MaxTokens: 8192, MaxTokensField: "max_completion_tokens",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	// 키로 한 행 읽기(profileColsKey).
	p, err := d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("ProfileByID: %v, p=%v", err, p)
	}
	if p.MaxTokens != 8192 || p.MaxTokensField != "max_completion_tokens" {
		t.Fatalf("keyed load: max_tokens=%d field=%q", p.MaxTokens, p.MaxTokensField)
	}

	// 목록 읽기(profileCols, 힌트 변형, 다른 열 목록).
	ps, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var found *LLMProfile
	for _, x := range ps {
		if x.ID == id {
			found = x
		}
	}
	if found == nil {
		t.Fatal("profile missing from ListProfiles")
	}
	if found.MaxTokens != 8192 || found.MaxTokensField != "max_completion_tokens" {
		t.Fatalf("list load: max_tokens=%d field=%q", found.MaxTokens, found.MaxTokensField)
	}

	// 빈 키로 수정하면 "기존 키 유지" UPDATE 갈래를 탄다. 그 갈래는
	// 열 목록이 따로 있어 빼먹기 쉽다.
	p.APIKey = ""
	p.MaxTokens = 4096
	p.MaxTokensField = ""
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	after, err := d.ProfileByID(id)
	if err != nil || after == nil {
		t.Fatalf("reload: %v", err)
	}
	if after.MaxTokens != 4096 || after.MaxTokensField != "" {
		t.Fatalf("after update: max_tokens=%d field=%q", after.MaxTokens, after.MaxTokensField)
	}
	if after.APIKey != "k" {
		t.Fatalf("blank key on update must keep the stored one, got %q", after.APIKey)
	}
}

// 새 필드를 건드리지 않고 저장한 프로필은 "상한 없음,
// 예전 필드 이름"으로 다시 읽혀야 한다. 예전 행도 그 동작을 받는다.
func TestProfileMaxTokensDefaults(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	// Cleanup으로 닫는다. 먼저 등록해 마지막에 돈다. cleanup은 LIFO라
	// 평범한 `defer d.Close()`는 그보다 먼저 실행된다. 행을 지우는 cleanup이
	// 닫힌 풀에 대고 돌면 테스트 행이 조용히 남는다.
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveProfile(&LLMProfile{Name: "t-maxtok-default", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	p, err := d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("ProfileByID: %v", err)
	}
	if p.MaxTokens != 0 || p.MaxTokensField != "" {
		t.Fatalf("defaults: max_tokens=%d field=%q, want 0 and \"\"", p.MaxTokens, p.MaxTokensField)
	}
}
