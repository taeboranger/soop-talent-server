// Package shop은 달란트 시장(보상 페이지)을 다룬다.
//
// 흐름:
//  1. 스트리머가 보상 페이지를 만들고 보상을 등록한다 (여러 개 가능, 열 수 있는 건 하나)
//  2. 자기 방송국에 글을 쓰고 그 주소를 등록한다 (제목은 자유)
//  3. 시청자가 확장으로 보상을 골라 정해진 형식의 댓글을 단다
//  4. 스트리머가 페이지를 닫으면 서버가 댓글을 읽어 달란트를 차감한다
//
// 3번에서 확장이 보낸 값은 아무 의미가 없다. **서버가 SOOP 댓글을 직접 읽는다.**
// 확장은 댓글 형식을 대신 맞춰 주는 편의 도구일 뿐이며, 손으로 같은 형식을 써도 동일하게 처리된다.
package shop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/store"
)

// 구매 신청 댓글 형식:
//
//	[구매신청] 상품이름 x2, 다른상품 x1
//
// 확장이 이 형식으로 댓글을 만들어 준다.
const OrderPrefix = "[구매신청]"

var (
	ErrNotFound    = apperr.New("보상 페이지를 찾을 수 없습니다")
	ErrAlreadyOpen = apperr.New("이미 열려 있는 보상 페이지가 있습니다. 먼저 닫아 주세요")
	ErrNotDraft    = apperr.New("작성 중인 페이지만 열 수 있습니다")
	ErrNotOpen     = apperr.New("열려 있는 페이지가 아닙니다")
	ErrClosed      = apperr.New("이미 닫힌 페이지입니다. 다시 열 수 없습니다")
	ErrNoItems     = apperr.New("보상을 하나 이상 등록해 주세요")
	ErrBadPostURL  = apperr.New("방송국 게시글 주소가 올바르지 않습니다")
	ErrPostOwner   = apperr.New("본인 방송국의 게시글만 등록할 수 있습니다")
)

type Status string

const (
	StatusDraft  Status = "draft"
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

type Page struct {
	ID          int64      `json:"id"`
	BID         string     `json:"bid"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Status      Status     `json:"status"`
	PostURL     string     `json:"postUrl,omitempty"`
	PostTitleNo *int64     `json:"postTitleNo,omitempty"`
	OpenedAt    *time.Time `json:"openedAt,omitempty"`
	ClosedAt    *time.Time `json:"closedAt,omitempty"`
	SettledAt   *time.Time `json:"settledAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	// omitempty를 쓰지 않는다. 상품이 없을 때 필드가 통째로 사라지면
	// 클라이언트가 매번 undefined를 방어해야 하고, 빠뜨리면 화면이 깨진다.
	Items []Item `json:"items"`
}

type Item struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Stock은 이 페이지에서 풀리는 **총 수량**이다. nil이면 무제한.
	// PerUserLimit은 **한 사람이** 가져갈 수 있는 최대 개수다. nil이면 무제한.
	// 둘 다 걸면 더 빡빡한 쪽이 먼저 걸린다.
	Stock        *int `json:"stock,omitempty"`
	PerUserLimit *int `json:"perUserLimit,omitempty"`
	Price        int  `json:"price"`
	SortOrder    int  `json:"sortOrder"`
	// 미리보기 이미지. 없으면 nil.
	ImageID *int64 `json:"imageId,omitempty"`
}

// ---------------------------------------------------------------------------
// 페이지 관리
// ---------------------------------------------------------------------------

func List(ctx context.Context, pool *store.DB, bid string) ([]Page, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, bid, title, COALESCE(description,''), status, COALESCE(post_url,''),
		       post_title_no, opened_at, closed_at, settled_at, created_at
		  FROM shop_pages WHERE bid = $1 ORDER BY created_at DESC`, bid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Page
	for rows.Next() {
		var p Page
		if err := rows.Scan(&p.ID, &p.BID, &p.Title, &p.Description, &p.Status,
			&p.PostURL, &p.PostTitleNo, &p.OpenedAt, &p.ClosedAt, &p.SettledAt,
			&p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 목록에서도 상품을 함께 보여준다. 페이지 수가 많지 않으므로 한 번에 읽는다.
	for i := range out {
		items, err := Items(ctx, pool, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Items = items
	}
	return out, nil
}

func Get(ctx context.Context, pool *store.DB, bid string, id int64) (*Page, error) {
	var p Page
	err := pool.QueryRow(ctx, `
		SELECT id, bid, title, COALESCE(description,''), status, COALESCE(post_url,''),
		       post_title_no, opened_at, closed_at, settled_at, created_at
		  FROM shop_pages WHERE id = $1 AND bid = $2`, id, bid).
		Scan(&p.ID, &p.BID, &p.Title, &p.Description, &p.Status, &p.PostURL,
			&p.PostTitleNo, &p.OpenedAt, &p.ClosedAt, &p.SettledAt, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Items, err = Items(ctx, pool, p.ID)
	return &p, err
}

func Items(ctx context.Context, pool *store.DB, pageID int64) ([]Item, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, price, stock, per_user_limit, sort_order, image_id
		  FROM shop_items WHERE page_id = $1 ORDER BY sort_order, id`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Name, &it.Price, &it.Stock, &it.PerUserLimit,
			&it.SortOrder, &it.ImageID); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func Create(ctx context.Context, pool *store.DB, bid, title, desc string) (*Page, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, apperr.New("페이지 이름을 입력해 주세요")
	}
	tag, err := pool.Exec(ctx, `INSERT INTO shop_pages (bid, title, description) VALUES ($1, $2, NULLIF($3,''))`, bid, title, desc)
	if err != nil {
		return nil, err
	}
	id, err := tag.LastInsertId()
	if err != nil {
		return nil, err
	}
	return Get(ctx, pool, bid, id)
}

