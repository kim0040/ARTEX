package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 자산 가로채기 규칙의 일치/실행 계층이다. asset_intercept.go 는 규칙 저장만 맡고, 여기서는
// 「대상 자산」의 도메인/IP/URL 을 켜져 있는 규칙과 맞춘다. 에이전트 도구(add_intent,
// insert_assets)가 의도를 내리거나 자산을 넣기 전에 호출하며, 맞으면 거부한다.

// AssetInterceptKindLabel 은 kind 의 한국어 라벨을 반환한다. 에이전트 안내 메시지에 쓴다.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "도메인(완전 일치)"
	case "exact_ip":
		return "IP(완전 일치)"
	case "exact_url":
		return "URL(완전 일치)"
	case "fuzzy_domain":
		return "도메인(부분 일치)"
	case "fuzzy_ip":
		return "IP(부분 일치)"
	case "fuzzy_url":
		return "URL(부분 일치)"
	case "cidr":
		return "CIDR 대역"
	}
	return kind
}

// Reason 은 사람이 읽을 수 있는 일치 이유를 한 줄로 반환한다. 형태: 자산 가로채기 규칙에 맞음 [도메인(부분 일치): .gov.cn](메모).
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("자산 가로채기 규칙에 맞음 [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne 은 켜져 있는 규칙 하나가 주어진 도메인/IP/URL 후보 문자열에 맞는지 판단하고, 맞은 구체적 값을 반환한다.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules는 주어진 도메인/IP/URL 후보 문자열에 처음 히트한 활성 규칙과, 히트한 구체 값을 반환한다.
// insert_assets가 원본 입력(아직 저장되지 않은 assetInputItem)으로 매칭하는 데 쓴다.
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates는 이미 저장된 자산에서 가로채기 매칭에 쓸 도메인/IP/URL 후보 문자열을 뽑는다.
// URL의 host를 분리해 분류하므로, URL만 있는 서비스형 자산도 도메인/IP 규칙에 히트할 수 있다.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel은 자산의 짧은 식별자를 반환하며, agent에 주는 설명 메시지에 쓴다.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("자산#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule은 규칙 집합에 활성 규칙이 하나라도 있는지 판단한다.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision은 「먼저 가로채기, 그다음 허용」 게이트가 후보 문자열 묶음에 내린 판정 결과이다.
// 이 판정은 자산 그래프에 넣기 전에 작업 범위를 걸러, UI와 워커가 같은 허용 범위를 보게 한다.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 거부 사유(자산 식별자는 제외). Allowed=true이면 비어 있다.
}

// EvaluateAssetGate는 작업 수준 게이트 판정을 수행한다.
//  1. 활성 blockRules 중 하나라도 히트하면 → 거부(가로채기 사유).
//  2. 그렇지 않고 allowRules에 활성 항목이 있는데 하나도 히트하지 않으면 → 거부(허용 범위 밖).
//  3. 그 외에는 통과.
//
// allowRules가 비어 있거나 활성 항목이 없으면 허용 게이트는 적용되지 않는다(화이트리스트를 켜지 않고 전부 통과).
// 「허용 규칙을 설정하지 않음」이 모든 자산을 막는 일을 피하기 위해서다.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "작업 허용(화이트리스트) 범위 밖이라 테스트를 허용하지 않음"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit는 게이트가 거부한 자산(가로채기 히트 또는 허용 범위 밖)을 기술한다.
// 거부된 항목은 자산 그래프에 반영되기 전에 UI가 차단 사유를 보여 주는 데 쓰인다.
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 사람이 읽을 수 있는 사유
}

// Describe는 읽을 수 있는 설명 한 줄을 반환한다. 자산 정보 + 사유.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules는 *DB 같은 이름 메서드의 통과 전달이며, AssetStore만 가진 호출자
// (예: agent 도구)도 규칙을 읽을 수 있게 한다.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept는 id로 자산을 불러와 하나씩 「먼저 가로채기, 그다음 허용」 게이트 판정을 하고, 거부된
// 자산을 모두 반환한다. 가로채기 규칙 = 전역 ∪ 작업 수준 block. 허용 규칙 = 작업 수준 allow(이 작업만).
// id가 없으면 바로 반환한다. 전역 GetByIDs(작업 범위 필터를 받지 않음)를 써서 가로채기가 scope에 약해지지 않게 한다.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// 가로채기 규칙도 없고 활성 허용 규칙도 없으면 → 판정할 필요 없이 전부 통과.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
