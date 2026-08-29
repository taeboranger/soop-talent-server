package api

// 연결 상태 화면.
//
// "지금 잘 돌고 있나?"에 답하려면 서로 다른 층을 봐야 한다.
//
//	1. 서버      — 프로세스가 살아 있고 DB에 닿는가
//	2. 세션      — 로그인이 아직 유효한가, 언제까지인가
//	3. 채팅 연동 — SOOP 채팅 소켓에 붙어 있는가
//	4. 방송      — 지금 방송 중인가
//
// 문구(label)와 색(tone)까지 서버가 정해 내려보낸다.
// 대시보드와 확장 두 곳이 각자 판정하면 같은 상황을 다르게 부르게 된다.

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/sooptalent/server/internal/auth"
	"github.com/sooptalent/server/internal/collector"
	"github.com/sooptalent/server/internal/soop"
)

// tone은 화면의 배지 색이다. off는 "고장이 아니라 꺼 둔 것"이다.
const (
	toneOK   = "ok"
	toneWarn = "warn"
	toneBad  = "bad"
	toneOff  = "off"
)

type componentJSON struct {
	Tone   string `json:"tone"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	// LatencyMs는 DB 왕복 시간이다. 느려지는 조짐을 눈으로 볼 수 있게 둔다.
	// 보통 1ms 아래라 소수점을 남긴다 — 정수로 반올림하면 죄다 0이 된다.
	LatencyMs float64 `json:"latencyMs"`
}

type sessionJSON struct {
	Tone      string     `json:"tone"`
	Label     string     `json:"label"`
	Detail    string     `json:"detail,omitempty"`
	UserID    string     `json:"userId"`
	Nickname  string     `json:"nickname,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	DaysLeft  int        `json:"daysLeft,omitempty"`
}

type chatJSON struct {
	Enabled      bool       `json:"enabled"`
	Running      bool       `json:"running"`
	Phase        string     `json:"phase"`
	Tone         string     `json:"tone"`
	Label        string     `json:"label"`
	Detail       string     `json:"detail,omitempty"`
	BroadNo      string     `json:"broadNo,omitempty"`
	Since        *time.Time `json:"since,omitempty"`
	ConnectedAt  *time.Time `json:"connectedAt,omitempty"`
	LastPacketAt *time.Time `json:"lastPacketAt,omitempty"`
	Packets      int64      `json:"packets"`
	Retries      int        `json:"retries,omitempty"`
	Source       string     `json:"source,omitempty"`
}

type statusJSON struct {
	BID        string        `json:"bid"`
	ServerTime time.Time     `json:"serverTime"`
	Database   componentJSON `json:"database"`
	Session    sessionJSON   `json:"session"`
	SOOPAuth   componentJSON `json:"soopAuth"`
	Chat       chatJSON      `json:"chat"`
	Live       bool          `json:"live"`
	// Overall은 위 항목 중 가장 나쁜 색이다. 배지 하나만 그릴 때 쓴다.
	Overall string `json:"overall"`
}

