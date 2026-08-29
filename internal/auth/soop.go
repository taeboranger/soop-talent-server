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

const soopOAuthNote = "SOOP OAuth로 본인 인증됨"

// LoginWithVerifiedSOOP은 공식 OAuth 토큰에서 검증한 SOOP user_id로 로그인한다.
// 로그인 자체는 시청자와 스트리머 모두에게 열려 있다. 스트리머 권한은
// ActivateVerifiedStreamer에서 별도로 활성화한다.
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

// ActivateVerifiedStreamer는 로그인한 사람이 OAuth로 검증된 자기 SOOP ID를
// 스트리머 방송국으로 활성화한다. 요청 본문에서 받은 ID가 아니라 DB에 저장된
// OAuth identity만 사용하므로, 시청자가 남의 방송국을 관리할 수 없다.
func ActivateVerifiedStreamer(ctx context.Context, pool *store.DB, accountID int64) (*Claim, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bid, nick string
	err = tx.QueryRow(ctx, `
		SELECT provider_user_id, COALESCE(nickname,'')
		  FROM oauth_identities
		 WHERE account_id = $1 AND provider = 'soop'`, accountID).Scan(&bid, &nick)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New("SOOP 로그인 후 스트리머 기능을 활성화할 수 있습니다")
	}
	if err != nil {
		return nil, err
	}

	var claimID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM streamer_claims
		 WHERE account_id = $1 AND lower(bid) = lower($2)`, accountID, bid).Scan(&claimID)
	switch {
	case err == nil:
		_, err = tx.Exec(ctx, `
			UPDATE streamer_claims
			   SET status = 'approved', soop_nick = NULLIF($2,''), note = $3,
			       decided_at = NOW(6), decided_by = NULL
			 WHERE id = $1`, claimID, nick, soopOAuthNote)
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO streamer_claims
		       (account_id, bid, soop_nick, status, note, decided_at, decided_by)
			VALUES ($1, $2, $3, 'approved', $4, NOW(6), NULL)`, accountID, bid, nick, soopOAuthNote)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return nil, ErrBidTaken
		}
	default:
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	claims, err := MyClaims(ctx, pool, accountID)
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		if strings.EqualFold(claim.BID, bid) {
			return &claim, nil
		}
	}
	return nil, fmt.Errorf("스트리머 연결 상태를 확인하지 못했습니다")
}
