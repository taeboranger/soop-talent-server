package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSOOPAuthCodeURL_공식명세파라미터만사용(t *testing.T) {
	r := &Registry{publicURL: "https://service.example", configs: map[Provider]Config{
		Soop: {
			ClientID: "client", ClientSecret: "secret",
			AuthURL: "https://openapi.sooplive.com/auth/code",
			Scopes:  []string{"validate_live_status", "user_stationinfo", "broad_access_chatinfo"},
		},
	}}
	raw, err := r.AuthCodeURL(Soop, "server-state-must-not-leak")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("client_id"); got != "client" {
		t.Fatalf("client_id=%q", got)
	}
	if got := u.Query().Get("scope"); got != "validate_live_status user_stationinfo broad_access_chatinfo" {
		t.Fatalf("scope=%q", got)
	}
	for _, forbidden := range []string{"state", "client_secret", "redirect_uri", "response_type"} {
		if u.Query().Has(forbidden) {
			t.Errorf("SOOP auth/code에 명세되지 않은 %s가 포함됨", forbidden)
		}
	}
}

func TestSOOPExchange_토큰소유자ID검증(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			t.Error(err)
		}
		switch req.URL.Path {
		case "/auth/token":
			if req.Method != http.MethodPost || req.Form.Get("grant_type") != "authorization_code" ||
				req.Form.Get("client_id") != "client" || req.Form.Get("client_secret") != "secret" ||
				req.Form.Get("code") != "one-time-code" ||
				req.Form.Get("redirect_uri") != "https://service.example/api/oauth/soop/callback" {
				t.Errorf("잘못된 토큰 요청: %v", req.Form)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":28800,"scope":"validate_live_status user_stationinfo broad_access_chatinfo","token_type":"Bearer"}`))
		case "/validate/live/status":
			if req.Method != http.MethodPost || req.Form.Get("access_token") != "access" {
				t.Errorf("잘못된 사용자 검증 요청: %v", req.Form)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":1,"msg":"성공","data":{"user_id":"verified_bj","status":"streamend"}}`))
		case "/user/stationinfo":
			if req.Form.Get("access_token") != "access" {
				t.Errorf("잘못된 방송국 정보 요청: %v", req.Form)
			}
			_, _ = w.Write([]byte(`{"result":1,"msg":"성공","data":{"user_nick":"검증된닉네임","station_name":"방송국"}}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	r := &Registry{
		publicURL: "https://service.example",
		http:      server.Client(),
		configs: map[Provider]Config{Soop: {
			ClientID: "client", ClientSecret: "secret",
			TokenURL: server.URL + "/auth/token", UserInfoURL: server.URL + "/validate/live/status",
			StationURL: server.URL + "/user/stationinfo",
		}},
	}
	info, err := r.Exchange(context.Background(), Soop, "one-time-code", "unused-state")
	if err != nil {
		t.Fatal(err)
	}
	if info.Provider != Soop || info.ID != "verified_bj" || info.Nickname != "검증된닉네임" ||
		info.Tokens == nil || info.Tokens.RefreshToken != "refresh" {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func TestSOOPExchange_방송국정보의CamelCaseID도받는다(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"access"}`))
		case "/live":
			// 방송 상태 API는 오프라인일 때 ID를 생략할 수 있다.
			_, _ = w.Write([]byte(`{"result":1,"msg":"성공","data":{"status":"offline"}}`))
		case "/station":
			_, _ = w.Write([]byte(`{"result":1,"msg":"성공","data":{"userId":"streamer01","userNick":"스트리머"}}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	r := &Registry{publicURL: "https://service.example", http: server.Client(), configs: map[Provider]Config{
		Soop: {ClientID: "client", ClientSecret: "secret", TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/live", StationURL: server.URL + "/station"},
	}}
	info, err := r.Exchange(context.Background(), Soop, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "streamer01" || info.Nickname != "스트리머" {
		t.Fatalf("unexpected identity: %+v", info)
	}
}

func TestSOOPExchange_방송국닉네임으로정확히일치하는ID를찾는다(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"access"}`))
		case "/live":
			_, _ = w.Write([]byte(`{"result":1,"data":{"status":"offline"}}`))
		case "/station":
			_, _ = w.Write([]byte(`{"result":1,"data":{"user_nick":"케이지이지요"}}`))
		case "/search":
			if req.Method != http.MethodGet || req.URL.Query().Get("m") != "bjSearch" ||
				req.URL.Query().Get("szKeyword") != "케이지이지요" ||
				req.URL.Query().Get("nPageNo") != "1" || req.URL.Query().Get("nListCnt") != "20" {
				t.Errorf("잘못된 SOOP 검색 요청: %s", req.URL.String())
			}
			_, _ = w.Write([]byte(`{"DATA":[{"user_id":"similar","user_nick":"케이지"},{"user_id":"lightstone1","user_nick":"케이지이지요"}]}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	r := &Registry{publicURL: "https://service.example", http: server.Client(), configs: map[Provider]Config{
		Soop: {ClientID: "client", ClientSecret: "secret", TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/live", StationURL: server.URL + "/station", SearchURL: server.URL + "/search"},
	}}
	info, err := r.Exchange(context.Background(), Soop, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "lightstone1" || info.Nickname != "케이지이지요" {
		t.Fatalf("unexpected identity: %+v", info)
	}
}

func TestResolveSOOPIdentityByNickname_동명이인은거부한다(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"DATA":[{"user_id":"first","user_nick":"같은닉"},{"user_id":"second","user_nick":"같은닉"}]}`))
	}))
	defer server.Close()
	r := &Registry{http: server.Client()}
	if _, err := r.resolveSOOPIdentityByNickname(context.Background(), server.URL, "같은닉"); err == nil {
		t.Fatal("동명이인 검색 결과가 계정 식별에 사용됨")
	}
}

