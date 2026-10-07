package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// --- 자산 가로채기 규칙 생성·조회·수정·삭제 ---
//
// 전역 자산 가로채기 목록입니다. exact/fuzzy 도메인·ip·url 과 CIDR를 담습니다. 이 층은
// 규칙만 저장하고, 실제로 맞추고 막는 일은 다른 곳에 있습니다.
// 초보용: 화면의 전역 가로채기 목록입니다. 자산 그래프의 대상을 막을지는 여기서 정하지 않습니다.

func (s *Server) assetInterceptListRules(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	rules, err := pg.ListAssetInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rules == nil {
		rules = []db.AssetInterceptRule{}
	}
	writeJSON(w, 200, map[string]any{"rules": rules})
}

func (s *Server) assetInterceptCreateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req assetInterceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateAssetInterceptRuleReq(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.CreateAssetInterceptRule(req.Kind, req.Pattern, req.Note, req.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rule)
}

func (s *Server) assetInterceptUpdateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "규칙 id가 올바르지 않습니다")
		return
	}
	var req assetInterceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateAssetInterceptRuleReq(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.UpdateAssetInterceptRule(id, req.Kind, req.Pattern, req.Note, req.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rule)
}

func (s *Server) assetInterceptDeleteRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "규칙 id가 올바르지 않습니다")
		return
	}
	if err := pg.DeleteAssetInterceptRule(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) assetInterceptToggleRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "규칙 id가 올바르지 않습니다")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.ToggleAssetInterceptRule(id, req.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": req.Enabled})
}

// --- 도우미 ---

type assetInterceptRuleReq struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Note    string `json:"note"`
}

// validateAssetInterceptRuleReq는 규칙을 맞추고 검사합니다. pattern의 앞뒤 공백을 지웁니다.
// 형식이 엄격한 종류(exact_ip / cidr)는 잘못된 값을 거절합니다.
// fuzzy 종류와 domain/url 패턴은 자유 글로 둡니다.
// 그 글은 집행하는 쪽이 어떻게 읽을지 정합니다.
func validateAssetInterceptRuleReq(req *assetInterceptRuleReq) error {
	req.Pattern = strings.TrimSpace(req.Pattern)
	if req.Pattern == "" {
		return fmt.Errorf("pattern은 비울 수 없습니다")
	}
	switch req.Kind {
	case "exact_domain", "exact_url", "fuzzy_domain", "fuzzy_ip", "fuzzy_url":
		// 자유 형식입니다. 형식 검사는 하지 않습니다.
	case "exact_ip":
		if net.ParseIP(req.Pattern) == nil {
			return fmt.Errorf("exact_ip가 올바른 IP 주소가 아닙니다: %s", req.Pattern)
		}
	case "cidr":
		if _, _, err := net.ParseCIDR(req.Pattern); err != nil {
			return fmt.Errorf("cidr가 올바른 대역이 아닙니다(예: 192.168.0.0/16): %s", req.Pattern)
		}
	default:
		return fmt.Errorf("kind가 올바르지 않습니다: %s", req.Kind)
	}
	return nil
}
