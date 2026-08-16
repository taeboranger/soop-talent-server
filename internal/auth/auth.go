// Package auth는 계정·비밀번호·세션을 다룬다.
//
// 신뢰 모델의 핵심: 요청자의 신원은 **세션 토큰에서만** 나온다.
// 클라이언트가 보낸 SOOP 아이디는 어떤 경우에도 권한 판단에 쓰지 않는다.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/store"
)

var (
	ErrBadCredentials   = apperr.New("이메일 또는 비밀번호가 올바르지 않습니다")
	ErrEmailTaken       = apperr.New("이미 가입된 이메일입니다")
	ErrWeakPassword     = apperr.New("비밀번호는 8자 이상이어야 합니다")
	ErrNoSession        = errors.New("로그인이 필요합니다")
	ErrDisabled         = errors.New("사용이 중지된 계정입니다")
	ErrBadExtensionCode = apperr.New("확장 로그인 코드가 만료되었거나 올바르지 않습니다")
)

const (
	SessionTTL     = 14 * 24 * time.Hour // 확장이 오래 붙어 있어야 하므로 넉넉히
	sessionRenewAt = 7 * 24 * time.Hour  // 남은 수명이 이보다 짧으면 연장
	minPasswordLen = 8
)

// argon2id 파라미터. 서버가 수십 명 규모에서 로그인 폭주를 겪을 일은 없으므로
// 메모리를 넉넉히 잡아 무차별 대입 비용을 올린다.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

type Account struct {
	ID      int64
	Email   string
	IsAdmin bool
}

// ---------------------------------------------------------------------------
// 비밀번호
// ---------------------------------------------------------------------------

// HashPassword는 argon2id 해시를 표준 인코딩 문자열로 만든다.
// 파라미터가 문자열에 포함되므로 나중에 비용을 올려도 기존 해시를 검증할 수 있다.
func HashPassword(pw string) (string, error) {
	if len([]rune(pw)) < minPasswordLen {
		return "", ErrWeakPassword
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword는 저장된 해시와 비교한다. 타이밍 공격을 피해 상수 시간 비교를 쓴다.
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory uint32
	var timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------------------------------------------------------------------------
// 계정
// ---------------------------------------------------------------------------

func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// 완전한 RFC 검증은 하지 않는다. 오타를 걸러 내는 것이 목적이며,
// 실제 도달 가능 여부는 어차피 메일을 보내 봐야 알 수 있다.
var emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@.]+(\.[^\s@.]+)+$`)

func Register(ctx context.Context, pool *store.DB, email, pw string, isAdmin bool) (*Account, error) {
	email = NormalizeEmail(email)
	if !emailRe.MatchString(email) {
		return nil, apperr.New("올바른 이메일을 입력해 주세요")
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return nil, err
	}

	tag, err := pool.Exec(ctx, `
		INSERT INTO accounts (email, password_hash, is_admin)
		VALUES ($1, $2, $3)`, email, hash, isAdmin)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	id, err := tag.LastInsertId()
	if err != nil {
		return nil, err
	}
	var a Account
	err = pool.QueryRow(ctx, `SELECT id, email, is_admin FROM accounts WHERE id = $1`, id).Scan(&a.ID, &a.Email, &a.IsAdmin)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Login은 자격 증명을 확인하고 세션 토큰을 발급한다.
// 이메일이 없든 비밀번호가 틀리든 같은 오류를 돌려준다 — 가입 여부가 새지 않게.
func Login(ctx context.Context, pool *store.DB, email, pw, userAgent string) (string, *Account, error) {
	var (
		a        Account
		hash     string
		disabled bool
	)
	err := pool.QueryRow(ctx, `
		SELECT id, email, password_hash, is_admin, disabled
		  FROM accounts WHERE lower(email) = $1`, NormalizeEmail(email)).
		Scan(&a.ID, &a.Email, &hash, &a.IsAdmin, &disabled)

	if errors.Is(err, sql.ErrNoRows) {
		// 존재하지 않는 계정도 해시 비용을 치르게 해 응답 시간으로 구분되지 않게 한다.
		VerifyPassword("$argon2id$v=19$m=65536,t=2,p=4$"+
			strings.Repeat("A", 22)+"$"+strings.Repeat("A", 43), pw)
		return "", nil, ErrBadCredentials
	}
	if err != nil {
		return "", nil, err
	}
	if !VerifyPassword(hash, pw) {
		return "", nil, ErrBadCredentials
	}
	if disabled {
		return "", nil, ErrDisabled
	}

	token, err := issueSession(ctx, pool, a.ID, userAgent)
	if err != nil {
		return "", nil, err
	}
	_, _ = pool.Exec(ctx, `UPDATE accounts SET last_login_at = now() WHERE id = $1`, a.ID)
	return token, &a, nil
}

// ---------------------------------------------------------------------------
// 세션
// ---------------------------------------------------------------------------

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func issueSession(ctx context.Context, pool *store.DB, accountID int64, ua string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if len(ua) > 200 {
		ua = ua[:200]
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, account_id, expires_at, user_agent)
		VALUES ($1, $2, DATE_ADD(NOW(6), INTERVAL $3 SECOND), $4)`,
		hashToken(token), accountID, int(SessionTTL.Seconds()), ua)
	if err != nil {
		return "", err
	}
	return token, nil
}

