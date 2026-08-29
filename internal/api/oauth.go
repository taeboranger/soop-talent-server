package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
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

// OAuth state는 DB의 1회용 값인 동시에 로그인을 시작한 브라우저에만 묶는다.
// DB state만 검사하면 공격자가 자기 OAuth 콜백 URL을 피해자에게 열어 피해자
// 브라우저를 공격자 계정으로 로그인시키는 login CSRF가 가능해진다.
const oauthStateCookieName = "__Host-sooptalent_oauth_state"

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
	w.Header().Set("Cache-Control", "no-store")
	if s.OAuth == nil {
		return httpError{http.StatusNotFound, "소셜 로그인이 설정되지 않았습니다."}
	}
	p, ok := oauth.Parse(r.PathValue("provider"))
	if !ok {
		return httpError{http.StatusNotFound, "알 수 없는 로그인 제공자입니다."}
	}
	if p != oauth.Soop {
		return httpError{http.StatusNotFound, "SOOP 로그인만 사용할 수 있습니다."}
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
	setOAuthStateCookie(w, state)
	authURL, err := s.OAuth.AuthCodeURL(p, state)
	if err != nil {
		return httpError{http.StatusInternalServerError, err.Error()}
	}
	http.Redirect(w, r, authURL, http.StatusFound)
	return nil
}

// handleOAuthCallback은 제공자가 돌려보낸 코드를 세션 토큰으로 바꾼다.
// 웹과 확장은 각자 OAuth 결과의 프래그먼트에서 자기 세션 토큰을 보관한다.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	if s.OAuth == nil {
		return httpError{http.StatusNotFound, "소셜 로그인이 설정되지 않았습니다."}
	}
	p, ok := oauth.Parse(r.PathValue("provider"))
	if !ok {
		return httpError{http.StatusNotFound, "알 수 없는 로그인 제공자입니다."}
	}
	if p != oauth.Soop {
		return httpError{http.StatusNotFound, "SOOP 로그인만 사용할 수 있습니다."}
	}

	q := r.URL.Query()
	state := oauthCallbackState(r, p)
	if !oauthStateCookieMatches(r, state) {
		clearOAuthStateCookie(w)
		return httpError{http.StatusBadRequest, "이 브라우저에서 시작한 로그인 요청이 아닙니다. 다시 로그인해 주세요."}
	}
	clearOAuthStateCookie(w)

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
	// 구 Chat SDK 문서는 authCode라는 이름을 사용했다. 전환기 응답도 받는다.
	if code == "" && p == oauth.Soop {
		code = q.Get("authCode")
	}
	if code == "" {
		return bounce(w, r, redirectURI, url.Values{"error": {"인증 코드를 받지 못했습니다."}})
	}

	info, err := s.OAuth.Exchange(r.Context(), p, code, state)
	if err != nil {
		s.Log.Warn("소셜 로그인 실패", "provider", p, "err", err)
		if p == oauth.Soop && errors.Is(err, oauth.ErrSOOPLiveRequired) {
			return bounce(w, r, redirectURI, url.Values{"error": {"SOOP 로그인은 현재 방송을 켠 상태에서 완료할 수 있습니다. 방송을 시작한 뒤 다시 시도해 주세요."}})
		}
		return bounce(w, r, redirectURI, url.Values{"error": {"로그인 처리에 실패했습니다."}})
	}

	var token string
	var acct *auth.Account
	if p == oauth.Soop {
		token, acct, err = auth.LoginWithVerifiedSOOP(r.Context(), s.Store.Pool, info.ID, info.Nickname, r.UserAgent())
	} else {
		token, acct, err = auth.LoginWithSocial(r.Context(), s.Store.Pool, auth.SocialUser{
			Provider: string(info.Provider), ID: info.ID, Email: info.Email,
			EmailVerified: info.EmailVerified, Nickname: info.Nickname,
		}, r.UserAgent())
	}
	if err != nil {
		s.Log.Warn("소셜 계정 처리 실패", "provider", p, "err", err)
		return bounce(w, r, redirectURI, url.Values{"error": {err.Error()}})
	}
	if p == oauth.Soop {
		if info.Tokens == nil {
			return bounce(w, r, redirectURI, url.Values{"error": {"SOOP 채팅 권한 토큰을 받지 못했습니다. 다시 로그인해 주세요."}})
		}
		if err := s.OAuth.SaveSOOPTokens(r.Context(), s.Store.Pool, acct.ID, info.ID, *info.Tokens); err != nil {
			s.Log.Error("SOOP OAuth 토큰 저장 실패", "account", acct.ID, "bid", info.ID, "err", err)
			return bounce(w, r, redirectURI, url.Values{"error": {"SOOP 연동 정보를 안전하게 저장하지 못했습니다. 운영자에게 문의해 주세요."}})
		}
		// 시청자는 토큰을 보관해 두되, 로그인만으로 방송국 수집이나 시장 관리
		// 권한을 얻지는 않는다. 이미 활성화한 스트리머만 즉시 수집을 재개한다.
		if bid, claimErr := auth.ApprovedBID(r.Context(), s.Store.Pool, acct.ID); claimErr == nil {
			if official, ok := s.Streamer.(OfficialChatHook); ok {
				official.PromoteOfficial(context.WithoutCancel(r.Context()), bid)
			}
			if err := s.Store.EnsureStreamer(r.Context(), bid, info.Nickname); err != nil {
				s.Log.Warn("SOOP OAuth 스트리머 등록 실패", "bid", bid, "err", err)
			} else if s.Streamer != nil {
				s.Streamer.Add(context.WithoutCancel(r.Context()), bid)
			}
		} else if !errors.Is(claimErr, auth.ErrNoApprovedBid) {
			return bounce(w, r, redirectURI, url.Values{"error": {"스트리머 연결 상태를 확인하지 못했습니다."}})
		}
	}

	s.Log.Info("소셜 로그인", "provider", p, "account", acct.ID)
	return bounce(w, r, redirectURI, url.Values{
		"token": {token},
		"id":    {acct.SoopID},
	})
}

