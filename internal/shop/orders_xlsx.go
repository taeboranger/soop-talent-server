package shop

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const XLSXContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// OrdersXLSX는 시장의 구매 내역을 Excel 통합 문서로 만든다. 외부 라이브러리 없이
// 표준 XLSX(Open XML)만 작성해 서버 이미지 크기와 런타임 의존성을 늘리지 않는다.
func OrdersXLSX(page *Page, orders []Order) ([]byte, error) {
	if page == nil {
		return nil, fmt.Errorf("보상 페이지가 필요합니다")
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	// 순서를 고정하면 같은 주문 내역을 다시 내보낼 때 ZIP 구조까지 안정적이다.
	// (내보낸 시각은 달라질 수 있다.)
	files := []struct{ name, content string }{
		{"[Content_Types].xml", xlsxContentTypes},
		{"_rels/.rels", xlsxRootRels},
		{"xl/workbook.xml", xlsxWorkbook},
		{"xl/_rels/workbook.xml.rels", xlsxWorkbookRels},
		{"xl/styles.xml", xlsxStyles},
		{"xl/worksheets/sheet1.xml", ordersSheetXML(page, orders)},
	}
	for _, file := range files {
		name, content := file.name, file.content
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ordersSheetXML(page *Page, orders []Order) string {
	rows := make([]string, 0, len(orders)+5)
	rows = append(rows,
		xlsxRow(1, textCell("A1", page.Title+" 구매 내역", 1)),
		xlsxRow(2, textCell("A2", "내보낸 시각", 0), textCell("B2", time.Now().Format("2006-01-02 15:04:05"), 0)),
		xlsxRow(3, textCell("A3", "전체 주문 수", 0), numberCell("B3", len(orders), 2)),
		xlsxRow(5,
			textCell("A5", "구매 시각", 1), textCell("B5", "시청자 ID", 1), textCell("C5", "닉네임", 1),
			textCell("D5", "보상", 1), textCell("E5", "결제 달란트", 1), textCell("F5", "상태", 1), textCell("G5", "사유", 1)),
	)
	for i, order := range orders {
		r := i + 6
		at := order.OrderedAt
		if at.IsZero() && order.CommentedAt != nil {
			at = *order.CommentedAt
		}
		rewards := make([]string, 0, len(order.Lines))
		for _, line := range order.Lines {
			if line.Quantity > 1 {
				rewards = append(rewards, fmt.Sprintf("%s x%d", line.ItemName, line.Quantity))
			} else {
				rewards = append(rewards, line.ItemName)
			}
		}
		reason := order.RejectReason
		if order.Status == "cancelled" && reason == "" {
			reason = "시장 취소로 전액 환불됨"
		}
		rows = append(rows, xlsxRow(r,
			textCell(cellRef('A', r), at.Format("2006-01-02 15:04:05"), 0),
			textCell(cellRef('B', r), order.UserID, 0),
			textCell(cellRef('C', r), order.Nickname, 0),
			textCell(cellRef('D', r), strings.Join(rewards, ", "), 0),
			numberCell(cellRef('E', r), order.TotalPrice, 2),
			textCell(cellRef('F', r), orderStatusLabel(order.Status), 0),
			textCell(cellRef('G', r), reason, 0),
		))
	}
	lastRow := len(orders) + 5
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
 <sheetViews><sheetView workbookViewId="0"><pane ySplit="5" topLeftCell="A6" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>
 <cols><col min="1" max="1" width="20" customWidth="1"/><col min="2" max="2" width="20" customWidth="1"/><col min="3" max="3" width="18" customWidth="1"/><col min="4" max="4" width="34" customWidth="1"/><col min="5" max="5" width="15" customWidth="1"/><col min="6" max="6" width="14" customWidth="1"/><col min="7" max="7" width="32" customWidth="1"/></cols>
 <sheetData>%s</sheetData><autoFilter ref="A5:G%d"/>
</worksheet>`, strings.Join(rows, ""), lastRow)
}

func xlsxRow(n int, cells ...string) string {
	return fmt.Sprintf(`<row r="%d">%s</row>`, n, strings.Join(cells, ""))
}

func cellRef(column rune, row int) string { return string(column) + strconv.Itoa(row) }

func textCell(ref, value string, style int) string {
	// Excel은 =, +, -, @로 시작하는 외부 입력을 수식으로 해석한다. 주문자 이름·보상
	// 이름이 수식이 되는 CSV/XLSX 인젝션을 막기 위해 눈에 보이지 않는 접두사를 붙인다.
	if strings.HasPrefix(value, "=") || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "@") {
		value = "'" + value
	}
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return fmt.Sprintf(`<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, escaped.String())
}

func numberCell(ref string, value, style int) string {
	return fmt.Sprintf(`<c r="%s" s="%d"><v>%d</v></c>`, ref, style, value)
}

func orderStatusLabel(status string) string {
	switch status {
	case "fulfilled":
		return "확정"
	case "cancelled":
		return "환불 완료"
	case "rejected":
		return "거절"
	default:
		return status
	}
}

const xlsxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
 <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
 <Default Extension="xml" ContentType="application/xml"/>
 <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
 <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
 <Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
</Types>`

const xlsxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
 <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

const xlsxWorkbook = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
 <sheets><sheet name="구매 내역" sheetId="1" r:id="rId1"/></sheets>
</workbook>`

const xlsxWorkbookRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
 <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
 <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`

const xlsxStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
 <fonts count="2"><font><sz val="10"/><name val="Malgun Gothic"/></font><font><b/><sz val="10"/><name val="Malgun Gothic"/></font></fonts>
 <fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FF087FC2"/><bgColor indexed="64"/></patternFill></fill></fills>
 <borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>
 <cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
 <cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="1" borderId="0" xfId="0" applyFont="1" applyFill="1"/><xf numFmtId="3" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs>
</styleSheet>`
