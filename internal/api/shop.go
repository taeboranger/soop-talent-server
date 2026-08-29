package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/shop"
	"github.com/sooptalent/server/internal/soop"
)

func (s *Server) shopRoutes(mux *http.ServeMux) {
	// 스트리머용
	mux.Handle("GET /api/shop/pages", s.approved(s.handleShopList))
	mux.Handle("POST /api/shop/pages", s.approved(s.handleShopCreate))
	mux.Handle("PUT /api/shop/pages/{id}/items", s.approved(s.handleShopSetItems))
	mux.Handle("POST /api/shop/pages/{id}/items", s.approved(s.handleShopAddItems))
	mux.Handle("POST /api/shop/pages/{id}/open", s.approved(s.handleShopOpen))
	mux.Handle("POST /api/shop/pages/{id}/close", s.approved(s.handleShopClose))
	mux.Handle("POST /api/shop/pages/{id}/cancel", s.approved(s.handleShopCancel))
	mux.Handle("GET /api/shop/pages/{id}/orders", s.approved(s.handleShopOrders))
	mux.Handle("GET /api/shop/pages/{id}/orders.xlsx", s.approved(s.handleShopOrdersXLSX))
	mux.Handle("POST /api/shop/pages/{id}/delete", s.approved(s.handleShopDelete))

	// 달란트 로그 · 수동 지급
	mux.Handle("GET /api/points/log", s.approved(s.handlePointLog))
	mux.Handle("GET /api/points/holders", s.approved(s.handleHolders))
	mux.Handle("POST /api/points/grant", s.approved(s.handleGrant))

	// 보상 미리보기 이미지
	mux.Handle("POST /api/shop/images", s.approved(s.handleImageUpload))
	mux.Handle("GET /api/shop/images/{id}", s.h(s.handleImageGet))

	// 공유 링크로 들어온 로그인 시청자용 시장. 스트리머 권한은 요구하지 않는다.
	mux.Handle("GET /api/market/{token}/state", s.h(s.handleMarketState))
	mux.Handle("GET /api/market/{token}", s.session(s.handleMarketGet))
	mux.Handle("POST /api/market/{token}/orders", s.session(s.handleMarketOrder))
	mux.Handle("POST /api/market/{token}/orders/{id}/cancel", s.session(s.handleMarketOrderCancel))
}

// handleImageUpload는 보상 미리보기를 받는다.
// 본문은 이미지 바이트 그대로다 — 멀티파트를 쓸 만큼 복잡하지 않다.
func (s *Server) handleImageUpload(w http.ResponseWriter, r *http.Request) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, shop.MaxImageBytes+1))
	if err != nil {
		return httpError{http.StatusBadRequest,
			fmt.Sprintf("이미지는 %dMB 이하만 올릴 수 있습니다.", shop.MaxImageBytes>>20)}
	}
	img, err := shop.SaveImage(r.Context(), s.Store.Pool, bidOf(r), data)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": img.ID, "url": fmt.Sprintf("/api/shop/images/%d", img.ID),
	})
	return nil
}

// handleImageGet은 미리보기를 돌려준다.
// 시청자 확장도 봐야 하므로 인증하지 않는다 — 열린 보상 페이지의 이미지는 공개 정보다.
func (s *Server) handleImageGet(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return httpError{http.StatusBadRequest, "잘못된 이미지 번호입니다."}
	}
	img, err := shop.GetImage(r.Context(), s.Store.Pool, id)
	if err != nil {
		return err
	}

	// 업로드 시 매직 바이트로 형식을 확인했지만, 여기서도 방어한다.
	// 우리 도메인에서 내보내는 파일이라 HTML로 해석되면 그대로 XSS가 된다.
	w.Header().Set("Content-Type", img.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	_, _ = w.Write(img.Data)
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, httpError{http.StatusBadRequest, "잘못된 페이지 번호입니다."}
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// 상품 페이지
// ---------------------------------------------------------------------------

func (s *Server) handleShopList(w http.ResponseWriter, r *http.Request) error {
	pages, err := shop.List(r.Context(), s.Store.Pool, bidOf(r))
	if err != nil {
		return err
	}
	if pages == nil {
		pages = []shop.Page{}
	}
	writeJSON(w, http.StatusOK, pages)
	return nil
}

func (s *Server) handleShopCreate(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Title, Description string }
	if err := decode(w, r, &req); err != nil {
		return err
	}
	p, err := shop.Create(r.Context(), s.Store.Pool, bidOf(r), req.Title, req.Description)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, p)
	return nil
}

