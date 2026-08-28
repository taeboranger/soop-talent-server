package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/store"
)

var (
	ErrClaimExists   = apperr.New("이미 신청한 아이디입니다")
	ErrBidTaken      = apperr.New("이미 다른 계정에 연결된 SOOP 아이디입니다")
	ErrNoApprovedBid = errors.New("승인된 SOOP 아이디가 없습니다")
)

type ClaimStatus string

const (
	StatusPending  ClaimStatus = "pending"
	StatusApproved ClaimStatus = "approved"
	StatusRejected ClaimStatus = "rejected"
)

type Claim struct {
	ID          int64       `json:"id"`
	AccountID   int64       `json:"accountId"`
	UserID      string      `json:"userId,omitempty"`
	Nickname    string      `json:"nickname,omitempty"`
	BID         string      `json:"bid"`
	SoopNick    string      `json:"soopNick,omitempty"`
	Status      ClaimStatus `json:"status"`
	Note        string      `json:"note,omitempty"`
	RequestedAt time.Time   `json:"requestedAt"`
	DecidedAt   *time.Time  `json:"decidedAt,omitempty"`
}

// SubmitClaim은 연결 신청을 등록한다.
// 신청만으로는 아무 권한도 생기지 않는다 — 관리자가 승인해야 한다.
func SubmitClaim(ctx context.Context, pool *store.DB, accountID int64, bid, nick string) (*Claim, error) {
	bid = strings.TrimSpace(bid)
	if bid == "" {
		return nil, apperr.New("SOOP 아이디를 입력해 주세요")
	}

	// 이미 승인된 아이디는 다른 계정이 가져갈 수 없다.
	var ownerID int64
	err := pool.QueryRow(ctx, `
		SELECT account_id FROM streamer_claims
		 WHERE lower(bid) = lower($1) AND status = 'approved'`, bid).Scan(&ownerID)
	if err == nil && ownerID != accountID {
		return nil, ErrBidTaken
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO streamer_claims (account_id, bid, soop_nick)
		VALUES ($1, $2, NULLIF($3,''))
		ON DUPLICATE KEY UPDATE
		   soop_nick = COALESCE(NULLIF(VALUES(soop_nick),''), soop_nick),
		       -- 거절된 신청은 다시 대기 상태로 되돌린다
		       requested_at = IF(status = 'rejected', NOW(6), requested_at),
		       status = IF(status = 'rejected', 'pending', status)`, accountID, bid, nick)
	if err != nil {
		return nil, err
	}
	var c Claim
	err = pool.QueryRow(ctx, `SELECT id, account_id, bid, COALESCE(soop_nick,''), status, COALESCE(note,''), requested_at, decided_at
        FROM streamer_claims WHERE account_id = $1 AND bid = $2`, accountID, bid).
		Scan(&c.ID, &c.AccountID, &c.BID, &c.SoopNick, &c.Status, &c.Note, &c.RequestedAt, &c.DecidedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MyClaims는 한 계정의 신청 목록을 돌려준다.
func MyClaims(ctx context.Context, pool *store.DB, accountID int64) ([]Claim, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, account_id, bid, COALESCE(soop_nick,''), status,
		       COALESCE(note,''), requested_at, decided_at
		  FROM streamer_claims WHERE account_id = $1
		 ORDER BY requested_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanClaims(rows)
}

// ApprovedBID는 계정이 관리할 수 있는 SOOP 아이디를 돌려준다.
//
// **모든 권한 판단의 단일 진입점이다.** 클라이언트가 보낸 아이디는 절대 쓰지 않는다.
func ApprovedBID(ctx context.Context, pool *store.DB, accountID int64) (string, error) {
	var bid string
	err := pool.QueryRow(ctx, `
		SELECT bid FROM streamer_claims
		 WHERE account_id = $1 AND status = 'approved'
		 ORDER BY decided_at IS NULL, decided_at LIMIT 1`, accountID).Scan(&bid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoApprovedBid
	}
	return bid, err
}

// ---------------------------------------------------------------------------
// 관리자
// ---------------------------------------------------------------------------

// ListClaims는 관리자 화면용 목록이다. status가 비면 전체.
func ListClaims(ctx context.Context, pool *store.DB, status string) ([]Claim, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.id, c.account_id, COALESCE(o.provider_user_id,''), COALESCE(o.nickname,''),
		       c.bid, COALESCE(c.soop_nick,''), c.status,
		       COALESCE(c.note,''), c.requested_at, c.decided_at
		  FROM streamer_claims c
		  LEFT JOIN oauth_identities o ON o.account_id = c.account_id AND o.provider = 'soop'
		 WHERE ($1 = '' OR c.status = $1)
		 ORDER BY CASE c.status WHEN 'pending' THEN 0 ELSE 1 END, c.requested_at DESC
		 LIMIT 500`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.AccountID, &c.UserID, &c.Nickname, &c.BID, &c.SoopNick,
			&c.Status, &c.Note, &c.RequestedAt, &c.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Decide는 신청을 승인하거나 거절한다. 승인된 아이디는 수집이 시작된다.
func Decide(ctx context.Context, pool *store.DB, claimID, adminID int64, approve bool, note string) (*Claim, error) {
	status := StatusRejected
	if approve {
		status = StatusApproved
	}

	tag, err := pool.Exec(ctx, `
		UPDATE streamer_claims
		   SET status = $2, note = NULLIF($3,''), decided_at = NOW(6), decided_by = $4
		 WHERE id = $1`, claimID, string(status), note, adminID)

	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			return nil, ErrBidTaken
		}
		return nil, err
	}
	n, _ := tag.RowsAffected()
	if n == 0 {
		return nil, apperr.New("신청을 찾을 수 없습니다")
	}
	var c Claim
	err = pool.QueryRow(ctx, `SELECT id, account_id, bid, COALESCE(soop_nick,''), status, COALESCE(note,''), requested_at, decided_at FROM streamer_claims WHERE id = $1`, claimID).
		Scan(&c.ID, &c.AccountID, &c.BID, &c.SoopNick, &c.Status, &c.Note, &c.RequestedAt, &c.DecidedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ApprovedBIDs는 승인된 모든 SOOP 아이디를 돌려준다. 기동 시 수집기를 띄우는 데 쓴다.
func ApprovedBIDs(ctx context.Context, pool *store.DB) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT bid FROM streamer_claims WHERE status = 'approved' ORDER BY bid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanClaims(rows *sql.Rows) ([]Claim, error) {
	var out []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.AccountID, &c.BID, &c.SoopNick,
			&c.Status, &c.Note, &c.RequestedAt, &c.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
