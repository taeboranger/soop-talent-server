package collector

import (
	"context"
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
}

// Status는 상태 화면이 필요로 하는 것을 한 번에 담는다.
type Status struct {
	BID       string
	Known     bool // 수집 대상으로 등록돼 있는가
	Enabled   bool // 채팅 연동 스위치가 켜져 있는가
	Running   bool // 실제로 고루틴이 돌고 있는가
	StartedAt time.Time
	Chat      soop.ChatState
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
	}
	if e.chat != nil {
		st.Chat = e.chat.State()
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
	c.Log.Info("수집 중단", "bid", bid)
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

			// 시청자 목록과 팬클럽 정보는 양이 지나치게 많아 원본 저장에서 제외한다.
			// (2.4만 시청자 방송 기준 25초에 3천 패킷)
			if pkt.Svc != soop.SvcViewerList && pkt.Svc != soop.SvcFanclub {
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

// touchUser는 시청자 이름과 팬클럽 주차를 최신으로 유지한다.
func (c *Collector) touchUser(ctx context.Context, bid string, pkt soop.Packet) {
	switch pkt.Svc {
	case soop.SvcChat, soop.SvcEmoticon:
		if ev, ok := soop.ParseChat(pkt); ok {
			c.upsert(ctx, bid, ev.UserID, ev.Nickname, ev.FanclubWeeks)
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
