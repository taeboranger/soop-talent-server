package soop

import "testing"

// 아래 패킷은 2026-08-10 천양(243000) 방송에서 실제로 수신한 것이다.
// 필드 인덱스를 추측으로 바꾸지 못하도록 실측값을 테스트로 고정한다.

func pkt(svc string, parts ...string) Packet {
	// 실제 수신 패킷은 앞뒤에 빈 문자열이 붙으므로 동일하게 재현한다.
	all := append([]string{""}, parts...)
	return Packet{Svc: svc, Parts: append(all, "")}
}

func TestParseDonation_StarBalloon(t *testing.T) {
	p := pkt(SvcStarBalloon,
		"243000", "hammins", "환승710", "10", "0", "0", "565", "10", "0", "0",
		"kor_custom07", "0e6b128f-8368-48d0-bdd4-82ad17b7da8b", "ko_KR", "1774343693")

	got, ok := ParseDonation(p)
	if !ok {
		t.Fatal("별풍선 패킷 파싱 실패")
	}
	if got.UserID != "hammins" || got.Nickname != "환승710" {
		t.Errorf("후원자 불일치: %+v", got)
	}
	if got.Count != 10 {
		t.Errorf("개수 = %d, want 10", got.Count)
	}
	if got.Item != ItemStarBalloon {
		t.Errorf("아이템 = %q, want %q", got.Item, ItemStarBalloon)
	}
}

func TestParseDonation_AdBalloon(t *testing.T) {
	p := pkt(SvcAdBalloon,
		"565", "243000", "ghehd6131", "goTV",
		"애드벌룬 10개 선물!", "애드벌룬 10개를 선물했습니다!", "",
		"https://res.sooplive.com/new_player/items/img_adballoon.png",
		"https://res.sooplive.com/new_player/items/adballoon/ceremony/pc_10.png?t=1",
		"10", "0", "0", "0", "0", "junwoo")

	got, ok := ParseDonation(p)
	if !ok {
		t.Fatal("애드벌룬 패킷 파싱 실패")
	}
	if got.UserID != "ghehd6131" || got.Nickname != "goTV" {
		t.Errorf("후원자 불일치: %+v", got)
	}
	if got.Count != 10 {
		t.Errorf("개수 = %d, want 10", got.Count)
	}
	if got.Item != ItemAdBalloon {
		t.Errorf("아이템 = %q, want %q", got.Item, ItemAdBalloon)
	}
}

// 0093의 의미는 추정이다. 실측 패킷을 고정해 두어, 나중에 해석을 바꿀 때
// 무엇을 근거로 삼았는지 잃어버리지 않게 한다.
func TestParseSubscribe(t *testing.T) {
	p := pkt(SvcSubscribe,
		"243000", "babo5248", "까칠한동네형", "14", "565", "106", "14", "1",
		"6ed38990-e26e-448c-a09e-f561e6de73a2", "ko_KR", "0")

	got, ok := ParseSubscribe(p)
	if !ok {
		t.Fatal("구독 패킷 파싱 실패")
	}
	if got.UserID != "babo5248" || got.Nickname != "까칠한동네형" {
		t.Errorf("구독자 불일치: %+v", got)
	}
	if got.Months != 14 {
		t.Errorf("개월 수 = %d, want 14", got.Months)
	}
}

// 필드 해석이 틀렸을 때 말도 안 되는 값으로 달란트가 나가지 않아야 한다.
func TestParseSubscribe_거부(t *testing.T) {
	cases := map[string]Packet{
		"개월수 0":    pkt(SvcSubscribe, "243000", "u", "n", "0"),
		"개월수 과다": pkt(SvcSubscribe, "243000", "u", "n", "99999"),
		"유저 없음":   pkt(SvcSubscribe, "243000", "", "n", "14"),
		"다른 코드":   pkt(SvcChat, "243000", "u", "n", "14"),
	}
	for name, p := range cases {
		if _, ok := ParseSubscribe(p); ok {
			t.Errorf("%s: 거부되어야 하는데 통과함", name)
		}
	}
}

func TestParseChat(t *testing.T) {
	p := pkt(SvcChat,
		"이사준비한다는거구만", "nsamuel7371", "0", "0", "3", "지존폭도",
		"806961184|294912", "30", "DD32E0", "D15ED3", "30", "-1", "", "-1")

	got, ok := ParseChat(p)
	if !ok {
		t.Fatal("채팅 패킷 파싱 실패")
	}
	if got.UserID != "nsamuel7371" || got.Nickname != "지존폭도" {
		t.Errorf("작성자 불일치: %+v", got)
	}
	if got.Message != "이사준비한다는거구만" {
		t.Errorf("메시지 = %q", got.Message)
	}
	if got.FanclubWeeks != 30 {
		t.Errorf("팬클럽 주차 = %d, want 30", got.FanclubWeeks)
	}
	if got.Emoticon {
		t.Error("일반 채팅인데 Emoticon=true")
	}
}

func TestParseChat_Emoticon(t *testing.T) {
	p := pkt(SvcEmoticon,
		"565", "", "61de7e80d0979", "8", "1", "neet0101", "에포츠",
		"268501024|688128", "0", "-1", "", "png", "24", "E07E0A", "D39750",
		"24", "-1", "0", "-1")

	got, ok := ParseChat(p)
	if !ok {
		t.Fatal("이모티콘 채팅 파싱 실패")
	}
	if got.UserID != "neet0101" || got.Nickname != "에포츠" {
		t.Errorf("작성자 불일치: %+v", got)
	}
	if got.FanclubWeeks != 24 {
		t.Errorf("팬클럽 주차 = %d, want 24", got.FanclubWeeks)
	}
	if !got.Emoticon {
		t.Error("이모티콘 채팅인데 Emoticon=false")
	}
}

func TestParsePresence(t *testing.T) {
	enter := pkt(SvcEnter, "720928|163840", "leemk321", "Museum", "0", "0", "589856|163840")
	if got, ok := ParsePresence(enter); !ok || got.UserID != "leemk321" || !got.Entered {
		t.Errorf("입장 파싱 실패: %+v ok=%v", got, ok)
	}

	exit := pkt(SvcExit, "nanamoon777", "269026144|425984", "0", "나나문")
	got, ok := ParsePresence(exit)
	if !ok || got.UserID != "nanamoon777" || got.Entered {
		t.Errorf("퇴장 파싱 실패: %+v ok=%v", got, ok)
	}
	if got.Nickname != "나나문" {
		t.Errorf("퇴장 닉네임 = %q", got.Nickname)
	}
}

// 동시 접속 표기 제거. 달란트는 계정 단위로 적립되어야 한다.
func TestNormalizeUserID(t *testing.T) {
	cases := map[string]string{
		"cmr022426(2)":  "cmr022426",
		"llcbmassll(5)": "llcbmassll",
		"plain":         "plain",
		"weird(x)":      "weird(x)", // 숫자가 아니면 아이디의 일부로 본다
		"":              "",
	}
	for in, want := range cases {
		if got := NormalizeUserID(in); got != want {
			t.Errorf("NormalizeUserID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	frame := Encode(SvcJoin, "565", "", "", "", "")
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Svc != SvcJoin {
		t.Errorf("svc = %q, want %q", got.Svc, SvcJoin)
	}
	if got.Field(1) != "565" {
		t.Errorf("chatNo = %q, want 565", got.Field(1))
	}
}
