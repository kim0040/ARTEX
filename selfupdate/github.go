package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo는 배포 출처입니다. 설정 항목으로 만들지 않고 고정합니다. 업데이트 출처를 바꿀 수 있으면
// 설정을 고칠 수 있는 사람에게 원격 코드 실행 통로가 됩니다. 침투 테스트 플랫폼에서는 그 구멍을 열면 안 됩니다.
const Repo = "Autumn-27/artex"

// latestURL은 GitHub의 "최신 정식판" 인터페이스입니다. prerelease와 draft는 자동으로 건너뜁니다.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts는 업그레이드 경로가 접속할 수 있는 도메인을 제한합니다. 아래 checkRedirect와 함께,
// 어느 한 홉이라도 명단 밖 호스트로 리다이렉트되면 바로 실패합니다. DNS 오염 / 중간자가
// 바이너리를 바꾸는 것을 막는 첫 문이고, 두 번째는 SHA256SUMS 대조입니다.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 자산이 실제로 놓이는 객체 저장소
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release는 GitHub Release에서 우리가 보는 필드입니다.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset은 Release에 붙은 파일 하나입니다.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient는 GitHub 도메인만 인정하는 HTTP 클라이언트를 만듭니다. proxy가 비면 직접 연결합니다.
//
// 기본 Transport를 일부러 재사용하지 않습니다. 업그레이드 경로는 TLS를 강제하고 인증서를 검증해야 하며,
// 다른 곳이 둔 InsecureSkipVerify 같은 설정의 영향을 받으면 안 됩니다.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 패키지 전체를 받습니다. 요청 단위 시간 제한으로 멈추면 안 됩니다
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리다이렉트가 너무 많습니다")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL은 https와 도메인 허용 목록을 강제합니다.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS가 아닌 주소를 거부합니다: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub가 아닌 도메인을 거부합니다: %s", u.Hostname())
	}
	return nil
}

// FetchLatest는 최신 정식판을 조회합니다.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 접속 실패(시스템 설정에서 전역 프록시를 둘 수 있습니다): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 인증하지 않은 GitHub API는 IP당 시간당 60회입니다. 출구 IP를 같이 쓰면 쉽게 부딪칩니다.
		return nil, fmt.Errorf("GitHub 인터페이스 속도 제한(시간당 60회)입니다. 잠시 뒤 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s에 아직 정식 버전이 없습니다", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub가 %d를 반환했습니다", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 해석 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release에 tag가 없습니다")
	}
	return &rel, nil
}

// AssetName은 현재 플랫폼에 해당하는 배포 패키지 이름을 돌려줍니다. build.sh의 package_binary와 같습니다.
// artex-<버전>-<os>-<arch>.zip (버전 번호에 v 접두사는 없습니다).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset은 Release에서 이름으로 자산을 찾습니다.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