func (s *Server) handleShopSetItems(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req struct {
		Items []shop.Item `json:"items"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if err := shop.SetItems(r.Context(), s.Store.Pool, bidOf(r), id, req.Items); err != nil {
		return err
	}
	p, err := shop.Get(r.Context(), s.Store.Pool, bidOf(r), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

// handleShopAddItems는 열린 시장에 새 보상만 덧붙인다. 기존 보상·가격은
// 이 경로로 전혀 수정할 수 없다.
func (s *Server) handleShopAddItems(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req struct {
		Items []shop.Item `json:"items"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	p, err := shop.AddItems(r.Context(), s.Store.Pool, bidOf(r), id, req.Items)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, p)
	return nil
}

// handleShopOpen은 게시글 연결 없이, 추측 불가능한 공유 링크를 만들어 페이지를 연다.
func (s *Server) handleShopOpen(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	bid := bidOf(r)
	p, err := shop.OpenWeb(r.Context(), s.Store.Pool, bid, id)
	if err != nil {
		return err
	}
	s.Log.Info("공유 링크 시장 열림", "bid", bid, "page", p.ID)
	writeJSON(w, http.StatusOK, p)
	return nil
}

// handleShopClose는 즉시 시장을 닫는다. 댓글 정산은 더 이상 하지 않는다.
func (s *Server) handleShopClose(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	bid := bidOf(r)

	p, err := shop.MarkClosed(r.Context(), s.Store.Pool, bid, id)
	if err != nil {
		return err
	}
	s.Log.Info("공유 링크 시장 닫힘", "bid", bid, "page", id)
	writeJSON(w, http.StatusOK, p)
	return nil
}

// handleShopCancel은 열린 시장을 취소하고, 확정된 구매 금액을 모두 돌려준다.
// 환불·주문 상태 변경·시장 폐쇄는 shop.CancelWeb의 한 트랜잭션으로 묶인다.
func (s *Server) handleShopCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	result, err := shop.CancelWeb(r.Context(), s.Store.Pool, bidOf(r), id)
	if err != nil {
		return err
	}
	s.Log.Info("공유 링크 시장 취소·환불", "bid", bidOf(r), "page", id,
		"orders", result.RefundedOrders, "talents", result.RefundedTalents)
	writeJSON(w, http.StatusOK, result)
	return nil
}

func (s *Server) handleShopOrders(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	// 본인 페이지인지 확인한다.
	if _, err := shop.Get(r.Context(), s.Store.Pool, bidOf(r), id); err != nil {
		return err
	}
	orders, err := shop.Orders(r.Context(), s.Store.Pool, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, orders)
	return nil
}

// handleShopOrdersXLSX는 스트리머 본인 시장의 구매 내역만 .xlsx로 내보낸다.
// 브라우저 토큰은 URL에 넣지 않고 Authorization 헤더로만 전달된다.
func (s *Server) handleShopOrdersXLSX(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	page, err := shop.Get(r.Context(), s.Store.Pool, bidOf(r), id)
	if err != nil {
		return err
	}
	orders, err := shop.Orders(r.Context(), s.Store.Pool, id)
	if err != nil {
		return err
	}
	file, err := shop.OrdersXLSX(page, orders)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", shop.XLSXContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="sooptalent-market-%d-orders.xlsx"`, id))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(file)
	return nil
}

