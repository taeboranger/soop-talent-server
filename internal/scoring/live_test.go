package scoring

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sooptalent/server/internal/soop"
)

func newTestLive() *Live {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := NewLive("243000", nil, log, DefaultRules())
	l.SetBroadcast("296249615")
	return l
}

func sdkEvent(id, action, body string) SDKEvent {
	return SDKEvent{ID: id, Action: action, BroadNo: "296249615", Message: json.RawMessage(body)}
}

func TestSDK_채팅후원팬가입구독(t *testing.T) {
	l := newTestLive()
	now := time.Now()
	chat := l.HandleSDK(sdkEvent("chat-1", "MESSAGE", `{"userId":"viewer","userNickname":"시청자","message":"안녕하세요"}`), now)
	if len(chat) != 1 || chat[0].Kind != KindChat || chat[0].IdemKey != "sdk:chat-1:chat" {
		t.Fatalf("공식 채팅 적립=%+v", chat)
	}
	donation := l.HandleSDK(sdkEvent("gift-1", "BALLOON_GIFTED", `{"userId":"supporter","userNickname":"후원자","count":10,"fanNumber":42}`), now)
	if len(donation) != 2 || donation[0].Kind != KindDonation || donation[1].Kind != KindFanclub {
		t.Fatalf("후원+신규 팬가입 적립=%+v", donation)
	}
	if donation[0].Amount != 10*DefaultRules().Donation.StarBalloonRate {
		t.Errorf("후원 환산=%d", donation[0].Amount)
	}
	renew := l.HandleSDK(sdkEvent("sub-1", "SUBSCRIPTION_RENEWED", `{"userId":"subscriber","subscriptionMonths":6}`), now)
	want := DefaultRules().Subscribe.Base + 6*DefaultRules().Subscribe.PerMonth
	if len(renew) != 1 || renew[0].Amount != want {
		t.Fatalf("연속 구독 적립=%+v want=%d", renew, want)
	}
	gift := l.HandleSDK(sdkEvent("sub-gift", "SUBSCRIPTION_GIFTED", `{"userId":"giver","receiverId":"receiver","type":"GIFT_90"}`), now)
	if len(gift) != 1 || gift[0].UserID != "giver" {
		t.Fatalf("구독 선물 구매자 적립=%+v", gift)
	}
}

func TestSDK_IN_OUT_체류는공식퇴장까지유지(t *testing.T) {
	l := newTestLive()
	start := time.Now()
	l.HandleSDK(sdkEvent("in", "IN", `{"userList":[{"userId":"viewer","userNickname":"시청자"}]}`), start)
	// 비공개 목록 방식의 graceSeconds를 지나도 공식 IN 상태는 유지된다.
	if got := l.CollectStayTicks(start.Add(11 * time.Minute)); len(got) != 1 {
		t.Fatalf("공식 체류 적립=%d건, want 1", len(got))
	}
	l.HandleSDK(sdkEvent("out", "OUT", `{"userList":[{"userId":"viewer"}]}`), start.Add(12*time.Minute))
	if _, ok := l.seen["viewer"]; ok {
		t.Fatal("공식 OUT 뒤 체류 상태가 남아 있음")
	}
}

func TestSDK_JOIN과BJ채팅은스트리머본인에게적립하지않음(t *testing.T) {
	l := newTestLive()
	now := time.Now()
	l.HandleSDK(sdkEvent("join", "JOIN", `{"userId":"243000","userNickname":"스트리머"}`), now)
	if _, exists := l.seen["243000"]; exists {
		t.Fatal("SDK 자신의 JOIN을 시청자로 기록함")
	}
	if got := l.HandleSDK(sdkEvent("bj-chat", "MESSAGE", `{"userId":"243000","message":"방송자 채팅"}`), now); len(got) != 0 {
		t.Fatalf("스트리머 본인 채팅에 달란트 %d건 적립", len(got))
	}
}

