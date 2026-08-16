package soop

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SOOP 채팅 서버는 브라우저와 동일한 핸드셰이크 헤더를 요구한다.
// Origin이 없으면 소켓은 열리지만 어떤 패킷도 내려오지 않는다 — 실측 확인.
var handshakeHeader = http.Header{
	"Origin":     {"https://play.sooplive.com"},
	"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"},
}

const (
	pingInterval  = 20 * time.Second
	readTimeout   = 90 * time.Second
	dialTimeout   = 15 * time.Second
	minBackoff    = 1 * time.Second
	maxBackoff    = 60 * time.Second
	sendQueueSize = 16
)

// 채팅 연결이 지날 수 있는 상태. 화면 문구는 여기서 갈라진다.
const (
	PhaseConnecting = "connecting" // 채널을 찾고 소켓을 여는 중
	PhaseConnected  = "connected"  // 붙어서 패킷을 받는 중
	PhaseOffline    = "offline"    // 방송이 꺼져 있어 기다리는 중 (오류 아님)
	PhaseRetrying   = "retrying"   // 끊겨서 다시 붙기를 기다리는 중
	PhaseStopped    = "stopped"    // 수집이 중지됨
)

// ChatState는 채팅 연결의 현재 모습이다.
// 시각 필드는 값이 없을 수 있으므로 IsZero로 확인하고 쓴다.
type ChatState struct {
	Phase       string    // 위 Phase* 중 하나
	Detail      string    // 사람이 읽을 한 줄
	Since       time.Time // 지금 상태가 된 시각
	ConnectedAt time.Time // 마지막으로 붙은 시각
	LastPacket  time.Time // 마지막으로 패킷을 받은 시각
	BroadNo     string
	Packets     int64
	LastError   string // 마지막 실패 사유 (다시 붙어도 지우지 않는다)
	Retries     int    // 마지막 정상 접속 이후 재시도 횟수
}

// ChatClient는 한 스트리머의 채팅방에 붙어 패킷을 흘려보낸다.
// 재접속·핑·채널 재조회를 스스로 처리하므로 호출자는 Out만 읽으면 된다.
type ChatClient struct {
	BID string
	API *Client
	Log *slog.Logger

	// Out은 수신한 모든 패킷을 전달한다. Run이 반환될 때 닫힌다.
	Out chan Packet

	// 상태는 상태 화면이 아무 때나 읽으므로 잠금으로 보호한다.
	// 패킷당 한 번 잠그지만 경합이 없어 비용은 무시할 만하다.
	mu    sync.Mutex
	state ChatState
}

func NewChatClient(bid string, api *Client, log *slog.Logger) *ChatClient {
	return &ChatClient{
		BID: bid,
		API: api,
		Log: log.With("bid", bid),
		// 대형 방송은 초당 100+ 패킷을 쏟아낸다. 소비자가 잠시 느려져도
		// 소켓 읽기가 막히지 않도록 넉넉히 잡는다.
		Out: make(chan Packet, 4096),
		state: ChatState{
			Phase:  PhaseConnecting,
			Detail: "채팅 서버를 찾는 중입니다.",
			Since:  time.Now(),
		},
	}
}

// State는 지금 상태의 사본을 돌려준다.
func (c *ChatClient) State() ChatState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// setPhase는 상태가 실제로 바뀔 때만 Since를 갱신한다.
// 60초마다 도는 방송 대기 루프가 "방금 바뀜"으로 보이지 않게 하기 위해서다.
func (c *ChatClient) setPhase(phase, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Phase != phase {
		c.state.Since = time.Now()
	}
	c.state.Phase = phase
	c.state.Detail = detail
}

func (c *ChatClient) markConnected(broadNo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.state.Phase != PhaseConnected {
		c.state.Since = now
	}
	c.state.Phase = PhaseConnected
	c.state.Detail = "채팅을 받고 있습니다."
	c.state.ConnectedAt = now
	c.state.BroadNo = broadNo
	c.state.Retries = 0
}

func (c *ChatClient) markPacket() {
	c.mu.Lock()
	c.state.LastPacket = time.Now()
	c.state.Packets++
	c.mu.Unlock()
}

