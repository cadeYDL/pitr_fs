package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditShorteningAndOpenFlags(t *testing.T) {
	if got := shortenRunes("一二三四五六", 4); got != "一二三四..." {
		t.Fatalf("shortenRunes=%q", got)
	}
	if got := shortenRunes(" short ", 10); got != "short" {
		t.Fatalf("short value=%q", got)
	}
	flags := formatOpenFlags(0x1 | 0x40 | 0x200)
	if flags != "O_WRONLY|O_CREAT|O_TRUNC" {
		t.Fatalf("flags=%q", flags)
	}
	if got := lookupPasswdName(
		"root:x:0:0:root:/root:/bin/sh\nydl:x:501:501::/home/ydl:/bin/bash\n",
		501,
	); got != "ydl" {
		t.Fatalf("passwd name=%q", got)
	}
}

func TestAuditContentSummaryIsBounded(t *testing.T) {
	before := contentPreview{
		exists: true, kind: "file", data: []byte("v1sdadsadas-long"), size: 16,
	}
	after := contentPreview{
		exists: true, kind: "file", data: []byte("v2dasdas-more"), size: 13,
	}
	got := summarizeContent(before, after, nil)
	if got != `"v1sdadsadas-..." -> "v2dasdas-mor..."` {
		t.Fatalf("summary=%q", got)
	}
	binary := previewValue(contentPreview{
		exists: true, kind: "file", data: []byte{0, 1, 2}, size: 9000,
	})
	if binary != "<binary,9000B>" {
		t.Fatalf("binary=%q", binary)
	}
	if newline := previewValue(contentPreview{
		exists: true, kind: "file", data: []byte("v1sdadsadas\n"), size: 12,
	}); newline != `"v1sdadsadas\n"` {
		t.Fatalf("newline=%q", newline)
	}
}

func TestAuditWriteSampleAndAggregateOperation(t *testing.T) {
	samples := []writeSample{
		{offset: 4, length: 3, calls: 1, before: []byte("old"), after: []byte("new")},
		{offset: 10, length: 5, calls: 1, before: []byte("12345"), after: []byte("abcde")},
	}
	if got := summarizeContent(
		contentPreview{exists: true, kind: "file", data: []byte("same"), size: 4},
		contentPreview{exists: true, kind: "file", data: []byte("same"), size: 4},
		samples,
	); got != `@4 "old" -> "new"` {
		t.Fatalf("sample summary=%q", got)
	}
	if got := summarizeWriteOp("/data/a", "open", samples); got != `write("/data/a", offset=4, total=8, calls=2)` {
		t.Fatalf("write op=%q", got)
	}
}

func TestAuditSampleLimitStillCountsAllWrites(t *testing.T) {
	file := &trackedFile{}
	for index := 0; index < 5; index++ {
		file.addWriteSample(writeSample{
			offset: int64(index), length: 2, calls: 1,
			before: []byte("a"), after: []byte("b"),
		})
	}
	_, _, _, samples := file.auditState()
	if len(samples) != maxWriteSamples {
		t.Fatalf("samples=%d", len(samples))
	}
	if got := summarizeWriteOp("/data/a", "", samples); got != `write("/data/a", offset=0, total=10, calls=5)` {
		t.Fatalf("write op=%q", got)
	}
}

func TestAuditOpaqueCopySummary(t *testing.T) {
	got := summarizeContent(
		contentPreview{exists: true, kind: "file", data: []byte("head"), size: 10},
		contentPreview{exists: true, kind: "file", data: []byte("head"), size: 10},
		[]writeSample{{offset: 4096, length: 128, calls: 1}},
	)
	if got != "@4096 <not sampled> -> <changed,128B>" {
		t.Fatalf("copy summary=%q", got)
	}
}

func TestCaptureContentConfinedToBackendAndLimited(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	if err := os.Mkdir(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("a", contentCaptureLimit+100)
	if err := os.WriteFile(filepath.Join(backend, "large"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	loopback := &Loopback{Mount: "/mnt/data", Backend: backend}
	preview := loopback.captureContent("/mnt/data/large")
	if !preview.exists || preview.size != int64(len(content)) ||
		len(preview.data) != contentCaptureLimit {
		t.Fatalf("preview=%+v data=%d", preview, len(preview.data))
	}
	if escaped := loopback.captureContent("/mnt/other/large"); escaped.exists {
		t.Fatalf("mount 外路径不应读取: %+v", escaped)
	}
}
