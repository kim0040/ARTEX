package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache는 GitHub 할당량을 지키는 층이다. 미인증 API는 시간당 IP당 60회뿐이다.
// 그런데 상단 바의 "새 버전 있음" 안내는 전체 페이지를 로드할 때마다 한 번씩 조회한다. 캐시가 무효가 되면 사용자가 탭을 여러 개 열기만 해도
// 할당량을 소진하고, 나중에 정말 업데이트하려 해도 조회가 안 된다. 이 캐시는 상단 바 UI의 버전 확인이 그 한도를 혼자 쓰지 않게 한다.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("조회 5회는 오리진을 1회만 다시 조회해야 하는데 실제 %d회", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 사용자가 「업데이트 확인」을 누르면 실시간 결과를 받아야 한다. 그렇지 않으면 방금 나온 버전은 캐시가 만료될 때까지 보이지 않는다.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force는 캐시를 우회해야 하며, 오리진 재조회 2회를 기대하는데 실제 %d회", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 저장된 시각을 방금 만료된 때로 되돌려 TTL이 된 상황을 흉내 낸다.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 만료 후 오리진을 다시 조회해야 하며, 2회를 기대하는데 실제 %d회", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github에 도달할 수 없음")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류 반환을 기대함")
	}
	// 실패 결과도 잠시 캐시한다. 그렇지 않으면 GitHub에 닿지 않을 때 페이지를 로드할 때마다 타임아웃을 헛되이 한 번씩 기다린다.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류 반환을 기대함")
	}
	if calls != 1 {
		t.Errorf("오류는 짧게 캐시되어야 하며, 오리진 재조회 1회를 기대하는데 실제 %d회", calls)
	}

	// 다만 오류의 TTL은 성공보다 분명히 짧아야 하며, 네트워크가 돌아오면 빨리 스스로 회복해야 한다.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("오류 TTL(%v)은 성공 TTL(%v)보다 짧아야 합니다", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("오류 반환을 기대함")
	}
	if calls != 2 {
		t.Errorf("오류 TTL이 만료된 뒤에는 재시도해야 한다. 기대 2회, 실제 %d회", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 방문자가 탭을 닫으면 요청이 취소된다. 그것은 GitHub에 문제가 있다는 뜻이 아니므로 "취소됨"을
	// 캐시에 쓰면 안 된다. 그러면 이후 30분 동안 모든 방문자가 알 수 없는 오류를 받게 된다.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // 캐시를 만료시켜 원본으로 다시 가게 한다

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("호출자가 이미 취소한 경우에는 오류를 호출자에게 그대로 전달해야 한다")
	}

	// 핵심 불변 조건: 취소된 그 한 번은 흔적을 남기지 않는다. 캐시에는 "취소됨"이라는 오류도 없고,
	// 이전의 좋은 결과도 그대로 남아 있다.
	if c.err != nil {
		t.Fatalf("취소 오류는 캐시에 기록되면 안 된다. 얻은 값 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("캐시는 직전의 정상 결과를 유지해야 한다. 얻은 값 %+v", c.rel)
	}

	// 그 취소는 새 데이터를 하나도 가져오지 못했으므로 다음 방문자는 다시 원본을 조회해야 하고, 정상적으로 결과를 받으며,
	// 이전 취소에 끌려가지 않는다.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("취소 이후의 정상 요청은 오류가 나면 안 된다: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("정상 결과를 받아야 한다. 얻은 값 %+v", rel)
	}
}