// markFailure는 재접속 대기 상태로 넘긴다. 사유는 다시 붙어도 남겨 둔다 —
// "지금은 멀쩡한데 아까 왜 끊겼지"에 답할 수 있어야 한다.
func (c *ChatClient) markFailure(detail string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Phase != PhaseRetrying {
		c.state.Since = time.Now()
	}
	c.state.Phase = PhaseRetrying
	c.state.Detail = detail
	c.state.Retries++
	if err != nil {
		c.state.LastError = err.Error()
	}
}

// Run은 ctx가 끝날 때까지 접속을 유지한다.
// 방송이 꺼져 있으면 켜질 때까지 폴링하며 기다린다.
func (c *ChatClient) Run(ctx context.Context) {
	defer close(c.Out)
	defer c.setPhase(PhaseStopped, "수집이 중지되었습니다.")

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		ch, err := c.API.LiveChannel(ctx, c.BID)
		switch {
		case err != nil:
			c.markFailure("SOOP 채널 정보를 가져오지 못했습니다. 곧 다시 시도합니다.", err)
			c.Log.Warn("채널 조회 실패", "err", err)
		case !ch.IsLive():
			// 방송 전에는 조용히 기다린다. 오류가 아니므로 백오프도 늘리지 않는다.
			c.setPhase(PhaseOffline, "방송이 꺼져 있습니다. 시작하면 자동으로 붙습니다.")
			if !sleepCtx(ctx, 60*time.Second) {
				return
			}
			continue
		default:
			c.setPhase(PhaseConnecting, "채팅 서버에 접속하는 중입니다.")
			if err := c.session(ctx, ch); err != nil {
				if ctx.Err() != nil {
					return // 수집 중단 — 오류로 기록할 일이 아니다
				}
				c.markFailure("연결이 끊겼습니다. 곧 다시 붙습니다.", err)
				c.Log.Warn("세션 종료", "err", err)
			}
			// 정상 세션이 한 번이라도 성립하면 백오프를 초기화한다.
			backoff = minBackoff
		}

		if !sleepCtx(ctx, jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session은 한 번의 WebSocket 연결을 처음부터 끝까지 담당한다.
func (c *ChatClient) session(ctx context.Context, ch *Channel) error {
	url := fmt.Sprintf("wss://%s:%d/Websocket/%s", ch.ChatDomain, ch.ChatPort+1, c.BID)
	c.Log.Info("채팅 접속", "url", url, "chatNo", ch.ChatNo, "broadNo", ch.BroadNo)

	dialer := &websocket.Dialer{
		HandshakeTimeout: dialTimeout,
		Subprotocols:     []string{"chat"},
	}
	conn, _, err := dialer.DialContext(ctx, url, handshakeHeader)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 쓰기는 한 고루틴으로 직렬화한다. gorilla/websocket은 동시 쓰기를 허용하지 않는다.
	send := make(chan []byte, sendQueueSize)
	go c.writeLoop(ctx, conn, send)

	send <- connectFrame()

	// 서버가 조용해지면 죽은 연결로 간주한다. 핑 응답이 읽기 데드라인을 갱신한다.
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

		pkt, err := Decode(frame)
		if err != nil {
			continue // 깨진 프레임 하나 때문에 세션을 버리지 않는다
		}
		c.markPacket()

		// CONNECT 응답을 받은 뒤에야 방에 입장할 수 있다.
		if pkt.Svc == SvcConnect {
			send <- joinFrame(ch.ChatNo)
			go c.pingLoop(ctx, send)
			c.markConnected(ch.BroadNo)
		}

		pkt.BroadNo = ch.BroadNo
		select {
		case c.Out <- pkt:
		default:
			// 소비자가 밀렸다. 패킷을 버리되 침묵하지는 않는다.
			c.Log.Warn("수신 버퍼 포화 — 패킷 유실", "svc", pkt.Svc)
		}
	}
}

func (c *ChatClient) writeLoop(ctx context.Context, conn *websocket.Conn, send <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-send:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
				c.Log.Debug("쓰기 실패 — 재접속 대기", "err", err)
				conn.Close() // 읽기 루프를 깨워 세션을 정리시킨다
				return
			}
		}
	}
}

func (c *ChatClient) pingLoop(ctx context.Context, send chan<- []byte) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case send <- pingFrame():
			default: // 큐가 막혔으면 이번 핑은 건너뛴다
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter는 여러 수집기가 동시에 재접속해 몰리는 것을 막는다.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d/2+1)))
}
