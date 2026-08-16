// Package oauth는 Google / Naver / Kakao 소셜 로그인을 다룬다.
//
// 자격증명은 전부 환경변수에서 읽는다. 설정되지 않은 제공자는 비활성 상태가 되고,
// 확장에는 사용 가능한 제공자만 노출된다.
//
// 각 제공자 콘솔에 등록할 Redirect URI:
//
//	{PUBLIC_URL}/api/oauth/google/callback
//	{PUBLIC_URL}/api/oauth/naver/callback
//	{PUBLIC_URL}/api/oauth/kakao/callback
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Provider string

const (
	Google Provider = "google"
	Naver  Provider = "naver"
	Kakao  Provider = "kakao"
)

// UserInfo는 제공자마다 다른 응답을 통일한 형태다.
type UserInfo struct {
	Provider      Provider
	ID            string // 제공자 내부 고유 식별자. 이메일이 바뀌어도 변하지 않는다.
	Email         string
	EmailVerified bool
	Nickname      string
}

type Config struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
	Scopes       []string
}

func (c Config) Enabled() bool { return c.ClientID != "" && c.ClientSecret != "" }

// Registry는 설정된 제공자들을 담는다.
type Registry struct {
	configs   map[Provider]Config
	publicURL string
	http      *http.Client
}

// NewRegistry는 환경변수에서 자격증명을 읽는다.
//
//	GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET
//	NAVER_CLIENT_ID  / NAVER_CLIENT_SECRET
//	KAKAO_CLIENT_ID  / KAKAO_CLIENT_SECRET
//	PUBLIC_URL       콜백 주소를 만들 때 쓰는 서버 공개 주소
func NewRegistry(publicURL string) *Registry {
	return &Registry{
		publicURL: strings.TrimRight(publicURL, "/"),
		http:      &http.Client{Timeout: 15 * time.Second},
		configs: map[Provider]Config{
			Google: {
				ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
				ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
				AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL:     "https://oauth2.googleapis.com/token",
				UserInfoURL:  "https://www.googleapis.com/oauth2/v3/userinfo",
				Scopes:       []string{"openid", "email", "profile"},
			},
			Naver: {
				ClientID:     os.Getenv("NAVER_CLIENT_ID"),
				ClientSecret: os.Getenv("NAVER_CLIENT_SECRET"),
				AuthURL:      "https://nid.naver.com/oauth2.0/authorize",
				TokenURL:     "https://nid.naver.com/oauth2.0/token",
				UserInfoURL:  "https://openapi.naver.com/v1/nid/me",
			},
			Kakao: {
				ClientID:     os.Getenv("KAKAO_CLIENT_ID"),
				ClientSecret: os.Getenv("KAKAO_CLIENT_SECRET"),
				AuthURL:      "https://kauth.kakao.com/oauth/authorize",
				TokenURL:     "https://kauth.kakao.com/oauth/token",
				UserInfoURL:  "https://kapi.kakao.com/v2/user/me",
				Scopes:       []string{"account_email"},
			},
		},
	}
}

func (r *Registry) Config(p Provider) (Config, bool) {
	c, ok := r.configs[p]
	return c, ok && c.Enabled()
}

// Enabled는 사용 가능한 제공자 목록이다. 확장이 버튼을 그릴 때 쓴다.
func (r *Registry) Enabled() []Provider {
	var out []Provider
	for _, p := range []Provider{Google, Naver, Kakao} {
		if c, ok := r.configs[p]; ok && c.Enabled() {
			out = append(out, p)
		}
	}
	return out
}

func (r *Registry) CallbackURL(p Provider) string {
	return fmt.Sprintf("%s/api/oauth/%s/callback", r.publicURL, p)
}

// AuthCodeURL은 사용자를 보낼 제공자 인증 주소를 만든다.
func (r *Registry) AuthCodeURL(p Provider, state string) (string, error) {
	c, ok := r.Config(p)
	if !ok {
		return "", fmt.Errorf("%s 로그인이 설정되지 않았습니다", p)
	}
	q := url.Values{
		"client_id":     {c.ClientID},
		"redirect_uri":  {r.CallbackURL(p)},
		"response_type": {"code"},
		"state":         {state},
	}
	if len(c.Scopes) > 0 {
		q.Set("scope", strings.Join(c.Scopes, " "))
	}
	return c.AuthURL + "?" + q.Encode(), nil
}

