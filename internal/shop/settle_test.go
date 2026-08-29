package shop

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sooptalent/server/internal/soop"
	"github.com/sooptalent/server/internal/store"
)

// 정산은 달란트가 실제로 깎이는 지점이다. DB를 붙여 끝까지 확인한다.
//
//	TEST_DATABASE_URL=user:pass@tcp(mysql:3306)/soop-talent?parseTime=true go test ./internal/shop/
//
// 설정되지 않으면 건너뛴다.
func testPool(t *testing.T) *store.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 미설정 — DB 테스트를 건너뜁니다")
	}
	st, err := store.Open(context.Background(), dsn, slog.Default())
	if err != nil {
		t.Fatalf("DB 접속: %v", err)
	}
	t.Cleanup(st.Close)
	return st.Pool
}

// fakeComments는 SOOP 대신 미리 준비한 댓글을 돌려준다.
type fakeComments []soop.Comment

func (f fakeComments) Comments(context.Context, string, int64) ([]soop.Comment, error) {
	return f, nil
}

func comment(no int64, user, text string, min int) soop.Comment {
	return soop.Comment{
		CommentNo: no, UserID: user, UserNick: user + "님", Comment: text,
		RegDate: time.Date(2026, 8, 11, 10, min, 0, 0, time.Local).Format("2006-01-02 15:04:05"),
	}
}

// setupPage는 격리된 스트리머와 열린 페이지를 만든다.
func setupPage(t *testing.T, pool *store.DB, items []Item) (*Page, string) {
	t.Helper()
	ctx := context.Background()
	bid := fmt.Sprintf("test_%d", time.Now().UnixNano())

	if _, err := pool.Exec(ctx, `INSERT INTO streamers (bid) VALUES ($1)`, bid); err != nil {
		t.Fatalf("스트리머 생성: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM streamers WHERE bid = $1`, bid)
	})

	p, err := Create(ctx, pool, bid, "테스트 시장", "")
	if err != nil {
		t.Fatalf("페이지 생성: %v", err)
	}
	if err := SetItems(ctx, pool, bid, p.ID, items); err != nil {
		t.Fatalf("상품 등록: %v", err)
	}
	titleNo := time.Now().UnixNano() % 1000000000
	if _, err := pool.Exec(ctx, `
		UPDATE shop_pages SET status='open', post_title_no=$2, opened_at=now() WHERE id=$1`,
		p.ID, titleNo); err != nil {
		t.Fatalf("페이지 열기: %v", err)
	}
	p, err = Get(ctx, pool, bid, p.ID)
	if err != nil {
		t.Fatalf("페이지 조회: %v", err)
	}
	return p, bid
}

