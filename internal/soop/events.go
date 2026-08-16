package soop

import (
	"strconv"
	"strings"
)

// 아래 필드 인덱스는 전부 실측으로 확인한 값이다 (2026-08-10, 천양 방송).
// SOOP은 프로토콜을 문서화하지 않으므로 인덱스를 추측하지 않는다.

// ChatEvent는 일반 채팅(0005)과 이모티콘 채팅(0109)을 함께 표현한다.
type ChatEvent struct {
	UserID       string
	Nickname     string
	Message      string
	FanclubWeeks int // -1이면 팬클럽 미가입
	Emoticon     bool
}

// PresenceEvent는 입장(0012)/퇴장(0013)이다.
type PresenceEvent struct {
	UserID   string
	Nickname string
	Entered  bool
}

// DonationEvent는 별풍선(0018)과 애드벌룬(0087)을 통합한 표현이다.
// 두 코드는 필드 배치가 서로 달라 파싱을 분리해야 한다.
type DonationEvent struct {
	UserID   string
	Nickname string
	Item     string // "starballoon" | "adballoon"
	Count    int
}

// ParseChat은 0005/0109를 ChatEvent로 바꾼다. 해당 코드가 아니면 ok=false.
func ParseChat(p Packet) (ChatEvent, bool) {
	switch p.Svc {
	case SvcChat:
		// [1]메시지 [2]유저ID [6]닉네임 [8]팬클럽주차
		return ChatEvent{
			UserID:       NormalizeUserID(p.Field(2)),
			Nickname:     p.Field(6),
			Message:      p.Field(1),
			FanclubWeeks: atoiDefault(p.Field(8), -1),
		}, p.Field(2) != ""
	case SvcEmoticon:
		// [6]유저ID [7]닉네임 [13]팬클럽주차 — 채팅 본문 대신 이모티콘 식별자가 온다
		return ChatEvent{
			UserID:       NormalizeUserID(p.Field(6)),
			Nickname:     p.Field(7),
			Message:      "",
			FanclubWeeks: atoiDefault(p.Field(13), -1),
			Emoticon:     true,
		}, p.Field(6) != ""
	}
	return ChatEvent{}, false
}

// ParsePresence는 0012/0013을 PresenceEvent로 바꾼다.
func ParsePresence(p Packet) (PresenceEvent, bool) {
	switch p.Svc {
	case SvcEnter:
		// [2]유저ID [3]닉네임
		id := NormalizeUserID(p.Field(2))
		return PresenceEvent{UserID: id, Nickname: p.Field(3), Entered: true}, id != ""
	case SvcExit:
		// [1]유저ID [4]닉네임
		id := NormalizeUserID(p.Field(1))
		return PresenceEvent{UserID: id, Nickname: p.Field(4), Entered: false}, id != ""
	}
	return PresenceEvent{}, false
}

// ParseDonation은 0018/0087을 DonationEvent로 바꾼다.
func ParseDonation(p Packet) (DonationEvent, bool) {
	switch p.Svc {
	case SvcStarBalloon:
		// [1]대상 [2]후원자ID [3]닉네임 [4]개수
		id := NormalizeUserID(p.Field(2))
		n := atoiDefault(p.Field(4), 0)
		return DonationEvent{
			UserID: id, Nickname: p.Field(3),
			Item: ItemStarBalloon, Count: n,
		}, id != "" && n > 0

	case SvcAdBalloon:
		// [3]후원자ID [4]닉네임 [8]아이템 이미지 URL [10]개수
		id := NormalizeUserID(p.Field(3))
		n := atoiDefault(p.Field(10), 0)
		return DonationEvent{
			UserID: id, Nickname: p.Field(4),
			Item: itemFromURL(p.Field(8)), Count: n,
		}, id != "" && n > 0
	}
	return DonationEvent{}, false
}

// SubscribeEvent는 구독(0093)이다.
type SubscribeEvent struct {
	UserID   string
	Nickname string
	Months   int
}

// ParseSubscribe는 0093을 SubscribeEvent로 바꾼다.
//
// 주의: 이 코드의 의미는 **단일 샘플에 근거한 추정**이다.
// 2026-08-10 천양 방송에서 관측한 한 건은 다음과 같았다.
//
//	[1]=243000  [2]=babo5248  [3]=까칠한동네형  [4]=14  [5]=565  [6]=106  [7]=14
//
// [1]~[3]이 스트리머·유저·닉네임인 것은 0018/0087과 동일한 배치라 확실하다.
// [4]와 [7]에 같은 값이 반복되는 점, 그리고 그 값의 크기로 미루어 개월 수로 본다.
// [6]=106의 의미는 확인하지 못했다.
//
// 두 번째 샘플이 확보되기 전까지는 raw_chat_events에 원본이 계속 쌓이므로,
// 해석이 틀린 것으로 밝혀져도 소급 재처리가 가능하다.
func ParseSubscribe(p Packet) (SubscribeEvent, bool) {
	if p.Svc != SvcSubscribe {
		return SubscribeEvent{}, false
	}
	id := NormalizeUserID(p.Field(2))
	months := atoiDefault(p.Field(4), 0)
	// 개월 수가 비상식적이면 필드 해석이 틀렸을 가능성이 크다. 적립하지 않는다.
	if id == "" || months <= 0 || months > 600 {
		return SubscribeEvent{}, false
	}
	return SubscribeEvent{UserID: id, Nickname: p.Field(3), Months: months}, true
}

const (
	ItemStarBalloon = "starballoon"
	ItemAdBalloon   = "adballoon"
	ItemUnknown     = "unknown"
)

// itemFromURL은 0087의 아이템 이미지 URL에서 종류를 뽑는다.
// 예: .../items/img_adballoon.png → adballoon
// 새 아이템이 추가돼도 unknown으로 떨어질 뿐 파싱이 깨지지 않는다.
func itemFromURL(u string) string {
	if u == "" {
		return ItemUnknown
	}
	base := u
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".png")
	base = strings.TrimPrefix(base, "img_")
	if base == "" {
		return ItemUnknown
	}
	return base
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
