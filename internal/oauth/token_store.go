package oauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/store"
)

// BridgeTarget은 내부 Chat SDK 브리지에만 전달되는 접속 정보다.
// 외부 API와 로그에는 절대 액세스 토큰을 노출하지 않는다.
type BridgeTarget struct {
	BID         string    `json:"bid"`
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func (r *Registry) seal(plain string) ([]byte, error) {
	if len(r.tokenKey) != 32 {
		return nil, fmt.Errorf("SOOP_TOKEN_ENCRYPTION_KEY가 올바르게 설정되지 않았습니다")
	}
	block, err := aes.NewCipher(r.tokenKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (r *Registry) open(ciphertext []byte) (string, error) {
	if len(r.tokenKey) != 32 {
		return "", fmt.Errorf("SOOP_TOKEN_ENCRYPTION_KEY가 올바르게 설정되지 않았습니다")
	}
	block, err := aes.NewCipher(r.tokenKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", fmt.Errorf("암호화된 SOOP 토큰이 손상되었습니다")
	}
	plain, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("SOOP 토큰 복호화 실패: %w", err)
	}
	return string(plain), nil
}

func (r *Registry) SaveSOOPTokens(ctx context.Context, db *store.DB, accountID int64, bid string, tokens TokenSet) error {
	access, err := r.seal(tokens.AccessToken)
	if err != nil {
		return err
	}
	var refresh any
	if tokens.RefreshToken != "" {
		refresh, err = r.seal(tokens.RefreshToken)
		if err != nil {
			return err
		}
	}
	_, err = db.Exec(ctx, `
		INSERT INTO soop_oauth_tokens
			(account_id,bid,access_token_enc,refresh_token_enc,scope,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON DUPLICATE KEY UPDATE
			bid=VALUES(bid), access_token_enc=VALUES(access_token_enc),
			refresh_token_enc=COALESCE(VALUES(refresh_token_enc),refresh_token_enc),
			scope=VALUES(scope), expires_at=VALUES(expires_at)`,
		accountID, bid, access, refresh, tokens.Scope, tokens.ExpiresAt)
	return err
}

type storedToken struct {
	accountID int64
	bid       string
	access    []byte
	refresh   []byte
	scope     sql.NullString
	expiresAt time.Time
}

func (r *Registry) loadSOOPToken(ctx context.Context, db *store.DB, bid string) (storedToken, error) {
	var t storedToken
	err := db.QueryRow(ctx, `SELECT account_id,bid,access_token_enc,refresh_token_enc,scope,expires_at
		FROM soop_oauth_tokens WHERE bid=$1`, bid).Scan(
		&t.accountID, &t.bid, &t.access, &t.refresh, &t.scope, &t.expiresAt)
	return t, err
}

// AccessForBID는 유효한 액세스 토큰을 돌려준다. 만료 5분 전부터 한 번만
// 리프레시하며, 동시에 여러 브리지 폴링이 와도 refresh token을 경쟁 사용하지 않는다.
func (r *Registry) AccessForBID(ctx context.Context, db *store.DB, bid string) (BridgeTarget, error) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	t, err := r.loadSOOPToken(ctx, db, bid)
	if err != nil {
		return BridgeTarget{}, err
	}
	access, err := r.open(t.access)
	if err != nil {
		return BridgeTarget{}, err
	}
	if time.Until(t.expiresAt) > 5*time.Minute {
		return BridgeTarget{BID: bid, AccessToken: access, ExpiresAt: t.expiresAt}, nil
	}
	refresh, err := r.open(t.refresh)
	if err != nil || refresh == "" {
		return BridgeTarget{}, fmt.Errorf("SOOP 로그인이 만료되었습니다. 다시 로그인해 주세요")
	}
	cfg, ok := r.Config(Soop)
	if !ok {
		return BridgeTarget{}, fmt.Errorf("SOOP OAuth가 설정되지 않았습니다")
	}
	body, err := r.postForm(ctx, cfg.TokenURL, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {cfg.ClientID},
		"client_secret": {cfg.ClientSecret}, "refresh_token": {refresh},
	}, "SOOP 토큰 갱신")
	if err != nil {
		return BridgeTarget{}, err
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		Message      string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return BridgeTarget{}, fmt.Errorf("SOOP 토큰 갱신 응답 파싱: %w", err)
	}
	if out.AccessToken == "" {
		msg := strings.TrimSpace(out.Message)
		if msg == "" {
			msg = out.Error
		}
		return BridgeTarget{}, fmt.Errorf("SOOP 토큰 갱신 실패 (%s)", msg)
	}
	if out.RefreshToken == "" {
		out.RefreshToken = refresh
	}
	if out.Scope == "" {
		out.Scope = t.scope.String
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 8 * 60 * 60
	}
	next := TokenSet{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		Scope: out.Scope, ExpiresAt: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}
	if err := r.SaveSOOPTokens(ctx, db, t.accountID, bid, next); err != nil {
		return BridgeTarget{}, err
	}
	return BridgeTarget{BID: bid, AccessToken: next.AccessToken, ExpiresAt: next.ExpiresAt}, nil
}

func (r *Registry) HasSOOPToken(ctx context.Context, db *store.DB, bid string) bool {
	var n int
	return db.QueryRow(ctx, `SELECT 1 FROM soop_oauth_tokens WHERE bid=$1`, bid).Scan(&n) == nil
}

// BridgeTargets는 채팅 연동이 켜져 있고 SOOP OAuth까지 완료한 방송국만 반환한다.
func (r *Registry) BridgeTargets(ctx context.Context, db *store.DB) ([]BridgeTarget, error) {
	rows, err := db.Query(ctx, `SELECT t.bid FROM soop_oauth_tokens t
		JOIN streamers s ON s.bid=t.bid WHERE s.enabled=TRUE AND s.chat_enabled=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bids []string
	for rows.Next() {
		var bid string
		if err := rows.Scan(&bid); err != nil {
			return nil, err
		}
		bids = append(bids, bid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]BridgeTarget, 0, len(bids))
	for _, bid := range bids {
		t, err := r.AccessForBID(ctx, db, bid)
		if err != nil {
			// 한 사용자의 만료 토큰 때문에 다른 방송국 수집까지 막지 않는다.
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// LiveForBID는 승인받은 방송 여부 API로 현재 방송 상태를 확인한다.
func (r *Registry) LiveForBID(ctx context.Context, db *store.DB, bid string) (bool, error) {
	target, err := r.AccessForBID(ctx, db, bid)
	if err != nil {
		return false, err
	}
	cfg, ok := r.Config(Soop)
	if !ok {
		return false, fmt.Errorf("SOOP OAuth가 설정되지 않았습니다")
	}
	body, err := r.postForm(ctx, cfg.UserInfoURL,
		url.Values{"access_token": {target.AccessToken}}, "SOOP 방송 여부 확인")
	if err != nil {
		return false, err
	}
	var out struct {
		Result int    `json:"result"`
		Msg    string `json:"msg"`
		Data   struct {
			UserID string `json:"user_id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, err
	}
	if out.Result != 1 || !strings.EqualFold(strings.TrimSpace(out.Data.UserID), bid) {
		return false, fmt.Errorf("SOOP 방송 상태의 계정이 일치하지 않습니다")
	}
	return strings.EqualFold(strings.TrimSpace(out.Data.Status), "streaming"), nil
}
