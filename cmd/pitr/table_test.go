package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteAlignedTableUsesTerminalDisplayWidth(t *testing.T) {
	rows := [][]string{
		{"配置项", "当前值", "说明"},
		{"retention", "compact", "英文键"},
		{"空间上限", "100.00 GiB", "中文键"},
	}
	var output bytes.Buffer
	if err := writeAlignedTable(&output, rows); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("行数=%d，输出：\n%s", len(lines), output.String())
	}
	for column := 1; column < 3; column++ {
		wantStart := -1
		for rowIndex, line := range lines {
			cell := rows[rowIndex][column]
			byteIndex := strings.Index(line, cell)
			if byteIndex < 0 {
				t.Fatalf("第 %d 行缺少 %q：%q", rowIndex, cell, line)
			}
			start := terminalDisplayWidth(line[:byteIndex])
			if wantStart == -1 {
				wantStart = start
			} else if start != wantStart {
				t.Fatalf("第 %d 列未左对齐：want=%d got=%d，输出：\n%s",
					column, wantStart, start, output.String())
			}
		}
	}
}

func TestWriteAlignedTableEscapesEmbeddedControls(t *testing.T) {
	var output bytes.Buffer
	if err := writeAlignedTable(&output, [][]string{{"字段", "值"},
		{"操作", "a\tb\nc\rd"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `a\tb\nc\rd`) {
		t.Fatalf("控制字符未转义：%q", output.String())
	}
}