func (s *Server) handleShopDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if err := shop.Delete(r.Context(), s.Store.Pool, bidOf(r), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------------------
// 달란트 로그 · 수동 지급
// ---------------------------------------------------------------------------

func (s *Server) handlePointLog(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))

	entries, err := scoring.Log(r.Context(), s.Store.Pool, bidOf(r), scoring.LogQuery{
		UserID: strings.TrimSpace(q.Get("userId")),
		Kind:   strings.TrimSpace(q.Get("kind")),
		Limit:  limit,
		Before: before,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, entries)
	return nil
}

func (s *Server) handleHolders(w http.ResponseWriter, r *http.Request) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	holders, err := scoring.Holders(r.Context(), s.Store.Pool, bidOf(r),
		strings.TrimSpace(r.URL.Query().Get("q")), limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, holders)
	return nil
}

// awardReason은 수동 지급 내역에 남길 사유를 만든다.
// 어떤 글/댓글에 준 것인지 나중에 확인할 수 있도록 링크를 함께 적는다.
func awardReason(kind, title, excerpt, url string) string {
	label := "게시글 작성"
	if kind == scoring.KindBoardComment {
		label = "댓글 작성"
	}

	var b strings.Builder
	b.WriteString(label)
	if t := strings.TrimSpace(title); t != "" {
		b.WriteString(" · ")
		b.WriteString(truncate(t, 40))
	}
	if e := strings.TrimSpace(excerpt); e != "" {
		b.WriteString(` · "`)
		b.WriteString(truncate(e, 40))
		b.WriteString(`"`)
	}
	if u := strings.TrimSpace(url); u != "" {
		b.WriteString(" · ")
		b.WriteString(u)
	}
	return b.String()
}

// truncate는 글자 수 기준으로 자른다. 한글이 바이트 중간에서 깨지지 않게 룬을 센다.
func truncate(s string, n int) string {
	rs := []rune(strings.Join(strings.Fields(s), " "))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n]) + "…"
}

