package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sooptalent/server/internal/scoring"
	"github.com/sooptalent/server/internal/soop"
	"github.com/sooptalent/server/internal/store"
)

const (
	// 달란트 이벤트를 모아 쓰는 주기·크기. 채팅이 몰려도 DB 왕복을 줄인다.
	awardFlushEvery = 2 * time.Second
	awardFlushSize  = 200
	// 체류 정산 주기. 지급 주기(기본 10분)보다 촘촘해야 tick을 놓치지 않는다.
	stayFlushEvery = 1 * time.Minute
	// 규칙 변경을 반영하는 주기. 설정을 바꾸면 이만큼 안에 적용된다.
	rulesReloadEvery = 30 * time.Second
)

// entry는 수집 대상 한 명이다.
//
// 승인된 스트리머는 채팅 연동을 꺼도 목록에 남는다.
// "꺼 둔 것"과 "애초에 대상이 아닌 것"은 화면에서 다르게 보여야 하기 때문이다.
type entry struct {
	base      context.Context    // 이 스트리머 수집의 수명을 관장하는 부모
	cancel    context.CancelFunc // nil이면 지금 멈춰 있다
	chat      *soop.ChatClient   // 멈춰 있으면 nil
	enabled   bool               // 채팅 연동 스위치
	startedAt time.Time
	official  bool
	sdk       chan scoring.SDKEvent
	live      *scoring.Live
	state     soop.ChatState
}

// Status는 상태 화면이 필요로 하는 것을 한 번에 담는다.
type Status struct {
	BID       string
	Known     bool // 수집 대상으로 등록돼 있는가
	Enabled   bool // 채팅 연동 스위치가 켜져 있는가
	Running   bool // 실제로 고루틴이 돌고 있는가
	StartedAt time.Time
	Chat      soop.ChatState
	Official  bool // 공식 Chat SDK 경로인지, 구 비공개 소켓 폴백인지
}

// Collector는 여러 스트리머의 채팅을 동시에 수집하고 달란트를 적립한다.
// 스트리머 하나당 고루틴 몇 개만 쓰므로 수백 명도 감당한다.
type Collector struct {
	Store *store.Store
	Raw   *store.RawWriter
	API   *soop.Client
	Log   *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
}

func New(st *store.Store, raw *store.RawWriter, log *slog.Logger) *Collector {
	return &Collector{
		Store:   st,
		Raw:     raw,
		API:     soop.NewClient(),
		Log:     log,
		entries: make(map[string]*entry),
	}
}

// Add는 스트리머를 수집 대상에 넣는다. 이미 있으면 아무 일도 하지 않는다.
// 채팅 연동이 꺼져 있으면 등록만 하고 소켓은 열지 않는다.
func (c *Collector) Add(ctx context.Context, bid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[bid]; ok {
		return
	}

	enabled, err := c.Store.ChatEnabled(ctx, bid)
	if err != nil {
		c.Log.Warn("채팅 연동 설정 조회 실패 — 켜진 것으로 봅니다", "bid", bid, "err", err)
		enabled = true
	}

	e := &entry{base: ctx, enabled: enabled}
	c.entries[bid] = e
	if enabled {
		c.startLocked(bid, e)
	} else {
		c.Log.Info("채팅 연동 꺼짐 — 수집하지 않습니다", "bid", bid)
	}
}

// Remove는 수집 대상에서 완전히 뺀다 (승인 철회).
func (c *Collector) Remove(bid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[bid]; ok {
		c.stopLocked(bid, e)
		delete(c.entries, bid)
	}
}

// SetChat은 채팅 연동을 켜고 끈다. 설정은 DB에 남아 서버를 다시 켜도 유지된다.
func (c *Collector) SetChat(ctx context.Context, bid string, on bool) error {
	if err := c.Store.SetChatEnabled(ctx, bid, on); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[bid]
	if !ok {
		// 아직 수집 대상이 아니다. 승인될 때 저장된 값을 읽어 반영한다.
		return nil
	}
	e.enabled = on
	switch {
	case on && e.cancel == nil:
		c.startLocked(bid, e)
	case !on && e.cancel != nil:
		c.stopLocked(bid, e)
	}
	return nil
}

