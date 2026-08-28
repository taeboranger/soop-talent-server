package auth

import (
	"errors"
	"testing"

	"github.com/sooptalent/server/internal/apperr"
)

// 검증 오류는 apperr.Invalid여야 한다.
// 그렇지 않으면 HTTP 계층이 500으로 내보내고, 사용자는 무엇을 고쳐야 할지 알 수 없다.
func TestValidationErrorsAreInvalid(t *testing.T) {
	var inv apperr.Invalid
	for name, err := range map[string]error{
		"ErrBadCredentials": ErrBadCredentials,
		"ErrEmailTaken":     ErrEmailTaken,
		"ErrWeakPassword":   ErrWeakPassword,
		"ErrBidTaken":       ErrBidTaken,
		"ErrClaimExists":    ErrClaimExists,
		"ErrBadState":       ErrBadState,
	} {
		if !errors.As(err, &inv) {
			t.Errorf("%s: apperr.Invalid가 아님 — 500으로 나갈 것", name)
		}
	}
}

func TestEmailValidation(t *testing.T) {
	ok := []string{
		"user@example.com", "a.b+c@sub.example.co.kr", "  User@Example.COM  ",
	}
	for _, e := range ok {
		if !emailRe.MatchString(NormalizeEmail(e)) {
			t.Errorf("%q: 통과해야 함", e)
		}
	}

	bad := []string{
		"", "@", "notanemail", "user@", "@example.com",
		"user@example", // TLD 없음
		"user example@x.com", "user@exam ple.com", "user@@example.com",
	}
	for _, e := range bad {
		if emailRe.MatchString(NormalizeEmail(e)) {
			t.Errorf("%q: 거부해야 함", e)
		}
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("짧은 비밀번호가 통과함: %v", err)
	}
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("해시 생성: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Error("맞는 비밀번호가 거부됨")
	}
	if VerifyPassword(hash, "wrong password here") {
		t.Error("틀린 비밀번호가 통과함")
	}
	// 손상된 해시로 패닉이 나면 안 된다
	for _, broken := range []string{"", "$argon2id$", "plain", "$argon2id$v=19$bad$x$y"} {
		if VerifyPassword(broken, "anything") {
			t.Errorf("손상된 해시 %q 가 통과함", broken)
		}
	}
}

func TestProfileImageURL(t *testing.T) {
	if got, want := (Account{SoopID: "lightstone1"}).ProfileImageURL(),
		"https://profile.img.sooplive.com/LOGO/li/lightstone1/lightstone1.jpg"; got != want {
		t.Fatalf("프로필 주소 = %q, want %q", got, want)
	}
	if got := (Account{}).ProfileImageURL(); got != "" {
		t.Fatalf("빈 SOOP ID의 프로필 주소 = %q", got)
	}
}
