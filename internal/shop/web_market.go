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
	ErrMarketClosed = apperr.New("이 달란트 시장은 종료되었습니다")
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

// OpenWeb은 게시글 연결 없이 시장을 열고, 추측할 수 없는 공유 링크 토큰을 만든다.
func OpenWeb(ctx context.Context, pool *store.DB, bid string, pageID int64) (*Page, error) {
	p, err := Get(ctx, pool, bid, pageID)
	if err != nil {
		return nil, err
	}
	if p.Status == StatusClosed {
		return nil, ErrClosed
	}
	if p.Status != StatusDraft {
		return nil, ErrNotDraft
	}
	if len(p.Items) == 0 {
		return nil, ErrNoItems
	}

	for range 4 {
		token, err := newPublicToken()
		if err != nil {
			return nil, err
		}
		tag, err := pool.Exec(ctx, `
			UPDATE shop_pages
			   SET status = 'open', public_token = $3, opened_at = NOW(6)
			 WHERE id = $1 AND bid = $2 AND status = 'draft'`, pageID, bid, token)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "shop_pages_one_open_per_bid") {
				return nil, ErrAlreadyOpen
			}
			// 충돌은 사실상 토큰 중복뿐이다. 새 토큰으로 한 번 더 시도한다.
			if strings.Contains(strings.ToLower(err.Error()), "shop_pages_public_token_uniq") {
				continue
			}
			return nil, err
		}
		if n, _ := tag.RowsAffected(); n == 0 {
			return nil, ErrNotDraft
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
		return nil, apperr.Newf("달란트가 부족합니다 (보유 %d / 필요 %d)", balance, total)
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
		       total_price,status,COALESCE(reject_reason,''),commented_at
		  FROM purchase_orders WHERE id=$1`, id).
		Scan(&o.ID, &o.UserID, &o.Nickname, &o.CommentNo, &o.RawComment, &o.TotalPrice, &o.Status, &o.RejectReason, &o.CommentedAt)
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
