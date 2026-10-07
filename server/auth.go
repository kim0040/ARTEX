package server

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	jwtTTL         = 7 * 24 * time.Hour
	keyChars       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// loadOrCreateJWTKey는 keyDir/jwt.key에서 32바이트 서명 키를 읽습니다. keyDir는
// 실행 파일 옆의 프로젝트 기본 디렉터리입니다. 브라우저로 볼 수 있는 작업 공간 루트
// (dataDir)가 아닙니다. 서명 키는 파일 관리자에서 보이거나 내려받을 수 있으면 안 됩니다.
// 예전 설치는 dataDir/jwt.key에 두었습니다. 거기 있고 새 위치에는 아직 없으면
// 키를 그대로 옮깁니다. 그래서 세션은 유지되고, 옛 파일은 지워 작업 공간에서 사라집니다.
// 처음 실행이면 무작위 키를 만들어
// 파일에 저장해 둡니다.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	// 작업 공간 안에 있던 옛 위치에서 한 번만 옮깁니다.
	if legacy := filepath.Join(dataDir, jwtKeyFilename); legacy != path {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if data, rerr := os.ReadFile(legacy); rerr == nil {
				if werr := os.WriteFile(path, data, 0o600); werr == nil {
					_ = os.Remove(legacy)
					log.Printf("[auth] JWT key를 %s에서 %s(으)로 옮겼습니다(탐색 가능한 작업 공간 밖으로 이동)", legacy, path)
				}
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] 새 JWT key를 %s에 기록했습니다", path)
	return buf, nil
}

// signJWT는 사용자 ARTEX용으로 7일짜리 HS256 토큰을 발급합니다.
func signJWT(key []byte) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(key)
}

// verifyJWT는 tokenStr이 유효하고 만료되지 않은 HS256 토큰이면 true를 돌려줍니다.
func verifyJWT(tokenStr string, key []byte) bool {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	})
	return err == nil && t.Valid
}

// extractToken은 Authorization: Bearer 헤더,
// artex_token 쿠키, 또는 ?token= 쿼리(SSE 연결용)에서 JWT를 읽습니다.
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("artex_token"); err == nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// requireAuth는 h를 JWT 검사로 감쌉니다.
// /api/auth/* 와 /api/health 는 검사하지 않습니다.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" {
			h.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		if tok == "" {
			writeErr(w, 401, "권한 없음")
			return
		}
		if !verifyJWT(tok, s.jwtKey) {
			writeErr(w, 401, "token이 유효하지 않거나 만료되었습니다")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// GET /api/auth/status — 관리자 비밀번호를 이미 설정했는지 알려 줍니다.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, _ := pg.GetSetting(authPassKey)
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// POST /api/auth/init — 비밀번호를 처음 설정합니다. 이미 있으면 거절합니다.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, _ := pg.GetSetting(authPassKey)
	if existing != "" {
		writeErr(w, 403, "비밀번호가 설정되었습니다")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeErr(w, 400, "비밀번호는 비워 둘 수 없습니다")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "비밀번호 암호화에 실패했습니다")
		return
	}
	if err := pg.SetSetting(authPassKey, string(hash)); err != nil {
		writeErr(w, 500, "저장 실패: "+err.Error())
		return
	}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "token 생성에 실패했습니다")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}

// POST /api/auth/change-password — 관리자 비밀번호를 바꿉니다. 유효한
// 토큰이 필요합니다. 이 경로는 /api/auth/* 라 requireAuth가 빼 두므로, 여기서
// 토큰을 검사합니다. 현재 비밀번호도 함께 있어야 합니다.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if !verifyJWT(extractToken(r), s.jwtKey) {
		writeErr(w, 401, "권한 없음")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "요청 형식 오류")
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "새 비밀번호는 비워 둘 수 없습니다")
		return
	}
	hash, ok, _ := pg.GetSetting(authPassKey)
	if !ok || hash == "" {
		writeErr(w, 403, "비밀번호가 초기화되지 않았습니다. 먼저 비밀번호를 설정하세요")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeErr(w, 401, "현재 비밀번호가 올바르지 않습니다")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "비밀번호 암호화에 실패했습니다")
		return
	}
	if err := pg.SetSetting(authPassKey, string(newHash)); err != nil {
		writeErr(w, 500, "저장 실패: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/login — 사용자 이름과 비밀번호를 확인하고 JWT를 돌려줍니다.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "요청 형식 오류")
		return
	}
	if req.Username != "ARTEX" {
		writeErr(w, 401, "사용자 이름 또는 비밀번호가 올바르지 않습니다")
		return
	}
	hash, ok, _ := pg.GetSetting(authPassKey)
	if !ok || hash == "" {
		writeErr(w, 403, "비밀번호가 초기화되지 않았습니다. 먼저 비밀번호를 설정하세요")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		writeErr(w, 401, "사용자 이름 또는 비밀번호가 올바르지 않습니다")
		return
	}
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "token 생성에 실패했습니다")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}
