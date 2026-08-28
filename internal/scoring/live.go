package scoring

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sooptalent/server/internal/soop"
	"github.com/sooptalent/server/internal/store"
)

// Live는 실시간 채팅 이벤트를 달란트로 바꾼다.
// 스트리머 한 명당 하나씩 두며, 내부 상태는 뮤텍스로 보호한다.
type Live struct {
	BID  string
	Pool *store.DB
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

}

type presence struct {
	startedAt     time.Time
	lastSeen      time.Time
	ticksPaid     int
	authoritative bool
}

type minuteCount struct {
	minute string
	n      int
}

// SDKEvent는 공식 SOOP Chat SDK가 전달한 한 이벤트다.
type SDKEvent struct {
	ID       string          `json:"id"`
	Action   string          `json:"action"`
	BroadNo  string          `json:"broadNo,omitempty"`
	Message  json.RawMessage `json:"message"`
	Received time.Time       `json:"receivedAt"`
}

type sdkUser struct {
	UserID       string `json:"userId"`
	UserNickname string `json:"userNickname"`
}

type sdkMessage struct {
	sdkUser
	BJID                  string    `json:"bjId"`
	Message               string    `json:"message"`
	ImageURL              string    `json:"imageUrl"`
	Count                 int       `json:"count"`
	FanNumber             int       `json:"fanNumber"`
	SubscriptionMonths    int       `json:"subscriptionMonths"`
	AccSubscriptionMonths int       `json:"accSubscriptionMonths"`
	Type                  string    `json:"type"`
	ReceiverID            string    `json:"receiverId"`
	ReceiverNickname      string    `json:"receiverNickname"`
	UserList              []sdkUser `json:"userList"`
}

// HandleSDK는 문서화된 공식 Chat SDK 이벤트를 달란트 이벤트로 바꾼다.
func (l *Live) HandleSDK(ev SDKEvent, now time.Time) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ev.BroadNo != "" {
		if n, err := strconv.ParseInt(ev.BroadNo, 10, 64); err == nil && l.broadNo != n {
			l.broadNo = n
			l.seen = make(map[string]*presence)
		}
	}
	var m sdkMessage
	if err := json.Unmarshal(ev.Message, &m); err != nil {
		return nil
	}
	key := ev.ID
	if key == "" {
		key = fmt.Sprintf("%d", now.UnixNano())
	}
	switch ev.Action {
	case "IN":
		for _, u := range m.UserList {
			l.touchOfficial(u.UserID, now)
		}
	case "JOIN":
		// JOIN은 SDK 자신(스트리머)의 입장이다. 시청자로 적립하지 않는다.
	case "OUT":
		for _, u := range m.UserList {
			delete(l.seen, u.UserID)
		}
	case "QUIT":
		// SDK 자신의 강제 퇴장이다. 브리지 종료 상태가 전체 체류를 정리한다.
	case "USERSTATUS_CHANGED":
		for _, u := range m.UserList {
			l.touchOfficial(u.UserID, now)
		}
		l.touchOfficial(m.UserID, now)
	case "MESSAGE", "MANAGER_MESSAGE":
		l.touchOfficial(m.UserID, now)
		return l.awardChat(m.UserID, m.Message, m.Message == "" && m.ImageURL != "", key, now)
	case "BALLOON_GIFTED", "VIDEOBALLOON_GIFTED", "BATTLE_MISSION_GIFTED", "CHALLENGE_MISSION_GIFTED":
		return l.awardSDKDonation(m, "star_balloon", l.rules.Donation.StarBalloonRate, key, now)
	case "ADBALLOON_GIFTED":
		return l.awardSDKDonation(m, "ad_balloon", l.rules.Donation.AdBalloonRate, key, now)
	case "SUBSCRIBED":
		return l.awardSDKSubscribe(m.UserID, subscriptionTypeMonths(m.Type), key, now)
	case "SUBSCRIPTION_RENEWED":
		months := m.SubscriptionMonths
		if months <= 0 {
			months = m.AccSubscriptionMonths
		}
		return l.awardSDKSubscribe(m.UserID, months, key, now)
	case "SUBSCRIPTION_GIFTED":
		// 구독권을 결제한 기여는 선물한 사용자에게 적립한다. 수령자의 실제 구독
		// 활성화는 별도 SUBSCRIBED 이벤트로 오므로 수령자에게 중복 지급하지 않는다.
		return l.awardSDKSubscribe(m.UserID, subscriptionTypeMonths(m.Type), key, now)
	}
	return nil
}

func (l *Live) touchOfficial(userID string, now time.Time) {
	if !soop.IsUserID(userID) || soop.NormalizeUserID(userID) == soop.NormalizeUserID(l.BID) {
		return
	}
	if p := l.seen[userID]; p != nil {
		p.lastSeen, p.authoritative = now, true
		return
	}
	l.seen[userID] = &presence{startedAt: now, lastSeen: now, authoritative: true}
}

