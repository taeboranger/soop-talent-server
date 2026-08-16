package scoring

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LogEntry는 달란트 증감 내역 한 줄이다.
// 화면에는 "[타임라인] 달란트 증감내역 / 증감사유" 형태로 표시된다.
type LogEntry struct {
	ID         int64     `json:"id"`
	UserID     string    `json:"userId"`
	Nickname   string    `json:"nickname,omitempty"`
	Kind       string    `json:"kind"`
	Amount     int       `json:"amount"`
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurredAt"`
	Line       string    `json:"line"` // 완성된 한 줄. 그대로 출력하면 된다
}

type LogQuery struct {
	UserID string
	Kind   string
	Limit  int
	Before int64 // 이 id보다 작은 항목만 (커서 페이지네이션)
}

// Log는 달란트 증감 내역을 최신순으로 돌려준다.
func Log(ctx context.Context, pool *pgxpool.Pool, bid string, q LogQuery) ([]LogEntry, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	before := q.Before
	if before <= 0 {
		before = 1 << 62
	}

	rows, err := pool.Query(ctx, `
		SELECT e.id, e.user_id, COALESCE(u.nickname,''), e.kind, e.amount,
		       COALESCE(e.reason,''), e.occurred_at
		  FROM point_events e
		  LEFT JOIN users u ON u.bid = e.bid AND u.user_id = e.user_id
		 WHERE e.bid = $1
		   AND ($2 = '' OR e.user_id = $2)
		   AND ($3 = '' OR e.kind = $3)
		   AND e.id < $4
		 ORDER BY e.id DESC
		 LIMIT $5`, bid, q.UserID, q.Kind, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.Nickname, &e.Kind,
			&e.Amount, &e.Reason, &e.OccurredAt); err != nil {
			return nil, err
		}
		if e.Reason == "" {
			e.Reason = KindLabel(e.Kind)
		}
		e.Line = FormatLogLine(e)
		out = append(out, e)
	}
	return out, rows.Err()
}

// FormatLogLine은 요청받은 표기 형식을 만든다.
//
//	[2026-08-11 01:23:45] 홍길동(user123) +50 달란트 / 채팅 참여
func FormatLogLine(e LogEntry) string {
	who := e.UserID
	if e.Nickname != "" {
		who = fmt.Sprintf("%s(%s)", e.Nickname, e.UserID)
	}
	sign := "+"
	if e.Amount < 0 {
		sign = "" // 음수는 부호가 이미 붙어 있다
	}
	return fmt.Sprintf("[%s] %s %s%d 달란트 / %s",
		e.OccurredAt.Format("2006-01-02 15:04:05"), who, sign, e.Amount, e.Reason)
}

// Grant는 스트리머가 직접 달란트를 주거나 회수한다.
// 사유는 반드시 남긴다 — 나중에 왜 줬는지 설명할 수 없으면 분쟁이 생긴다.
func Grant(ctx context.Context, pool *pgxpool.Pool, bid, userID string, amount int, reason string) (*LogEntry, error) {
	if userID == "" {
		return nil, fmt.Errorf("대상 아이디를 입력해 주세요")
	}
	if amount == 0 {
		return nil, fmt.Errorf("0이 아닌 값을 입력해 주세요")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("지급 사유를 입력해 주세요")
	}

	now := time.Now()
	// 같은 사람에게 같은 금액을 여러 번 줄 수 있어야 하므로 시각을 키에 넣는다.
	idem := fmt.Sprintf("manual:%s:%d", userID, now.UnixNano())

	var e LogEntry
	err := pool.QueryRow(ctx, `
		INSERT INTO point_events (bid, user_id, kind, amount, occurred_at, idem_key, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, user_id, kind, amount, COALESCE(reason,''), occurred_at`,
		bid, userID, KindManual, amount, now, idem, reason).
		Scan(&e.ID, &e.UserID, &e.Kind, &e.Amount, &e.Reason, &e.OccurredAt)
	if err != nil {
		return nil, err
	}
	_ = pool.QueryRow(ctx,
		`SELECT COALESCE(nickname,'') FROM users WHERE bid = $1 AND user_id = $2`,
		bid, userID).Scan(&e.Nickname)
	e.Line = FormatLogLine(e)
	return &e, nil
}

// Holder는 달란트 보유자 한 명이다.
type Holder struct {
	UserID   string `json:"userId"`
	Nickname string `json:"nickname,omitempty"`
	Total    int    `json:"total"`
}

// Holders는 보유량 순으로 시청자를 돌려준다. 검색어로 좁힐 수 있다.
func Holders(ctx context.Context, pool *pgxpool.Pool, bid, search string, limit int) ([]Holder, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := pool.Query(ctx, `
		SELECT p.user_id, COALESCE(u.nickname,''), p.total::int
		  FROM user_points p
		  LEFT JOIN users u ON u.bid = p.bid AND u.user_id = p.user_id
		 WHERE p.bid = $1
		   AND ($2 = '' OR p.user_id ILIKE '%'||$2||'%' OR u.nickname ILIKE '%'||$2||'%')
		 ORDER BY p.total DESC
		 LIMIT $3`, bid, search, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Holder{}
	for rows.Next() {
		var h Holder
		if err := rows.Scan(&h.UserID, &h.Nickname, &h.Total); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
