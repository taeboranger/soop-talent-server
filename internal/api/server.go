// Package api는 확장 프로그램과 관리자 화면이 호출하는 HTTP 엔드포인트를 제공한다.
// 라우팅은 Go 1.22의 표준 ServeMux 패턴만 쓴다 — 외부 의존성을 늘리지 않는다.
//
// # 신뢰 모델
//
// 요청자의 신원은 **세션 토큰에서만** 나온다.
// 확장이 보낸 SOOP 아이디는 어떤 경우에도 권한 판단에 쓰지 않는다.
// 세션 → 계정 → 관리자가 승인한 연결(streamer_claims) 순으로 소유자를 유도한다.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sooptalent/server/internal/apperr"
	"github.com/sooptalent/server/internal/auth"
	"github.com/sooptalent/server/internal/collector"
	"github.com/sooptalent/server/internal/oauth"
	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/shop"
	"github.com/sooptalent/server/internal/soop"
	"github.com/sooptalent/server/internal/store"
)

// StreamerHook은 수집기를 다루는 창구다.
// Add/Remove는 승인·철회에, SetChat/Status는 연결 상태 화면에 쓴다.
type StreamerHook interface {
	Add(ctx context.Context, bid string)
	Remove(bid string)
	SetChat(ctx context.Context, bid string, on bool) error
	Status(bid string) collector.Status
}

type Server struct {
	Store    *store.Store
	Log      *slog.Logger
	Streamer StreamerHook
	OAuth    *oauth.Registry // nil이면 소셜 로그인 비활성
	SoopAPI  *soop.Client    // 게시글·댓글 조회
}

type ctxKey int

const (
	ctxAccount ctxKey = iota
	ctxBID
)

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// 인증 불필요
	mux.Handle("GET /api/health", s.h(s.handleHealth))
	s.oauthRoutes(mux)

	// 로그인만 필요 (아직 승인 전이어도 접근 가능)
	mux.Handle("POST /api/logout", s.session(s.handleLogout))
	mux.Handle("GET /api/me", s.session(s.handleMe))
	mux.Handle("POST /api/claim", s.session(s.handleSubmitClaim))
	mux.Handle("GET /api/claims", s.session(s.handleMyClaims))

	// 승인된 스트리머만 접근
	mux.Handle("GET /api/rules", s.approved(s.handleGetRules))
	mux.Handle("PUT /api/rules", s.approved(s.handlePutRules))
	mux.Handle("POST /api/rules/reset", s.approved(s.handleResetRules))
	mux.Handle("POST /api/rank/ingest", s.approved(s.handleRankIngest))
	mux.Handle("POST /api/award", s.approved(s.handleAward))
	mux.Handle("GET /api/award/status", s.approved(s.handleAwardStatus))
	mux.Handle("GET /api/summary", s.approved(s.handleSummary))
	mux.Handle("GET /api/status", s.approved(s.handleStatus))
	mux.Handle("PUT /api/collector/chat", s.approved(s.handleSetChat))

	s.shopRoutes(mux)

	// 스트리머 대시보드 (웹)
	mux.Handle("GET /", s.dashboardPage())
	mux.Handle("GET /guide", s.guidePage())
	mux.Handle("GET /guide/detail", s.guideDetailPage())

	// 관리자
	mux.Handle("GET /admin", s.adminPage())
	mux.Handle("GET /api/admin/claims", s.admin(s.handleAdminList))
	mux.Handle("POST /api/admin/claims/{id}/decide", s.admin(s.handleAdminDecide))

	return s.cors(mux)
}

// ---------------------------------------------------------------------------
// 미들웨어
// ---------------------------------------------------------------------------

// cors는 확장에서의 Bearer 토큰 호출을 허용한다.
// 확장 오리진(chrome-extension://)은 설치할 때마다 ID가 달라 화이트리스트할 수 없다.
// 웹 쿠키는 SameSite=Lax이고, 상태 변경 API는 POST/PUT이라 타 사이트 폼·fetch가
// 쿠키를 싣지 못한다. 확장 세션 브리지는 최상위 OAuth 창 + 1회 코드만 사용한다.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

