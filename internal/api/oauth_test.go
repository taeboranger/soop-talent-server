package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// redirect_uri 검증이 뚫리면 공격자가 피해자의 세션 토큰을 자기 서버로 받아낼 수 있다.
// 오픈 리다이렉트는 조용히 실패하므로 테스트로 고정한다.
func TestValidateRedirect(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_ALLOWLIST", "")
	t.Setenv("EXTENSION_REDIRECT_ALLOWLIST", "https://abcdefghijklmnop.chromiumapp.org/")
	t.Setenv("PUBLIC_URL", "")

	allowed := []string{
		"https://abcdefghijklmnop.chromiumapp.org/",
		"https://abcdefghijklmnop.chromiumapp.org/provider_cb",
	}
	for _, u := range allowed {
		if err := validateRedirect(u); err != nil {
			t.Errorf("허용되어야 함: %q (%v)", u, err)
		}
	}

	denied := map[string]string{
		"빈 값":           "",
		"http":          "http://abc.chromiumapp.org/",
		"임의 도메인":        "https://evil.example.com/steal",
		"호스트 접미사 위조":    "https://evil.com/chromiumapp.org",
		"서브도메인 위조":      "https://chromiumapp.org.evil.com/",
		"스킴 없음":         "//evil.example.com",
		"javascript 스킴": "javascript:alert(1)",
		"데이터 URI":       "data:text/html,<script>",
	}
	for name, u := range denied {
		if err := validateRedirect(u); err == nil {
			t.Errorf("%s: 거부되어야 하는데 통과함 (%q)", name, u)
		}
	}
}

func TestValidateRedirect_OnlyRegisteredExtension(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_ALLOWLIST", "")
	t.Setenv("EXTENSION_REDIRECT_ALLOWLIST", "https://abcdefghijklmnop.chromiumapp.org/")
	t.Setenv("PUBLIC_URL", "")
	if err := validateRedirect("https://abcdefghijklmnop.chromiumapp.org/session"); err != nil {
		t.Fatalf("registered extension denied: %v", err)
	}
	if err := validateRedirect("https://ponmlkjihgfedcba.chromiumapp.org/"); err == nil {
		t.Fatal("unregistered extension must be denied")
	}
}

func TestValidateRedirect_허용목록(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_ALLOWLIST", "https://app.example.com/oauth")
	t.Setenv("EXTENSION_REDIRECT_ALLOWLIST", "")
	t.Setenv("PUBLIC_URL", "")

	if err := validateRedirect("https://app.example.com/oauth/done"); err != nil {
		t.Errorf("허용 목록의 주소가 거부됨: %v", err)
	}
	if err := validateRedirect("https://app.example.com.evil.com/oauth"); err == nil {
		t.Error("허용 목록 접두사를 흉내낸 주소가 통과함")
	}
	if err := validateRedirect("https://app.example.com/oauth.evil"); err == nil {
		t.Error("허용 경로 접두사를 흉내낸 주소가 통과함")
	}
	if err := validateRedirect("https://app.example.com/other"); err == nil {
		t.Error("허용 경로 밖 주소가 통과함")
	}
}

func TestValidateRedirect_로컬PublicURL만HTTP허용(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_ALLOWLIST", "")
	t.Setenv("EXTENSION_REDIRECT_ALLOWLIST", "")
	t.Setenv("PUBLIC_URL", "http://localhost:8099")

	if err := validateRedirect("http://localhost:8099/"); err != nil {
		t.Fatalf("로컬 PUBLIC_URL이 거부됨: %v", err)
	}
	if err := validateRedirect("http://localhost:8099/admin"); err != nil {
		t.Fatalf("로컬 PUBLIC_URL 하위 경로가 거부됨: %v", err)
	}
	for _, raw := range []string{
		"http://localhost:8100/",
		"http://127.0.0.1:8099/",
		"http://evil.example.com/",
		"http://localhost.evil.example.com:8099/",
	} {
		if err := validateRedirect(raw); err == nil {
			t.Errorf("허용되지 않은 HTTP 주소가 통과함: %s", raw)
		}
	}
}

func TestOAuthStateCookieMatches(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/oauth/google/callback?state=state", nil)
	r.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "state"})
	if !oauthStateCookieMatches(r, "state") {
		t.Fatal("matching state cookie rejected")
	}
	if oauthStateCookieMatches(r, "other") {
		t.Fatal("different state cookie accepted")
	}
}

func TestOAuthCallbackState_SOOP은HttpOnly쿠키사용(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/oauth/soop/callback?code=one-time", nil)
	r.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "cookie-state"})
	if got := oauthCallbackState(r, "soop"); got != "cookie-state" {
		t.Fatalf("state=%q, want cookie-state", got)
	}

	// state를 정상적으로 돌려주는 제공자는 콜백 값을 우선한다.
	r = httptest.NewRequest("GET", "/api/oauth/google/callback?state=query-state", nil)
	r.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "cookie-state"})
	if got := oauthCallbackState(r, "google"); got != "query-state" {
		t.Fatalf("state=%q, want query-state", got)
	}
}
