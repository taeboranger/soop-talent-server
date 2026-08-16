package api

import (
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
	mux.Handle("POST /api/shop/pages/{id}/open", s.approved(s.handleShopOpen))
	mux.Handle("POST /api/shop/pages/{id}/close", s.approved(s.handleShopClose))
	mux.Handle("GET /api/shop/pages/{id}/orders", s.approved(s.handleShopOrders))
	mux.Handle("POST /api/shop/pages/{id}/delete", s.approved(s.handleShopDelete))

	// 달란트 로그 · 수동 지급
	mux.Handle("GET /api/points/log", s.approved(s.handlePointLog))
	mux.Handle("GET /api/points/holders", s.approved(s.handleHolders))
	mux.Handle("POST /api/points/grant", s.approved(s.handleGrant))

	// 보상 미리보기 이미지
	mux.Handle("POST /api/shop/images", s.approved(s.handleImageUpload))
	mux.Handle("GET /api/shop/images/{id}", s.h(s.handleImageGet))

	// 시청자 확장용 — 로그인 없이 호출된다.
	// 열려 있는 페이지의 보상 목록과 본인 잔액만 알려주므로 민감한 정보가 아니다.
	mux.Handle("GET /api/viewer/shop", s.h(s.handleViewerShop))
	mux.Handle("GET /api/viewer/markets", s.h(s.handleViewerMarkets))
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

// handleShopOpen은 게시글을 연결하고 페이지를 연다.
// 게시글이 정말 본인 것인지 **서버가 SOOP에 직접 물어** 확인한다.
// 제목은 자유다 — 소유자 확인이 진짜 방어선이고, 제목 검사는 오타로 막히기만 했다.
func (s *Server) handleShopOpen(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req struct {
		PostURL string `json:"postUrl"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}

	bid := bidOf(r)

	// 상태를 먼저 본다. 이미 닫힌 페이지라면 SOOP에 물어볼 필요가 없고,
	// "제목이 키워드로 시작해야 합니다" 같은 엉뚱한 안내가 나가지도 않는다.
	if page, err := shop.Get(r.Context(), s.Store.Pool, bid, id); err != nil {
		return err
	} else if page.Status != shop.StatusDraft {
		if page.Status == shop.StatusClosed {
			return httpError{http.StatusConflict, shop.ErrClosed.Error()}
		}
		return httpError{http.StatusConflict, shop.ErrNotDraft.Error()}
	}

	postBID, titleNo, err := shop.ParsePostURL(req.PostURL)
	if err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	if !strings.EqualFold(postBID, bid) {
		return httpError{http.StatusForbidden, shop.ErrPostOwner.Error()}
	}

	post, err := s.SoopAPI.Post(r.Context(), bid, titleNo)
	if err != nil {
		return httpError{http.StatusBadRequest, "게시글을 확인하지 못했습니다. 주소를 다시 확인해 주세요."}
	}
	if !strings.EqualFold(soop.NormalizeUserID(post.UserID), bid) {
		return httpError{http.StatusForbidden, shop.ErrPostOwner.Error()}
	}

	p, err := shop.Open(r.Context(), s.Store.Pool, bid, id, strings.TrimSpace(req.PostURL), titleNo)
	if err != nil {
		return err
	}
	s.Log.Info("상품 페이지 열림", "bid", bid, "page", p.ID, "post", titleNo)
	writeJSON(w, http.StatusOK, p)
	return nil
}

// handleShopClose는 페이지를 닫고 곧바로 정산한다.
// 닫힌 페이지는 다시 열 수 없다.
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
	result, err := shop.Settle(r.Context(), s.Store.Pool, s.SoopAPI, p)
	if err != nil {
		// 페이지는 이미 닫혔다. 정산은 다시 시도할 수 있도록 오류만 알린다.
		s.Log.Error("정산 실패", "bid", bid, "page", id, "err", err)
		return httpError{http.StatusBadGateway,
			"페이지는 닫혔지만 댓글 정산에 실패했습니다. 잠시 후 다시 시도해 주세요."}
	}
	s.Log.Info("상품 페이지 정산", "bid", bid, "page", id,
		"주문", result.Orders, "확정", result.Fulfilled, "거절", result.Rejected)

	orders, err := shop.Orders(r.Context(), s.Store.Pool, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page": p, "result": result, "orders": orders,
	})
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