func (s *Server) h(next handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := next(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func sessionToken(r *http.Request) string {
	return bearer(r)
}

// session은 로그인만 요구한다.
func (s *Server) session(next handlerFunc) http.Handler {
	return s.h(func(w http.ResponseWriter, r *http.Request) error {
		token := sessionToken(r)
		acct, err := auth.Resolve(r.Context(), s.Store.Pool, token)
		if err != nil {
			return err
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), ctxAccount, acct)))
	})
}

// approved는 로그인에 더해 **관리자가 승인한 SOOP 아이디**를 요구한다.
// 여기서 유도한 bid만 이후 처리에 쓰인다.
func (s *Server) approved(next handlerFunc) http.Handler {
	return s.session(func(w http.ResponseWriter, r *http.Request) error {
		acct := accountOf(r)
		bid, err := auth.ApprovedBID(r.Context(), s.Store.Pool, acct.ID)
		if err != nil {
			return err
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), ctxBID, bid)))
	})
}

func (s *Server) admin(next handlerFunc) http.Handler {
	return s.session(func(w http.ResponseWriter, r *http.Request) error {
		if !accountOf(r).IsAdmin {
			return httpError{http.StatusForbidden, "관리자만 접근할 수 있습니다."}
		}
		return next(w, r)
	})
}

func accountOf(r *http.Request) *auth.Account {
	a, _ := r.Context().Value(ctxAccount).(*auth.Account)
	return a
}

