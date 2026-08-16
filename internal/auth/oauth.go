package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/store"
)

var ErrBadState = apperr.New("인증 요청이 만료되었거나 올바르지 않습니다")

// adminEmails는 서버 기동 시 ADMIN_EMAILS로 설정한다. OAuth 제공자가 검증했다고
// 알려 준 이메일만 이 목록과 비교한다. 클라이언트가 보낸 이메일은 쓰지 않는다.
var adminEmails = map[string]struct{}{}

func SetAdminEmails(emails []string) {
	adminEmails = make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if email = NormalizeEmail(email); email != "" {
			adminEmails[email] = struct{}{}
		}
	}
}

func IsAdminEmail(email string) bool {
	_, ok := adminEmails[NormalizeEmail(email)]
	return ok
}

const stateTTL = 10 * time.Minute

// NewState는 CSRF 방지용 1회용 state를 발급하고 저장한다.
// redirectURI는 인증이 끝난 뒤 돌려보낼 확장 주소다.
func NewState(ctx context.Context, pool *store.DB, provider, redirectURI string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := hex.EncodeToString(b)
	_, err := pool.Exec(ctx, `
		INSERT INTO oauth_states (state, provider, redirect_uri, expires_at)
		VALUES ($1, $2, $3, DATE_ADD(NOW(6), INTERVAL $4 SECOND))`,
		state, provider, redirectURI, int(stateTTL.Seconds()))
	return state, err
}

// ConsumeState는 state를 검증하고 즉시 삭제한다(1회용).
func ConsumeState(ctx context.Context, pool *store.DB, state, provider string) (redirectURI string, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT redirect_uri FROM oauth_states WHERE state = $1 AND provider = $2 AND expires_at > NOW(6)`, state, provider).Scan(&redirectURI)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadState
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM oauth_states WHERE state = $1`, state); err != nil {
		return "", err
	}
	return redirectURI, tx.Commit(ctx)
}

func PurgeStates(ctx context.Context, pool *store.DB) {
	_, _ = pool.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at < now()`)
}

// SocialUser는 제공자에서 받아온 최소 정보다.
type SocialUser struct {
	Provider      string
	ID            string
	Email         string
	EmailVerified bool
	Nickname      string
}

// LoginWithSocial은 소셜 계정으로 로그인하고, 없으면 계정을 만든다.
//
// 계정 연결 규칙:
//  1. 이미 연결된 (provider, id)가 있으면 그 계정으로 로그인한다.
//  2. 없고 **제공자가 이메일을 검증했다면** 같은 이메일의 기존 계정에 연결한다.
//  3. 그 외에는 새 계정을 만든다.
//
// 2번에서 검증 여부를 따지는 이유: 검증되지 않은 이메일로 자동 연결하면,
// 남의 이메일을 등록한 소셜 계정으로 그 사람의 계정을 탈취할 수 있다.
// 네이버·카카오는 검증 여부를 신뢰할 수 없으므로 대부분 3번으로 떨어진다.
func LoginWithSocial(ctx context.Context, pool *store.DB, su SocialUser, userAgent string) (string, *Account, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a Account
	var disabled bool

	// 1. 이미 연결된 소셜 계정
	err = tx.QueryRow(ctx, `
		SELECT a.id, COALESCE(a.email,''), a.is_admin, a.disabled
		  FROM oauth_identities o JOIN accounts a ON a.id = o.account_id
		 WHERE o.provider = $1 AND o.provider_user_id = $2`,
		su.Provider, su.ID).Scan(&a.ID, &a.Email, &a.IsAdmin, &disabled)

	switch {
	case err == nil:
		if disabled {
			return "", nil, ErrDisabled
		}
		_, _ = tx.Exec(ctx, `
			UPDATE oauth_identities SET last_login_at = now(), email = NULLIF($3,'')
			 WHERE provider = $1 AND provider_user_id = $2`, su.Provider, su.ID, su.Email)

	case errors.Is(err, sql.ErrNoRows):
		// 2. 검증된 이메일이면 기존 계정에 연결
		linked := false
		if su.EmailVerified && su.Email != "" {
			e := NormalizeEmail(su.Email)
			err = tx.QueryRow(ctx, `
				SELECT id, COALESCE(email,''), is_admin, disabled
				  FROM accounts WHERE lower(email) = $1`, e).
				Scan(&a.ID, &a.Email, &a.IsAdmin, &disabled)
			if err == nil {
				if disabled {
					return "", nil, ErrDisabled
				}
				linked = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return "", nil, err
			}
		}

		// 3. 새 계정
		if !linked {
			var email *string
			if su.Email != "" {
				e := NormalizeEmail(su.Email)
				// 같은 이메일이 이미 있으면(미검증 소셜) 이메일 없이 만든다 — 탈취 방지.
				var exists bool
				if err := tx.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM accounts WHERE lower(email) = $1)`, e).
					Scan(&exists); err != nil {
					return "", nil, err
				}
				if !exists {
					email = &e
				}
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO accounts (email, password_hash, is_admin)
				VALUES ($1, NULL, FALSE)`, email)
			if err != nil {
				return "", nil, fmt.Errorf("계정 생성: %w", err)
			}
			a.ID, err = tag.LastInsertId()
			if err != nil {
				return "", nil, err
			}
			if email != nil {
				a.Email = *email
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO oauth_identities
			       (provider, provider_user_id, account_id, email, email_verified, last_login_at)
			VALUES ($1, $2, $3, NULLIF($4,''), $5, NOW(6))
			ON DUPLICATE KEY UPDATE last_login_at = NOW(6)`,
			su.Provider, su.ID, a.ID, su.Email, su.EmailVerified); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				return "", nil, apperr.New("이 계정에는 이미 같은 소셜 로그인이 연결돼 있습니다")
			}
			return "", nil, err
		}

	default:
		return "", nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", nil, err
	}

	// 지정 이메일의 실제 소유자만 관리자다. 검증되지 않은 제공자 프로필의
	// 이메일 문자열만으로 권한을 얻지 못하게 EmailVerified를 반드시 확인한다.
	if su.EmailVerified && IsAdminEmail(su.Email) {
		if _, err := pool.Exec(ctx, `UPDATE accounts SET is_admin = TRUE WHERE id = $1`, a.ID); err != nil {
			return "", nil, err
		}
		a.IsAdmin = true
	}

	token, err := issueSession(ctx, pool, a.ID, userAgent)
	if err != nil {
		return "", nil, err
	}
	_, _ = pool.Exec(ctx, `UPDATE accounts SET last_login_at = now() WHERE id = $1`, a.ID)
	return token, &a, nil
}

// LinkedProviders는 계정에 연결된 소셜 제공자 목록이다.
func LinkedProviders(ctx context.Context, pool *store.DB, accountID int64) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT provider FROM oauth_identities WHERE account_id = $1 ORDER BY provider`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
