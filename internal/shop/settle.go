package shop

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/soop"
	"github.com/sooptalent/server/internal/store"
)

// Order는 정산 결과 한 건이다.
type Order struct {
	ID           int64      `json:"id"`
	UserID       string     `json:"userId"`
	Nickname     string     `json:"nickname,omitempty"`
	CommentNo    int64      `json:"commentNo"`
	RawComment   string     `json:"rawComment"`
	TotalPrice   int        `json:"totalPrice"`
	Status       string     `json:"status"`
	RejectReason string     `json:"rejectReason,omitempty"`
	CommentedAt  *time.Time `json:"commentedAt,omitempty"`
	Lines        []Line     `json:"lines,omitempty"`
}

type Line struct {
	ItemName  string `json:"itemName"`
	Quantity  int    `json:"quantity"`
	UnitPrice int    `json:"unitPrice"`
}

type SettleResult struct {
	Comments  int `json:"comments"`  // 읽은 댓글 수
	Orders    int `json:"orders"`    // 구매 신청 형식인 댓글 수
	Fulfilled int `json:"fulfilled"` // 처리된 건
	Rejected  int `json:"rejected"`  // 거절된 건
	Spent     int `json:"spent"`     // 차감된 총 달란트
}

// CommentSource는 게시글 댓글을 가져온다.
// 실제로는 *soop.Client가 들어가지만, 정산 로직을 SOOP 없이 시험할 수 있도록 좁혀 둔다.
// 달란트가 실제로 깎이는 지점이라 테스트로 고정할 수 있어야 한다.
type CommentSource interface {
	Comments(ctx context.Context, bid string, titleNo int64) ([]soop.Comment, error)
}