func oauthCallbackState(r *http.Request, p oauth.Provider) string {
	state := r.URL.Query().Get("state")
	// SOOP 공식 auth/code는 state를 명세하지 않는다. 로그인 시작 시 만든
	// HttpOnly 쿠키의 1회용 state를 대신 사용한다.
	if p == oauth.Soop && state == "" {
		if c, err := r.Cookie(oauthStateCookieName); err == nil {
			return c.Value
		}
	}
	return state
}

// bounce는 확장 또는 명시적으로 허용한 웹 주소로 결과를 돌려보낸다.
// 값은 프래그먼트(#)에 실어 서버 접근 로그에 민감 값이 남지 않게 한다.
func bounce(w http.ResponseWriter, r *http.Request, redirectURI string, v url.Values) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, redirectURI+"#"+v.Encode(), http.StatusFound)
	return nil
}

// validateRedirect는 로그인 결과를 돌려보낼 주소를 검증한다.
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
	if err != nil || u.Host == "" {
		return httpError{http.StatusBadRequest, "redirect_uri가 올바르지 않습니다."}
	}
	// 운영 서비스와 확장은 HTTPS만 허용한다. 공식 OAuth 앱에 등록한 로컬
	// 개발 콜백은 http://localhost 형태이므로 PUBLIC_URL과 정확히 일치하는
	// 루프백 주소에 한해서만 먼저 허용한다.
	publicURL := strings.TrimSpace(os.Getenv("PUBLIC_URL"))
	if publicURL != "" && allowedLocation(raw, publicURL) {
		return nil
	}
	if u.Scheme != "https" {
		return httpError{http.StatusBadRequest, "redirect_uri가 올바르지 않습니다."}
	}
	if isExtensionRedirect(raw) {
		return nil
	}
	for _, allowed := range strings.Split(os.Getenv("OAUTH_REDIRECT_ALLOWLIST"), ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && allowedLocation(raw, allowed) {
			return nil
		}
	}
	return httpError{http.StatusBadRequest, "허용되지 않은 redirect_uri입니다."}
}

func validateExtensionRedirect(raw string) error {
	if !isExtensionRedirect(raw) {
		return httpError{http.StatusBadRequest, "확장 프로그램 리다이렉트 주소가 올바르지 않습니다."}
	}
	return nil
}

func isExtensionRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" &&
		strings.HasSuffix(u.Hostname(), extensionRedirectHost) && allowedExtensionRedirect(raw)
}

// allowedExtensionRedirect는 설치하지 않은 확장이 OAuth 로그인 결과를 받지 못하게,
// 운영자가 등록한 chromiumapp.org 주소만 허용한다.
// 값은 "https://<확장ID>.chromiumapp.org/" 형태를 쉼표로 넣는다.
func allowedExtensionRedirect(raw string) bool {
	for _, allowed := range strings.Split(os.Getenv("EXTENSION_REDIRECT_ALLOWLIST"), ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && allowedLocation(raw, allowed) {
			return true
		}
	}
	return false
}

// allowedLocation은 문자열 접두사 대신 URL의 scheme·host·포트와 경로 경계를
// 비교한다. "https://app.example.com" 설정에 ".evil.com"을 덧붙여 우회하는
// open redirect를 막는다.
func allowedLocation(raw, allowed string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	a, err := url.Parse(allowed)
	if err != nil || a.User != nil || a.Fragment != "" ||
		(a.Scheme != "https" && !(a.Scheme == "http" && isLoopbackHost(a.Hostname()))) {
		return false
	}
	if u.Scheme != a.Scheme || u.Host != a.Host {
		return false
	}
	base := strings.TrimRight(a.EscapedPath(), "/")
	path := u.EscapedPath()
	return base == "" || path == base || strings.HasPrefix(path, base+"/")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func setOAuthStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookieName, Value: state, Path: "/",
		MaxAge: 10 * 60, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func clearOAuthStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookieName, Value: "", Path: "/",
		MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func oauthStateCookieMatches(r *http.Request, state string) bool {
	c, err := r.Cookie(oauthStateCookieName)
	return err == nil && state != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) == 1
}
