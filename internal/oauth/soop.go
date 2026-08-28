package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// exchangeSOOP은 SOOP의 1회용 code를 토큰으로 교환한 뒤, 토큰 소유자의
// 실제 방송국 ID를 공식 API 응답으로 검증한다. user/stationinfo 응답에 ID가
// 없을 때에는 그 응답의 닉네임으로 SOOP 검색 API를 조회해 정확히 일치하는
// 방송국 ID만 사용한다.
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
		if v := soopNickname(station.Data); v != "" {
			nickname = v
		}
	}
	if id == "" && nickname != "" && c.SearchURL != "" {
		var err error
		id, err = r.resolveSOOPIdentityByNickname(ctx, c.SearchURL, nickname)
		if err != nil {
			return nil, err
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

// resolveSOOPIdentityByNickname는 OAuth로 검증된 방송국 정보의 닉네임만 검색한다.
// 부분 일치 결과나 서로 다른 ID의 동명이인 후보가 있으면 계정을 임의로 고르지
// 않고 실패한다. 프로필 이미지 URL처럼 선택적인 프로필 항목에는 의존하지 않는다.
func (r *Registry) resolveSOOPIdentityByNickname(ctx context.Context, endpoint, nickname string) (string, error) {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return "", fmt.Errorf("SOOP 닉네임이 비어 있어 방송국 ID를 검색할 수 없습니다")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("SOOP 방송국 검색 주소가 올바르지 않습니다: %w", err)
	}
	q := u.Query()
	q.Set("m", "bjSearch")
	q.Set("szKeyword", nickname)
	q.Set("nPageNo", "1")
	q.Set("nListCnt", "20")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	// SOOP 검색 API는 브라우저가 아닌 기본 Go User-Agent 요청을 차단할 수 있다.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SoopTalent/1.0)")
	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("SOOP 방송국 검색 실패: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("SOOP 방송국 검색 거부 (HTTP %d)", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			UserID     string `json:"user_id"`
			UserIDV2   string `json:"userId"`
			UserNick   string `json:"user_nick"`
			UserNickV2 string `json:"userNick"`
		} `json:"DATA"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("SOOP 방송국 검색 응답 파싱: %w", err)
	}
	ids := map[string]struct{}{}
	for _, candidate := range result.Data {
		candidateNick := strings.TrimSpace(candidate.UserNick)
		if candidateNick == "" {
			candidateNick = strings.TrimSpace(candidate.UserNickV2)
		}
		// 검색 API의 부분 일치 결과는 절대 계정 식별에 쓰지 않는다.
		if candidateNick != nickname {
			continue
		}
		candidateID := strings.TrimSpace(candidate.UserID)
		if candidateID == "" {
			candidateID = strings.TrimSpace(candidate.UserIDV2)
		}
		if validSOOPID(candidateID) {
			ids[candidateID] = struct{}{}
		}
	}
	if len(ids) == 1 {
		for id := range ids {
			return id, nil
		}
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("SOOP 닉네임 검색 결과가 여러 계정과 정확히 일치합니다; 방송국 ID를 안전하게 선택할 수 없습니다")
	}
	return "", fmt.Errorf("SOOP 닉네임과 정확히 일치하는 방송국 ID를 찾지 못했습니다")
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
