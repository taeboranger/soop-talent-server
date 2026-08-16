package api

import (
	"os"
	"testing"
)

// redirect_uri 검증이 뚫리면 공격자가 피해자의 세션 토큰을 자기 서버로 받아낼 수 있다.
// 오픈 리다이렉트는 조용히 실패하므로 테스트로 고정한다.
func TestValidateRedirect(t *testing.T) {
	t.Setenv("OAUTH_REDIRECT_ALLOWLIST", "")

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
		"빈 값":            "",
		"http":           "http://abc.chromiumapp.org/",
		"임의 도메인":         "https://evil.example.com/steal",
		"호스트 접미사 위조":     "https://evil.com/chromiumapp.org",
		"서브도메인 위조":       "https://chromiumapp.org.evil.com/",
		"스킴 없음":          "//evil.example.com",
		"javascript 스킴":  "javascript:alert(1)",
		"데이터 URI":        "data:text/html,<script>",
	}
	for name, u := range denied {
		if err := validateRedirect(u); err == nil {
			t.Errorf("%s: 거부되어야 하는데 통과함 (%q)", name, u)
		}
	}
}

func TestValidateRedirect_허용목록(t *testing.T) {
	os.Setenv("OAUTH_REDIRECT_ALLOWLIST", "https://app.example.com/oauth")
	defer os.Unsetenv("OAUTH_REDIRECT_ALLOWLIST")

	if err := validateRedirect("https://app.example.com/oauth/done"); err != nil {
		t.Errorf("허용 목록의 주소가 거부됨: %v", err)
	}
	if err := validateRedirect("https://app.example.com.evil.com/oauth"); err == nil {
		t.Error("허용 목록 접두사를 흉내낸 주소가 통과함")
	}
}