// bidOf는 approved 미들웨어가 유도한 아이디다. 클라이언트 입력이 아니다.
func bidOf(r *http.Request) string {
	b, _ := r.Context().Value(ctxBID).(string)
	return b
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	if err := auth.Logout(r.Context(), s.Store.Pool, sessionToken(r)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleMe는 확장이 화면을 그리는 데 필요한 상태를 한 번에 준다.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	acct := accountOf(r)
	claims, err := auth.MyClaims(r.Context(), s.Store.Pool, acct.ID)
	if err != nil {
		return err
	}
	bid, err := auth.ApprovedBID(r.Context(), s.Store.Pool, acct.ID)
	if err != nil && !errors.Is(err, auth.ErrNoApprovedBid) {
		return err
	}
	linked, err := auth.LinkedProviders(r.Context(), s.Store.Pool, acct.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"email": acct.Email, "isAdmin": acct.IsAdmin,
		"approvedBid": bid, "claims": claims, "linkedProviders": linked,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 연결 신청
// ---------------------------------------------------------------------------

func (s *Server) handleSubmitClaim(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Bid, Nick string }
	if err := decode(w, r, &req); err != nil {
		return err
	}
	c, err := auth.SubmitClaim(r.Context(), s.Store.Pool, accountOf(r).ID, req.Bid, req.Nick)
	if err != nil {
		return err
	}
	s.Log.Info("연결 신청", "account", accountOf(r).Email, "bid", c.BID, "status", c.Status)
	writeJSON(w, http.StatusOK, c)
	return nil
}

func (s *Server) handleMyClaims(w http.ResponseWriter, r *http.Request) error {
	claims, err := auth.MyClaims(r.Context(), s.Store.Pool, accountOf(r).ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, claims)
	return nil
}

// ---------------------------------------------------------------------------
// 관리자
// ---------------------------------------------------------------------------

func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) error {
	claims, err := auth.ListClaims(r.Context(), s.Store.Pool, r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, claims)
	return nil
}

func (s *Server) handleAdminDecide(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return httpError{http.StatusBadRequest, "잘못된 신청 번호입니다."}
	}
	var req struct {
		Approve bool   `json:"approve"`
		Note    string `json:"note"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}

	c, err := auth.Decide(r.Context(), s.Store.Pool, id, accountOf(r).ID, req.Approve, req.Note)
	if err != nil {
		return err
	}

	// 승인하면 즉시 수집을 시작하고, 철회하면 멈춘다.
	if s.Streamer != nil {
		if c.Status == auth.StatusApproved {
			if err := s.Store.EnsureStreamer(r.Context(), c.BID, c.SoopNick); err != nil {
				s.Log.Warn("스트리머 등록 실패", "bid", c.BID, "err", err)
			}
			s.Streamer.Add(context.WithoutCancel(r.Context()), c.BID)
		} else {
			s.Streamer.Remove(c.BID)
		}
	}
	s.Log.Info("신청 처리", "bid", c.BID, "status", c.Status, "admin", accountOf(r).Email)
	writeJSON(w, http.StatusOK, c)
	return nil
}

// ---------------------------------------------------------------------------
// 규칙
// ---------------------------------------------------------------------------

func (s *Server) handleGetRules(w http.ResponseWriter, r *http.Request) error {
	rules, err := scoring.Load(r.Context(), s.Store.Pool, bidOf(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rules)
	return nil
}

func (s *Server) handlePutRules(w http.ResponseWriter, r *http.Request) error {
	// 기본값 위에 덮어쓰므로 일부만 보내도 나머지는 유지된다.
	rules := scoring.DefaultRules()
	if err := decode(w, r, &rules); err != nil {
		return err
	}
	if err := scoring.Save(r.Context(), s.Store.Pool, bidOf(r), rules); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rules)
	return nil
}

func (s *Server) handleResetRules(w http.ResponseWriter, r *http.Request) error {
	rules, err := scoring.Reset(r.Context(), s.Store.Pool, bidOf(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rules)
	return nil
}

// ---------------------------------------------------------------------------
// 랭킹 수신 (기능 ①)
// ---------------------------------------------------------------------------

func (s *Server) handleRankIngest(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Batches []struct {
			Date string            `json:"date"`
			Kind string            `json:"kind"`
			Rows []scoring.RankRow `json:"rows"`
		} `json:"batches"`
	}
	if err := decodeLimit(w, r, &req, 4<<20); err != nil {
		return err
	}

	// 대상은 세션에서 유도한다. 페이로드에 bid가 있어도 무시한다.
	bid := bidOf(r)
	rules, err := scoring.Load(r.Context(), s.Store.Pool, bid)
	if err != nil {
		return err
	}

	total := 0
	for _, b := range req.Batches {
		n, err := scoring.IngestRanking(r.Context(), s.Store.Pool, bid, b.Kind, b.Date, b.Rows, rules)
		if err != nil {
			s.Log.Warn("랭킹 적립 실패", "bid", bid, "date", b.Date, "kind", b.Kind, "err", err)
			continue
		}
		total += n
	}
	s.Log.Info("랭킹 수신", "bid", bid, "batches", len(req.Batches), "newEvents", total)
	writeJSON(w, http.StatusOK, map[string]int{"batches": len(req.Batches), "awarded": total})
	return nil
}

// ---------------------------------------------------------------------------
// 수동 적립 (기능 ⑥⑦)
// ---------------------------------------------------------------------------

func (s *Server) handleAward(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		UserID string `json:"userId"`
		Kind   string `json:"kind"`
		RefID  string `json:"refId"`
		Amount *int   `json:"amount"`
		// 나중에 어떤 글/댓글에 준 것인지 확인할 수 있도록 함께 기록한다.
		URL     string `json:"url"`
		Title   string `json:"title"`
		Excerpt string `json:"excerpt"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.UserID == "" || req.RefID == "" {
		return httpError{http.StatusBadRequest, "대상 유저와 글 번호가 필요합니다."}
	}

	bid := bidOf(r)
	rules, err := scoring.Load(r.Context(), s.Store.Pool, bid)
	if err != nil {
		return err
	}

	var amount int
	var prefix string
	switch req.Kind {
	case scoring.KindBoardPost:
		amount, prefix = rules.Manual.PostPoints, "board"
	case scoring.KindBoardComment:
		amount, prefix = rules.Manual.CommentPoints, "comment"
	default:
		return httpError{http.StatusBadRequest, "게시글 또는 댓글만 수동 적립할 수 있습니다."}
	}
	if req.Amount != nil && *req.Amount > 0 {
		amount = *req.Amount
	}

	// refId를 멱등키로 쓴다 — 같은 글에 두 번 줄 수 없다 (실수 방지).
	n, err := scoring.Award(r.Context(), s.Store.Pool, []scoring.Event{{
		BID: bid, UserID: req.UserID, Kind: req.Kind,
		Amount: amount, OccurredAt: time.Now(),
		IdemKey: fmt.Sprintf("%s:%s", prefix, req.RefID),
		Meta: map[string]any{
			"refId": req.RefID, "url": req.URL,
			"title": req.Title, "excerpt": req.Excerpt,
		},
		Reason: awardReason(req.Kind, req.Title, req.Excerpt, req.URL),
	}})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"awarded": n > 0, "amount": amount, "duplicate": n == 0,
	})
	return nil
}