// IssueSession은 OAuth 콜백과 확장 세션 코드 교환이 같은 방식으로 세션을
// 발급하게 하는 공개 진입점이다.
func IssueSession(ctx context.Context, pool *store.DB, accountID int64, ua string) (string, error) {
	return issueSession(ctx, pool, accountID, ua)
}

const extensionCodeTTL = time.Minute

// ExtensionOAuthRequest는 OAuth 제공자 왕복 전에 저장한 확장 PKCE 정보다.
type ExtensionOAuthRequest struct {
	CodeChallenge string
	ClientState   string
}

// NewExtensionOAuthRequest는 OAuth state와 확장이 만든 PKCE 정보를 묶는다.
func NewExtensionOAuthRequest(ctx context.Context, pool *store.DB, oauthState, codeChallenge, clientState string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO extension_oauth_requests (oauth_state_hash, code_challenge, client_state, expires_at)
		VALUES ($1, $2, $3, DATE_ADD(NOW(6), INTERVAL $4 SECOND))`,
		hashToken(oauthState), codeChallenge, clientState, int(stateTTL.Seconds()))
	return err
}

// ConsumeExtensionOAuthRequest는 OAuth state에 딸린 PKCE 정보를 한 번만 꺼낸다.
func ConsumeExtensionOAuthRequest(ctx context.Context, pool *store.DB, oauthState string) (*ExtensionOAuthRequest, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var req ExtensionOAuthRequest
	err = tx.QueryRow(ctx, `
		SELECT code_challenge, client_state
		  FROM extension_oauth_requests
		 WHERE oauth_state_hash = $1 AND expires_at > NOW(6)
		 FOR UPDATE`, hashToken(oauthState)).Scan(&req.CodeChallenge, &req.ClientState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBadExtensionCode
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM extension_oauth_requests WHERE oauth_state_hash = $1`, hashToken(oauthState)); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &req, nil
}