func TestResolveSOOPIdentityWithChatBridge_내부키와BJID검증(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/identity" || req.Header.Get("Authorization") != "Bearer bridge-secret" {
			t.Fatalf("잘못된 브리지 요청: %s %s %q", req.Method, req.URL.Path, req.Header.Get("Authorization"))
		}
		var in struct {
			AccessToken string `json:"accessToken"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.AccessToken != "access" {
			t.Fatalf("잘못된 브리지 본문: %+v, %v", in, err)
		}
		_, _ = w.Write([]byte(`{"bjId":"verified_bj","chatNumber":"123"}`))
	}))
	defer bridge.Close()
	t.Setenv("CHAT_BRIDGE_SECRET", "bridge-secret")
	t.Setenv("CHAT_BRIDGE_URL", bridge.URL)
	r := &Registry{http: bridge.Client()}
	if got, err := r.resolveSOOPIdentityWithChatBridge(context.Background(), "access"); err != nil || got != "verified_bj" {
		t.Fatalf("identity=%q err=%v", got, err)
	}
}

func TestSOOPTokenEncryption_키가다르면복호화불가(t *testing.T) {
	r1 := &Registry{tokenKey: []byte("01234567890123456789012345678901")}
	r2 := &Registry{tokenKey: []byte("abcdefghijklmnopqrstuvwxyzABCDEF")}
	sealed, err := r1.seal("sensitive-access-token")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := r1.open(sealed)
	if err != nil || plain != "sensitive-access-token" {
		t.Fatalf("round trip=%q err=%v", plain, err)
	}
	if _, err := r2.open(sealed); err == nil {
		t.Fatal("다른 암호화 키로 토큰이 복호화됨")
	}
}

func TestSOOPExchange_검증되지않은ID거부(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/token") {
			_, _ = w.Write([]byte(`{"access_token":"access"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":-1,"msg":"권한 없음","data":{}}`))
	}))
	defer server.Close()
	r := &Registry{publicURL: "https://service.example", http: server.Client(), configs: map[Provider]Config{
		Soop: {ClientID: "client", ClientSecret: "secret", TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/me"},
	}}
	if _, err := r.Exchange(context.Background(), Soop, "code", ""); err == nil {
		t.Fatal("SOOP user_id 검증 실패가 로그인 성공으로 처리됨")
	}
}