// Status는 한 스트리머의 지금 상태를 돌려준다.
func (c *Collector) Status(bid string) Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[bid]
	if !ok {
		return Status{BID: bid}
	}
	st := Status{
		BID: bid, Known: true,
		Enabled: e.enabled, Running: e.cancel != nil,
		StartedAt: e.startedAt,
		Official:  e.official,
	}
	if e.chat != nil {
		st.Chat = e.chat.State()
	} else if e.official {
		st.Chat = e.state
	}
	return st
}

// Active는 지금 실제로 돌고 있는 스트리머만 돌려준다.
func (c *Collector) Active() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for b, e := range c.entries {
		if e.cancel != nil {
			out = append(out, b)
		}
	}
	return out
}

// startLocked는 c.mu를 쥔 채로 호출해야 한다.
func (c *Collector) startLocked(bid string, e *entry) {
	sub, cancel := context.WithCancel(e.base)
	e.cancel = cancel
	e.startedAt = time.Now()

	rules, err := scoring.Load(sub, c.Store.Pool, bid)
	if err != nil {
		c.Log.Warn("규칙 로드 실패 — 기본값 사용", "bid", bid, "err", err)
		rules = scoring.DefaultRules()
	}
	live := scoring.NewLive(bid, c.Store.Pool, c.Log, rules)
	e.live = live

	// 공식 OAuth 토큰이 있으면 문서화된 Chat SDK 브리지를 사용한다.
	// 토큰이 없는 기존 수동 연결만 종전 비공개 소켓을 임시 폴백으로 유지한다.
	var one int
	e.official = c.Store.Pool.QueryRow(sub,
		`SELECT 1 FROM soop_oauth_tokens WHERE bid=$1`, bid).Scan(&one) == nil
	if e.official {
		e.sdk = make(chan scoring.SDKEvent, 4096)
		e.state = soop.ChatState{Phase: soop.PhaseConnecting,
			Detail: "SOOP 공식 Chat SDK 연결을 기다리는 중입니다.", Since: time.Now()}
		go live.FlushStayLoop(sub, stayFlushEvery)
		go c.reloadRules(sub, bid, live)
		go c.consumeSDK(sub, bid, e.sdk, live)
		c.Log.Info("공식 Chat SDK 수집 시작", "bid", bid)
		return
	}

	client := soop.NewChatClient(bid, c.API, c.Log)
	e.chat = client
	go client.Run(sub)
	go live.FlushStayLoop(sub, stayFlushEvery)
	go c.reloadRules(sub, bid, live)
	go c.consume(sub, bid, client, live)

	c.Log.Info("수집 시작", "bid", bid)
}

// stopLocked는 c.mu를 쥔 채로 호출해야 한다.
func (c *Collector) stopLocked(bid string, e *entry) {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.cancel = nil
	e.chat = nil
	e.sdk = nil
	e.live = nil
	e.official = false
	c.Log.Info("수집 중단", "bid", bid)
}

// PromoteOfficial은 SOOP OAuth 로그인 직후 기존 비공개 소켓을 끊고 공식 SDK로 전환한다.
func (c *Collector) PromoteOfficial(ctx context.Context, bid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[bid]
	if !ok {
		return
	}
	if e.cancel != nil {
		c.stopLocked(bid, e)
	}
	if e.enabled {
		c.startLocked(bid, e)
	}
}

// IngestSDK는 내부 브리지가 보낸 공식 이벤트를 수집 큐에 넣는다.
func (c *Collector) IngestSDK(ctx context.Context, bid string, ev scoring.SDKEvent) error {
	c.mu.Lock()
	e := c.entries[bid]
	if e == nil || !e.official || e.sdk == nil {
		c.mu.Unlock()
		return fmt.Errorf("공식 채팅 수집 대상이 아닙니다: %s", bid)
	}
	e.state.LastPacket = time.Now()
	e.state.Packets++
	ch := e.sdk
	c.mu.Unlock()
	select {
	case ch <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("공식 채팅 이벤트 큐가 가득 찼습니다: %s", bid)
	}
}