func give(t *testing.T, pool *store.DB, bid, user string, amount int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO point_events (bid, user_id, kind, amount, occurred_at, idem_key, reason)
		VALUES ($1,$2,'manual',$3,now(),$4,'테스트 지급')`,
		bid, user, amount, fmt.Sprintf("t:%s:%d", user, time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("달란트 지급: %v", err)
	}
}

func balanceOf(t *testing.T, pool *store.DB, bid, user string) int {
	t.Helper()
	n, err := Balance(context.Background(), pool, bid, user)
	if err != nil {
		t.Fatalf("잔액 조회: %v", err)
	}
	return n
}

func TestSettle_잔액부족은거절(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "치킨", Price: 300}})

	give(t, pool, bid, "rich", 500)
	give(t, pool, bid, "poor", 100)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "rich", "[구매신청] 치킨", 1),
		comment(2, "poor", "[구매신청] 치킨", 2),
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}

	if res.Fulfilled != 1 || res.Rejected != 1 {
		t.Errorf("확정 %d 거절 %d, want 1/1", res.Fulfilled, res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "rich"); got != 200 {
		t.Errorf("rich 잔액 = %d, want 200", got)
	}
	if got := balanceOf(t, pool, bid, "poor"); got != 100 {
		t.Errorf("poor 잔액 = %d, want 100 (차감되면 안 됨)", got)
	}
}

// 같은 사람이 여러 번 신청하면 잔액이 누적 차감되어야 한다.
// 매번 원래 잔액으로 판단하면 보유량보다 많이 살 수 있다.
func TestSettle_같은사람연속신청(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "빵", Price: 100}})
	give(t, pool, bid, "user1", 250)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "user1", "[구매신청] 빵", 1),
		comment(2, "user1", "[구매신청] 빵", 2),
		comment(3, "user1", "[구매신청] 빵", 3), // 잔액 50 — 거절되어야 한다
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Fulfilled != 2 || res.Rejected != 1 {
		t.Errorf("확정 %d 거절 %d, want 2/1", res.Fulfilled, res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 50 {
		t.Errorf("잔액 = %d, want 50", got)
	}
}

// 늦게 단 댓글이 먼저 처리되면 선착순이 깨진다.
func TestSettle_선착순(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "한정판", Price: 100, Stock: ptr(1)}})
	give(t, pool, bid, "early", 500)
	give(t, pool, bid, "late", 500)

	// 일부러 늦은 댓글을 앞에 넣어 정렬이 동작하는지 본다.
	res, err := Settle(context.Background(), pool, fakeComments{
		comment(9, "late", "[구매신청] 한정판", 30),
		comment(1, "early", "[구매신청] 한정판", 5),
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Fulfilled != 1 || res.Rejected != 1 {
		t.Fatalf("확정 %d 거절 %d, want 1/1", res.Fulfilled, res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "early"); got != 400 {
		t.Errorf("early 잔액 = %d, want 400 (먼저 신청했으므로 구매)", got)
	}
	if got := balanceOf(t, pool, bid, "late"); got != 500 {
		t.Errorf("late 잔액 = %d, want 500 (재고 없어 거절)", got)
	}
}

func TestSettle_여러상품과수량(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{
		{Name: "치킨 기프티콘", Price: 300},
		{Name: "손편지", Price: 50},
	})
	give(t, pool, bid, "buyer", 1000)

	if _, err := Settle(context.Background(), pool, fakeComments{
		// 띄어쓰기를 다르게 써도 인식되어야 한다
		comment(1, "buyer", "[구매신청] 치킨기프티콘 x2, 손편지 x3", 1),
	}, page); err != nil {
		t.Fatalf("정산: %v", err)
	}
	// 300*2 + 50*3 = 750
	if got := balanceOf(t, pool, bid, "buyer"); got != 250 {
		t.Errorf("잔액 = %d, want 250", got)
	}
}

func TestSettle_없는상품은거절(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "빵", Price: 100}})
	give(t, pool, bid, "user1", 500)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "user1", "[구매신청] 없는상품", 1),
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Rejected != 1 {
		t.Errorf("거절 %d, want 1", res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 500 {
		t.Errorf("잔액 = %d, want 500", got)
	}
}

// 정산을 다시 돌려도 두 번 차감되면 안 된다.
func TestSettle_멱등(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "빵", Price: 100}})
	give(t, pool, bid, "user1", 500)

	src := fakeComments{comment(1, "user1", "[구매신청] 빵", 1)}
	ctx := context.Background()

	if _, err := Settle(ctx, pool, src, page); err != nil {
		t.Fatalf("1차 정산: %v", err)
	}
	after := balanceOf(t, pool, bid, "user1")

	if _, err := Settle(ctx, pool, src, page); err != nil {
		t.Fatalf("2차 정산: %v", err)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != after {
		t.Errorf("재정산 후 잔액 = %d, want %d (중복 차감)", got, after)
	}
}

// 동시 접속 표기가 붙은 아이디도 같은 사람으로 봐야 한다.
func TestSettle_아이디정규화(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "빵", Price: 100}})
	give(t, pool, bid, "user1", 150)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "user1(2)", "[구매신청] 빵", 1),
		comment(2, "user1", "[구매신청] 빵", 2), // 잔액 50 — 거절
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Fulfilled != 1 || res.Rejected != 1 {
		t.Errorf("확정 %d 거절 %d, want 1/1", res.Fulfilled, res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 50 {
		t.Errorf("잔액 = %d, want 50", got)
	}
}

// 인당 제한은 한 사람이 그 보상을 몇 개까지 가져갈 수 있는지다.
// 총 수량이 남아 있어도 한 사람이 독식하지 못하게 막는다.
func TestSettle_인당제한(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{
		{Name: "한정굿즈", Price: 100, Stock: ptr(10), PerUserLimit: ptr(2)},
	})
	give(t, pool, bid, "greedy", 1000)
	give(t, pool, bid, "other", 1000)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "greedy", "[구매신청] 한정굿즈 x2", 1), // 제한 딱 맞음 → 통과
		comment(2, "greedy", "[구매신청] 한정굿즈", 2),    // 3개째 → 거절
		comment(3, "other", "[구매신청] 한정굿즈 x2", 3),  // 다른 사람은 통과
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Fulfilled != 2 || res.Rejected != 1 {
		t.Errorf("확정 %d 거절 %d, want 2/1", res.Fulfilled, res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "greedy"); got != 800 {
		t.Errorf("greedy 잔액 = %d, want 800 (2개만 구매)", got)
	}
	if got := balanceOf(t, pool, bid, "other"); got != 800 {
		t.Errorf("other 잔액 = %d, want 800", got)
	}
}

// 한 댓글에 제한을 넘겨 적은 경우도 막아야 한다.
func TestSettle_인당제한_한번에초과(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{
		{Name: "굿즈", Price: 50, PerUserLimit: ptr(3)},
	})
	give(t, pool, bid, "user1", 1000)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "user1", "[구매신청] 굿즈 x5", 1),
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Rejected != 1 {
		t.Errorf("거절 %d, want 1", res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 1000 {
		t.Errorf("잔액 = %d, want 1000 (차감되면 안 됨)", got)
	}
}

// 같은 댓글에 같은 보상을 나눠 적어도 합산해서 제한을 봐야 한다.
func TestSettle_인당제한_같은댓글에분산(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{
		{Name: "굿즈", Price: 50, PerUserLimit: ptr(2)},
	})
	give(t, pool, bid, "user1", 1000)

	res, err := Settle(context.Background(), pool, fakeComments{
		// "굿즈 x2, 굿즈" = 합계 3개 → 제한 2를 넘는다
		comment(1, "user1", "[구매신청] 굿즈 x2, 굿즈", 1),
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Rejected != 1 {
		t.Errorf("거절 %d, want 1 (합산 3개는 제한 초과)", res.Rejected)
	}
}

// 정산을 다시 돌려도 총 수량과 인당 제한이 되살아나면 안 된다.
// 건너뛴 주문의 소모량을 세지 않으면 제한을 넘겨 나간다.
func TestSettle_재정산시제한유지(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{
		{Name: "굿즈", Price: 100, Stock: ptr(3), PerUserLimit: ptr(2)},
	})
	give(t, pool, bid, "user1", 1000)
	ctx := context.Background()

	first := fakeComments{comment(1, "user1", "[구매신청] 굿즈 x2", 1)}
	if _, err := Settle(ctx, pool, first, page); err != nil {
		t.Fatalf("1차 정산: %v", err)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 800 {
		t.Fatalf("1차 후 잔액 = %d, want 800", got)
	}

	// 댓글이 하나 더 달린 뒤 다시 정산
	second := fakeComments{
		comment(1, "user1", "[구매신청] 굿즈 x2", 1), // 이미 처리됨
		comment(2, "user1", "[구매신청] 굿즈", 2),    // 3개째 → 인당 제한으로 거절되어야 함
	}
	res, err := Settle(ctx, pool, second, page)
	if err != nil {
		t.Fatalf("2차 정산: %v", err)
	}
	if res.Rejected != 1 {
		t.Errorf("거절 %d, want 1 (이전 2개가 반영되어야 함)", res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 800 {
		t.Errorf("2차 후 잔액 = %d, want 800 (추가 차감 없어야 함)", got)
	}
}

// 총 수량도 재정산에서 되살아나면 안 된다.
func TestSettle_재정산시총수량유지(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "한정", Price: 100, Stock: ptr(1)}})
	give(t, pool, bid, "first", 1000)
	give(t, pool, bid, "second", 1000)
	ctx := context.Background()

	if _, err := Settle(ctx, pool, fakeComments{
		comment(1, "first", "[구매신청] 한정", 1),
	}, page); err != nil {
		t.Fatalf("1차 정산: %v", err)
	}

	res, err := Settle(ctx, pool, fakeComments{
		comment(1, "first", "[구매신청] 한정", 1),  // 이미 처리됨
		comment(2, "second", "[구매신청] 한정", 2), // 재고 없음 → 거절
	}, page)
	if err != nil {
		t.Fatalf("2차 정산: %v", err)
	}
	if res.Rejected != 1 {
		t.Errorf("거절 %d, want 1 (재고가 되살아나면 안 됨)", res.Rejected)
	}
	if got := balanceOf(t, pool, bid, "second"); got != 1000 {
		t.Errorf("second 잔액 = %d, want 1000", got)
	}
}

// 제한이 없으면 예전처럼 총 수량만 본다.
func TestSettle_인당제한없으면독식가능(t *testing.T) {
	pool := testPool(t)
	page, bid := setupPage(t, pool, []Item{{Name: "굿즈", Price: 10, Stock: ptr(5)}})
	give(t, pool, bid, "user1", 1000)

	res, err := Settle(context.Background(), pool, fakeComments{
		comment(1, "user1", "[구매신청] 굿즈 x5", 1),
	}, page)
	if err != nil {
		t.Fatalf("정산: %v", err)
	}
	if res.Fulfilled != 1 {
		t.Errorf("확정 %d, want 1", res.Fulfilled)
	}
	if got := balanceOf(t, pool, bid, "user1"); got != 950 {
		t.Errorf("잔액 = %d, want 950", got)
	}
}

// 시청자 확장은 이 목록만 보고 "이 글이 시장인가"를 판단한다.
// 여기가 틀리면 구매 버튼이 아예 안 뜨거나, 닫힌 시장에 뜬다.
func TestOpenPostNos(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	page, bid := setupPage(t, pool, []Item{{Name: "굿즈", Price: 10}})

	nos, err := OpenPostNos(ctx, pool, bid)
	if err != nil {
		t.Fatalf("조회: %v", err)
	}
	if len(nos) != 1 || nos[0] != *page.PostTitleNo {
		t.Fatalf("열린 글 번호 = %v, want [%d]", nos, *page.PostTitleNo)
	}

	// 다른 스트리머의 시장이 섞여 나오면 안 된다.
	other, otherBID := setupPage(t, pool, []Item{{Name: "다른 굿즈", Price: 10}})
	nos, err = OpenPostNos(ctx, pool, bid)
	if err != nil {
		t.Fatalf("조회: %v", err)
	}
	if len(nos) != 1 || nos[0] == *other.PostTitleNo {
		t.Errorf("%s 조회에 %s의 글이 섞임: %v", bid, otherBID, nos)
	}

	// 닫으면 목록에서 빠져야 한다.
	if _, err := MarkClosed(ctx, pool, bid, page.ID); err != nil {
		t.Fatalf("닫기: %v", err)
	}
	nos, err = OpenPostNos(ctx, pool, bid)
	if err != nil {
		t.Fatalf("조회: %v", err)
	}
	if len(nos) != 0 {
		t.Errorf("닫은 뒤에도 남음: %v", nos)
	}
}

// 아직 게시글을 연결하지 않은 작성 중 페이지는 목록에 없어야 한다.
func TestOpenPostNos_초안제외(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bid := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO streamers (bid) VALUES ($1)`, bid); err != nil {
		t.Fatalf("스트리머 생성: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM streamers WHERE bid = $1`, bid)
	})
	if _, err := Create(ctx, pool, bid, "아직 안 연 시장", ""); err != nil {
		t.Fatalf("페이지 생성: %v", err)
	}

	nos, err := OpenPostNos(ctx, pool, bid)
	if err != nil {
		t.Fatalf("조회: %v", err)
	}
	if len(nos) != 0 {
		t.Errorf("초안이 목록에 나옴: %v", nos)
	}
}

