package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/store"
)

// LoginWithVerifiedSOOP은 공식 OAuth 토큰에서 검증한 SOOP user_id로 로그인한다.
// 로그인한 모든 사람은 우선 시청자다. 스트리머 기능은 관리자 승인 뒤에만 열린다.
//
// 예전에 관리자가 같은 bid를 승인한 계정이 있으면 그 계정에 SOOP 로그인을
// 붙여 기존 달란트 설정과 기록을 보존한다. 승인된 기존 소유자가 없을 때만
// 새 계정을 만든다.
func LoginWithVerifiedSOOP(ctx context.Context, pool *store.DB, bid, nickname, userAgent string) (token string, acct *Account, err error) {
	bid = strings.TrimSpace(bid)
	if bid == "" || strings.HasPrefix(bid, "-") || strings.ContainsAny(bid, "|/\\") {
		return "", nil, apperr.New("SOOP 계정 식별자가 올바르지 않습니다")
	}
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = bid
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a Account
	var disabled bool
	identityExists := true
	err = tx.QueryRow(ctx, `
		SELECT a.id, COALESCE(a.email,''), a.is_admin, a.disabled,
		       o.provider_user_id, COALESCE(o.nickname,'')
		  FROM oauth_identities o JOIN accounts a ON a.id = o.account_id
		 WHERE o.provider = 'soop' AND lower(o.provider_user_id) = lower($1)`, bid).
		Scan(&a.ID, &a.Email, &a.IsAdmin, &disabled, &a.SoopID, &a.Nickname)
	if errors.Is(err, sql.ErrNoRows) {
		identityExists = false
	} else if err != nil {
		return "", nil, err
	} else if disabled {
		return "", nil, ErrDisabled
	}

	// 기존 수동 승인 계정이 있으면 정식 SOOP 로그인을 그 계정에 귀속시킨다.
	var approvedOwner int64
	err = tx.QueryRow(ctx, `
		SELECT account_id FROM streamer_claims
		 WHERE lower(bid) = lower($1) AND status = 'approved'`, bid).Scan(&approvedOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	if identityExists && approvedOwner != 0 && approvedOwner != a.ID {
		return "", nil, ErrBidTaken
	}

	if !identityExists {
		if approvedOwner != 0 {
			err = tx.QueryRow(ctx, `
				SELECT id, COALESCE(email,''), is_admin, disabled
				  FROM accounts WHERE id = $1`, approvedOwner).
				Scan(&a.ID, &a.Email, &a.IsAdmin, &disabled)
			if err != nil {
				return "", nil, err
			}
			if disabled {
				return "", nil, ErrDisabled
			}
		} else {
			tag, err := tx.Exec(ctx, `
				INSERT INTO accounts (email, password_hash, is_admin)
				VALUES (NULL, NULL, FALSE)`)
			if err != nil {
				return "", nil, fmt.Errorf("SOOP 계정 생성: %w", err)
			}
			a.ID, err = tag.LastInsertId()
			if err != nil {
				return "", nil, err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO oauth_identities
			       (provider, provider_user_id, nickname, account_id, email, email_verified, last_login_at)
			VALUES ('soop', $1, $2, $3, NULL, FALSE, NOW(6))`, bid, nickname, a.ID); err != nil {
			return "", nil, err
		}
		a.SoopID, a.Nickname = bid, nickname
	} else {
		_, _ = tx.Exec(ctx, `
			UPDATE oauth_identities
			   SET nickname = NULLIF($2,''), email = NULL, email_verified = FALSE, last_login_at = NOW(6)
			 WHERE provider = 'soop' AND lower(provider_user_id) = lower($1)`, bid, nickname)
		a.SoopID, a.Nickname = bid, nickname
	}

	if err := tx.Commit(ctx); err != nil {
		return "", nil, err
	}
	if IsAdminSOOPID(bid) {
		if _, err := pool.Exec(ctx, `UPDATE accounts SET is_admin = TRUE WHERE id = $1`, a.ID); err != nil {
			return "", nil, err
		}
		a.IsAdmin = true
	}
	token, err = issueSession(ctx, pool, a.ID, userAgent)
	if err != nil {
		return "", nil, err
	}
	_, _ = pool.Exec(ctx, `UPDATE accounts SET last_login_at = NOW(6) WHERE id = $1`, a.ID)
	return token, &a, nil
}

// RequestVerifiedStreamer는 OAuth로 검증된 본인 SOOP ID로 스트리머 승인을
// 신청한다. 요청 본문에는 방송국 ID를 받지 않으므로 다른 사람 방송국을 신청할 수
// 없고, 승인 전에는 어떤 스트리머 API도 열리지 않는다.
func RequestVerifiedStreamer(ctx context.Context, pool *store.DB, accountID int64) (*Claim, error) {
	var bid, nick string
	err := pool.QueryRow(ctx, `
		SELECT provider_user_id, COALESCE(nickname,'')
		  FROM oauth_identities
		 WHERE account_id = $1 AND provider = 'soop'`, accountID).Scan(&bid, &nick)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New("SOOP 로그인 후 스트리머 기능을 활성화할 수 있습니다")
	}
	if err != nil {
		return nil, err
	}

	return SubmitClaim(ctx, pool, accountID, bid, nick)
}
