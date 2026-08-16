// Package apperr은 "사용자 입력이 잘못됐다"는 사실을 도메인 코드에서 표현한다.
//
// 이 표시가 없으면 HTTP 계층이 검증 실패와 진짜 서버 오류를 구분할 수 없어
// 전부 500으로 내보내게 된다. 사용자는 무엇을 고쳐야 하는지 알 수 없고,
// 로그에는 실제 장애와 단순 오타가 섞인다.
package apperr

import "fmt"

// Invalid는 400으로 매핑된다. Msg는 그대로 사용자에게 보인다.
type Invalid struct{ Msg string }

func (e Invalid) Error() string { return e.Msg }

// New는 사용자에게 보여 줄 문구로 검증 오류를 만든다.
func New(msg string) error { return Invalid{Msg: msg} }

func Newf(format string, args ...any) error {
	return Invalid{Msg: fmt.Sprintf(format, args...)}
}
