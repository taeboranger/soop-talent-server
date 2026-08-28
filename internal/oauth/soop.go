package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// exchangeSOOP은 SOOP의 1회용 code를 토큰으로 교환한 뒤, 토큰 소유자의
// 실제 방송국 ID를 공식 API 응답으로 검증한다. user/stationinfo는 직접 ID를
// 주지 않지만 SOOP이 생성한 profile_image URL에 방송국 ID를 중복해 넣는다.
func (r *Registry) exchangeSOOP(ctx context.Context, c Config, code string) (*UserInfo, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"redirect_uri":  {r.CallbackURL(Soop)},
		"code":          {code},
	}
	body, err := r.postForm(ctx, c.TokenURL, form, "SOOP 토큰 요청")
	if err != nil {
		return nil, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		UserID       string `json:"user_id"`
		UserIDCamel  string `json:"userId"`
		Error        string `json:"error"`
		Message      string `json:"message"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("SOOP 토큰 응답 파싱: %w", err)
	}
	if tok.AccessToken == "" {
		msg := strings.TrimSpace(tok.Message)
		if msg == "" {
			msg = strings.TrimSpace(tok.Error)
		}
		return nil, fmt.Errorf("SOOP 액세스 토큰을 받지 못했습니다 (%s)", msg)
	}

	body, err = r.postForm(ctx, c.UserInfoURL,
		url.Values{"access_token": {tok.AccessToken}}, "SOOP 사용자 정보 조회")
	if err != nil {
		return nil, err
	}
	live, err := decodeSOOPEnvelope(body, "SOOP 방송 여부")
	if err != nil {
		return nil, fmt.Errorf("SOOP 사용자 정보 응답 파싱: %w", err)
	}
	if live.Result != 1 {
		return nil, fmt.Errorf("SOOP 방송 여부를 확인하지 못했습니다 (result=%d, msg=%s)", live.Result, live.Msg)
	}
	id := strings.TrimSpace(tok.UserID)
	if id == "" {
		id = strings.TrimSpace(tok.UserIDCamel)
	}
	if id == "" {
		id = soopIdentity(live.Data)
	}
	nickname := id
	stationFields := "(미호출)"
	bridgeErr := ""
	if c.StationURL != "" {
		stationBody, stationErr := r.postForm(ctx, c.StationURL,
			url.Values{"access_token": {tok.AccessToken}}, "SOOP 방송국 정보 조회")
		if stationErr != nil {
			return nil, stationErr
		}
		station, err := decodeSOOPEnvelope(stationBody, "SOOP 방송국 정보")
		if err != nil {
			return nil, fmt.Errorf("SOOP 방송국 정보 응답 파싱: %w", err)
		}
		if station.Result != 1 {
			return nil, fmt.Errorf("SOOP 방송국 정보를 확인하지 못했습니다 (result=%d, msg=%s)", station.Result, station.Msg)
		}
		stationFields = soopJSONFields(station.Data)
		if id == "" {
			id = soopIdentity(station.Data)
		}
		if id == "" {
			id = soopProfileImageID(station.Data)
		}
		if v := soopNickname(station.Data); v != "" {
			nickname = v
		}
	}
	if id == "" {
		var err error
		id, err = r.resolveSOOPIdentityWithChatBridge(ctx, tok.AccessToken)
		if err != nil {
			if errors.Is(err, ErrSOOPLiveRequired) {
				return nil, ErrSOOPLiveRequired
			}
			bridgeErr = err.Error()
		}
	}
	if !validSOOPID(id) {
		return nil, fmt.Errorf("SOOP 사용자 식별자를 확인하지 못했습니다 (토큰·방송 상태·방송국 정보에 ID 없음; 방송 상태 필드=%s, 방송국 정보 필드=%s, Chat SDK=%s)", soopJSONFields(live.Data), stationFields, bridgeErr)
	}
	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 8 * 60 * 60
	}
	return &UserInfo{
		Provider: Soop, ID: id, Nickname: nickname,
		Tokens: &TokenSet{
			AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
			Scope: tok.Scope, ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
		},
	}, nil
}

type soopEnvelope struct {
	Result int             `json:"result"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

func decodeSOOPEnvelope(body []byte, what string) (soopEnvelope, error) {
	var out soopEnvelope
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	if len(out.Data) == 0 || string(out.Data) == "null" {
		out.Data = json.RawMessage(`{}`)
	}
	return out, nil
}

// soopIdentity는 실제 SOOP API에서 확인된 snake_case와 현재 Developers 문서의
// camelCase 표기를 모두 받는다. 일반적인 "id" 필드는 다른 리소스 ID일 수 있어
// 의도적으로 사용하지 않는다.
func soopIdentity(data json.RawMessage) string {
	for _, path := range [][]string{
		{"user_id"}, {"userId"}, {"user", "user_id"}, {"user", "userId"},
		{"station", "user_id"}, {"station", "userId"},
	} {
		if id := soopStringAt(data, path...); id != "" {
			return id
		}
	}
	return ""
}

func soopNickname(data json.RawMessage) string {
	for _, path := range [][]string{
		{"user_nick"}, {"userNick"}, {"user", "user_nick"}, {"user", "userNick"},
		{"station", "user_nick"}, {"station", "userNick"},
	} {
		if nick := soopStringAt(data, path...); nick != "" {
			return nick
		}
	}
	return ""
}

// soopProfileImageID는 SOOP이 user/stationinfo에서 반환하는 표준 프로필 주소
// https://profile.img.sooplive.com/LOGO/{접두사}/{ID}/{ID}.jpg 형태만 받는다.
// API 응답 자체가 OAuth 토큰 소유자에 묶여 있고, 호스트·경로·파일명까지 대조하므로
// 브라우저나 확장이 전달한 임의 ID를 신뢰하는 경로가 아니다.
func soopProfileImageID(data json.RawMessage) string {
	raw := soopStringAt(data, "profile_image")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil ||
		!strings.EqualFold(u.Hostname(), "profile.img.sooplive.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "LOGO" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return ""
	}
	id := parts[2]
	filenameID := strings.TrimSuffix(parts[3], path.Ext(parts[3]))
	if filenameID != id || !validSOOPID(id) {
		return ""
	}
	return id
}

func validSOOPID(id string) bool {
	return id != "" && !strings.HasPrefix(id, "-") && !strings.ContainsAny(id, "|/\\")
}

func soopStringAt(data json.RawMessage, path ...string) string {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	for _, part := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value, ok = object[part]
		if !ok {
			return ""
		}
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// soopJSONFields는 값 대신 필드명만 남긴다. API 변경을 진단할 때 계정 정보나
// 토큰을 로그에 남기지 않기 위한 안전한 오류 단서다.
func soopJSONFields(data json.RawMessage) string {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return "(파싱 불가)"
	}
	fields := []string{}
	var walk func(any, string)
	walk = func(v any, prefix string) {
		object, ok := v.(map[string]any)
		if !ok {
			return
		}
		for key, child := range object {
			name := key
			if prefix != "" {
				name = prefix + "." + key
			}
			fields = append(fields, name)
			walk(child, name)
		}
	}
	walk(value, "")
	if len(fields) == 0 {
		return "(없음)"
	}
	return strings.Join(fields, ",")
}

func (r *Registry) postForm(ctx context.Context, endpoint string, form url.Values, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s 실패: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s 거부 (HTTP %d)", what, resp.StatusCode)
	}
	return body, nil
}