func chatPkt(userID, nick, msg string, weeks int) soop.Packet {
	return soop.Packet{Svc: soop.SvcChat, Parts: []string{
		"", msg, userID, "0", "0", "3", nick, "589856|163840",
		itoa(weeks), "DD32E0", "D15ED3", itoa(weeks), "-1", "", "-1", "",
	}}
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// 도배로 달란트를 무한정 쌓지 못해야 한다.
func TestChat_분당상한(t *testing.T) {
	l := newTestLive()
	now := time.Date(2026, 8, 10, 21, 0, 0, 0, time.Local)

	awarded := 0
	for range 10 {
		awarded += len(l.Handle(chatPkt("spammer", "도배", "안녕하세요", -1), now))
	}
	if awarded != DefaultRules().Chat.PerMinuteCap {
		t.Errorf("분당 적립 = %d, want %d", awarded, DefaultRules().Chat.PerMinuteCap)
	}

	// 다음 분에는 다시 열려야 한다
	next := now.Add(time.Minute)
	if got := len(l.Handle(chatPkt("spammer", "도배", "안녕하세요", -1), next)); got != 1 {
		t.Errorf("다음 분 적립 = %d, want 1", got)
	}
}

func TestChat_짧은메시지제외(t *testing.T) {
	l := newTestLive()
	now := time.Now()
	if got := len(l.Handle(chatPkt("u1", "닉", "ㅋ", -1), now)); got != 0 {
		t.Errorf("1글자 채팅이 적립됨 (%d건)", got)
	}
	if got := len(l.Handle(chatPkt("u1", "닉", "ㅋㅋ", -1), now)); got != 1 {
		t.Errorf("2글자 채팅이 적립 안 됨 (%d건)", got)
	}
}

func TestFanclub_적립비활성(t *testing.T) {
	l := newTestLive()
	now := time.Now()

	// 채팅에 팬클럽 주차가 실려 와도 채팅 달란트만 처리한다.
	for _, event := range l.Handle(chatPkt("newbie", "뉴비", "가입했어요", 1), now) {
		if event.Kind == KindFanclub {
			t.Fatal("팬가입 달란트가 생성됨")
		}
	}
	// 0127 상태 목록도 처리하지 않는다.
	if got := l.Handle(soop.Packet{Svc: soop.SvcFanclub}, now); len(got) != 0 {
		t.Fatalf("팬클럽 상태 패킷에서 이벤트 %d건 생성됨", len(got))
	}
}

func TestStay_주기마다적립(t *testing.T) {
	l := newTestLive()
	start := time.Date(2026, 8, 10, 21, 0, 0, 0, time.Local)

	// 입장
	l.Handle(soop.Packet{Svc: soop.SvcEnter, Parts: []string{
		"", "flag", "viewer1", "시청자", "0", "0", "flag", ""}}, start)

	// 실제 방송에서는 시청자 목록이 계속 내려온다. 30초마다 관측되는 상황을 재현한다.
	// (유예 120초를 넘겨 신호가 끊기면 세션이 만료되는 것이 정상 동작이다)
	viewerList := soop.Packet{Svc: soop.SvcViewerList, Parts: []string{
		"", "1", "viewer1", "시청자", "589856|163840", ""}}

	// 5분 뒤엔 아직 한 주기(10분)를 못 채웠다
	for t := 30 * time.Second; t <= 5*time.Minute; t += 30 * time.Second {
		l.Handle(viewerList, start.Add(t))
	}
	if got := len(l.CollectStayTicks(start.Add(5 * time.Minute))); got != 0 {
		t.Errorf("5분에 적립됨 (%d건)", got)
	}

	// 계속 보이는 상태로 25분 경과 → 2회분
	mid := start.Add(25 * time.Minute)
	for t := 5*time.Minute + 30*time.Second; t <= 25*time.Minute; t += 30 * time.Second {
		l.Handle(viewerList, start.Add(t))
	}

	events := l.CollectStayTicks(mid)
	if len(events) != 2 {
		t.Fatalf("25분 경과 적립 = %d건, want 2", len(events))
	}
	if events[0].IdemKey == events[1].IdemKey {
		t.Error("멱등키가 중복됨 — 재실행 시 한 건만 저장된다")
	}

	// 같은 시각에 다시 정산해도 추가 적립이 없어야 한다
	if got := len(l.CollectStayTicks(mid)); got != 0 {
		t.Errorf("재정산에 중복 적립 (%d건)", got)
	}
}

// 0004에는 목록형(parts[1]="1")과 단일갱신형(parts[1]="-1")이 섞여 있다.
// 단일갱신을 3칸씩 훑으면 parts[5]의 "-1"을 아이디로 읽어
// 존재하지 않는 시청자에게 달란트가 쌓인다. 실제로 발생했던 사고다.
func TestViewerList_단일갱신형에서유령유저금지(t *testing.T) {
	l := newTestLive()
	now := time.Now()

	// 2026-08-11 천양 방송에서 실제로 받은 패킷
	l.Handle(soop.Packet{Svc: soop.SvcViewerList, Parts: []string{
		"", "-1", "nhw486", "달비날", "1", "-1", "537477152|33554432", ""}}, now)

	if _, ok := l.seen["-1"]; ok {
		t.Error(`"-1"이 시청자로 기록됨 — 유령 유저에게 달란트가 나간다`)
	}
	if _, ok := l.seen["nhw486"]; !ok {
		t.Error("실제 시청자 nhw486이 기록되지 않음")
	}
	if len(l.seen) != 1 {
		t.Errorf("시청자 %d명 기록됨, want 1: %v", len(l.seen), keysOf(l.seen))
	}
}

func TestViewerList_목록형(t *testing.T) {
	l := newTestLive()
	now := time.Now()

	l.Handle(soop.Packet{Svc: soop.SvcViewerList, Parts: []string{
		"", "1",
		"gufalqk789(2)", "mune7026", "65568|163840",
		"rltjddl5789(2)", "BAD.WOLF", "2684420128|4243456",
		"jh00429", "쪄먹는고구마", "65536|163840",
		""}}, now)

	want := []string{"gufalqk789", "rltjddl5789", "jh00429"} // 동시접속 표기는 제거된다
	if len(l.seen) != len(want) {
		t.Fatalf("시청자 %d명, want %d: %v", len(l.seen), len(want), keysOf(l.seen))
	}
	for _, id := range want {
		if _, ok := l.seen[id]; !ok {
			t.Errorf("%s 가 기록되지 않음", id)
		}
	}
}

// 정렬이 어긋난 패킷을 만나면 그 지점에서 멈춰야 한다.
// 계속 읽으면 닉네임을 아이디로 오인한다.
func TestViewerList_정렬깨지면중단(t *testing.T) {
	l := newTestLive()
	l.Handle(soop.Packet{Svc: soop.SvcViewerList, Parts: []string{
		"", "1",
		"realuser", "정상닉", "65568|163840",
		"someid", "깨진닉", "플래그아님", // 여기서 멈춰야 한다
		"뒤에오는닉", "이건아이디아님", "65568|163840",
		""}}, time.Now())

	if _, ok := l.seen["realuser"]; !ok {
		t.Error("정렬이 맞는 앞부분이 기록되지 않음")
	}
	if _, ok := l.seen["뒤에오는닉"]; ok {
		t.Error("정렬이 깨진 뒤의 값을 아이디로 읽음")
	}
}

func keysOf(m map[string]*presence) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStay_유예초과시세션종료(t *testing.T) {
	l := newTestLive()
	start := time.Now()
	l.Handle(soop.Packet{Svc: soop.SvcEnter, Parts: []string{
		"", "flag", "leaver", "떠난이", "0", "0", "flag", ""}}, start)

	// 유예(120초)를 크게 넘겨 정산 → 세션 정리, 적립 없음
	if got := len(l.CollectStayTicks(start.Add(30 * time.Minute))); got != 0 {
		t.Errorf("사라진 시청자에게 적립됨 (%d건)", got)
	}
	if len(l.seen) != 0 {
		t.Errorf("세션이 정리되지 않음 (%d개 남음)", len(l.seen))
	}
}

func TestDonation_아이템별환산(t *testing.T) {
	l := newTestLive()
	now := time.Now()

	star := soop.Packet{Svc: soop.SvcStarBalloon, Parts: []string{
		"", "243000", "hammins", "환승710", "10", "0", "0", "565", "10", ""}}
	events := l.Handle(star, now)
	if len(events) != 1 {
		t.Fatalf("별풍선 적립 = %d건", len(events))
	}
	want := 10 * DefaultRules().Donation.StarBalloonRate
	if events[0].Amount != want {
		t.Errorf("별풍선 달란트 = %d, want %d", events[0].Amount, want)
	}
}
