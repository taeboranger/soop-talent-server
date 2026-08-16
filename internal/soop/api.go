package soop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	liveAPIURL = "https://live.sooplive.com/afreeca/player_live_api.php"
	maxBodyLen = 4 << 20
)

// Client는 SOOP의 비공개 HTTP API를 감싼다.
type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Channel은 방송 중인 채널의 접속 정보다.
type Channel struct {
	Result     int
	BID        string
	Nick       string
	BroadNo    string
	Title      string
	ChatDomain string
	ChatPort   int
	ChatNo     string
}

func (c *Channel) IsLive() bool { return c != nil && c.Result == 1 && c.ChatDomain != "" }

// liveAPIResponse는 응답 중 실제로 쓰는 필드만 담는다.
// SOOP은 100개가 넘는 필드를 돌려주지만 대부분 플레이어 전용이다.
type liveAPIResponse struct {
	Channel struct {
		Result     json.Number `json:"RESULT"`
		BJID       string      `json:"BJID"`
		BJNick     string      `json:"BJNICK"`
		BroadNo    json.Number `json:"BNO"`
		Title      string      `json:"TITLE"`
		ChatDomain string      `json:"CHDOMAIN"`
		ChatPort   json.Number `json:"CHPT"`
		ChatNo     json.Number `json:"CHATNO"`
	} `json:"CHANNEL"`
}

// LiveChannel은 방송 중이면 채팅 접속 정보를 돌려준다.
// 방송 중이 아니어도 오류가 아니다 — Result가 1이 아닌 Channel을 돌려준다.
func (c *Client) LiveChannel(ctx context.Context, bid string) (*Channel, error) {
	form := url.Values{
		"bid": {bid}, "type": {"live"}, "pwd": {""},
		"player_type": {"html5"}, "stream_type": {"common"},
		"quality": {"HD"}, "mode": {"landing"},
		"from_api": {"0"}, "is_revive": {"false"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		liveAPIURL+"?bjid="+url.QueryEscape(bid), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("player_live_api: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyLen))
	if err != nil {
		return nil, err
	}

	var r liveAPIResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("player_live_api 응답 파싱: %w", err)
	}

	ch := &Channel{
		Result:     atoiSafe(r.Channel.Result.String()),
		BID:        r.Channel.BJID,
		Nick:       r.Channel.BJNick,
		BroadNo:    r.Channel.BroadNo.String(),
		Title:      r.Channel.Title,
		ChatDomain: r.Channel.ChatDomain,
		ChatPort:   atoiSafe(r.Channel.ChatPort.String()),
		ChatNo:     r.Channel.ChatNo.String(),
	}
	if ch.BID == "" {
		ch.BID = bid
	}
	return ch, nil
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