// SetItems는 보상 목록을 통째로 교체한다.
// 열린 페이지는 상품을 바꿀 수 없다 — 시청자가 본 목록과 정산 기준이 달라지면 안 된다.
func SetItems(ctx context.Context, pool *store.DB, bid string, pageID int64, items []Item) error {
	p, err := Get(ctx, pool, bid, pageID)
	if err != nil {
		return err
	}
	if p.Status != StatusDraft {
		return apperr.New("작성 중인 페이지만 보상을 수정할 수 있습니다")
	}

	seen := map[string]bool{}
	for _, it := range items {
		name := strings.TrimSpace(it.Name)
		if name == "" {
			return apperr.New("보상 이름을 입력해 주세요")
		}
		// 댓글에서 이름으로 상품을 찾으므로, 구분자가 이름에 들어가면 파싱이 깨진다.
		if strings.ContainsAny(name, ",\n\r") {
			return apperr.Newf("보상 이름에 쉼표나 줄바꿈을 넣을 수 없습니다: %q", name)
		}
		if xQuantity.MatchString(name) {
			return apperr.Newf("보상 이름이 수량 표기와 겹칩니다: %q", name)
		}
		if it.Price < 0 {
			return apperr.New("가격은 0 이상이어야 합니다")
		}
		if it.Stock != nil && *it.Stock < 0 {
			return apperr.New("총 수량은 0 이상이어야 합니다")
		}
		if it.PerUserLimit != nil && *it.PerUserLimit < 1 {
			return apperr.New("인당 제한은 1 이상이어야 합니다")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return apperr.Newf("보상 이름이 중복됩니다: %q", name)
		}
		seen[key] = true
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM shop_items WHERE page_id = $1`, pageID); err != nil {
		return err
	}
	for i, it := range items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO shop_items
			       (page_id, name, price, stock, per_user_limit, sort_order, image_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			pageID, strings.TrimSpace(it.Name), it.Price, it.Stock, it.PerUserLimit,
			i, it.ImageID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func Delete(ctx context.Context, pool *store.DB, bid string, pageID int64) error {
	tag, err := pool.Exec(ctx,
		`DELETE FROM shop_pages WHERE id = $1 AND bid = $2 AND status = 'draft'`, pageID, bid)
	if err != nil {
		return err
	}
	n, _ := tag.RowsAffected()
	if n == 0 {
		return apperr.New("작성 중인 페이지만 삭제할 수 있습니다")
	}
	return nil
}

// ParsePostURL은 방송국 게시글 주소에서 아이디와 글 번호를 뽑는다.
//
//	https://www.sooplive.com/station/243000/post/203551799
var postURLRe = regexp.MustCompile(`/station/([^/]+)/post/(\d+)`)

func ParsePostURL(raw string) (bid string, titleNo int64, err error) {
	m := postURLRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", 0, ErrBadPostURL
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", 0, ErrBadPostURL
	}
	return m[1], n, nil
}

// Open은 게시글을 연결하고 페이지를 연다.
// 게시글의 소유자와 제목은 호출자가 SOOP API로 확인해 넘긴다.
func Open(ctx context.Context, pool *store.DB, bid string, pageID int64, postURL string, titleNo int64) (*Page, error) {
	p, err := Get(ctx, pool, bid, pageID)
	if err != nil {
		return nil, err
	}
	switch p.Status {
	case StatusOpen:
		return nil, ErrNotDraft
	case StatusClosed:
		return nil, ErrClosed
	}
	if len(p.Items) == 0 {
		return nil, ErrNoItems
	}

	_, err = pool.Exec(ctx, `
		UPDATE shop_pages
		   SET status = 'open', post_url = $3, post_title_no = $4, opened_at = now()
		 WHERE id = $1 AND bid = $2 AND status = 'draft'`,
		pageID, bid, postURL, titleNo)
	if err != nil {
		if strings.Contains(err.Error(), "shop_pages_one_open_per_bid") {
			return nil, ErrAlreadyOpen
		}
		if strings.Contains(err.Error(), "shop_pages_post_uniq") {
			return nil, apperr.New("이미 다른 보상 페이지에 연결된 게시글입니다")
		}
		return nil, err
	}
	return Get(ctx, pool, bid, pageID)
}

// OpenPageByPost는 게시글 번호로 열려 있는 페이지를 찾는다. 시청자 확장이 쓴다.
func OpenPageByPost(ctx context.Context, pool *store.DB, titleNo int64) (*Page, error) {
	var p Page
	err := pool.QueryRow(ctx, `
		SELECT id, bid, title, COALESCE(description,''), status, COALESCE(post_url,''),
		       post_title_no, opened_at, closed_at, settled_at, created_at
		  FROM shop_pages WHERE post_title_no = $1 AND status = 'open'`, titleNo).
		Scan(&p.ID, &p.BID, &p.Title, &p.Description, &p.Status, &p.PostURL,
			&p.PostTitleNo, &p.OpenedAt, &p.ClosedAt, &p.SettledAt, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Items, err = Items(ctx, pool, p.ID)
	return &p, err
}

// OpenPostNos는 한 방송국에서 지금 열려 있는 시장 게시글 번호를 돌려준다.
//
// 시청자 확장이 "이 글이 시장인가"를 판단하는 데 쓴다. 예전에는 게시글 제목이
// "[달란트 시장]"으로 시작하는지 보았지만, 그러려면 글을 열 때마다 SOOP API를
// 먼저 불러야 했다. 방송국당 열린 페이지는 하나뿐이므로(shop_pages_one_open_per_bid)
// 이 목록을 한 번 받아 캐시해 두면 이후 판단은 요청 없이 끝난다.
//
// 게시글 번호는 이미 공개된 정보라 인증 없이 준다. 보상 목록이나 잔액은 포함하지 않는다.
func OpenPostNos(ctx context.Context, pool *store.DB, bid string) ([]int64, error) {
	rows, err := pool.Query(ctx, `
		SELECT post_title_no FROM shop_pages
		 WHERE bid = $1 AND status = 'open' AND post_title_no IS NOT NULL`, bid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []int64{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkClosed는 페이지를 닫는다. 정산은 호출자가 이어서 수행한다.
func MarkClosed(ctx context.Context, pool *store.DB, bid string, pageID int64) (*Page, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE shop_pages SET status = 'closed', closed_at = now()
		 WHERE id = $1 AND bid = $2 AND status = 'open'`, pageID, bid)
	if err != nil {
		return nil, err
	}
	n, _ := tag.RowsAffected()
	if n == 0 {
		return nil, ErrNotOpen
	}
	return Get(ctx, pool, bid, pageID)
}

