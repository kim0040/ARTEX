package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// ParsedScope는 파싱된 자산 범위 항목 하나다. kind가 Domain, Net, Value 중 무엇을 쓸지 정한다.
// 기업 범위 파서와 CompanyStore가 내부에서 쓴다. 이 범위가 자산 그래프에 올릴 자산의 기업 경계를 정한다.
type ParsedScope struct {
	Kind   string // "domain" | "ip" | "cidr" | "icp" | "keyword" (범위 종류)
	Domain string // 정규화한 등록 가능/루트 도메인 (kind=domain)
	Net    string // 정규화한 CIDR. IP 하나는 /32 또는 /128 (kind=ip|cidr)
	Value  string // 정규화한 글 (kind=icp|keyword)
	Raw    string // 원래 입력 줄
}

// ScopeInput은 기업 범위 규칙의 구조화된 API 형태다. Kind가 비어 있으면
// 글상자 하나짜리 UI와 같은 자동 분류를 쓴다.
type ScopeInput struct {
	Kind  string `json:"kind,omitempty"`
	Value string `json:"value"`
}

// NormalizeICP는 유니코드 공백을 모두 지우고 대소문자를 접는다.
// ICP 맞춤은 일부러 비슷한 글자나 문장 부호를 정규화하지 않는다.
func NormalizeICP(value string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(value)))
}

func normalizeKeyword(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func looksLikeIPAddress(value string) bool {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	if strings.Count(value, ":") >= 2 {
		// IPv6처럼 보이는 접두가 있어야 한다. 2001:db8::zz 같은 잘못된 값은 잡고,
		// 콜론으로 나뉜 평범한 키워드는 IP로 보지 않는다.
		parts := strings.Split(value, ":")
		validSegments := 0
		for _, part := range parts {
			if part == "" {
				if validSegments > 0 || strings.HasPrefix(value, "::") {
					return true
				}
				continue
			}
			if len(part) > 4 {
				return false
			}
			for _, r := range part {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
					return false
				}
			}
			validSegments++
			if validSegments >= 2 {
				return true
			}
		}
		return false
	}
	if !strings.Contains(value, ".") {
		return false
	}
	for _, r := range value {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// ParseScopeInput은 종류를 명시한 규칙을 검사한다. 예전 호출자는
// Kind를 빼고, 글상자 하나짜리 UI와 같은 자동 분류를 쓸 수 있다.
func ParseScopeInput(input ScopeInput) (ParsedScope, error) {
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	raw := strings.TrimSpace(input.Value)
	if kind == "" {
		return ParseAutoScopeLine(raw)
	}
	switch kind {
	case "domain", "ip", "cidr":
		rule, err := ParseScopeLine(raw)
		if err != nil {
			return rule, err
		}
		if rule.Kind != kind {
			return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("%q은 유효한 %s 범위가 아닙니다", raw, kind)
		}
		return rule, nil
	case "icp":
		value := NormalizeICP(raw)
		if value == "" {
			return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("ICP는 비울 수 없습니다")
		}
		return ParsedScope{Kind: kind, Value: value, Raw: raw}, nil
	case "keyword":
		value := normalizeKeyword(raw)
		if value == "" {
			return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("기업 키워드는 비울 수 없습니다")
		}
		return ParsedScope{Kind: kind, Value: value, Raw: raw}, nil
	default:
		return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("지원하지 않는 범위 유형: %s", kind)
	}
}

// ParseAutoScopeLine은 종류가 없는 글상자 한 줄을 분류한다.
// 네트워크나 도메인처럼 보이는 값은 엄격하게 본다. 잘못된 범위가 조용히 키워드가 되지 않게 한다.
// 그 밖의 비어 있지 않은 글은 키워드다.
func ParseAutoScopeLine(line string) (ParsedScope, error) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return ParsedScope{}, fmt.Errorf("빈 줄")
	}

	if _, _, err := net.ParseCIDR(raw); err == nil {
		return ParseScopeLine(raw)
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ParseScopeLine(raw)
	}
	if slash := strings.LastIndexByte(raw, '/'); slash > 0 {
		address := strings.TrimSpace(raw[:slash])
		if net.ParseIP(address) != nil || looksLikeIPAddress(address) {
			return ParsedScope{Raw: raw}, fmt.Errorf("유효하지 않은 CIDR: %s", raw)
		}
	}

	if looksLikeIPAddress(raw) {
		return ParsedScope{Raw: raw}, fmt.Errorf("유효하지 않은 IP: %s", raw)
	}

	looksLikeDomain := strings.Contains(raw, "://") ||
		(strings.Contains(raw, ".") && strings.IndexFunc(raw, unicode.IsSpace) < 0)
	if looksLikeDomain {
		return ParseScopeLine(raw)
	}
	// 등록번호 자체에는 점(.)이 없다(예: ICP12345678-1). 점이 있는 텍스트는 대개 도메인이나 버전 번호가 섞여 있고,
	// ICP로 저장하면 어떤 자산과도 영원히 맞지 않는 죽은 규칙만 생긴다. ICP 귀속은 정확한
	// 동등 비교를 따른다(companies.go의 kind="icp" 귀속 조회를 보라). 그래서 이런 텍스트는 키워드로 분류한다.
	// 아래 조건의 등록번호 리터럴은 매칭 패턴이라 원문 그대로 둔다. 그 표기나 글자 icp가 있으면 ICP로 본다.
	lower := strings.ToLower(raw)
	if !strings.ContainsAny(raw, ".．。") &&
		(strings.Contains(lower, "icp") || strings.Contains(raw, "备案")) { // han-allow 프로토콜 원문
		return ParseScopeInput(ScopeInput{Kind: "icp", Value: raw})
	}
	return ParseScopeInput(ScopeInput{Kind: "keyword", Value: raw})
}