// Exchange는 인증 코드를 액세스 토큰으로 바꾸고 사용자 정보를 가져온다.
func (r *Registry) Exchange(ctx context.Context, p Provider, code, state string) (*UserInfo, error) {
	c, ok := r.Config(p)
	if !ok {
		return nil, fmt.Errorf("%s 로그인이 설정되지 않았습니다", p)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"code":          {code},
		"redirect_uri":  {r.CallbackURL(p)},
	}
	// 네이버는 토큰 교환에도 state를 요구한다.
	if p == Naver {
		form.Set("state", state)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("토큰 요청 실패: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("토큰 발급 거부 (HTTP %d)", resp.StatusCode)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("토큰 응답 파싱: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("액세스 토큰을 받지 못했습니다 (%s)", tok.Error)
	}

	return r.fetchUser(ctx, p, c, tok.AccessToken)
}

func (r *Registry) fetchUser(ctx context.Context, p Provider, c Config, accessToken string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.UserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("사용자 정보 조회 실패: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("사용자 정보 조회 거부 (HTTP %d)", resp.StatusCode)
	}

	switch p {
	case Google:
		var g struct {
			Sub           string `json:"sub"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
			Name          string `json:"name"`
		}
		if err := json.Unmarshal(body, &g); err != nil {
			return nil, err
		}
		if g.Sub == "" {
			return nil, fmt.Errorf("구글 사용자 식별자를 받지 못했습니다")
		}
		return &UserInfo{Provider: p, ID: g.Sub, Email: g.Email,
			EmailVerified: g.EmailVerified, Nickname: g.Name}, nil

	case Naver:
		var n struct {
			ResultCode string `json:"resultcode"`
			Response   struct {
				ID       string `json:"id"`
				Email    string `json:"email"`
				Nickname string `json:"nickname"`
				Name     string `json:"name"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &n); err != nil {
			return nil, err
		}
		if n.Response.ID == "" {
			return nil, fmt.Errorf("네이버 사용자 식별자를 받지 못했습니다")
		}
		nick := n.Response.Nickname
		if nick == "" {
			nick = n.Response.Name
		}
		// 네이버는 검증 여부를 따로 알려주지 않는다. 검증되지 않은 것으로 취급한다.
		return &UserInfo{Provider: p, ID: n.Response.ID, Email: n.Response.Email,
			EmailVerified: false, Nickname: nick}, nil

	case Kakao:
		var k struct {
			ID           int64 `json:"id"`
			KakaoAccount struct {
				Email         string `json:"email"`
				EmailValid    bool   `json:"is_email_valid"`
				EmailVerified bool   `json:"is_email_verified"`
				Profile       struct {
					Nickname string `json:"nickname"`
				} `json:"profile"`
			} `json:"kakao_account"`
		}
		if err := json.Unmarshal(body, &k); err != nil {
			return nil, err
		}
		if k.ID == 0 {
			return nil, fmt.Errorf("카카오 사용자 식별자를 받지 못했습니다")
		}
		return &UserInfo{
			Provider: p, ID: fmt.Sprintf("%d", k.ID),
			Email:         k.KakaoAccount.Email,
			EmailVerified: k.KakaoAccount.EmailValid && k.KakaoAccount.EmailVerified,
			Nickname:      k.KakaoAccount.Profile.Nickname,
		}, nil
	}
	return nil, fmt.Errorf("알 수 없는 제공자: %s", p)
}

func Parse(s string) (Provider, bool) {
	switch Provider(strings.ToLower(s)) {
	case Google:
		return Google, true
	case Naver:
		return Naver, true
	case Kakao:
		return Kakao, true
	}
	return "", false
}

func (p Provider) Label() string {
	switch p {
	case Google:
		return "Google"
	case Naver:
		return "네이버"
	case Kakao:
		return "카카오"
	}
	return string(p)
}
