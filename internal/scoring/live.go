package scoring

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sooptalent/server/internal/soop"
)

// Live는 실시간 채팅 이벤트를 달란트로 바꾼다.
// 스트리머 한 명당 하나씩 두며, 내부 상태는 뮤텍스로 보호한다.
type Live struct {
	BID  string
	Pool *pgxpool.Pool
	Log  *slog.Logger

	mu    sync.Mutex
	rules Rules

	// 체류 추적
	seen    map[string]*presence // userID → 현재 세션
	broadNo int64

	// 채팅 도배 제한
	chatMin  map[string]*minuteCount // userID → 이번 분 카운트
	chatDay  map[string]int          // userID → 오늘 채팅으로 받은 달란트
	dayStamp string                  // chatDay가 어느 날짜 기준인지

	// 팬클럽 상태. 첫 관측은 기준선으로만 쓴다.
	fanclub map[string]int
}

type presence struct {
	startedAt time.Time
	lastSeen  time.Time
	ticksPaid int
}

type minuteCount struct {
	minute string
	n      int
}

func NewLive(bid string, pool *pgxpool.Pool, log *slog.Logger, rules Rules) *Live {
	return &Live{
		BID: bid, Pool: pool, Log: log.With("bid", bid), rules: rules,
		seen:    make(map[string]*presence),
		chatMin: make(map[string]*minuteCount),
		chatDay: make(map[string]int),
		fanclub: make(map[string]int),
	}
}

// SetRules는 설정 변경을 반영한다. 과거 적립은 건드리지 않는다.
func (l *Live) SetRules(r Rules) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rules = r
}

// SetBroadcast는 현재 방송 번호를 알린다. 방송이 바뀌면 체류 세션을 모두 정리한다.
func (l *Live) SetBroadcast(broadNo string) {
	n, err := strconv.ParseInt(broadNo, 10, 64)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.broadNo != n {
		l.broadNo = n
		l.seen = make(map[string]*presence)
	}
}