// SetSDKState는 브리지의 접속·방송대기·오류 상태를 대시보드에 반영한다.
func (c *Collector) SetSDKState(bid, phase, broadNo, detail, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[bid]
	if e == nil || !e.official {
		return
	}
	now := time.Now()
	if e.state.Phase != phase {
		e.state.Since = now
	}
	e.state.Phase, e.state.Detail = phase, detail
	if broadNo != "" {
		e.state.BroadNo = broadNo
	}
	if phase == soop.PhaseConnected {
		e.state.ConnectedAt, e.state.Retries = now, 0
	}
	if lastErr != "" {
		e.state.LastError = lastErr
		e.state.Retries++
	}
	if phase != soop.PhaseConnected && e.live != nil {
		e.live.OfficialDisconnected()
	}
}

func (c *Collector) consumeSDK(ctx context.Context, bid string, in <-chan scoring.SDKEvent, live *scoring.Live) {
	var pending []scoring.Event
	flushAt := time.NewTicker(awardFlushEvery)
	defer flushAt.Stop()
	flush := func(base context.Context) {
		if len(pending) == 0 {
			return
		}
		if _, err := scoring.Award(base, c.Store.Pool, pending); err != nil {
			c.Log.Warn("공식 SDK 달란트 적립 실패", "bid", bid, "n", len(pending), "err", err)
		}
		pending = pending[:0]
	}
	for {
		select {
		case <-ctx.Done():
			end, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			flush(end)
			cancel()
			return
		case <-flushAt.C:
			flush(ctx)
		case ev := <-in:
			now := ev.Received
			if now.IsZero() {
				now = time.Now()
			}
			if ev.BroadNo != "" {
				_ = c.Store.StartBroadcast(ctx, bid, ev.BroadNo, "")
			}
			c.Raw.Write(store.RawEvent{BID: bid, BroadNo: ev.BroadNo, Svc: "sdk:" + ev.Action,
				Parts: []string{}, Raw: ev.Message, RecvAt: now})
			pending = append(pending, live.HandleSDK(ev, now)...)
			c.touchSDKUser(ctx, bid, ev.Message)
			if len(pending) >= awardFlushSize {
				flush(ctx)
			}
		}
	}
}

