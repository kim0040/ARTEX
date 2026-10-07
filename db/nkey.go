package db

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// 정규화된 자연 키(nkey). 옛 graph/id.go에서 옮겨 왔고, StableID 해시는 뺐다(PG는 BIGSERIAL 기본 키 +
// UNIQUE(type, nkey)로 중복을 없앤다). 자식 자산의 nkey는 부모 자산의 int64 id를 넣어 계층을 키에 인코딩한다.

func DomainKey(fqdn string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(fqdn)), ".")
}

func IPKey(ip string) string { return strings.TrimSpace(ip) }

// RootDomain은 호스트의 등록 가능 도메인(eTLD+1)과
// 호스트 자신이 그 apex인지(§3.1)를 반환한다. 경계 처리(§3.1): IP 리터럴이거나
// publicsuffix가 분류할 수 없는 호스트(localhost / internal / non-ICANN TLD)는
// 바꾸지 않고 자기 자신을 루트로 반환하며 isApex=true다. 최선으로 처리하고, 서브도메인으로 보지 않는다.
func RootDomain(host string) (root string, isApex bool) {
	h := DomainKey(host)
	if h == "" || net.ParseIP(h) != nil {
		return h, true
	}
	etld1, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil || etld1 == "" {
		return h, true
	}
	return etld1, h == etld1
}

func PortKey(ipID int64, proto string, port int) string {
	return itoa(ipID) + "|" + strings.ToLower(proto) + "|" + strconv.Itoa(port)
}

func ServiceKey(portID int64, svcName string) string {
	return itoa(portID) + "|" + strings.ToLower(svcName)
}

func SiteKey(scheme, host string, port int) string {
	return strings.ToLower(scheme) + "|" + strings.ToLower(host) + "|" + strconv.Itoa(port)
}

func EndpointKey(siteID int64, method, urlTemplate string) string {
	return itoa(siteID) + "|" + strings.ToUpper(method) + "|" + urlTemplate
}

func ParameterKey(endpointID int64, location, name string) string {
	return itoa(endpointID) + "|" + strings.ToLower(location) + "|" + name
}

// NormalizeParamName은 파라미터 이름을 정규화한다(endpoint.params 원소의 같은 참조 판정).
// 규칙: lower + trim. 동의어는 합치지 않는다(userId/user_id/uid는 서로 다른 것으로 본다). 쓰기와 조회가 이 구현을 공유하여,
// 파라미터 이름으로 같은 기업의 인터페이스를 찾는 일이 재현되게 한다.
func NormalizeParamName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func TechKey(name, version string) string {
	return strings.ToLower(name) + "|" + version
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

var (
	reNumeric = regexp.MustCompile(`^\d+$`)
	reUUID    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHex     = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reLong    = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)
)

// TemplatePath는 경우의 수가 많은 경로 조각을 자리표시자로 바꾼다.
// 그래서 /user/123 같은 주소가 자산 그래프의 endpoint를 건마다 채우지 않는다. 예: /user/123 → /user/{id}.
func TemplatePath(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		switch {
		case s == "":
			continue
		case reNumeric.MatchString(s):
			segs[i] = "{id}"
		case reUUID.MatchString(s):
			segs[i] = "{uuid}"
		case reHex.MatchString(s):
			segs[i] = "{hex}"
		case reLong.MatchString(s):
			segs[i] = "{token}"
		}
	}
	return strings.Join(segs, "/")
}

// SplitURL은 원본 URL을 scheme, host, port, urlTemplate, params로 나눈다.
// 이 값으로 site, endpoint, param 키를 만들어 자산 그래프의 endpoint를 구분한다.
func SplitURL(raw, method string) (scheme, host string, port int, urlTemplate string, params []string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, "", nil, err
	}
	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Hostname())
	port = defaultPort(scheme, u.Port())
	urlTemplate = TemplatePath(u.EscapedPath())
	for k := range u.Query() {
		params = append(params, k)
	}
	return scheme, host, port, urlTemplate, params, nil
}

func defaultPort(scheme, p string) int {
	if p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	switch scheme {
	case "https":
		return 443
	case "http":
		return 80
	default:
		return 0
	}
}