// Settle은 게시글 댓글을 읽어 구매를 확정한다.
//
// 처리 원칙:
//   - **댓글 시각 순으로 선착순** 처리한다. 잔액이 모자라면 뒤에 온 신청이 거절된다.
//   - 확장이 보낸 값은 쓰지 않는다. SOOP 댓글이 유일한 근거다.
//   - comment_no가 멱등키이므로 정산을 여러 번 돌려도 두 번 차감되지 않는다.
func Settle(ctx context.Context, pool *store.DB, api CommentSource, page *Page) (*SettleResult, error) {
	if page.PostTitleNo == nil {
		return nil, ErrBadPostURL
	}

	comments, err := api.Comments(ctx, page.BID, *page.PostTitleNo)
	if err != nil {
		return nil, fmt.Errorf("댓글을 읽지 못했습니다: %w", err)
	}

	items, err := Items(ctx, pool, page.ID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Item, len(items))
	stock := make(map[int64]int, len(items))
	for _, it := range items {
		byName[normalizeName(it.Name)] = it
		if it.Stock != nil {
			stock[it.ID] = *it.Stock
		}
	}

	// 이미 정산된 댓글은 건너뛴다.
	done, err := settledComments(ctx, pool, page.ID)
	if err != nil {
		return nil, err
	}

	// 이전 정산에서 이미 나간 수량을 먼저 반영한다.
	//
	// 건너뛴 주문의 소모량을 세지 않으면, 정산을 다시 돌릴 때 재고가 되살아나
	// 총 수량과 인당 제한을 넘겨 나가게 된다.
	prior, err := priorConsumption(ctx, pool, page.ID)
	if err != nil {
		return nil, err
	}
	// perUser[itemID][userID] = 그 사람이 이 보상을 지금까지 가져간 개수
	perUser := make(map[int64]map[string]int, len(items))
	for key, qty := range prior {
		if _, ok := stock[key.itemID]; ok {
			stock[key.itemID] -= qty
		}
		if perUser[key.itemID] == nil {
			perUser[key.itemID] = map[string]int{}
		}
		perUser[key.itemID][key.userID] += qty
	}

	// 선착순 판정을 위해 댓글 시각 순으로 정렬한다.
	// 시각이 같으면 댓글 번호가 작은 쪽이 먼저다.
	sort.SliceStable(comments, func(i, j int) bool {
		ti, tj := comments[i].RegTime(), comments[j].RegTime()
		if ti.Equal(tj) {
			return comments[i].CommentNo < comments[j].CommentNo
		}
		return ti.Before(tj)
	})

	// 잔액은 정산 도중 계속 줄어든다. 한 번만 읽고 메모리에서 관리한다.
	balances := make(map[string]int)
	res := &SettleResult{Comments: len(comments)}

	for _, cm := range comments {
		lines, ok := ParseComment(cm.Comment)
		if !ok {
			continue
		}
		res.Orders++
		if done[cm.CommentNo] {
			continue
		}

		userID := soop.NormalizeUserID(cm.UserID)
		if userID == "" {
			continue
		}

		order := Order{
			UserID: userID, Nickname: cm.UserNick,
			CommentNo: cm.CommentNo, RawComment: cm.Comment,
		}
		if t := cm.RegTime(); !t.IsZero() {
			order.CommentedAt = &t
		}

		// 1) 보상 대조
		var reject string
		total := 0
		var resolved []struct {
			item Item
			qty  int
		}
		for _, l := range lines {
			it, found := byName[normalizeName(l.Name)]
			if !found {
				reject = fmt.Sprintf("등록되지 않은 보상입니다: %s", l.Name)
				break
			}
			resolved = append(resolved, struct {
				item Item
				qty  int
			}{it, l.Quantity})
			total += it.Price * l.Quantity
		}

		// 2) 총 수량과 인당 제한 확인
		//
		// 같은 댓글에 같은 보상을 여러 번 적은 경우까지 합산해야 하므로
		// 이 주문에서 요청한 개수를 보상별로 먼저 모은다.
		wanted := map[int64]int{}
		for _, r := range resolved {
			wanted[r.item.ID] += r.qty
		}
		if reject == "" {
			for _, r := range resolved {
				n := wanted[r.item.ID]
				if r.item.Stock != nil && stock[r.item.ID] < n {
					reject = fmt.Sprintf("남은 수량이 부족합니다: %s (남은 수량 %d)",
						r.item.Name, max(stock[r.item.ID], 0))
					break
				}
				if r.item.PerUserLimit != nil {
					already := perUser[r.item.ID][userID]
					if already+n > *r.item.PerUserLimit {
						reject = fmt.Sprintf("인당 구매 제한을 넘었습니다: %s (최대 %d개",
							r.item.Name, *r.item.PerUserLimit)
						if already > 0 {
							reject += fmt.Sprintf(", 이미 %d개 구매", already)
						}
						reject += ")"
						break
					}
				}
			}
		}

		// 3) 잔액 확인
		if reject == "" {
			bal, seen := balances[userID]
			if !seen {
				bal, err = currentBalance(ctx, pool, page.BID, userID)
				if err != nil {
					return nil, err
				}
			}
			if bal < total {
				reject = fmt.Sprintf("달란트가 부족합니다 (보유 %d / 필요 %d)", bal, total)
			} else {
				balances[userID] = bal - total
			}
		}

		if reject != "" {
			order.Status = "rejected"
			order.RejectReason = reject
			res.Rejected++
		} else {
			order.Status = "fulfilled"
			order.TotalPrice = total
			for _, r := range resolved {
				if r.item.Stock != nil {
					stock[r.item.ID] -= r.qty
				}
				if perUser[r.item.ID] == nil {
					perUser[r.item.ID] = map[string]int{}
				}
				perUser[r.item.ID][userID] += r.qty
				order.Lines = append(order.Lines, Line{
					ItemName: r.item.Name, Quantity: r.qty, UnitPrice: r.item.Price,
				})
			}
			res.Fulfilled++
			res.Spent += total
		}

		if err := saveOrder(ctx, pool, page, order, resolved); err != nil {
			return nil, err
		}
	}

	if _, err := pool.Exec(ctx,
		`UPDATE shop_pages SET settled_at = now() WHERE id = $1`, page.ID); err != nil {
		return nil, err
	}
	return res, nil
}

// saveOrder는 주문과 차감을 한 트랜잭션으로 기록한다.
// 차감이 성공했는데 주문 기록이 없거나, 그 반대가 되면 안 된다.
func saveOrder(ctx context.Context, pool *store.DB, page *Page, o Order,
	resolved []struct {
		item Item
		qty  int
	}) error {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT IGNORE INTO purchase_orders
		       (page_id, bid, user_id, nickname, comment_no, raw_comment,
		        total_price, status, reject_reason, commented_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,NULLIF($9,''),$10)
		`,
		page.ID, page.BID, o.UserID, o.Nickname, o.CommentNo, o.RawComment,
		o.TotalPrice, o.Status, o.RejectReason, o.CommentedAt)
	if err != nil {
		return err
	}
	n, _ := tag.RowsAffected()
	if n == 0 {
		return tx.Commit(ctx)
	}
	orderID, err := tag.LastInsertId()
	if err != nil {
		return err
	}

	if o.Status == "fulfilled" {
		for _, r := range resolved {
			if _, err := tx.Exec(ctx, `
				INSERT INTO purchase_lines (order_id, item_id, item_name, quantity, unit_price)
				VALUES ($1,$2,$3,$4,$5)`,
				orderID, r.item.ID, r.item.Name, r.qty, r.item.Price); err != nil {
				return err
			}
		}
		if o.TotalPrice > 0 {
			names := make([]string, 0, len(resolved))
			for _, r := range resolved {
				if r.qty > 1 {
					names = append(names, fmt.Sprintf("%s x%d", r.item.Name, r.qty))
				} else {
					names = append(names, r.item.Name)
				}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO point_events
				       (bid, user_id, kind, amount, occurred_at, idem_key, meta, reason)
				VALUES ($1,$2,$3,$4,now(),$5,$6,$7)
				ON DUPLICATE KEY UPDATE idem_key = idem_key`,
				page.BID, o.UserID, scoring.KindPurchase, -o.TotalPrice,
				fmt.Sprintf("purchase:%d:%d", page.ID, o.CommentNo),
				fmt.Sprintf(`{"pageId":%d,"commentNo":%d}`, page.ID, o.CommentNo),
				fmt.Sprintf("%s 구매 — %s", page.Title, strings.Join(names, ", "))); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// itemUser는 "누가 어떤 보상을" 을 가리키는 키다.