// 열린 시장에는 새 보상만 더할 수 있고, 기존 가격은 바뀌지 않아야 한다.
func TestAddItems_열린시장에는추가만(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	page, bid := setupPage(t, pool, []Item{{Name: "기존 보상", Price: 100}})

	updated, err := AddItems(ctx, pool, bid, page.ID, []Item{{Name: "새 보상", Price: 250, Stock: ptr(3)}})
	if err != nil {
		t.Fatalf("보상 추가: %v", err)
	}
	if len(updated.Items) != 2 || updated.Items[0].Name != "기존 보상" || updated.Items[0].Price != 100 {
		t.Fatalf("기존 보상 가격이 보존되지 않음: %+v", updated.Items)
	}
	if updated.Items[1].Name != "새 보상" || updated.Items[1].Price != 250 {
		t.Fatalf("새 보상 = %+v", updated.Items[1])
	}
	if _, err := AddItems(ctx, pool, bid, page.ID, []Item{{Name: "기존 보상", Price: 999}}); err == nil {
		t.Fatal("기존 보상과 같은 이름을 추가할 수 있으면 안 됨")
	}
}

// 시장 취소는 이미 차감한 달란트와 주문 상태를 한 트랜잭션으로 되돌려야 한다.
func TestCancelWeb_확정구매전액환불(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	page, bid := setupPage(t, pool, []Item{{Name: "굿즈", Price: 300}})
	give(t, pool, bid, "buyer", 1000)

	tag, err := pool.Exec(ctx, `
		INSERT INTO purchase_orders (page_id,bid,user_id,nickname,comment_no,raw_comment,total_price,status)
		VALUES ($1,$2,'buyer','구매자',987654,'구매',300,'fulfilled')`, page.ID, bid)
	if err != nil {
		t.Fatalf("구매 기록: %v", err)
	}
	orderID, err := tag.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO purchase_lines (order_id,item_id,item_name,quantity,unit_price)
		VALUES ($1,$2,'굿즈',1,300)`, orderID, page.Items[0].ID); err != nil {
		t.Fatalf("구매 항목: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO point_events (bid,user_id,kind,amount,occurred_at,idem_key,reason)
		VALUES ($1,'buyer','purchase',-300,NOW(6),'purchase:test-cancel','테스트 구매')`, bid); err != nil {
		t.Fatalf("구매 차감: %v", err)
	}
	if got := balanceOf(t, pool, bid, "buyer"); got != 700 {
		t.Fatalf("취소 전 잔액 = %d, want 700", got)
	}

	result, err := CancelWeb(ctx, pool, bid, page.ID)
	if err != nil {
		t.Fatalf("시장 취소: %v", err)
	}
	if result.Page.Status != StatusCancelled || result.RefundedOrders != 1 || result.RefundedTalents != 300 {
		t.Fatalf("취소 결과 = %+v", result)
	}
	if got := balanceOf(t, pool, bid, "buyer"); got != 1000 {
		t.Errorf("환불 후 잔액 = %d, want 1000", got)
	}
	orders, err := Orders(ctx, pool, page.ID)
	if err != nil || len(orders) != 1 || orders[0].Status != "cancelled" {
		t.Fatalf("환불 주문 상태 = %+v, %v", orders, err)
	}
	if _, err := CancelWeb(ctx, pool, bid, page.ID); err == nil {
		t.Fatal("취소된 시장을 다시 취소할 수 있으면 안 됨")
	}
}

