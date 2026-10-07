//go:build embedui

package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webuiDist는 정적으로 내보낸 화면을 담습니다(`cd web && npm run build:static`로
// 만든 뒤 server/webui/dist로 복사). `-tags embedui`로 빌드할 때만 바이너리에
// 들어갑니다. `all:` 접두사가 있어야 Next의 `_next/` 자산 디렉터리가 포함됩니다.
// 그 디렉터리는 밑줄로 시작해서, 접두사 없이는 embed가 빼먹습니다.
//
//go:embed all:webui/dist
var webuiDist embed.FS

// webuiHandler는 바이너리에 넣은 SPA를 제공합니다. 공개입니다(JWT 없음). 인증은
// 클라이언트와 /api에서 합니다. 경로마다 내보낸 index.html을 주고,
// 없는 경로는 index.html로 넘겨 클라이언트 라우팅이 되게 합니다.
// 초보용: 브라우저에 띄우는 화면입니다. 로그인 검사는 이 파일이 아니라 /api에서 합니다.
func (s *Server) webuiHandler() http.Handler {
	root, err := fs.Sub(webuiDist, "webui/dist")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		// 1) 파일이 그대로 있으면 그것을 줍니다(자산: _next/*, favicon.ico, ...)
		// 2) 경로 디렉터리 → <p>/index.html (trailingSlash로 내보낸 것)
		// 3) <p>.html 파일
		// 4) SPA 폴백 → index.html (클라이언트 라우터에 맡김)
		for _, cand := range []string{p, p + "/index.html", p + ".html"} {
			if serveFileIfExists(w, r, root, cand) {
				return
			}
		}
		http.ServeFileFS(w, r, root, "index.html")
	})
}

// serveFileIfExists는 name이 일반 파일로 있을 때만 fsys에서 그 파일을 줍니다.
func serveFileIfExists(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	st, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil || st.IsDir() {
		return false
	}
	http.ServeFileFS(w, r, fsys, name)
	return true
}
