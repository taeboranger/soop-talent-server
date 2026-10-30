package shop

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOrdersXLSX_유효한엑셀과안전한문자열(t *testing.T) {
	at := time.Date(2026, 8, 29, 14, 30, 0, 0, time.UTC)
	data, err := OrdersXLSX(&Page{Title: "8월 달란트 시장"}, []Order{
		{
			UserID: "=HYPERLINK(\"https://bad.example\")", Nickname: "+수식처럼 보이는 닉네임",
			TotalPrice: 120, Status: "fulfilled", OrderedAt: at,
			Lines: []Line{{ItemName: "포토카드", Quantity: 2, UnitPrice: 60}},
		},
		{
			UserID: "viewer2", Nickname: "시청자", Status: "cancelled",
			TotalPrice: 50, OrderedAt: at, RejectReason: "시장 취소로 전액 환불됨",
			Lines: []Line{{ItemName: "=보상", Quantity: 1, UnitPrice: 50}},
		},
	})
	if err != nil {
		t.Fatalf("OrdersXLSX: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("빈 XLSX를 만들었습니다")
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("유효한 ZIP/XLSX가 아닙니다: %v", err)
	}
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	for _, name := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/styles.xml", "xl/worksheets/sheet1.xml",
	} {
		if entries[name] == nil {
			t.Errorf("필수 XLSX 항목이 없습니다: %s", name)
		}
	}

	r, err := entries["xl/worksheets/sheet1.xml"].Open()
	if err != nil {
		t.Fatal(err)
	}
	sheet, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	xml := string(sheet)
	for _, want := range []string{"구매 내역", "결제 달란트", "환불 완료", "포토카드 x2", "A5:G7"} {
		if !strings.Contains(xml, want) {
			t.Errorf("시트에 %q가 없습니다:\n%s", want, xml)
		}
	}
	// 외부 입력이 Excel 수식으로 실행되지 않도록 텍스트 접두사를 넣는다.
	// XML 안에서는 작은따옴표가 &#39;로 이스케이프된다.
	if !strings.Contains(xml, "&#39;=HYPERLINK") || !strings.Contains(xml, "&#39;+수식처럼") || !strings.Contains(xml, "&#39;=보상") {
		t.Errorf("수식 인젝션 방어 접두사가 없습니다:\n%s", xml)
	}
}

func TestOrdersXLSX_페이지없음(t *testing.T) {
	if _, err := OrdersXLSX(nil, nil); err == nil {
		t.Fatal("페이지가 없을 때 오류가 필요합니다")
	}
}

func TestOrdersXLSX_CustomRewardUnit(t *testing.T) {
	data, err := OrdersXLSX(&Page{Title: "달란트가 들어간 사용자 제목"}, nil, "달러")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			sheet, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(sheet), "결제 달러") || !strings.Contains(string(sheet), "달란트가 들어간 사용자 제목") {
				t.Fatal("unit or user title changed incorrectly")
			}
		}
	}
}
