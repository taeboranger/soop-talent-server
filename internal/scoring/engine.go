package scoring

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event는 적립 한 건이다.
// IdemKey가 이미 존재하면 조용히 무시되므로, 같은 입력을 몇 번 넣어도 안전하다.
type Event struct {
	BID        string
	UserID     string
	Kind       string
	Amount     int
	OccurredAt time.Time
	IdemKey    string
	BroadNo    *int64
	Meta       map[string]any
	Reason     string // 비어 있으면 화면에서 KindLabel(Kind)로 대체된다
}

// 적립 종류
const (
	KindRankWatch    = "rank_watch"
	KindRankChat     = "rank_chat"
	KindStay         = "stay"
	KindChat         = "chat"
	KindSubscribe    = "subscribe"
	KindFanclub      = "fanclub"
	KindDonation     = "donation"
	KindBoardPost    = "board_post"
	KindBoardComment = "board_comment"
	KindManual       = "manual"   // 스트리머가 직접 지급/차감
	KindPurchase     = "purchase" // 달란트 시장 구매 차감
)

// KindLabel은 로그 화면에 보여줄 한국어 사유다.
// reason이 따로 저장돼 있으면 그쪽이 우선한다.
func KindLabel(kind string) string {
	switch kind {
	case KindRankWatch:
		return "일별 시청 랭킹"
	case KindRankChat:
		return "일별 채팅 랭킹"
	case KindStay:
		return "방송 시청"
	case KindChat:
		return "채팅 참여"
	case KindSubscribe:
		return "구독"
	case KindFanclub:
		return "팬클럽 가입"
	case KindDonation:
		return "후원"
	case KindBoardPost:
		return "게시글 작성"
	case KindBoardComment:
		return "댓글 작성"
	case KindManual:
		return "직접 지급"
	case KindPurchase:
		return "보상 교환"
	}
	return kind
}

// Award는 이벤트들을 적립한다. 중복 idem_key는 건너뛴다.
// 실제로 삽입된 건수를 돌려준다.
func Award(ctx context.Context, pool *pgxpool.Pool, events []Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, e := range events {
		if e.Amount == 0 || e.UserID == "" || e.IdemKey == "" {
			continue
		}
		meta, err := json.Marshal(e.Meta)
		if err != nil {
			meta = []byte("null")
		}
		batch.Queue(`
			INSERT INTO point_events
			       (bid, user_id, kind, amount, occurred_at, idem_key, broad_no, meta, reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,''))
			ON CONFLICT (bid, idem_key) DO NOTHING`,
			e.BID, e.UserID, e.Kind, e.Amount, e.OccurredAt, e.IdemKey, e.BroadNo, meta, e.Reason)
	}
	if batch.Len() == 0 {
		return 0, nil
	}

	br := pool.SendBatch(ctx, batch)
	defer br.Close()

	inserted := 0
	for range batch.Len() {
		tag, err := br.Exec()
		if err != nil {
			return inserted, fmt.Errorf("적립 실패: %w", err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// RankRow는 확장이 올린 랭킹 한 줄이다.
type RankRow struct {
	Rank    int    `json:"rank"`
	UserID  string `json:"userId"`
	Minutes int    `json:"minutes"`
	Raw     string `json:"raw"`
}

// IngestRanking은 하루치 랭킹을 적립 이벤트로 바꾼다.
//
// 멱등키가 `rank:{kind}:{date}:{user}` 형태이므로 같은 날짜를 몇 번 올려도
// 중복 적립되지 않는다. 확장이 최근 7일을 매번 다시 올리는 이유가 이것이다.
func IngestRanking(ctx context.Context, pool *pgxpool.Pool, bid, kind, date string, rows []RankRow, r Rules) (int, error) {
	switch kind {
	case "watch":
		if !r.Rank.EnableWatch {
			return 0, nil
		}
	case "chat":
		if !r.Rank.EnableChat {
			return 0, nil
		}
	default:
		return 0, fmt.Errorf("알 수 없는 랭킹 종류: %q", kind)
	}

	// 랭킹은 그 날짜의 활동에 대한 것이므로 날짜 자정을 발생 시각으로 삼는다.
	at, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, fmt.Errorf("날짜 형식: %w", err)
	}

	eventKind := KindRankWatch
	if kind == "chat" {
		eventKind = KindRankChat
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		pts := r.Rank.RankPoints(row.Rank)
		if pts <= 0 || row.UserID == "" {
			continue
		}
		events = append(events, Event{
			BID:        bid,
			UserID:     row.UserID,
			Kind:       eventKind,
			Amount:     pts,
			OccurredAt: at,
			IdemKey:    fmt.Sprintf("rank:%s:%s:%s", kind, date, row.UserID),
			Meta: map[string]any{
				"rank":    row.Rank,
				"minutes": row.Minutes,
				"raw":     row.Raw,
			},
		})
	}
	return Award(ctx, pool, events)
}