func scopeHostname(raw string) (string, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", fmt.Errorf("호스트 이름이 비어 있습니다")
	}
	if strings.HasPrefix(candidate, "//") {
		candidate = "http:" + candidate
	} else if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		if err == nil {
			err = fmt.Errorf("호스트 이름이 없습니다")
		}
		return "", err
	}
	host := strings.TrimSuffix(strings.TrimSpace(parsed.Hostname()), ".")
	if host == "" {
		return "", fmt.Errorf("호스트 이름이 비어 있습니다")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	host, err = idna.Lookup.ToASCII(host)
	if err != nil {
		return "", err
	}
	host = strings.ToLower(host)
	if len(host) > 253 {
		return "", fmt.Errorf("도메인이 253자를 초과합니다")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("도메인에는 라벨이 최소 두 개 필요합니다")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("도메인 라벨이 유효하지 않습니다")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return "", fmt.Errorf("도메인에 유효하지 않은 문자가 있습니다")
			}
		}
	}
	return host, nil
}

// ParseScopeLine은 범위 한 줄(루트 도메인 / IP / CIDR)을 분류하고 검사한다.
// 가드레일은 맨 TLD와 너무 넓은 네트워크를 거절한다. 규칙 하나가 인터넷 전체를 삼키지 못하게 한다.
// IP 범위는 CIDR로 적어야 한다.
func ParseScopeLine(line string) (ParsedScope, error) {
	raw := strings.TrimSpace(line)
	r := ParsedScope{Raw: raw}
	if raw == "" {
		return r, fmt.Errorf("빈 줄")
	}
	// CIDR을 먼저 본다. URL 파서는 슬래시를 경로 구분자로 보기 때문이다.
	if _, ipnet, err := net.ParseCIDR(raw); err == nil {
		ones, bits := ipnet.Mask.Size()
		if bits == 32 && ones < 16 {
			return r, fmt.Errorf("네트워크 대역이 너무 넓습니다(IPv4는 >= /16 필요): %s", raw)
		}
		if bits == 128 && ones < 32 {
			return r, fmt.Errorf("네트워크 대역이 너무 넓습니다(IPv6는 >= /32 필요): %s", raw)
		}
		r.Kind, r.Net = "cidr", ipnet.String()
		return r, nil
	}
	// IP 하나.
	if ip := net.ParseIP(raw); ip != nil {
		r.Kind = "ip"
		if ip.To4() != nil {
			r.Net = ip.String() + "/32"
		} else {
			r.Net = ip.String() + "/128"
		}
		return r, nil
	}
	host, err := scopeHostname(raw)
	if err != nil {
		return r, fmt.Errorf("유효한 도메인/IP/CIDR로 인식할 수 없습니다: %s", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		r.Kind = "ip"
		if ip.To4() != nil {
			r.Net = ip.String() + "/32"
		} else {
			r.Net = ip.String() + "/128"
		}
		return r, nil
	}
	if looksLikeIPAddress(host) {
		return r, fmt.Errorf("유효하지 않은 IP: %s", raw)
	}
	if strings.Contains(raw, "-") && strings.Count(raw, ".") >= 6 {
		return r, fmt.Errorf("IP 대역은 CIDR로 표기하세요(예: 1.2.3.0/24): %s", raw)
	}
	// 도메인(등록 가능). 맨 TLD와 공용 접미사는 거절한다.
	d := DomainKey(host)
	if suf, icann := publicsuffix.PublicSuffix(d); icann && suf == d {
		return r, fmt.Errorf("맨 TLD는 범위로 사용할 수 없습니다: %s", raw)
	}
	r.Kind, r.Domain = "domain", d
	return r, nil
}
