package api

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sooptalent/server/internal/auth"
	"github.com/sooptalent/server/internal/oauth"
)

// 확장의 chrome.identity.launchWebAuthFlow는 이 도메인으로 돌아온다.
// https://<확장ID>.chromiumapp.org/ 형태이며 실제로 존재하는 서버는 아니다.
const extensionRedirectHost = ".chromiumapp.org"

func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/oauth/providers", s.h(s.handleOAuthProviders))
	mux.Handle("GET /api/oauth/{provider}/start", s.h(s.handleOAuthStart))
	mux.Handle("GET /api/oauth/{provider}/callback", s.h(s.handleOAuthCallback))
}

// handleOAuthProviders는 설정된 제공자만 알려준다.
// 확장은 이 목록으로 버튼을 그리므로, 자격증명이 없는 제공자는 화면에 뜨지 않는다.
func (s *Server) handleOAuthProviders(w http.ResponseWriter, r *http.Request) error {
	if s.OAuth == nil {
		writeJSON(w, http.StatusOK, []any{})
		return nil
	}
	out := []map[string]string{}
	for _, p := range s.OAuth.Enabled() {
		out = append(out, map[string]string{"id": string(p), "label": p.Label()})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleOAuthStart는 사용자를 제공자 로그인 화면으로 보낸다.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) error {
	if s.OAuth == nil {
		return httpError{http.StatusNotFound, "소셜 로그인이 설정되지 않았습니다."}
	}
	p, ok := oauth.Parse(r.PathValue("provider"))
	if !ok {
		return httpError{http.StatusNotFound, "알 수 없는 로그인 제공자입니다."}
	}
	if _, enabled := s.OAuth.Config(p); !enabled {
		return httpError{http.StatusNotFound,
			fmt.Sprintf("%s 로그인이 설정되지 않았습니다.", p.Label())}
	}

	redirectURI := r.URL.Query().Get("redirect_uri")
	if err := validateRedirect(redirectURI); err != nil {
		return err
	}

	state, err := auth.NewState(r.Context(), s.Store.Pool, string(p), redirectURI)
	if err != nil {
		return err
	}
	authURL, err := s.OAuth.AuthCodeURL(p, state)
	if err != nil {
		return httpError{http.StatusInternalServerError, err.Error()}
	}
	http.Redirect(w, r, authURL, http.StatusFound)
	return nil
}

// handleOAuthCallback은 제공자가 돌려보낸 코드를 세션 토큰으로 바꾼다.
// 결과는 확장 리다이렉트 주소의 **프래그먼트**로 전달한다 —
// 프래그먼트는 서버 로그나 Referer에 남지 않는다.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) error {
	if s.OAuth == nil {
		return httpError{http.StatusNotFound, "소셜 로그인이 설정되지 않았습니다."}
	}
	p, ok := oauth.Parse(r.PathValue("provider"))
	if !ok {
		return httpError{http.StatusNotFound, "알 수 없는 로그인 제공자입니다."}
	}

	q := r.URL.Query()
	state := q.Get("state")

	// state는 1회용이다. 성공하든 실패하든 여기서 소비된다.
	redirectURI, err := auth.ConsumeState(r.Context(), s.Store.Pool, state, string(p))
	if err != nil {
		return httpError{http.StatusBadRequest, auth.ErrBadState.Error()}
	}

	// 사용자가 동의를 거부한 경우
	if e := q.Get("error"); e != "" {
		return bounce(w, r, redirectURI, url.Values{"error": {"로그인이 취소되었습니다."}})
	}
	code := q.Get("code")
	if code == "" {
		return bounce(w, r, redirectURI, url.Values{"error": {"인증 코드를 받지 못했습니다."}})
	}

	info, err := s.OAuth.Exchange(r.Context(), p, code, state)
	if err != nil {
		s.Log.Warn("소셜 로그인 실패", "provider", p, "err", err)
		return bounce(w, r, redirectURI, url.Values{"error": {"로그인 처리에 실패했습니다."}})
	}

	token, acct, err := auth.LoginWithSocial(r.Context(), s.Store.Pool, auth.SocialUser{
		Provider: string(info.Provider), ID: info.ID, Email: info.Email,
		EmailVerified: info.EmailVerified, Nickname: info.Nickname,
	}, r.UserAgent())
	if err != nil {
		s.Log.Warn("소셜 계정 처리 실패", "provider", p, "err", err)
		return bounce(w, r, redirectURI, url.Values{"error": {err.Error()}})
	}

	s.Log.Info("소셜 로그인", "provider", p, "account", acct.ID)
	return bounce(w, r, redirectURI, url.Values{
		"token": {token},
		"email": {acct.Email},
	})
}

// bounce는 확장으로 결과를 돌려보낸다.
// 값은 프래그먼트(#)에 실어 서버 접근 로그에 토큰이 남지 않게 한다.
func bounce(w http.ResponseWriter, r *http.Request, redirectURI string, v url.Values) error {
	http.Redirect(w, r, redirectURI+"#"+v.Encode(), http.StatusFound)
	return nil
}

// validateRedirect는 토큰을 돌려보낼 주소를 검증한다.
//
// 이 검사가 없으면 공격자가 임의 주소를 redirect_uri로 넘겨
// 피해자의 세션 토큰을 자기 서버로 받아낼 수 있다(오픈 리다이렉트).
// 확장의 chromiumapp.org 주소와 이 서비스 자신의 PUBLIC_URL을 기본 허용하고,
// 필요하면 OAUTH_REDIRECT_ALLOWLIST로 추가한다.
func validateRedirect(raw string) error {
	if raw == "" {
		return httpError{http.StatusBadRequest, "redirect_uri가 필요합니다."}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return httpError{http.StatusBadRequest, "redirect_uri가 올바르지 않습니다."}
	}
	if strings.HasSuffix(u.Hostname(), extensionRedirectHost) {
		return nil
	}
	publicURL := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	if publicURL != "" && (raw == publicURL || strings.HasPrefix(raw, publicURL+"/")) {
		return nil
	}
	for _, allowed := range strings.Split(os.Getenv("OAUTH_REDIRECT_ALLOWLIST"), ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && strings.HasPrefix(raw, allowed) {
			return nil
		}
	}
	return httpError{http.StatusBadRequest, "허용되지 않은 redirect_uri입니다."}
}