// Handle은 패킷 하나를 처리해 적립할 이벤트를 돌려준다.
// DB 쓰기는 호출자가 배치로 처리한다.
func (l *Live) Handle(p soop.Packet, now time.Time) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch p.Svc {
	case soop.SvcChat, soop.SvcEmoticon:
		return l.onChat(p, now)
	case soop.SvcViewerList:
		l.onViewerList(p, now)
	case soop.SvcEnter:
		if ev, ok := soop.ParsePresence(p); ok {
			l.touch(ev.UserID, now)
		}
	case soop.SvcExit:
		if ev, ok := soop.ParsePresence(p); ok {
			delete(l.seen, ev.UserID)
		}
	case soop.SvcStarBalloon, soop.SvcAdBalloon:
		return l.onDonation(p, now)
	case soop.SvcSubscribe:
		return l.onSubscribe(p, now)
	case soop.SvcFanclub:
		return l.onFanclubInfo(p, now)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 채팅
// ---------------------------------------------------------------------------

func (l *Live) onChat(p soop.Packet, now time.Time) []Event {
	ev, ok := soop.ParseChat(p)
	if !ok {
		return nil
	}
	l.touch(ev.UserID, now)

	var out []Event
	if e := l.checkFanclub(ev.UserID, ev.FanclubWeeks, now); e != nil {
		out = append(out, *e)
	}

	r := l.rules.Chat
	if r.Points <= 0 {
		return out
	}
	// 이모티콘 채팅은 본문이 없으므로 최소 길이 검사를 건너뛴다.
	if !ev.Emoticon && len([]rune(ev.Message)) < r.MinLength {
		return out
	}

	// 분당 상한 — 도배 파밍 차단
	minute := now.Format("2006-01-02T15:04")
	mc := l.chatMin[ev.UserID]
	if mc == nil || mc.minute != minute {
		mc = &minuteCount{minute: minute}
		l.chatMin[ev.UserID] = mc
	}
	if mc.n >= r.PerMinuteCap {
		return out
	}

	// 일일 상한
	day := now.Format("2006-01-02")
	if l.dayStamp != day {
		l.dayStamp = day
		l.chatDay = make(map[string]int)
	}
	if r.DailyCap > 0 && l.chatDay[ev.UserID]+r.Points > r.DailyCap {
		return out
	}

	mc.n++
	l.chatDay[ev.UserID] += r.Points

	return append(out, Event{
		BID: l.BID, UserID: ev.UserID, Kind: KindChat,
		Amount: r.Points, OccurredAt: now, BroadNo: l.broadPtr(),
		// 같은 분 안의 n번째 채팅 — 재접속으로 패킷을 다시 받아도 중복되지 않는다.
		IdemKey: fmt.Sprintf("chat:%d:%s:%s:%d", l.broadNo, ev.UserID, minute, mc.n),
		Meta:    map[string]any{"emoticon": ev.Emoticon},
	})
}

// ---------------------------------------------------------------------------
// 체류
// ---------------------------------------------------------------------------

// onViewerList는 시청자 목록(0004)에서 유저를 추출해 생존 신호로 쓴다.
//
// 0004에는 두 가지 형태가 있다 (실측 확인, 2026-08-11):
//
//	목록형   parts[1]=="1"  : ["", "1", id, nick, flag, id, nick, flag, ..., ""]
//	단일갱신 parts[1]=="-1" : ["", "-1", id, nick, "1", "-1", flag, ""]
//
// 단일갱신을 목록으로 착각해 3칸씩 훑으면 parts[5]의 "-1"을 유저 아이디로 읽어,
// 존재하지 않는 시청자에게 체류 달란트가 쌓인다. 실제로 그 사고가 있었다.
func (l *Live) onViewerList(p soop.Packet, now time.Time) {
	if p.Field(1) != "1" {
		// 단일 갱신 — 유저는 한 명뿐이다.
		if id := soop.NormalizeUserID(p.Field(2)); soop.IsUserID(id) {
			l.touch(id, now)
		}
		return
	}

	// 목록형. 앞의 두 칸("", "1")을 건너뛰고 3개씩 읽는다.
	for i := 2; i+2 < len(p.Parts); i += 3 {
		// 세 번째 칸이 플래그가 아니면 정렬이 어긋난 것이다.
		// 계속 읽으면 닉네임이나 제어 값을 아이디로 오인하므로 멈춘다.
		if !soop.IsFlagField(p.Parts[i+2]) {
			return
		}
		if id := soop.NormalizeUserID(p.Parts[i]); soop.IsUserID(id) {
			l.touch(id, now)
		}
	}
}

func (l *Live) touch(userID string, now time.Time) {
	pr := l.seen[userID]
	grace := time.Duration(l.rules.Stay.GraceSeconds) * time.Second
	if pr == nil || now.Sub(pr.lastSeen) > grace {
		// 새 세션. 유예 시간을 넘겨 사라졌다 돌아온 경우도 새 세션으로 본다.
		l.seen[userID] = &presence{startedAt: now, lastSeen: now}
		return
	}
	pr.lastSeen = now
}

// CollectStayTicks는 체류 달란트를 정산한다. 주기적으로 호출한다.
// 유예 시간을 넘겨 사라진 세션은 여기서 정리된다.
func (l *Live) CollectStayTicks(now time.Time) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	r := l.rules.Stay
	if r.IntervalMinutes <= 0 || r.Points <= 0 {
		return nil
	}
	interval := time.Duration(r.IntervalMinutes) * time.Minute
	grace := time.Duration(r.GraceSeconds) * time.Second

	var out []Event
	for userID, pr := range l.seen {
		if now.Sub(pr.lastSeen) > grace {
			delete(l.seen, userID)
			continue
		}
		// 세션 시작부터 지금까지 몇 번의 주기를 채웠는가
		due := int(now.Sub(pr.startedAt) / interval)
		for tick := pr.ticksPaid + 1; tick <= due; tick++ {
			out = append(out, Event{
				BID: l.BID, UserID: userID, Kind: KindStay,
				Amount: r.Points, OccurredAt: now, BroadNo: l.broadPtr(),
				IdemKey: fmt.Sprintf("stay:%d:%s:%d", l.broadNo, userID, tick),
				Meta:    map[string]any{"tick": tick, "intervalMinutes": r.IntervalMinutes},
			})
		}
		if due > pr.ticksPaid {
			pr.ticksPaid = due
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 후원 · 구독 · 팬클럽
// ---------------------------------------------------------------------------

func (l *Live) onDonation(p soop.Packet, now time.Time) []Event {
	ev, ok := soop.ParseDonation(p)
	if !ok {
		return nil
	}
	rate := l.rules.Donation.StarBalloonRate
	if ev.Item != soop.ItemStarBalloon {
		rate = l.rules.Donation.AdBalloonRate
	}
	amount := ev.Count * rate
	if amount <= 0 {
		return nil
	}
	return []Event{{
		BID: l.BID, UserID: ev.UserID, Kind: KindDonation,
		Amount: amount, OccurredAt: now, BroadNo: l.broadPtr(),
		// 같은 사람이 같은 초에 같은 개수를 두 번 후원하는 일은 사실상 없다.
		IdemKey: fmt.Sprintf("donation:%d:%s:%s:%d:%d",
			l.broadNo, ev.UserID, ev.Item, ev.Count, now.Unix()),
		Meta: map[string]any{"item": ev.Item, "count": ev.Count},
	}}
}

func (l *Live) onSubscribe(p soop.Packet, now time.Time) []Event {
	ev, ok := soop.ParseSubscribe(p)
	if !ok {
		return nil
	}
	r := l.rules.Subscribe
	months := min(ev.Months, r.MaxMonths)
	amount := r.Base + r.PerMonth*months
	if amount <= 0 {
		return nil
	}
	return []Event{{
		BID: l.BID, UserID: ev.UserID, Kind: KindSubscribe,
		Amount: amount, OccurredAt: now, BroadNo: l.broadPtr(),
		// 개월 수가 같으면 같은 구독으로 본다 — 재접속 시 중복 적립을 막는다.
		IdemKey: fmt.Sprintf("subscribe:%s:%d", ev.UserID, ev.Months),
		Meta:    map[string]any{"months": ev.Months},
	}}
}

// onFanclubInfo는 0127에서 유저별 팬클럽 주차를 읽는다.
// 형식: ["", 유저ID, "fw=45&afw=45", 유저ID, "fw=-1&afw=-1", ...]
func (l *Live) onFanclubInfo(p soop.Packet, now time.Time) []Event {
	var out []Event
	for i := 1; i+1 < len(p.Parts); i += 2 {
		id := soop.NormalizeUserID(p.Parts[i])
		if id == "" {
			continue
		}
		if e := l.checkFanclub(id, parseFW(p.Parts[i+1]), now); e != nil {
			out = append(out, *e)
		}
	}
	return out
}

// checkFanclub은 미가입(-1)에서 가입으로 바뀌는 순간을 잡는다.
//
// 중요: 처음 보는 유저는 기준선으로만 기록하고 적립하지 않는다.
// SOOP에는 "팬클럽 가입" 이벤트가 없어 상태 전환으로 추론할 수밖에 없는데,
// 첫 관측을 가입으로 치면 이미 오래된 팬 전원에게 가입 달란트가 나가버린다.
func (l *Live) checkFanclub(userID string, weeks int, now time.Time) *Event {
	prev, known := l.fanclub[userID]
	l.fanclub[userID] = weeks
	if !known || l.rules.Fanclub.JoinPoints <= 0 {
		return nil
	}
	if prev > 0 || weeks <= 0 {
		return nil
	}
	return &Event{
		BID: l.BID, UserID: userID, Kind: KindFanclub,
		Amount: l.rules.Fanclub.JoinPoints, OccurredAt: now, BroadNo: l.broadPtr(),
		IdemKey: fmt.Sprintf("fanclub:%s", userID), // 최초 1회만
		Meta:    map[string]any{"weeks": weeks},
	}
}

// parseFW는 "fw=45&afw=45"에서 주차를 뽑는다. 실패하면 -1(미가입).
func parseFW(s string) int {
	const key = "fw="
	i := 0
	for i < len(s) {
		// afw= 가 아닌 fw= 를 찾는다
		if (i == 0 || s[i-1] == '&') && len(s)-i >= len(key) && s[i:i+len(key)] == key {
			j := i + len(key)
			end := j
			for end < len(s) && s[end] != '&' {
				end++
			}
			if n, err := strconv.Atoi(s[j:end]); err == nil {
				return n
			}
			return -1
		}
		i++
	}
	return -1
}

func (l *Live) broadPtr() *int64 {
	if l.broadNo == 0 {
		return nil
	}
	n := l.broadNo
	return &n
}

// FlushStayLoop은 주기적으로 체류 달란트를 정산해 저장한다.
func (l *Live) FlushStayLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			events := l.CollectStayTicks(now)
			if len(events) == 0 {
				continue
			}
			if n, err := Award(ctx, l.Pool, events); err != nil {
				l.Log.Warn("체류 달란트 적립 실패", "err", err)
			} else if n > 0 {
				l.Log.Info("체류 달란트 적립", "events", n)
			}
		}
	}
}