func (c *Collector) touchSDKUser(ctx context.Context, bid string, raw json.RawMessage) {
	var m struct {
		UserID           string `json:"userId"`
		UserNickname     string `json:"userNickname"`
		ReceiverID       string `json:"receiverId"`
		ReceiverNickname string `json:"receiverNickname"`
		UserList         []struct {
			UserID       string `json:"userId"`
			UserNickname string `json:"userNickname"`
		} `json:"userList"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	c.upsert(ctx, bid, m.UserID, m.UserNickname, -1)
	c.upsert(ctx, bid, m.ReceiverID, m.ReceiverNickname, -1)
	for _, u := range m.UserList {
		c.upsert(ctx, bid, u.UserID, u.UserNickname, -1)
	}
}

// reloadRules는 설정 화면에서 바꾼 규칙을 주기적으로 반영한다.
// 과거 적립은 그대로 두고 이후 이벤트부터 새 규칙이 적용된다.
func (c *Collector) reloadRules(ctx context.Context, bid string, live *scoring.Live) {
	t := time.NewTicker(rulesReloadEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r, err := scoring.Load(ctx, c.Store.Pool, bid); err == nil {
				live.SetRules(r)
			}
		}
	}
}

// consume은 한 스트리머의 패킷 스트림을 처리한다.
func (c *Collector) consume(ctx context.Context, bid string, client *soop.ChatClient, live *scoring.Live) {
	var (
		lastBroad string
		pending   []scoring.Event
		flushAt   = time.NewTicker(awardFlushEvery)
	)
	defer flushAt.Stop()

	flush := func() {
		if len(pending) == 0 {
			return
		}
		n, err := scoring.Award(ctx, c.Store.Pool, pending)
		if err != nil {
			c.Log.Warn("달란트 적립 실패", "bid", bid, "n", len(pending), "err", err)
		} else if n > 0 {
			c.Log.Debug("달란트 적립", "bid", bid, "events", n)
		}
		pending = pending[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flushRemaining(context.WithoutCancel(ctx), c, bid, pending)
			return

		case <-flushAt.C:
			flush()

		case pkt, ok := <-client.Out:
			if !ok {
				flushRemaining(context.WithoutCancel(ctx), c, bid, pending)
				return
			}
			now := time.Now()

			// 방송이 바뀌면 기록하고 체류 세션을 새로 시작한다.
			if pkt.BroadNo != "" && pkt.BroadNo != lastBroad {
				lastBroad = pkt.BroadNo
				if err := c.Store.StartBroadcast(ctx, bid, pkt.BroadNo, ""); err != nil {
					c.Log.Warn("방송 기록 실패", "bid", bid, "err", err)
				}
				live.SetBroadcast(pkt.BroadNo)
			}

			// 팬가입은 상태 목록(0127)을 추론해 적립하던 방식이었다.
			// 공식 API 승인 전까지는 수집·상태 갱신·적립 모두 하지 않는다.
			if pkt.Svc == soop.SvcFanclub {
				continue
			}

			// 시청자 목록은 양이 지나치게 많아 원본 저장에서 제외한다.
			// (2.4만 시청자 방송 기준 25초에 3천 패킷)
			if pkt.Svc != soop.SvcViewerList {
				c.Raw.Write(store.RawEvent{
					BID: bid, BroadNo: pkt.BroadNo, Svc: pkt.Svc,
					Parts: pkt.Parts, Raw: pkt.Raw, RecvAt: now,
				})
			}

			pending = append(pending, live.Handle(pkt, now)...)
			if len(pending) >= awardFlushSize {
				flush()
			}

			c.touchUser(ctx, bid, pkt)
		}
	}
}

// flushRemaining은 종료 시 남은 적립을 밀어낸다.
// ctx가 이미 취소됐으므로 짧은 별도 컨텍스트를 쓴다.
func flushRemaining(base context.Context, c *Collector, bid string, pending []scoring.Event) {
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	if _, err := scoring.Award(ctx, c.Store.Pool, pending); err != nil {
		c.Log.Warn("종료 시 적립 실패", "bid", bid, "err", err)
	}
}

// touchUser는 시청자 이름을 최신으로 유지한다.
func (c *Collector) touchUser(ctx context.Context, bid string, pkt soop.Packet) {
	switch pkt.Svc {
	case soop.SvcChat, soop.SvcEmoticon:
		if ev, ok := soop.ParseChat(pkt); ok {
			c.upsert(ctx, bid, ev.UserID, ev.Nickname, -1)
		}
	case soop.SvcEnter, soop.SvcExit:
		if ev, ok := soop.ParsePresence(pkt); ok {
			c.upsert(ctx, bid, ev.UserID, ev.Nickname, -1)
		}
	case soop.SvcStarBalloon, soop.SvcAdBalloon:
		if ev, ok := soop.ParseDonation(pkt); ok {
			c.upsert(ctx, bid, ev.UserID, ev.Nickname, -1)
		}
	case soop.SvcSubscribe:
		if ev, ok := soop.ParseSubscribe(pkt); ok {
			c.upsert(ctx, bid, ev.UserID, ev.Nickname, -1)
		}
	}
}

func (c *Collector) upsert(ctx context.Context, bid, userID, nick string, weeks int) {
	if userID == "" {
		return
	}
	if err := c.Store.TouchUser(ctx, bid, userID, nick, weeks); err != nil {
		c.Log.Debug("시청자 기록 실패", "bid", bid, "user", userID, "err", err)
	}
}