// ---------------------------------------------------------------------------
// 댓글 파싱
// ---------------------------------------------------------------------------

// "x2", "X 2", "×2" 등을 수량으로 인식한다.
var xQuantity = regexp.MustCompile(`(?i)[x×]\s*(\d+)\s*$`)

// OrderLine은 댓글에서 읽어낸 주문 한 줄이다.
type OrderLine struct {
	Name     string
	Quantity int
}

// ParseComment는 구매 신청 댓글을 항목으로 나눈다.
// 형식이 아니면 ok=false — 일반 댓글은 조용히 무시된다.
func ParseComment(text string) (lines []OrderLine, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, OrderPrefix) {
		return nil, false
	}
	body := strings.TrimSpace(strings.TrimPrefix(t, OrderPrefix))
	// 여러 줄로 써도 받아들인다.
	body = strings.NewReplacer("\r", "", "\n", ",").Replace(body)

	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		qty := 1
		if m := xQuantity.FindStringSubmatch(part); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n <= 0 {
				continue
			}
			qty = n
			part = strings.TrimSpace(part[:len(part)-len(m[0])])
		}
		if part == "" {
			continue
		}
		lines = append(lines, OrderLine{Name: part, Quantity: qty})
	}
	return lines, len(lines) > 0
}

// FormatComment는 확장이 달아 줄 댓글 문구를 만든다.
func FormatComment(lines []OrderLine) string {
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.Quantity > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", l.Name, l.Quantity))
		} else {
			parts = append(parts, l.Name)
		}
	}
	return OrderPrefix + " " + strings.Join(parts, ", ")
}
