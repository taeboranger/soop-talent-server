package shop

import (
	"strings"
	"testing"
)

// 댓글 파싱은 달란트 차감의 근거다. 잘못 읽으면 남의 달란트가 사라진다.
func TestParseComment(t *testing.T) {
	cases := []struct {
		in   string
		want []OrderLine
	}{
		{"[구매신청] 사과", []OrderLine{{"사과", 1}}},
		{"[구매신청] 사과 x2", []OrderLine{{"사과", 2}}},
		{"[구매신청] 사과 x2, 배 x1", []OrderLine{{"사과", 2}, {"배", 1}}},
		{"[구매신청] 사과 X3", []OrderLine{{"사과", 3}}},
		{"[구매신청] 사과 ×3", []OrderLine{{"사과", 3}}},
		{"[구매신청] 사과x10", []OrderLine{{"사과", 10}}},
		// 앞뒤 공백과 줄바꿈을 허용한다
		{"  [구매신청]  사과 x2 ,  배  ", []OrderLine{{"사과", 2}, {"배", 1}}},
		{"[구매신청] 사과 x2\n배 x3", []OrderLine{{"사과", 2}, {"배", 3}}},
		// 이름 안의 x는 수량이 아니다 (끝에 있어야 수량으로 본다)
		{"[구매신청] xbox 게임기", []OrderLine{{"xbox 게임기", 1}}},
	}
	for _, c := range cases {
		got, ok := ParseComment(c.in)
		if !ok {
			t.Errorf("%q: 파싱 실패", c.in)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: 항목 수 %d, want %d (%+v)", c.in, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: [%d] = %+v, want %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// 일반 댓글이 구매로 오인되면 안 된다.
func TestParseComment_무시(t *testing.T) {
	for _, s := range []string{
		"안녕하세요",
		"구매신청 사과",       // 대괄호 없음
		"저도 [구매신청] 할게요", // 접두사가 앞에 있어야 한다
		"[구매신청]",        // 항목 없음
		"[구매신청]   ",
		"",
	} {
		if _, ok := ParseComment(s); ok {
			t.Errorf("%q: 구매 신청으로 잘못 인식됨", s)
		}
	}
}

func TestFormatComment(t *testing.T) {
	got := FormatComment([]OrderLine{{"사과", 2}, {"배", 1}})
	want := "[구매신청] 사과 x2, 배"
	if got != want {
		t.Errorf("FormatComment = %q, want %q", got, want)
	}
	// 만든 문구를 되읽으면 원래 항목이 나와야 한다
	lines, ok := ParseComment(got)
	if !ok || len(lines) != 2 || lines[0].Quantity != 2 || lines[1].Quantity != 1 {
		t.Errorf("왕복 실패: %+v", lines)
	}
}

func TestParsePostURL(t *testing.T) {
	bid, no, err := ParsePostURL("https://www.sooplive.com/station/243000/post/203551799")
	if err != nil || bid != "243000" || no != 203551799 {
		t.Errorf("= %q, %d, %v", bid, no, err)
	}
	for _, bad := range []string{
		"", "https://www.sooplive.com/station/243000",
		"https://example.com/", "그냥 문자열",
	} {
		if _, _, err := ParsePostURL(bad); err == nil {
			t.Errorf("%q: 거부되어야 함", bad)
		}
	}
}

// 상품명 대조는 띄어쓰기·대소문자에 관대해야 한다.
// 시청자가 손으로 쓴 댓글도 인식되어야 하기 때문이다.
func TestNormalizeName(t *testing.T) {
	same := [][2]string{
		{"치킨 기프티콘", "치킨기프티콘"},
		{"Steam 게임", "steam게임"},
		{"  여백  많은   상품 ", "여백많은상품"},
	}
	for _, p := range same {
		if normalizeName(p[0]) != normalizeName(p[1]) {
			t.Errorf("%q 와 %q 가 다르게 인식됨", p[0], p[1])
		}
	}
	if normalizeName("사과") == normalizeName("사과주스") {
		t.Error("다른 상품이 같게 인식됨")
	}
}

func TestNewPublicToken_추측불가공유링크형식(t *testing.T) {
	a, err := newPublicToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPublicToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 43 || a == b || strings.ContainsAny(a, "+/=") {
		t.Fatalf("공유 토큰 형식이 올바르지 않음: %q", a)
	}
}

func TestCheckout_요청형식은DB보다먼저검사한다(t *testing.T) {
	// DB가 없어도 잘못된 사용자·멱등키·수량이 결제 경로로 들어가지 않는지 고정한다.
	for _, tc := range []struct {
		name, user, key string
		lines           []CheckoutLine
	}{
		{"잘못된 사용자", "-1", strings.Repeat("a", 24), []CheckoutLine{{ItemID: 1, Quantity: 1}}},
		{"짧은 요청키", "viewer", "short", []CheckoutLine{{ItemID: 1, Quantity: 1}}},
		{"빈 장바구니", "viewer", strings.Repeat("a", 24), nil},
		{"잘못된 수량", "viewer", strings.Repeat("a", 24), []CheckoutLine{{ItemID: 1, Quantity: 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Checkout(t.Context(), nil, "token", tc.user, "닉", tc.key, tc.lines); err == nil {
				t.Fatal("잘못된 결제 요청이 DB 접근 전 거부되지 않음")
			}
		})
	}
}
