package txn

import "testing"

func TestParseSpaceBytes(t *testing.T) {
	tests := map[string]int64{
		"0":         0,
		"unlimited": 0,
		"1KiB":      1024,
		"1.5GiB":    1610612736,
		"2GB":       2000000000,
		"42":        42,
	}
	for input, expected := range tests {
		got, err := ParseSpaceBytes(input)
		if err != nil {
			t.Errorf("ParseSpaceBytes(%q): %v", input, err)
		} else if got != expected {
			t.Errorf("ParseSpaceBytes(%q)=%d，期望 %d", input, got, expected)
		}
	}
	for _, input := range []string{"", "-1GiB", "abc", "NaN"} {
		if _, err := ParseSpaceBytes(input); err == nil {
			t.Errorf("ParseSpaceBytes(%q) 应失败", input)
		}
	}
}