type itemUser struct {
	itemID int64
	userID string
}

// priorConsumption은 이미 확정된 주문에서 나간 수량을 보상·사람별로 모은다.
// 정산을 다시 돌려도 총 수량과 인당 제한이 되살아나지 않게 하는 근거다.
func priorConsumption(ctx context.Context, pool *store.DB, pageID int64) (map[itemUser]int, error) {
	rows, err := pool.Query(ctx, `
		SELECT l.item_id, o.user_id, SUM(l.quantity)
		  FROM purchase_lines l JOIN purchase_orders o ON o.id = l.order_id
		 WHERE o.page_id = $1 AND o.status = 'fulfilled' AND l.item_id IS NOT NULL
		 GROUP BY l.item_id, o.user_id`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[itemUser]int{}
	for rows.Next() {
		var k itemUser
		var qty int
		if err := rows.Scan(&k.itemID, &k.userID, &qty); err != nil {
			return nil, err
		}
		out[k] = qty
	}
	return out, rows.Err()
}

func settledComments(ctx context.Context, pool *store.DB, pageID int64) (map[int64]bool, error) {
	rows, err := pool.Query(ctx,
		`SELECT comment_no FROM purchase_orders WHERE page_id = $1`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Balance는 시청자가 그 스트리머에게 보유한 달란트다.
// 달란트는 스트리머별로 따로 쌓이므로 bid로 반드시 좁힌다.
func Balance(ctx context.Context, pool *store.DB, bid, userID string) (int, error) {
	return currentBalance(ctx, pool, bid, userID)
}

func currentBalance(ctx context.Context, pool *store.DB, bid, userID string) (int, error) {
	var n *int
	err := pool.QueryRow(ctx,
		`SELECT SUM(amount) FROM point_events WHERE bid = $1 AND user_id = $2`,
		bid, userID).Scan(&n)
	if err != nil || n == nil {
		return 0, err
	}
	return *n, nil
}

// Orders는 한 페이지의 구매 내역이다. 스트리머가 조회한다.
func Orders(ctx context.Context, pool *store.DB, pageID int64) ([]Order, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, user_id, COALESCE(nickname,''), COALESCE(comment_no,0), COALESCE(raw_comment,''),
		       total_price, status, COALESCE(reject_reason,''), commented_at
		  FROM purchase_orders WHERE page_id = $1
		 ORDER BY commented_at IS NULL, commented_at, comment_no`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	orders := []Order{}
	index := map[int64]int{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.Nickname, &o.CommentNo, &o.RawComment,
			&o.TotalPrice, &o.Status, &o.RejectReason, &o.CommentedAt); err != nil {
			return nil, err
		}
		index[o.ID] = len(orders)
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return orders, nil
	}

	lineRows, err := pool.Query(ctx, `
		SELECT l.order_id, l.item_name, l.quantity, l.unit_price
		  FROM purchase_lines l JOIN purchase_orders o ON o.id = l.order_id
		 WHERE o.page_id = $1 ORDER BY l.id`, pageID)
	if err != nil {
		return nil, err
	}
	defer lineRows.Close()

	for lineRows.Next() {
		var oid int64
		var l Line
		if err := lineRows.Scan(&oid, &l.ItemName, &l.Quantity, &l.UnitPrice); err != nil {
			return nil, err
		}
		if i, ok := index[oid]; ok {
			orders[i].Lines = append(orders[i].Lines, l)
		}
	}
	return orders, lineRows.Err()
}

// normalizeName은 보상 이름 대조를 관대하게 만든다.
// 시청자가 띄어쓰기나 대소문자를 다르게 써도 같은 상품으로 인식한다.
func normalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}
