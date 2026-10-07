//go:build !embedui

package server

import "net/http"

// webuiHandler는 화면을 넣지 않는 기본 빌드의 빈 처리기입니다. 이 바이너리에는
// 화면이 들어 있지 않습니다. 개발 중에는 `cd web && npm run dev`로 따로 띄우세요.
// 한 파일로 묶으려면 이렇게 빌드합니다.
//
//	cd web && npm run build:static     # web/out 을 만듭니다
//	cp -r web/out server/webui/dist    # (또는 빌드 스크립트를 사용)
//	go build -tags embedui ./cmd/artex  # 화면을 넣은 단일 바이너리
func (s *Server) webuiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "이 바이너리에는 프론트엔드가 들어 있지 않습니다(개발은 next dev, 배포는 -tags embedui로 빌드).", http.StatusNotFound)
	})
}
