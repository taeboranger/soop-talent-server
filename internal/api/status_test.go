package api

import (
	"context"
	"testing"
	"time"

	"github.com/sooptalent/server/internal/collector"
	"github.com/sooptalent/server/internal/soop"
)

// fakeStreamer는 수집기 대신 정해진 상태를 돌려준다.
type fakeStreamer struct{ st collector.Status }

func (f *fakeStreamer) Add(context.Context, string)                 {}
func (f *fakeStreamer) Remove(string)                               {}
func (f *fakeStreamer) SetChat(context.Context, string, bool) error { return nil }
func (f *fakeStreamer) Status(string) collector.Status              { return f.st }

// 화면에 뭐라고 뜨는지가 이 코드의 전부다.
// "꺼 둔 것"과 "고장난 것"이 섞이면 스트리머가 엉뚱한 곳을 뒤지게 된다.
func TestChatStatus라벨(t *testing.T) {
	cases := []struct {
		name  string
		in    collector.Status
		tone  string
		label string
	}{
		{"수집 대상이 아님", collector.Status{}, toneWarn, "대기"},
		{"꺼 둠", collector.Status{Known: true}, toneOff, "꺼짐"},
		{"켰는데 안 돎", collector.Status{Known: true, Enabled: true}, toneBad, "중지됨"},
		{"붙어 있음", collector.Status{Known: true, Enabled: true, Running: true,
			Chat: soop.ChatState{Phase: soop.PhaseConnected}}, toneOK, "수집 중"},
		{"방송 꺼짐", collector.Status{Known: true, Enabled: true, Running: true,
			Chat: soop.ChatState{Phase: soop.PhaseOffline}}, toneWarn, "방송 대기"},
		{"재접속 중", collector.Status{Known: true, Enabled: true, Running: true,
			Chat: soop.ChatState{Phase: soop.PhaseRetrying}}, toneBad, "재접속 중"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{Streamer: &fakeStreamer{st: c.in}}
			got := s.chatStatus("bj")
			if got.Tone != c.tone || got.Label != c.label {
				t.Fatalf("tone/label = %q/%q, 원하는 값 %q/%q", got.Tone, got.Label, c.tone, c.label)
			}
			if got.Detail == "" {
				t.Error("설명이 비어 있다 — 무엇을 해야 할지 알 수 없다")
			}
		})
	}
}

// 제로값 시각이 0001-01-01로 새어 나가면 화면에 그대로 찍힌다.
func TestChatStatus빈시각은생략(t *testing.T) {
	s := &Server{Streamer: &fakeStreamer{st: collector.Status{
		Known: true, Enabled: true, Running: true,
		Chat: soop.ChatState{Phase: soop.PhaseConnected, LastPacket: time.Now()},
	}}}
	got := s.chatStatus("bj")

	if got.ConnectedAt != nil {
		t.Error("접속한 적이 없는데 connectedAt이 채워졌다")
	}
	if got.LastPacketAt == nil {
		t.Error("패킷을 받았는데 lastPacketAt이 비었다")
	}
}

func TestWorstTone(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{toneOK, toneOK}, toneOK},
		{[]string{toneOK, toneOff}, toneOff}, // 꺼 둔 것은 고장이 아니다
		{[]string{toneOff, toneWarn}, toneWarn},
		{[]string{toneWarn, toneBad, toneOK}, toneBad}, // 가장 급한 것이 이긴다
		{nil, toneOK},
	}
	for _, c := range cases {
		if got := worstTone(c.in...); got != c.want {
			t.Errorf("worstTone(%v) = %q, 원하는 값 %q", c.in, got, c.want)
		}
	}
}
