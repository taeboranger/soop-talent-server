package api

import (
	_ "embed"
	"net/http"
)

// 관리자 화면. 단일 HTML 파일이며 인증은 세션 토큰(localStorage)으로 한다.
// 쿠키를 쓰지 않으므로 CSRF를 신경 쓸 필요가 없고,
// 페이지 자체는 공개돼 있어도 관리자 토큰 없이는 아무것도 조회되지 않는다.
//
//go:embed admin.html
var adminHTML []byte

// 스트리머 대시보드. 로그인·달란트 기준·달란트 내역·직접 지급·달란트 시장을 담는다.
//
//go:embed dashboard.html
var dashboardHTML []byte

// 사용 가이드. 스트리머용 기본 안내와, 규칙·원리를 담은 자세한 안내로 나뉜다.
//
//go:embed guide.html
var guideHTML []byte

//go:embed guide-detail.html
var guideDetailHTML []byte

func (s *Server) adminPage() http.Handler { return pageHandler(adminHTML) }

func (s *Server) guidePage() http.Handler       { return pageHandler(guideHTML) }
func (s *Server) guideDetailPage() http.Handler { return pageHandler(guideDetailHTML) }

func (s *Server) dashboardPage() http.Handler {
	inner := pageHandler(dashboardHTML)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "GET /"는 다른 경로와 겹치지 않는 요청을 전부 받는다.
		// 없는 주소에 대시보드를 돌려주지 않도록 정확히 루트일 때만 응답한다.
		if r.URL.Path != "/" {
			s.writeError(w, r, httpError{http.StatusNotFound, "페이지를 찾을 수 없습니다."})
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// pageHandler는 인라인 CSS/JS만 허용하는 정적 페이지를 서빙한다.
// 외부 리소스를 전부 차단해 공급망 표면을 없앤다.
func pageHandler(body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
				"connect-src 'self'; base-uri 'none'; form-action 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	})
}
