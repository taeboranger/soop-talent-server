package shop

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/store"
)

var (
	// ErrMarketClosed는 링크는 존재하지만 스트리머가 시장을 닫은 상태다.
	// API 계층에서 410 Gone으로 바꿔 시청자 화면이 종료 안내를 보여 준다.
	ErrMarketClosed = apperr.New("이 시장은 종료되었습니다")
	requestKeyRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
)

type CheckoutLine struct {
	ItemID   int64 `json:"itemId"`
	Quantity int   `json:"quantity"`
}

type CheckoutResult struct {
	Order    *Order `json:"order"`
	Balance  int    `json:"balance"`
	Replayed bool   `json:"replayed"`
}

// ViewerOrder는 시청자 프로필에 보여 줄 본인 구매 이력이다. 주문의 세부 내용에
// 시장 제목과 스트리머 ID만 덧붙인다. 공유 링크 토큰이나 다른 시청자 정보는 없다.
type ViewerOrder struct {
	Order
	PageID      int64  `json:"pageId"`
	MarketTitle string `json:"marketTitle"`
	StreamerID  string `json:"streamerId"`
	MarketOpen  bool   `json:"marketOpen"`
	RewardUnit  string `json:"rewardUnit"`
}

// OrderCancelResult는 시청자가 자기 구매를 취소한 뒤 갱신할 화면 상태다.
type OrderCancelResult struct {
	Order   *Order `json:"order"`
	Balance int    `json:"balance"`
}

// CancelResult는 시장 취소로 되돌린 주문·달란트 합계다.
// 주문 행을 지우지 않고 cancelled 상태로 남겨, 스트리머가 누구에게 무엇을
// 환불했는지 나중에도 확인할 수 있다.
type CancelResult struct {
	Page            *Page `json:"page"`
	RefundedOrders  int   `json:"refundedOrders"`
	RefundedTalents int   `json:"refundedTalents"`
}

// OpenWeb은 게시글 연결 없이 시장을 연다. 초안은 새 공유 링크 토큰을 만들고,
// 단순히 닫은 시장은 기존 토큰·구매 기록을 유지한 채 다시 연다. 취소·환불된 시장도
// 같은 링크로 다시 열 수 있는데, 취소 주문은 재고·인당 제한 계산에서 제외되어 새
// 시장처럼 다시 구매할 수 있다.
func OpenWeb(ctx context.Context, pool *store.DB, bid string, pageID int64) (*Page, error) {
	p, err := Get(ctx, pool, bid, pageID)
	if err != nil {
		return nil, err
	}
	if p.Status != StatusDraft && p.Status != StatusClosed && p.Status != StatusCancelled {
		return nil, ErrNotDraft
	}
	if len(p.Items) == 0 {
		return nil, ErrNoItems
	}

	for range 4 {
		token := p.PublicToken
		if token == "" {
			token, err = newPublicToken()
			if err != nil {
				return nil, err
			}
		}
		tag, err := pool.Exec(ctx, `
			UPDATE shop_pages
			   SET status = 'open', public_token = $3, opened_at = NOW(6), closed_at = NULL
			 WHERE id = $1 AND bid = $2 AND status IN ('draft','closed','cancelled')`, pageID, bid, token)
		if err != nil {
			// 충돌은 사실상 토큰 중복뿐이다. 새 토큰으로 한 번 더 시도한다.
			if strings.Contains(strings.ToLower(err.Error()), "shop_pages_public_token_uniq") {
				continue
			}
			return nil, err
		}
		if n, _ := tag.RowsAffected(); n == 0 {
			return nil, apperr.New("시장을 다시 열 수 없습니다. 현재 상태를 새로고침해 주세요")
		}
		return Get(ctx, pool, bid, pageID)
	}
	return nil, fmt.Errorf("시장 공유 링크를 만들지 못했습니다")
}

func newPublicToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CancelWeb은 열린 시장을 취소하고, 이미 확정된 모든 구매 달란트를 원자적으로
// 되돌린다. 페이지 행을 먼저 잠그므로 결제와 취소가 동시에 들어와도 하나가 끝난
// 뒤의 상태만 보게 된다.
func CancelWeb(ctx context.Context, pool *store.DB, bid string, pageID int64) (*CancelResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p Page
	err = tx.QueryRow(ctx, `
		SELECT id,bid,title,status FROM shop_pages WHERE id=$1 AND bid=$2 FOR UPDATE`, pageID, bid).
		Scan(&p.ID, &p.BID, &p.Title, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Status != StatusOpen {
		return nil, ErrNotOpen
	}

	rows, err := tx.Query(ctx, `
		SELECT id,user_id,total_price FROM purchase_orders
		 WHERE page_id=$1 AND status='fulfilled' FOR UPDATE`, pageID)
	if err != nil {
		return nil, err
	}
	type purchase struct {
		id     int64
		userID string
		price  int
	}
	orders := []purchase{}
	for rows.Next() {
		var o purchase
		if err := rows.Scan(&o.id, &o.userID, &o.price); err != nil {
			rows.Close()
			return nil, err
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	result := &CancelResult{RefundedOrders: len(orders)}
	for _, o := range orders {
		if o.price > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO point_events (bid,user_id,kind,amount,occurred_at,idem_key,meta,reason)
				VALUES ($1,$2,$3,$4,NOW(6),$5,$6,$7)
				ON DUPLICATE KEY UPDATE idem_key=idem_key`,
				p.BID, o.userID, scoring.KindPurchaseRefund, o.price,
				fmt.Sprintf("purchase-refund:%d", o.id),
				fmt.Sprintf(`{"pageId":%d,"orderId":%d}`, p.ID, o.id),
				fmt.Sprintf("%s 시장 취소 환불", p.Title)); err != nil {
				return nil, err
			}
			result.RefundedTalents += o.price
		}
		if _, err := tx.Exec(ctx, `
			UPDATE purchase_orders
			   SET status='cancelled', reject_reason='시장 취소로 전액 환불됨'
			 WHERE id=$1 AND status='fulfilled'`, o.id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE shop_pages SET status='cancelled', closed_at=NOW(6)
		 WHERE id=$1 AND bid=$2 AND status='open'`, p.ID, p.BID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	page, err := Get(ctx, pool, bid, pageID)
	if err != nil {
		return nil, err
	}
	result.Page = page
	return result, nil
}

// PublicPage은 공유 링크 토큰으로 열린 시장을 가져온다. 닫힌 링크에는 어떤
// 보상·잔액도 내보내지 않는다.
func PublicPage(ctx context.Context, pool *store.DB, token string) (*Page, error) {
	var p Page
	err := pool.QueryRow(ctx, `
		SELECT id, bid, title, COALESCE(description,''), status, COALESCE(public_token,''),
		       opened_at, closed_at, created_at
		  FROM shop_pages WHERE public_token = $1`, strings.TrimSpace(token)).
		Scan(&p.ID, &p.BID, &p.Title, &p.Description, &p.Status, &p.PublicToken,
			&p.OpenedAt, &p.ClosedAt, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Status != StatusOpen {
		return nil, ErrMarketClosed
	}
	p.Items, err = Items(ctx, pool, p.ID)
	return &p, err
}

// Checkout은 시장 행을 잠가 닫힘·재고·인당 제한·잔액 확인과 차감을 하나의
// 트랜잭션으로 처리한다. 링크가 열린 탭에 남아 있어도 닫힌 뒤에는 통과하지 않는다.
func Checkout(ctx context.Context, pool *store.DB, token, userID, nickname, requestKey string, lines []CheckoutLine) (*CheckoutResult, error) {
	userID = strings.TrimSpace(userID)
	nickname = strings.TrimSpace(nickname)
	requestKey = strings.TrimSpace(requestKey)
	if !validSOOPUserID(userID) {
		return nil, apperr.New("SOOP 로그인 정보를 확인하지 못했습니다")
	}
	if !requestKeyRe.MatchString(requestKey) {
		return nil, apperr.New("구매 요청 식별자가 올바르지 않습니다")
	}
	wanted := map[int64]int{}
	if len(lines) == 0 || len(lines) > 30 {
		return nil, apperr.New("보상을 하나 이상, 30개 이하로 선택해 주세요")
	}
	for _, line := range lines {
		if line.ItemID < 1 || line.Quantity < 1 || line.Quantity > 1000 {
			return nil, apperr.New("보상 수량이 올바르지 않습니다")
		}
		wanted[line.ItemID] += line.Quantity
		if wanted[line.ItemID] > 1000 {
			return nil, apperr.New("보상 하나당 1,000개까지만 요청할 수 있습니다")
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p Page
	err = tx.QueryRow(ctx, `
		SELECT id, bid, title, COALESCE(description,''), status, COALESCE(public_token,'')
		  FROM shop_pages WHERE public_token = $1 FOR UPDATE`, strings.TrimSpace(token)).
		Scan(&p.ID, &p.BID, &p.Title, &p.Description, &p.Status, &p.PublicToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Status != StatusOpen {
		return nil, ErrMarketClosed
	}

	// 재시도한 요청은 같은 주문을 돌려준다. 페이지 행 잠금 뒤에 검사하므로
	// 동시에 같은 요청이 와도 첫 요청의 결과가 확정된 다음 재사용된다.
	var existingID int64
	err = tx.QueryRow(ctx, `SELECT id FROM purchase_orders WHERE page_id = $1 AND request_key = $2`, p.ID, requestKey).Scan(&existingID)
	if err == nil {
		if err := tx.Rollback(ctx); err != nil {
			return nil, err
		}
		order, err := OrderByID(ctx, pool, existingID)
		if err != nil {
			return nil, err
		}
		balance, err := Balance(ctx, pool, p.BID, userID)
		return &CheckoutResult{Order: order, Balance: balance, Replayed: true}, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	ids := make([]int64, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	resolved := make([]struct {
		item Item
		qty  int
	}, 0, len(ids))
	total := 0
	for _, itemID := range ids {
		var it Item
		err := tx.QueryRow(ctx, `
			SELECT id, name, price, stock, per_user_limit, sort_order, image_id
			  FROM shop_items WHERE id = $1 AND page_id = $2 FOR UPDATE`, itemID, p.ID).
			Scan(&it.ID, &it.Name, &it.Price, &it.Stock, &it.PerUserLimit, &it.SortOrder, &it.ImageID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New("선택한 보상을 찾을 수 없습니다. 시장을 새로고침해 주세요")
		}
		if err != nil {
			return nil, err
		}
		qty := wanted[itemID]
		if it.Stock != nil {
			var used int
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(l.quantity),0)
				  FROM purchase_lines l JOIN purchase_orders o ON o.id=l.order_id
				 WHERE o.page_id=$1 AND o.status='fulfilled' AND l.item_id=$2`, p.ID, it.ID).Scan(&used); err != nil {
				return nil, err
			}
			if used+qty > *it.Stock {
				return nil, apperr.Newf("남은 수량이 부족합니다: %s", it.Name)
			}
		}
		if it.PerUserLimit != nil {
			var bought int
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(l.quantity),0)
				  FROM purchase_lines l JOIN purchase_orders o ON o.id=l.order_id
				 WHERE o.page_id=$1 AND o.user_id=$2 AND o.status='fulfilled' AND l.item_id=$3`, p.ID, userID, it.ID).Scan(&bought); err != nil {
				return nil, err
			}
			if bought+qty > *it.PerUserLimit {
				return nil, apperr.Newf("인당 구매 제한을 넘었습니다: %s", it.Name)
			}
		}
		resolved = append(resolved, struct {
			item Item
			qty  int
		}{it, qty})
		total += it.Price * qty
	}

	var before *int
	if err := tx.QueryRow(ctx, `SELECT SUM(amount) FROM point_events WHERE bid=$1 AND user_id=$2`, p.BID, userID).Scan(&before); err != nil {
		return nil, err
	}
	balance := 0
	if before != nil {
		balance = *before
	}
	if balance < total {
		return nil, apperr.Newf("잔액이 부족합니다 (보유 %d / 필요 %d)", balance, total)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO purchase_orders
		       (page_id,bid,user_id,nickname,comment_no,raw_comment,request_key,total_price,status,settled_at)
		VALUES ($1,$2,$3,NULLIF($4,''),NULL,NULL,$5,$6,'fulfilled',NOW(6))`,
		p.ID, p.BID, userID, nickname, requestKey, total)
	if err != nil {
		return nil, err
	}
	orderID, err := tag.LastInsertId()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resolved))
	for _, r := range resolved {
		if _, err := tx.Exec(ctx, `INSERT INTO purchase_lines (order_id,item_id,item_name,quantity,unit_price) VALUES ($1,$2,$3,$4,$5)`,
			orderID, r.item.ID, r.item.Name, r.qty, r.item.Price); err != nil {
			return nil, err
		}
		if r.qty > 1 {
			names = append(names, fmt.Sprintf("%s x%d", r.item.Name, r.qty))
		} else {
			names = append(names, r.item.Name)
		}
	}
	if total > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO point_events (bid,user_id,kind,amount,occurred_at,idem_key,meta,reason)
			VALUES ($1,$2,$3,$4,NOW(6),$5,$6,$7)`,
			p.BID, userID, scoring.KindPurchase, -total, fmt.Sprintf("purchase:%d:%s", p.ID, requestKey),
			fmt.Sprintf(`{"pageId":%d,"orderId":%d}`, p.ID, orderID),
			fmt.Sprintf("%s 구매 — %s", p.Title, strings.Join(names, ", "))); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &CheckoutResult{Order: &Order{ID: orderID, UserID: userID, Nickname: nickname, TotalPrice: total, Status: "fulfilled", Lines: toLines(resolved)}, Balance: balance - total}, nil
}

func toLines(resolved []struct {
	item Item
	qty  int
}) []Line {
	lines := make([]Line, 0, len(resolved))
	for _, r := range resolved {
		lines = append(lines, Line{ItemName: r.item.Name, Quantity: r.qty, UnitPrice: r.item.Price})
	}
	return lines
}

func validSOOPUserID(id string) bool {
	return id != "" && !strings.HasPrefix(id, "-") && !strings.ContainsAny(id, "|/\\")
}

func OrderByID(ctx context.Context, pool *store.DB, id int64) (*Order, error) {
	var o Order
	err := pool.QueryRow(ctx, `
		SELECT id,user_id,COALESCE(nickname,''),COALESCE(comment_no,0),COALESCE(raw_comment,''),
		       total_price,status,COALESCE(reject_reason,''),commented_at,settled_at
		  FROM purchase_orders WHERE id=$1`, id).
		Scan(&o.ID, &o.UserID, &o.Nickname, &o.CommentNo, &o.RawComment, &o.TotalPrice, &o.Status, &o.RejectReason, &o.CommentedAt, &o.OrderedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT item_name,quantity,unit_price FROM purchase_lines WHERE order_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var line Line
		if err := rows.Scan(&line.ItemName, &line.Quantity, &line.UnitPrice); err != nil {
			return nil, err
		}
		o.Lines = append(o.Lines, line)
	}
	return &o, rows.Err()
}

// CancelOrder는 시장이 열려 있는 동안에만 시청자 본인의 확정 구매를 취소한다.
// 재고는 확정(fulfilled) 주문만 합산하므로, 상태 변경만으로 즉시 수량과 인당
// 제한이 되돌아간다. 환불 이벤트·주문 상태 변경은 반드시 같은 트랜잭션이다.
// token은 시장 화면에서만 전달하며, 빈 값은 프로필 구매내역 화면을 뜻한다.
func CancelOrder(ctx context.Context, pool *store.DB, orderID int64, userID, token string) (*OrderCancelResult, error) {
	if orderID < 1 || !validSOOPUserID(userID) {
		return nil, apperr.New("구매 내역을 확인할 수 없습니다")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var order Order
	var page Page
	err = tx.QueryRow(ctx, `
		SELECT o.id,o.user_id,COALESCE(o.nickname,''),COALESCE(o.comment_no,0),COALESCE(o.raw_comment,''),
		       o.total_price,o.status,COALESCE(o.reject_reason,''),o.commented_at,o.settled_at,
		       p.id,p.bid,p.title,p.status
		  FROM purchase_orders o JOIN shop_pages p ON p.id=o.page_id
		 WHERE o.id=$1 AND o.user_id=$2
		   AND (NULLIF($3,'') IS NULL OR p.public_token=$3)
		 FOR UPDATE`, orderID, userID, strings.TrimSpace(token)).
		Scan(&order.ID, &order.UserID, &order.Nickname, &order.CommentNo, &order.RawComment,
			&order.TotalPrice, &order.Status, &order.RejectReason, &order.CommentedAt, &order.OrderedAt,
			&page.ID, &page.BID, &page.Title, &page.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if page.Status != StatusOpen {
		return nil, ErrMarketClosed
	}
	if order.Status != "fulfilled" {
		return nil, apperr.New("이미 취소되었거나 취소할 수 없는 구매입니다")
	}

	if order.TotalPrice > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO point_events (bid,user_id,kind,amount,occurred_at,idem_key,meta,reason)
			VALUES ($1,$2,$3,$4,NOW(6),$5,$6,$7)
			ON DUPLICATE KEY UPDATE idem_key=idem_key`,
			page.BID, order.UserID, scoring.KindPurchaseRefund, order.TotalPrice,
			fmt.Sprintf("purchase-cancel:%d", order.ID),
			fmt.Sprintf(`{"pageId":%d,"orderId":%d}`, page.ID, order.ID),
			fmt.Sprintf("%s 구매 취소 환불", page.Title)); err != nil {
			return nil, err
		}
	}
	const reason = "시청자가 시장이 열려 있는 동안 구매를 취소함"
	if _, err := tx.Exec(ctx, `
		UPDATE purchase_orders SET status='cancelled', reject_reason=$2
		 WHERE id=$1 AND status='fulfilled'`, order.ID, reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	order.Status, order.RejectReason = "cancelled", reason
	balance, err := Balance(ctx, pool, page.BID, order.UserID)
	if err != nil {
		return nil, err
	}
	return &OrderCancelResult{Order: &order, Balance: balance}, nil
}

// ViewerOrders는 한 SOOP 계정의 구매 이력만 최신순으로 읽는다. 호출자는 API에서
// 세션에 들어 있는 SOOP ID로 고정되므로, 전달받은 값으로 타인의 주문을 찾지 않는다.
func ViewerOrders(ctx context.Context, pool *store.DB, userID string) ([]ViewerOrder, error) {
	return viewerOrders(ctx, pool, userID, 0)
}

// ViewerOrdersForPage는 시장 화면에 표시할 로그인한 시청자 본인의 주문만 돌려준다.
func ViewerOrdersForPage(ctx context.Context, pool *store.DB, userID string, pageID int64) ([]ViewerOrder, error) {
	return viewerOrders(ctx, pool, userID, pageID)
}

func viewerOrders(ctx context.Context, pool *store.DB, userID string, pageID int64) ([]ViewerOrder, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return []ViewerOrder{}, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT o.id,o.user_id,COALESCE(o.nickname,''),COALESCE(o.comment_no,0),COALESCE(o.raw_comment,''),
		       o.total_price,o.status,COALESCE(o.reject_reason,''),o.commented_at,o.settled_at,
		       p.id,p.title,p.bid,(p.status='open'),COALESCE(ru.unit,'달란트')
		  FROM purchase_orders o JOIN shop_pages p ON p.id=o.page_id
		  LEFT JOIN streamer_reward_units ru ON ru.bid=p.bid
		 WHERE o.user_id=$1 AND ($2=0 OR o.page_id=$2)
		 ORDER BY o.settled_at DESC,o.id DESC
		 LIMIT 500`, userID, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ViewerOrder{}
	index := map[int64]int{}
	for rows.Next() {
		var o ViewerOrder
		if err := rows.Scan(&o.ID, &o.UserID, &o.Nickname, &o.CommentNo, &o.RawComment,
			&o.TotalPrice, &o.Status, &o.RejectReason, &o.CommentedAt, &o.OrderedAt,
			&o.PageID, &o.MarketTitle, &o.StreamerID, &o.MarketOpen, &o.RewardUnit); err != nil {
			return nil, err
		}
		index[o.ID] = len(out)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	lines, err := pool.Query(ctx, `
		SELECT l.order_id,l.item_name,l.quantity,l.unit_price
		  FROM purchase_lines l JOIN purchase_orders o ON o.id=l.order_id
		 WHERE o.user_id=$1 AND ($2=0 OR o.page_id=$2)
		 ORDER BY l.id`, userID, pageID)
	if err != nil {
		return nil, err
	}
	defer lines.Close()
	for lines.Next() {
		var id int64
		var line Line
		if err := lines.Scan(&id, &line.ItemName, &line.Quantity, &line.UnitPrice); err != nil {
			return nil, err
		}
		if i, ok := index[id]; ok {
			out[i].Lines = append(out[i].Lines, line)
		}
	}
	return out, lines.Err()
}