func (s *Server) handleAwardStatus(w http.ResponseWriter, r *http.Request) error {
	raw := r.URL.Query().Get("refIds")
	if raw == "" {
		writeJSON(w, http.StatusOK, []string{})
		return nil
	}
	ids := strings.Split(raw, ",")
	if len(ids) > 500 {
		ids = ids[:500]
	}
	keys := make([]string, 0, len(ids)*2)
	for _, id := range ids {
		keys = append(keys, "board:"+id, "comment:"+id)
	}

	marks := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+1)
	args = append(args, bidOf(r))
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := s.Store.Pool.Query(r.Context(),
		fmt.Sprintf(`SELECT idem_key FROM point_events WHERE bid = ? AND idem_key IN (%s)`, marks), args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return err
		}
		if i := strings.IndexByte(k, ':'); i >= 0 {
			out = append(out, k[i+1:])
		}
	}
	writeJSON(w, http.StatusOK, out)
	return rows.Err()
}

// ---------------------------------------------------------------------------
// 요약
// ---------------------------------------------------------------------------

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) error {
	bid := bidOf(r)
	var users, events int64
	var total *int64
	if err := s.Store.Pool.QueryRow(r.Context(), `
		SELECT COUNT(DISTINCT user_id), COUNT(*), SUM(amount)
		  FROM point_events WHERE bid = $1`, bid).Scan(&users, &events, &total); err != nil {
		return err
	}
	var live bool
	_ = s.Store.Pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM broadcasts WHERE bid = $1 AND ended_at IS NULL)`,
		bid).Scan(&live)

	var pts int64
	if total != nil {
		pts = *total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bid": bid, "users": users, "events": events, "totalPoints": pts, "live": live,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 도우미
// ---------------------------------------------------------------------------

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeLimit(w, r, v, 1<<16)
}

func decodeLimit(w http.ResponseWriter, r *http.Request, v any, max int64) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v); err != nil {
		return httpError{http.StatusBadRequest, "요청 형식이 올바르지 않습니다."}
	}
	return nil
}

// writeError는 도메인 오류를 적절한 상태 코드로 옮긴다.
// 사용자에게 보이는 문구는 도메인 패키지가 이미 한국어로 갖고 있다.
//
// 여기서 걸러지지 않은 오류만 500이 되며, 그 경우 반드시 로그를 남긴다.
// 로그가 없으면 "서버 내부 오류"라는 말만 남고 원인을 찾을 방법이 사라진다.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var he httpError
	var invalid apperr.Invalid

	switch {
	case errors.As(err, &he):
		writeJSON(w, he.status, map[string]string{"error": he.msg})

	// 없는 자원은 입력 오류가 아니라 404다.
	case errors.Is(err, shop.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})

	// 사용자 입력 문제 — 무엇을 고쳐야 하는지 그대로 알려 준다.
	case errors.As(err, &invalid):
		status := http.StatusBadRequest
		// "이미 있음" 계열만 충돌로 구분한다.
		// 값이 잘못된 것(짧은 비밀번호 등)은 충돌이 아니라 잘못된 요청이다.
		if errors.Is(err, auth.ErrEmailTaken) || errors.Is(err, auth.ErrBidTaken) ||
			errors.Is(err, auth.ErrClaimExists) {
			status = http.StatusConflict
		}
		if errors.Is(err, auth.ErrBadCredentials) {
			status = http.StatusUnauthorized
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})

	case errors.Is(err, auth.ErrNoSession), errors.Is(err, auth.ErrDisabled):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})

	case errors.Is(err, auth.ErrNoApprovedBid):
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": err.Error(), "code": "not_approved"})

	default:
		if s.Log != nil {
			s.Log.Error("처리되지 않은 오류", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "서버 내부 오류가 발생했습니다."})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