func (l *Live) OfficialDisconnected() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, p := range l.seen {
		if p.authoritative {
			delete(l.seen, id)
		}
	}
}

func subscriptionTypeMonths(kind string) int {
	switch strings.ToUpper(kind) {
	case "MEMBERSHIP_3", "GIFT_90":
		return 3
	case "MEMBERSHIP_6", "GIFT_180":
		return 6
	case "MEMBERSHIP_12":
		return 12
	default:
		return 1
	}
}

func (l *Live) awardSDKSubscribe(userID string, months int, key string, now time.Time) []Event {
	if !soop.IsUserID(userID) || soop.NormalizeUserID(userID) == soop.NormalizeUserID(l.BID) {
		return nil
	}
	months = max(1, min(months, l.rules.Subscribe.MaxMonths))
	amount := l.rules.Subscribe.Base + l.rules.Subscribe.PerMonth*months
	if amount <= 0 {
		return nil
	}
	return []Event{{BID: l.BID, UserID: userID, Kind: KindSubscribe, Amount: amount,
		OccurredAt: now, BroadNo: l.broadPtr(), IdemKey: "sdk:" + key + ":subscribe",
		Meta: map[string]any{"months": months}}}
}

func (l *Live) awardSDKDonation(m sdkMessage, item string, rate int, key string, now time.Time) []Event {
	if !soop.IsUserID(m.UserID) || soop.NormalizeUserID(m.UserID) == soop.NormalizeUserID(l.BID) || m.Count <= 0 {
		return nil
	}
	out := []Event{}
	if amount := m.Count * rate; amount > 0 {
		out = append(out, Event{BID: l.BID, UserID: m.UserID, Kind: KindDonation,
			Amount: amount, OccurredAt: now, BroadNo: l.broadPtr(), IdemKey: "sdk:" + key + ":donation",
			Meta: map[string]any{"item": item, "count": m.Count}})
	}
	if m.FanNumber > 0 && l.rules.Fanclub.JoinPoints > 0 {
		out = append(out, Event{BID: l.BID, UserID: m.UserID, Kind: KindFanclub,
			Amount: l.rules.Fanclub.JoinPoints, OccurredAt: now, BroadNo: l.broadPtr(),
			IdemKey: "fanclub:" + m.UserID,
			Meta:    map[string]any{"fanNumber": m.FanNumber, "source": item}})
	}
	return out
}

func NewLive(bid string, pool *store.DB, log *slog.Logger, rules Rules) *Live {
	return &Live{
		BID: bid, Pool: pool, Log: log.With("bid", bid), rules: rules,
		seen:    make(map[string]*presence),
		chatMin: make(map[string]*minuteCount),
		chatDay: make(map[string]int),
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
	return l.awardChat(ev.UserID, ev.Message, ev.Emoticon, "", now)
}

func (l *Live) awardChat(userID, message string, emoticon bool, sdkKey string, now time.Time) []Event {
	if !soop.IsUserID(userID) || soop.NormalizeUserID(userID) == soop.NormalizeUserID(l.BID) {
		return nil
	}
	r := l.rules.Chat
	if r.Points <= 0 {
		return nil
	}
	// 이모티콘 채팅은 본문이 없으므로 최소 길이 검사를 건너뛴다.
	if !emoticon && len([]rune(message)) < r.MinLength {
		return nil
	}

	// 분당 상한 — 도배 파밍 차단
	minute := now.Format("2006-01-02T15:04")
	mc := l.chatMin[userID]
	if mc == nil || mc.minute != minute {
		mc = &minuteCount{minute: minute}
		l.chatMin[userID] = mc
	}
	if mc.n >= r.PerMinuteCap {
		return nil
	}

	// 일일 상한
	day := now.Format("2006-01-02")
	if l.dayStamp != day {
		l.dayStamp = day
		l.chatDay = make(map[string]int)
	}
	if r.DailyCap > 0 && l.chatDay[userID]+r.Points > r.DailyCap {
		return nil
	}

	mc.n++
	l.chatDay[userID] += r.Points
	idem := fmt.Sprintf("chat:%d:%s:%s:%d", l.broadNo, userID, minute, mc.n)
	if sdkKey != "" {
		idem = "sdk:" + sdkKey + ":chat"
	}

	return []Event{{
		BID: l.BID, UserID: userID, Kind: KindChat,
		Amount: r.Points, OccurredAt: now, BroadNo: l.broadPtr(),
		// 같은 분 안의 n번째 채팅 — 재접속으로 패킷을 다시 받아도 중복되지 않는다.
		IdemKey: idem,
		Meta:    map[string]any{"emoticon": emoticon},
	}}
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
		if !pr.authoritative && now.Sub(pr.lastSeen) > grace {
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
// 후원 · 구독
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
