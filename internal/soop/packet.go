package soop

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SOOP 채팅 패킷 구조 (실측 확인)
//
//	ESC(0x1b) TAB(0x09) svc(4자리) bodyLen(6자리) "00" body
//
// body의 필드 구분자는 0x0c이며 body 앞뒤에도 붙는다.
// 따라서 Split 결과의 첫 원소와 마지막 원소는 항상 빈 문자열이다.
const (
	esc       = 0x1b
	tab       = 0x09
	sep       = 0x0c
	headerLen = 14 // ESC TAB + svc(4) + len(6) + reserved(2)
)

// 서비스 코드. 실측으로 의미가 확인된 것만 이름을 붙였다.
const (
	SvcPing       = "0000"
	SvcConnect    = "0001"
	SvcJoin       = "0002"
	SvcViewerList = "0004"
	SvcChat       = "0005"
	SvcEnter       = "0012"
	SvcExit        = "0013"
	SvcStarBalloon = "0018" // 별풍선 후원
	SvcSubscribe   = "0093" // 구독 (추정 — 아래 ParseSubscribe 주석 참조)
	SvcNotice      = "0104"
	SvcAdBalloon   = "0087" // 애드벌룬 후원
	SvcEmoticon    = "0109"
	SvcFanclub     = "0127"
)

// KnownSvc는 의미가 확인된 코드 집합이다.
// 여기 없는 코드는 raw_chat_events에 원본 저장되어 나중에 소급 처리된다.
var KnownSvc = map[string]bool{
	SvcPing: true, SvcConnect: true, SvcJoin: true, SvcViewerList: true,
	SvcChat: true, SvcEnter: true, SvcExit: true, SvcStarBalloon: true,
	SvcNotice: true, SvcAdBalloon: true, SvcEmoticon: true, SvcFanclub: true,
}

var ErrShortPacket = errors.New("soop: 패킷이 헤더보다 짧음")

// Packet은 수신한 원본 프레임을 파싱한 결과다.
type Packet struct {
	Svc     string
	Parts   []string // body를 0x0c로 분리한 결과 (앞뒤 빈 문자열 포함)
	Raw     []byte
	BroadNo string // 수신 시점의 방송 번호 (수집기가 채워 넣는다)
}

// Field는 범위를 벗어난 인덱스에 대해 빈 문자열을 돌려준다.
// 프로토콜이 문서화돼 있지 않으므로 길이 가정을 하지 않는다.
func (p Packet) Field(i int) string {
	if i < 0 || i >= len(p.Parts) {
		return ""
	}
	return p.Parts[i]
}

// Encode는 서비스 코드와 body 필드들로 프레임을 만든다.
// 필드 사이·앞뒤에 구분자를 넣는 규칙은 Decode와 대칭이다.
func Encode(svc string, fields ...string) []byte {
	var body bytes.Buffer
	for _, f := range fields {
		body.WriteByte(sep)
		body.WriteString(f)
	}
	body.WriteByte(sep)

	out := make([]byte, 0, headerLen+body.Len())
	out = append(out, esc, tab)
	out = append(out, []byte(fmt.Sprintf("%04s", svc))...)
	out = append(out, []byte(fmt.Sprintf("%06d", body.Len()))...)
	out = append(out, '0', '0')
	return append(out, body.Bytes()...)
}

// Decode는 수신 프레임을 파싱한다.
// 선언된 길이와 실제 길이가 어긋나도 실제 바이트를 신뢰한다 —
// 서버가 보내는 길이 필드가 항상 정확하지는 않다.
func Decode(frame []byte) (Packet, error) {
	if len(frame) < headerLen {
		return Packet{}, ErrShortPacket
	}
	svc := string(frame[2:6])
	body := frame[headerLen:]

	parts := make([]string, 0, 8)
	for _, seg := range bytes.Split(body, []byte{sep}) {
		parts = append(parts, string(seg))
	}
	return Packet{Svc: svc, Parts: parts, Raw: frame}, nil
}

// 핸드셰이크 프레임. 값은 실측한 브라우저 동작을 그대로 옮긴 것이다.
func connectFrame() []byte      { return Encode(SvcConnect, "", "16", "", "") }
func joinFrame(chatNo string) []byte {
	return Encode(SvcJoin, chatNo, "", "", "", "")
}
func pingFrame() []byte { return Encode(SvcPing) }

// flagField는 시청자 목록의 세 번째 칸 형태다. 예: "589856|163840"
var flagField = regexp.MustCompile(`^\d+\|\d+$`)

// IsFlagField는 그 칸이 플래그인지 본다.
// 목록을 3칸씩 훑을 때 정렬이 맞는지 확인하는 데 쓴다.
func IsFlagField(s string) bool { return flagField.MatchString(s) }

// IsUserID는 유저 아이디로 쓸 수 있는 값인지 본다.
//
// SOOP 아이디는 숫자만으로도 존재하므로(예: 243000) 숫자를 배제할 수 없다.
// 다만 음수나 빈 값은 프로토콜의 제어 값이지 계정이 아니다.
func IsUserID(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	return !strings.ContainsAny(s, "|")
}

// NormalizeUserID는 동시 접속 표기를 제거한다.
// SOOP은 같은 계정이 여러 번 접속하면 "userid(2)" 형태로 보낸다.
// 달란트 적립은 계정 단위이므로 반드시 정규화해야 한다.
func NormalizeUserID(id string) string {
	if i := bytes.IndexByte([]byte(id), '('); i > 0 {
		if j := bytes.IndexByte([]byte(id[i:]), ')'); j > 0 {
			if _, err := strconv.Atoi(id[i+1 : i+j]); err == nil {
				return id[:i]
			}
		}
	}
	return id
}