// NewExtensionCode는 웹의 HttpOnly 세션을 확장 세션으로 바꾸기 위한 1회용
// 코드를 만든다. 원문은 리다이렉트 프래그먼트로만 전달하고 DB에는 해시만 남긴다.
// 코드만 탈취해도 교환할 수 없도록 확장이 만든 PKCE challenge를 함께 묶는다.
func NewExtensionCode(ctx context.Context, pool *store.DB, accountID int64, redirectURI, codeChallenge, clientState string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := hex.EncodeToString(raw)
	_, err := pool.Exec(ctx, `
		INSERT INTO extension_login_codes_v2
		       (code_hash, account_id, redirect_uri, code_challenge, client_state, expires_at)
		VALUES ($1, $2, $3, $4, $5, DATE_ADD(NOW(6), INTERVAL $6 SECOND))`,
		hashToken(code), accountID, redirectURI, codeChallenge, clientState, int(extensionCodeTTL.Seconds()))
	return code, err
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ExchangeExtensionCode는 코드를 단 한 번 소비하고, 정확히 같은 확장 콜백
// 주소에 대해서만 새 확장 세션을 발급한다.
func ExchangeExtensionCode(ctx context.Context, pool *store.DB, code, redirectURI, verifier, clientState, ua string) (string, *Account, error) {
	if code == "" || redirectURI == "" || verifier == "" || clientState == "" {
		return "", nil, ErrBadExtensionCode
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback(ctx)
	var a Account
	var disabled bool
	var wantChallenge, wantClientState string
	err = tx.QueryRow(ctx, `
		SELECT a.id, COALESCE(a.email,''), a.is_admin, a.disabled
		       , c.code_challenge, c.client_state
		  FROM extension_login_codes_v2 c JOIN accounts a ON a.id = c.account_id
		 WHERE c.code_hash = $1 AND c.redirect_uri = $2 AND c.expires_at > NOW(6)
		 FOR UPDATE`,
		hashToken(code), redirectURI).Scan(&a.ID, &a.Email, &a.IsAdmin, &disabled, &wantChallenge, &wantClientState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrBadExtensionCode
	}
	if err != nil {
		return "", nil, err
	}
	if disabled {
		return "", nil, ErrDisabled
	}
	if subtle.ConstantTimeCompare([]byte(wantChallenge), []byte(pkceChallenge(verifier))) != 1 ||
		subtle.ConstantTimeCompare([]byte(wantClientState), []byte(clientState)) != 1 {
		return "", nil, ErrBadExtensionCode
	}
	if _, err = tx.Exec(ctx, `DELETE FROM extension_login_codes_v2 WHERE code_hash = $1`, hashToken(code)); err != nil {
		return "", nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", nil, err
	}
	token, err := IssueSession(ctx, pool, a.ID, ua)
	if err != nil {
		return "", nil, err
	}
	return token, &a, nil
}

// Resolve는 세션 토큰에서 계정을 얻는다. 만료된 세션은 통과하지 않는다.
// 수명이 절반 아래로 떨어지면 자동 연장해, 활발히 쓰는 확장이 갑자기 로그아웃되지 않게 한다.
func Resolve(ctx context.Context, pool *store.DB, token string) (*Account, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	var (
		a        Account
		disabled bool
		expires  time.Time
	)
	err := pool.QueryRow(ctx, `
		SELECT a.id, a.email, a.is_admin, a.disabled, s.expires_at
		  FROM sessions s JOIN accounts a ON a.id = s.account_id
		 WHERE s.token_hash = $1 AND s.expires_at > now()`, hashToken(token)).
		Scan(&a.ID, &a.Email, &a.IsAdmin, &disabled, &expires)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	if disabled {
		return nil, ErrDisabled
	}

	if time.Until(expires) < sessionRenewAt {
		_, _ = pool.Exec(ctx, `
			UPDATE sessions SET expires_at = DATE_ADD(NOW(6), INTERVAL $2 SECOND), last_used_at = NOW(6)
			 WHERE token_hash = $1`, hashToken(token), int(SessionTTL.Seconds()))
	} else {
		_, _ = pool.Exec(ctx, `UPDATE sessions SET last_used_at = now() WHERE token_hash = $1`,
			hashToken(token))
	}
	return &a, nil
}

// SessionExpiry는 지금 세션이 언제 끊기는지 알려 준다.
// 상태 화면에 "남은 기간"을 보여 주는 용도이며, 권한 판단에는 쓰지 않는다.
func SessionExpiry(ctx context.Context, pool *store.DB, token string) (time.Time, error) {
	var t time.Time
	err := pool.QueryRow(ctx,
		`SELECT expires_at FROM sessions WHERE token_hash = $1`, hashToken(token)).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNoSession
	}
	return t, err
}

func Logout(ctx context.Context, pool *store.DB, token string) error {
	_, err := pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// LogoutAccount은 명시적으로 로그아웃했을 때 해당 계정의 모든 웹·확장 세션을
// 폐기한다. 한쪽에서 로그아웃해도 다른 쪽이 로그인 상태로 남지 않게 한다.
func LogoutAccount(ctx context.Context, pool *store.DB, accountID int64) error {
	_, err := pool.Exec(ctx, `DELETE FROM sessions WHERE account_id = $1`, accountID)
	return err
}

// PurgeExpired는 만료 세션을 정리한다. 주기적으로 호출한다.
func PurgeExpired(ctx context.Context, pool *store.DB) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	// 만료된 1회 로그인 자료는 민감하지 않더라도 쌓아 둘 이유가 없다.
	_, _ = pool.Exec(ctx, `DELETE FROM extension_login_codes WHERE expires_at < now()`)
	_, _ = pool.Exec(ctx, `DELETE FROM extension_login_codes_v2 WHERE expires_at < now()`)
	_, _ = pool.Exec(ctx, `DELETE FROM extension_oauth_requests WHERE expires_at < now()`)
	return tag.RowsAffected()
}

// EnsureAdmin은 최초 기동 시 관리자 계정을 만든다.
// 이미 있으면 아무것도 하지 않는다 — 비밀번호를 덮어쓰지 않는다.
func EnsureAdmin(ctx context.Context, pool *store.DB, email, pw string) (created bool, err error) {
	email = NormalizeEmail(email)
	var exists bool
	if err = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM accounts WHERE lower(email) = $1)`, email).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		// 관리자 권한만 보장한다.
		_, err = pool.Exec(ctx, `UPDATE accounts SET is_admin = TRUE WHERE lower(email) = $1`, email)
		return false, err
	}
	if _, err = Register(ctx, pool, email, pw, true); err != nil {
		return false, err
	}
	return true, nil
}