// handleStatus는 승인된 스트리머의 연결 상태를 한 번에 준다.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) error {
	bid := bidOf(r)
	out := statusJSON{
		BID:        bid,
		ServerTime: time.Now(),
		Database:   s.databaseStatus(r.Context()),
		Session:    s.sessionStatus(r),
		Chat:       s.chatStatus(bid),
	}

	if s.OAuth != nil && s.OAuth.HasSOOPToken(r.Context(), s.Store.Pool, bid) {
		liveCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		if target, err := s.OAuth.AccessForBID(liveCtx, s.Store.Pool, bid); err != nil {
			out.SOOPAuth = componentJSON{Tone: toneBad, Label: "재로그인 필요", Detail: err.Error()}
		} else {
			out.SOOPAuth = componentJSON{Tone: toneOK, Label: "공식 인증됨",
				Detail: "OAuth 토큰 자동 갱신 사용 중 · " + target.ExpiresAt.Local().Format("2006-01-02 15:04") + "까지"}
			out.Live, _ = s.OAuth.LiveForBID(liveCtx, s.Store.Pool, bid)
		}
		cancel()
	} else {
		out.SOOPAuth = componentJSON{Tone: toneWarn, Label: "SOOP 로그인 필요",
			Detail: "SOOP으로 다시 로그인하면 공식 채팅 연동으로 전환됩니다."}
		_ = s.Store.Pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM broadcasts WHERE bid = $1 AND ended_at IS NULL)`,
			bid).Scan(&out.Live)
	}

	out.Overall = worstTone(out.Database.Tone, out.Session.Tone, out.SOOPAuth.Tone, out.Chat.Tone)
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleSetChat은 채팅 연동을 켜고 끈다.
// 대상은 세션에서 유도한 bid다 — 본문으로 남의 채널을 끌 수 없다.
func (s *Server) handleSetChat(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Enabled == nil {
		return httpError{http.StatusBadRequest, "enabled 값이 필요합니다."}
	}
	if s.Streamer == nil {
		return httpError{http.StatusServiceUnavailable, "수집기가 준비되지 않았습니다."}
	}

	bid := bidOf(r)
	if err := s.Streamer.SetChat(r.Context(), bid, *req.Enabled); err != nil {
		return err
	}
	// 평소 브리지는 30초마다 대상 목록을 읽는다. 껐다가 바로 켠 경우에는
	// 남아 있던 SDK 세션을 즉시 버리고 현재 설정으로 다시 연다.
	s.requestBridgeReconcile(r.Context(), bid)
	s.Log.Info("채팅 연동 설정 변경", "bid", bid, "enabled", *req.Enabled)
	writeJSON(w, http.StatusOK, s.chatStatus(bid))
	return nil
}

// handleHealth는 인증 없이 서버가 살아 있는지 알려 준다.
// 시청자 확장이 주소 오타를 즉시 잡는 데 쓴다.
// 스트리머 정보는 담지 않는다 — 누구나 부를 수 있는 자리다.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) error {
	db := s.databaseStatus(r.Context())
	status := http.StatusOK
	if db.Tone == toneBad {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ok":       db.Tone == toneOK,
		"time":     time.Now(),
		"database": db,
	})
	return nil
}

// ---------------------------------------------------------------------------
// 항목별 판정
// ---------------------------------------------------------------------------

func (s *Server) databaseStatus(ctx context.Context) componentJSON {
	// 상태 조회가 DB를 기다리다 멈추면 안 된다. 짧게 끊는다.
	ping, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	start := time.Now()
	err := s.Store.Pool.Ping(ping)
	took := math.Round(float64(time.Since(start).Microseconds())/100) / 10

	switch {
	case err != nil:
		return componentJSON{Tone: toneBad, Label: "응답 없음", Detail: err.Error()}
	case took > 1000:
		return componentJSON{Tone: toneWarn, Label: "느림",
			Detail: "데이터베이스 응답이 1초를 넘습니다.", LatencyMs: took}
	default:
		return componentJSON{Tone: toneOK, Label: "정상", LatencyMs: took}
	}
}

func (s *Server) sessionStatus(r *http.Request) sessionJSON {
	acct := accountOf(r)
	out := sessionJSON{Tone: toneOK, Label: "로그인 중"}
	if acct != nil {
		out.UserID = acct.SoopID
		out.Nickname = acct.Nickname
	}

	exp, err := auth.SessionExpiry(r.Context(), s.Store.Pool, sessionToken(r))
	if err != nil {
		// 여기까지 왔다면 세션은 유효하다. 만료 시각만 못 읽은 것이다.
		return out
	}
	days := int(time.Until(exp).Hours() / 24)
	out.ExpiresAt, out.DaysLeft = &exp, days

	if days <= 3 {
		out.Tone = toneWarn
		out.Label = "곧 만료"
		out.Detail = "곧 다시 로그인해야 합니다."
	}
	return out
}

func (s *Server) chatStatus(bid string) chatJSON {
	if s.Streamer == nil {
		return chatJSON{Phase: "unknown", Tone: toneWarn, Label: "알 수 없음",
			Detail: "이 서버에는 수집기가 붙어 있지 않습니다."}
	}
	st := s.Streamer.Status(bid)

	out := chatJSON{
		Enabled: st.Enabled, Running: st.Running,
		Phase:        st.Chat.Phase,
		Detail:       st.Chat.Detail,
		BroadNo:      st.Chat.BroadNo,
		Packets:      st.Chat.Packets,
		Retries:      st.Chat.Retries,
		Since:        nonZero(st.Chat.Since),
		ConnectedAt:  nonZero(st.Chat.ConnectedAt),
		LastPacketAt: nonZero(st.Chat.LastPacket),
	}
	if st.Official {
		out.Source = "SOOP 공식 Chat SDK"
	} else {
		out.Source = "레거시 채팅 폴백"
	}

	switch {
	case !st.Known:
		out.Phase, out.Tone, out.Label = "unknown", toneWarn, "대기"
		out.Detail = "아직 수집 대상이 아닙니다. 서버를 다시 시작하면 반영됩니다."
		return out

	case !st.Enabled:
		out.Phase, out.Tone, out.Label = "off", toneOff, "꺼짐"
		out.Detail = "채팅 연동을 꺼 두었습니다. 시청·채팅·후원·첫 후원 팬클럽·구독 달란트가 쌓이지 않습니다."
		return out

	case !st.Running:
		out.Phase, out.Tone, out.Label = "stopped", toneBad, "중지됨"
		out.Detail = "켜져 있지만 수집이 돌고 있지 않습니다. 서버 로그를 확인해 주세요."
		return out
	}

	// 설명은 수집기가 채워 두지만, 막 시작한 순간처럼 비어 있을 수 있다.
	// 배지만 덩그러니 뜨는 일이 없도록 단계별 기본 문구를 갖고 있는다.
	var fallback string
	switch st.Chat.Phase {
	case soop.PhaseConnected:
		out.Tone, out.Label = toneOK, "수집 중"
		fallback = "채팅을 받고 있습니다."
	case soop.PhaseOffline:
		// 방송이 꺼진 것은 고장이 아니다. 다만 지금 안 쌓이는 건 사실이라 노랑으로 둔다.
		out.Tone, out.Label = toneWarn, "방송 대기"
		fallback = "방송이 꺼져 있습니다. 시작하면 자동으로 붙습니다."
	case soop.PhaseConnecting:
		out.Tone, out.Label = toneWarn, "접속 중"
		fallback = "채팅 서버에 접속하는 중입니다."
	case soop.PhaseRetrying:
		out.Tone, out.Label = toneBad, "재접속 중"
		fallback = "연결이 끊겨 다시 붙는 중입니다."
	default:
		out.Tone, out.Label = toneWarn, "준비 중"
		fallback = "수집을 시작하는 중입니다."
	}
	if out.Detail == "" {
		out.Detail = fallback
	}
	return out
}

// ---------------------------------------------------------------------------
// 도우미
// ---------------------------------------------------------------------------

// nonZero는 값이 없는 시각을 JSON에서 아예 빼기 위한 것이다.
// time.Time의 제로값은 0001-01-01로 직렬화돼 화면에 그대로 새어 나온다.
func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// worstTone은 가장 나쁜 색을 고른다. off(꺼 둠)는 고장이 아니므로 정상보다 한 칸만 아래다.
func worstTone(tones ...string) string {
	rank := map[string]int{toneOK: 0, toneOff: 1, toneWarn: 2, toneBad: 3}
	worst, name := -1, toneOK
	for _, t := range tones {
		if r, ok := rank[t]; ok && r > worst {
			worst, name = r, t
		}
	}
	return name
}

// 컴파일 시점에 수집기가 상태 인터페이스를 만족하는지 확인한다.
var _ StreamerHook = (*collector.Collector)(nil)
