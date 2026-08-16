package api

import (
	"crypto/subtle"
	"errors"
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

// OAuth state는 DB의 1회용 값인 동시에 로그인을 시작한 브라우저에만 묶는다.
// DB state만 검사하면 공격자가 자기 OAuth 콜백 URL을 피해자에게 열어 피해자
// 브라우저를 공격자 계정으로 로그인시키는 login CSRF가 가능해진다.
const oauthStateCookieName = "__Host-sooptalent_oauth_state"

func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/oauth/providers", s.h(s.handleOAuthProviders))
	mux.Handle("GET /api/oauth/{provider}/start", s.h(s.handleOAuthStart))
	mux.Handle("GET /api/oauth/{provider}/callback", s.h(s.handleOAuthCallback))
	// 로그인 여부를 여기서 직접 판별해, 웹 세션이 없을 때도 확장이 자신의
	// chromiumapp.org 콜백에서 이유를 읽을 수 있게 한다. 유효 세션 없이는
	// 어떤 코드도 만들지 않으므로 공개 경로여도 권한이 생기지 않는다.
	mux.Handle("GET /api/extension/session/start", s.h(s.handleExtensionSessionStart))
	mux.Handle("POST /api/extension/session/exchange", s.h(s.handleExtensionSessionExchange))
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
	if _, enabled := s.OAuth.Config(p); !enabled {
		return httpError{http.StatusNotFound,
			fmt.Sprintf("%s 로그인이 설정되지 않았습니다.", p.Label())}
	}

	redirectURI := r.URL.Query().Get("redirect_uri")
	if err := validateRedirect(redirectURI); err != nil {
		return err
	}

	var codeChallenge, clientState string
	if isExtensionRedirect(redirectURI) {
		var err error
		codeChallenge, clientState, err = extensionFlowParams(r)
		if err != nil {
			return err
		}
	}

	state, err := auth.NewState(r.Context(), s.Store.Pool, string(p), redirectURI)
	if err != nil {
		return err
	}
	if isExtensionRedirect(redirectURI) {
		if err := auth.NewExtensionOAuthRequest(r.Context(), s.Store.Pool, state, codeChallenge, clientState); err != nil {
			return err
		}
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
// 웹은 HttpOnly 쿠키로, 확장은 1회용 교환 코드로 전달한다. 장기 세션 토큰을
// URL에 싣지 않아 브라우저 기록·확장 콜백·오류 수집기에 남지 않는다.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	if s.OAuth == nil {
		return httpError{http.StatusNotFound, "소셜 로그인이 설정되지 않았습니다."}
	}
	p, ok := oauth.Parse(r.PathValue("provider"))
	if !ok {
		return httpError{http.StatusNotFound, "알 수 없는 로그인 제공자입니다."}
	}

	q := r.URL.Query()
	state := q.Get("state")
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
	if code == "" {
		return bounce(w, r, redirectURI, url.Values{"error": {"인증 코드를 받지 못했습니다."}})
	}

	var extensionRequest *auth.ExtensionOAuthRequest
	if isExtensionRedirect(redirectURI) {
		extensionRequest, err = auth.ConsumeExtensionOAuthRequest(r.Context(), s.Store.Pool, state)
		if err != nil {
			return bounce(w, r, redirectURI, url.Values{"error": {"확장 로그인 요청이 만료되었습니다. 다시 시도해 주세요."}})
		}
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
	// 콜백 응답은 서비스 도메인에서 내려간다. 여기서 HttpOnly 웹 세션 쿠키를
	// 함께 설정하면 확장 OAuth 로그인 뒤 웹을 열어도 바로 로그인돼 있다.
	setSessionCookie(w, token)
	if isServiceRedirect(redirectURI) {
		http.Redirect(w, r, redirectURI, http.StatusFound)
		return nil
	}
	if isExtensionRedirect(redirectURI) {
		code, err := auth.NewExtensionCode(r.Context(), s.Store.Pool, acct.ID, redirectURI,
			extensionRequest.CodeChallenge, extensionRequest.ClientState)
		if err != nil {
			return err
		}
		return bounce(w, r, redirectURI, url.Values{
			"code":         {code},
			"client_state": {extensionRequest.ClientState},
			"email":        {acct.Email},
		})
	}
	// 외부 허용 주소로 장기 Bearer 토큰을 전달하지 않는다. 서비스 도메인의
	// HttpOnly 쿠키는 이미 설정됐으며, 외부 주소는 완료 안내만 할 수 있다.
	return bounce(w, r, redirectURI, url.Values{
		"logged_in": {"1"},
	})
}

// handleExtensionSessionStart는 웹의 HttpOnly 세션을 확장용 1회 코드로
// 바꾼다. 이 URL은 chrome.identity의 최상위 인증 창으로 열리므로 웹 쿠키를
// 직접 확장에 노출하지 않고도 읽을 수 있다.
func (s *Server) handleExtensionSessionStart(w http.ResponseWriter, r *http.Request) error {
	redirectURI := r.URL.Query().Get("redirect_uri")
	if err := validateExtensionRedirect(redirectURI); err != nil {
		return err
	}
	codeChallenge, clientState, err := extensionFlowParams(r)
	if err != nil {
		return err
	}
	acct, err := auth.Resolve(r.Context(), s.Store.Pool, sessionToken(r))
	if errors.Is(err, auth.ErrNoSession) || errors.Is(err, auth.ErrDisabled) {
		return bounce(w, r, redirectURI, url.Values{
			"error":        {"web_session_missing"},
			"client_state": {clientState},
		})
	}
	if err != nil {
		return err
	}
	code, err := auth.NewExtensionCode(r.Context(), s.Store.Pool, acct.ID, redirectURI, codeChallenge, clientState)
	if err != nil {
		return err
	}
	return bounce(w, r, redirectURI, url.Values{"code": {code}, "client_state": {clientState}})
}

func (s *Server) handleExtensionSessionExchange(w http.ResponseWriter, r *http.Request) error {
	// 응답에는 새 Bearer 세션이 들어가므로 브라우저·프록시 캐시를 명시적으로 막는다.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	var req struct {
		Code         string `json:"code"`
		RedirectURI  string `json:"redirectUri"`
		CodeVerifier string `json:"codeVerifier"`
		ClientState  string `json:"clientState"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := validateExtensionRedirect(req.RedirectURI); err != nil {
		return err
	}
	token, acct, err := auth.ExchangeExtensionCode(r.Context(), s.Store.Pool,
		req.Code, req.RedirectURI, req.CodeVerifier, req.ClientState, r.UserAgent())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "email": acct.Email})
	return nil
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
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return httpError{http.StatusBadRequest, "redirect_uri가 올바르지 않습니다."}
	}
	if isExtensionRedirect(raw) {
		return nil
	}
	publicURL := strings.TrimSpace(os.Getenv("PUBLIC_URL"))
	if publicURL != "" && allowedLocation(raw, publicURL) {
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

// allowedExtensionRedirect는 설치하지 않은 악성 확장이 웹 로그인 쿠키를 자기
// 확장 세션으로 바꾸지 못하게, 운영자가 등록한 chromiumapp.org 주소만 허용한다.
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

func isServiceRedirect(raw string) bool {
	publicURL := strings.TrimSpace(os.Getenv("PUBLIC_URL"))
	return publicURL != "" && allowedLocation(raw, publicURL)
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
	if err != nil || a.User != nil || a.Fragment != "" || a.Scheme != "https" {
		return false
	}
	if u.Scheme != a.Scheme || u.Host != a.Host {
		return false
	}
	base := strings.TrimRight(a.EscapedPath(), "/")
	path := u.EscapedPath()
	return base == "" || path == base || strings.HasPrefix(path, base+"/")
}

func extensionFlowParams(r *http.Request) (codeChallenge, clientState string, err error) {
	codeChallenge = r.URL.Query().Get("code_challenge")
	clientState = r.URL.Query().Get("client_state")
	if !flowValue(codeChallenge, 43, 128) || !flowValue(clientState, 32, 128) {
		return "", "", httpError{http.StatusBadRequest, "확장 로그인 PKCE 요청이 올바르지 않습니다."}
	}
	return codeChallenge, clientState, nil
}

func flowValue(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
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