func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		UserID string `json:"userId"`
		Amount int    `json:"amount"`
		Reason string `json:"reason"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	entry, err := scoring.Grant(r.Context(), s.Store.Pool, bidOf(r),
		soop.NormalizeUserID(strings.TrimSpace(req.UserID)), req.Amount, req.Reason)
	if err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	s.Log.Info("수동 지급", "bid", bidOf(r), "user", entry.UserID, "amount", entry.Amount)
	writeJSON(w, http.StatusOK, entry)
	return nil
}

// ---------------------------------------------------------------------------
// 시청자 확장
// ---------------------------------------------------------------------------

// handleViewerMarkets는 한 방송국에서 지금 열려 있는 시장 게시글 번호를 알려준다.
//
//	GET /api/viewer/markets?bid=abc  →  {"bid":"abc","titleNos":[203551799]}
//
// 시청자 확장은 이 목록을 캐시해 두고, 게시글을 열 때 번호만 대조한다.
// 그래서 시장이 아닌 글에서는 아무 요청도 나가지 않는다.
// 게시글 번호는 원래 공개된 값이므로 인증 없이 준다. 보상·잔액은 여기 담지 않는다.
func (s *Server) handleViewerMarkets(w http.ResponseWriter, r *http.Request) error {
	bid := soop.NormalizeUserID(strings.TrimSpace(r.URL.Query().Get("bid")))
	if bid == "" {
		return httpError{http.StatusBadRequest, "방송국 아이디가 필요합니다."}
	}

	nos, err := shop.OpenPostNos(r.Context(), s.Store.Pool, bid)
	if err != nil {
		return err
	}

	// 시장을 막 열었을 때 시청자가 오래 기다리지 않도록 짧게 잡는다.
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{"bid": bid, "titleNos": nos})
	return nil
}

// handleViewerShop은 시청자 확장이 게시글에서 호출한다.
//
//	GET /api/viewer/shop?titleNo=123&userId=abc
//
// 열려 있는 페이지가 없으면 404 — 확장은 조용히 아무것도 하지 않는다.
func (s *Server) handleViewerShop(w http.ResponseWriter, r *http.Request) error {
	titleNo, err := strconv.ParseInt(r.URL.Query().Get("titleNo"), 10, 64)
	if err != nil {
		return httpError{http.StatusBadRequest, "게시글 번호가 필요합니다."}
	}

	page, err := shop.OpenPageByPost(r.Context(), s.Store.Pool, titleNo)
	if err != nil {
		return httpError{http.StatusNotFound, "이 게시글에 연결된 달란트 시장이 없습니다."}
	}

	// 잔액은 요청한 아이디 기준으로 알려준다.
	// 이 값은 안내용이며, 실제 차감은 정산 때 서버가 다시 계산한다.
	balance := 0
	userID := soop.NormalizeUserID(strings.TrimSpace(r.URL.Query().Get("userId")))
	if userID != "" {
		if b, err := shop.Balance(r.Context(), s.Store.Pool, page.BID, userID); err == nil {
			balance = b
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"pageId":      page.ID,
		"bid":         page.BID,
		"title":       page.Title,
		"description": page.Description,
		"items":       page.Items,
		"balance":     balance,
		"userId":      userID,
		"prefix":      shop.OrderPrefix,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 공유 링크 시장
// ---------------------------------------------------------------------------

// handleMarketState는 로그인 전에 링크가 닫혔는지만 알린다. 닫힌 시장은
// 로그인 화면을 거치게 하지 않고 곧바로 종료 안내를 보여 줘야 한다.
func (s *Server) handleMarketState(w http.ResponseWriter, r *http.Request) error {
	p, err := shop.PublicPage(r.Context(), s.Store.Pool, r.PathValue("token"))
	if errors.Is(err, shop.ErrMarketClosed) {
		return httpError{http.StatusGone, err.Error()}
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": p.Status})
	return nil
}

func (s *Server) handleMarketGet(w http.ResponseWriter, r *http.Request) error {
	p, err := shop.PublicPage(r.Context(), s.Store.Pool, r.PathValue("token"))
	if errors.Is(err, shop.ErrMarketClosed) {
		return httpError{http.StatusGone, err.Error()}
	}
	if err != nil {
		return err
	}
	acct := accountOf(r)
	balance, err := shop.Balance(r.Context(), s.Store.Pool, p.BID, acct.SoopID)
	if err != nil {
		return err
	}
	orders, err := shop.ViewerOrdersForPage(r.Context(), s.Store.Pool, acct.SoopID, p.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page":    p,
		"balance": balance,
		"viewer":  map[string]string{"id": acct.SoopID, "nickname": acct.Nickname},
		"orders":  orders,
	})
	return nil
}

func (s *Server) handleMarketOrder(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		RequestKey string              `json:"requestKey"`
		Lines      []shop.CheckoutLine `json:"lines"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	acct := accountOf(r)
	result, err := shop.Checkout(r.Context(), s.Store.Pool, r.PathValue("token"),
		acct.SoopID, acct.Nickname, req.RequestKey, req.Lines)
	if errors.Is(err, shop.ErrMarketClosed) {
		return httpError{http.StatusGone, err.Error()}
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, result)
	return nil
}

// handleMarketOrderCancel은 시장 화면에서 본인 구매를 취소한다. URL 토큰까지
// 대조해 다른 시장 화면을 이용한 엉뚱한 주문 취소도 막는다.
func (s *Server) handleMarketOrderCancel(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return httpError{http.StatusBadRequest, "잘못된 구매 번호입니다."}
	}
	acct := accountOf(r)
	result, err := shop.CancelOrder(r.Context(), s.Store.Pool, id, acct.SoopID, r.PathValue("token"))
	if errors.Is(err, shop.ErrMarketClosed) {
		return httpError{http.StatusGone, err.Error()}
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, result)
	return nil
}