// 단순 종료는 판매를 잠시 멈추는 상태다. 재개해도 공유 링크·기존 구매·수량 제한이
// 바뀌면 안 된다. 전액 환불 시장은 이전 주문이 모두 취소된 뒤 새 시장처럼 재개된다.
func TestOpenWeb_닫은시장은같은링크로다시열린다(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bid := fmt.Sprintf("test_reopen_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO streamers (bid) VALUES ($1)`, bid); err != nil {
		t.Fatalf("스트리머 생성: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM streamers WHERE bid = $1`, bid)
	})

	p, err := Create(ctx, pool, bid, "재개 시장", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetItems(ctx, pool, bid, p.ID, []Item{{Name: "보상", Price: 100, Stock: ptr(3)}}); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenWeb(ctx, pool, bid, p.ID)
	if err != nil {
		t.Fatalf("시장 열기: %v", err)
	}
	if opened.PublicToken == "" {
		t.Fatal("공유 링크 토큰이 없습니다")
	}
	if _, err := MarkClosed(ctx, pool, bid, p.ID); err != nil {
		t.Fatalf("시장 닫기: %v", err)
	}
	reopened, err := OpenWeb(ctx, pool, bid, p.ID)
	if err != nil {
		t.Fatalf("시장 다시 열기: %v", err)
	}
	if reopened.Status != StatusOpen || reopened.ClosedAt != nil {
		t.Fatalf("재개 상태 = status:%s closedAt:%v", reopened.Status, reopened.ClosedAt)
	}
	if reopened.PublicToken != opened.PublicToken {
		t.Fatalf("재개 뒤 링크가 바뀜: %q != %q", reopened.PublicToken, opened.PublicToken)
	}
	if _, err := CancelWeb(ctx, pool, bid, p.ID); err != nil {
		t.Fatalf("시장 취소: %v", err)
	}
	refunded, err := OpenWeb(ctx, pool, bid, p.ID)
	if err != nil {
		t.Fatalf("취소·환불 시장 다시 열기: %v", err)
	}
	if refunded.Status != StatusOpen || refunded.PublicToken != opened.PublicToken {
		t.Fatalf("환불 뒤 재개 결과 = %+v", refunded)
	}
}

// 방송국별 동시 오픈 제한을 없앴다. 서로 다른 시장의 재고·구매 기록은 페이지 ID로
// 분리되므로, 여러 링크가 동시에 열려도 주문이 섞이면 안 된다.
func TestOpenWeb_여러시장을동시에열수있다(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bid := fmt.Sprintf("test_multi_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO streamers (bid) VALUES ($1)`, bid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM streamers WHERE bid = $1`, bid)
	})
	makePage := func(title string) *Page {
		p, err := Create(ctx, pool, bid, title, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := SetItems(ctx, pool, bid, p.ID, []Item{{Name: title + " 보상", Price: 10}}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first, second := makePage("첫 시장"), makePage("둘째 시장")
	if _, err := OpenWeb(ctx, pool, bid, first.ID); err != nil {
		t.Fatalf("첫 시장 열기: %v", err)
	}
	opened, err := OpenWeb(ctx, pool, bid, second.ID)
	if err != nil {
		t.Fatalf("둘째 시장 동시 열기: %v", err)
	}
	if opened.Status != StatusOpen || opened.PublicToken == "" {
		t.Fatalf("둘째 시장 상태 = %+v", opened)
	}
}

func TestViewerOrders_본인주문과보상상세만돌려준다(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	page, bid := setupPage(t, pool, []Item{{Name: "상세 보상", Price: 70}})
	tag, err := pool.Exec(ctx, `
		INSERT INTO purchase_orders (page_id,bid,user_id,nickname,total_price,status)
		VALUES ($1,$2,'viewer','시청자',70,'fulfilled')`, page.ID, bid)
	if err != nil {
		t.Fatal(err)
	}
	id, err := tag.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO purchase_lines (order_id,item_id,item_name,quantity,unit_price)
		VALUES ($1,$2,'상세 보상',2,35)`, id, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO purchase_orders (page_id,bid,user_id,nickname,total_price,status)
		VALUES ($1,$2,'other','다른 사람',1,'fulfilled')`, page.ID, bid); err != nil {
		t.Fatal(err)
	}
	orders, err := ViewerOrders(ctx, pool, "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].MarketTitle != page.Title || orders[0].StreamerID != bid ||
		len(orders[0].Lines) != 1 || orders[0].Lines[0].Quantity != 2 {
		t.Fatalf("시청자 구매 내역 = %+v", orders)
	}
}

// 시청자 구매 취소는 본인 주문만, 시장이 열려 있을 때만 가능해야 한다. 환불 이벤트와
// 주문 상태 변경이 한 번만 일어나야 재시도나 화면 조작으로 달란트가 늘지 않는다.
func TestCancelOrder_본인구매만열린시장에서환불(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	page, bid := setupPage(t, pool, []Item{{Name: "취소 보상", Price: 120}})
	give(t, pool, bid, "buyer", 500)
	tag, err := pool.Exec(ctx, `
		INSERT INTO purchase_orders (page_id,bid,user_id,nickname,total_price,status)
		VALUES ($1,$2,'buyer','구매자',120,'fulfilled')`, page.ID, bid)
	if err != nil {
		t.Fatal(err)
	}
	orderID, err := tag.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO point_events (bid,user_id,kind,amount,occurred_at,idem_key,reason)
		VALUES ($1,'buyer','purchase',-120,NOW(6),'purchase:test-viewer-cancel','테스트 구매')`, bid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO purchase_lines (order_id,item_id,item_name,quantity,unit_price)
		VALUES ($1,$2,'취소 보상',1,120)`, orderID, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}

	if _, err := CancelOrder(ctx, pool, orderID, "other", ""); err == nil {
		t.Fatal("다른 사람이 주문을 취소할 수 있으면 안 됨")
	}
	result, err := CancelOrder(ctx, pool, orderID, "buyer", "")
	if err != nil {
		t.Fatalf("구매 취소: %v", err)
	}
	if result.Order.Status != "cancelled" || result.Balance != 500 || balanceOf(t, pool, bid, "buyer") != 500 {
		t.Fatalf("구매 취소 결과 = %+v", result)
	}
	if _, err := CancelOrder(ctx, pool, orderID, "buyer", ""); err == nil {
		t.Fatal("이미 취소된 주문을 다시 환불할 수 있으면 안 됨")
	}
}

func ptr(n int) *int { return &n }
