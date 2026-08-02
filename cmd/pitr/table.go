package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

const tableColumnGap = 2

func formatHistoryLimit(limit int64) string {
	if limit == -1 {
		return "-1（不限制）"
	}
	return fmt.Sprintf("%d", limit)
}

// terminalDisplayWidth 按常见 Linux 终端的单元格宽度计算文本长度：中文等
// East Asian Wide/Fullwidth 字符占两格，组合字符不单独占格。
func terminalDisplayWidth(value string) int {
	result := 0
	for _, r := range value {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) ||
			unicode.Is(unicode.Cf, r) {
			continue
		}
		switch width.LookupRune(r).Kind() {
		case width.EastAsianWide, width.EastAsianFullwidth:
			result += 2
		default:
			result++
		}
	}
	return result
}

func normalizeTableCell(value string) string {
	return strings.NewReplacer(
		"\t", "\\t",
		"\r", "\\r",
		"\n", "\\n",
	).Replace(value)
}

// writeAlignedTable 统一渲染 CLI 的字段型列表。它不依赖制表符宽度，因此列
// 起点不会随字段长度、中文字符或终端 tabstop 设置发生偏移。
func writeAlignedTable(output io.Writer, rows [][]string) error {
	if len(rows) == 0 {
		return nil
	}
	columnCount := 0
	for _, row := range rows {
		if len(row) > columnCount {
			columnCount = len(row)
		}
	}
	widths := make([]int, columnCount)
	normalized := make([][]string, len(rows))
	for rowIndex, row := range rows {
		normalized[rowIndex] = make([]string, len(row))
		for columnIndex, cell := range row {
			cell = normalizeTableCell(cell)
			normalized[rowIndex][columnIndex] = cell
			if cellWidth := terminalDisplayWidth(cell); cellWidth > widths[columnIndex] {
				widths[columnIndex] = cellWidth
			}
		}
	}
	for _, row := range normalized {
		for columnIndex, cell := range row {
			if _, err := io.WriteString(output, cell); err != nil {
				return err
			}
			if columnIndex+1 < len(row) {
				padding := widths[columnIndex] - terminalDisplayWidth(cell) +
					tableColumnGap
				if _, err := io.WriteString(output, strings.Repeat(" ", padding)); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(output); err != nil {
			return err
		}
	}
	return nil
}
