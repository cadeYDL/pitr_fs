package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hanwen/go-fuse/v2/fuse"

	"pitr_fs/internal/txn"
)

const (
	contentCaptureLimit = 4096
	contentDisplayRunes = 12
	processDisplayRunes = 10
	maxWriteSamples     = 3
	hostPasswdPath      = "/host/etc/passwd"
)

type processCacheEntry struct {
	command   string
	expiresAt time.Time
}

type contentPreview struct {
	exists bool
	kind   string
	data   []byte
	size   int64
}

type writeSample struct {
	offset int64
	length int
	calls  int
	before []byte
	after  []byte
}

func shortenRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	return truncateRunes(value, limit)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func (l *Loopback) processCommand(pid uint32) string {
	if pid == 0 {
		return ""
	}
	now := time.Now()
	l.auditMu.Lock()
	if cached, ok := l.processCache[pid]; ok && now.Before(cached.expiresAt) {
		l.auditMu.Unlock()
		return cached.command
	}
	l.auditMu.Unlock()

	command := ""
	file, err := os.Open(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err == nil {
		content, readErr := io.ReadAll(io.LimitReader(file, 512))
		_ = file.Close()
		if readErr == nil {
			command = strings.TrimSpace(strings.ReplaceAll(
				string(content), "\x00", " "))
		}
	}
	if command == "" {
		content, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if readErr == nil {
			command = strings.TrimSpace(string(content))
		}
	}
	command = shortenRunes(command, processDisplayRunes)
	l.auditMu.Lock()
	l.processCache[pid] = processCacheEntry{
		command: command, expiresAt: now.Add(time.Second),
	}
	l.auditMu.Unlock()
	return command
}

func (l *Loopback) actorName(uid uint32) string {
	l.auditMu.Lock()
	if cached, ok := l.userCache[uid]; ok {
		l.auditMu.Unlock()
		return cached
	}
	l.auditMu.Unlock()
	name := ""
	if found, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
		name = found.Username
	}
	if name == "" {
		if content, err := os.ReadFile(hostPasswdPath); err == nil {
			name = lookupPasswdName(string(content), uid)
		}
	}
	l.auditMu.Lock()
	l.userCache[uid] = name
	l.auditMu.Unlock()
	return name
}

func lookupPasswdName(content string, uid uint32) string {
	uidText := strconv.FormatUint(uint64(uid), 10)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.SplitN(line, ":", 7)
		if len(fields) >= 3 && fields[2] == uidText {
			return fields[0]
		}
	}
	return ""
}

func (l *Loopback) versionMetadata(
	ctx context.Context,
	posixOp string,
) txn.VersionMetadata {
	metadata := txn.VersionMetadata{PosixOp: posixOp}
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return metadata
	}
	metadata.ActorUID = int64(caller.Uid)
	metadata.ActorGID = int64(caller.Gid)
	metadata.ActorPID = int64(caller.Pid)
	metadata.ActorName = l.actorName(caller.Uid)
	metadata.ProcessCommand = l.processCommand(caller.Pid)
	return metadata
}

func (l *Loopback) backendPath(visible string) (string, bool) {
	relative, err := filepath.Rel(l.Mount, filepath.Clean(visible))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.Join(l.Backend, relative), true
}

func (l *Loopback) captureContent(visible string) contentPreview {
	lower, ok := l.backendPath(visible)
	if !ok {
		return contentPreview{}
	}
	info, err := os.Lstat(lower)
	if errors.Is(err, os.ErrNotExist) {
		return contentPreview{}
	}
	if err != nil {
		return contentPreview{exists: true, kind: "unreadable"}
	}
	preview := contentPreview{exists: true, size: info.Size()}
	switch {
	case info.IsDir():
		preview.kind = "directory"
		return preview
	case info.Mode()&os.ModeSymlink != 0:
		preview.kind = "symlink"
		target, readErr := os.Readlink(lower)
		if readErr == nil {
			preview.data = []byte(target)
		}
		return preview
	case info.Mode().IsRegular():
		preview.kind = "file"
	default:
		preview.kind = "special"
		return preview
	}
	file, err := os.Open(lower)
	if err != nil {
		preview.kind = "unreadable"
		return preview
	}
	defer file.Close()
	preview.data, _ = io.ReadAll(io.LimitReader(file, contentCaptureLimit))
	return preview
}

func (l *Loopback) sampleWrite(
	visible string,
	data []byte,
	offset int64,
) writeSample {
	limit := len(data)
	if limit > 64 {
		limit = 64
	}
	sample := writeSample{
		offset: offset,
		length: len(data),
		calls:  1,
		after:  append([]byte(nil), data[:limit]...),
	}
	lower, ok := l.backendPath(visible)
	if !ok || limit == 0 {
		return sample
	}
	file, err := os.Open(lower)
	if err != nil {
		return sample
	}
	defer file.Close()
	before := make([]byte, limit)
	n, _ := file.ReadAt(before, offset)
	sample.before = before[:n]
	return sample
}

func (f *trackedFile) setAuditState(
	path string,
	posixOp string,
	before contentPreview,
) {
	f.auditMu.Lock()
	f.path = path
	f.posixOp = posixOp
	f.before = before
	f.samples = nil
	f.auditMu.Unlock()
}

func (f *trackedFile) addWriteSample(sample writeSample) {
	f.auditMu.Lock()
	defer f.auditMu.Unlock()
	if len(f.samples) < maxWriteSamples {
		f.samples = append(f.samples, sample)
		return
	}
	// 超过采样上限后仍累计总写入量，避免日志低报。
	f.samples[len(f.samples)-1].length += sample.length
	f.samples[len(f.samples)-1].calls += sample.calls
}

func (f *trackedFile) auditState() (
	string,
	string,
	contentPreview,
	[]writeSample,
) {
	f.auditMu.Lock()
	defer f.auditMu.Unlock()
	samples := append([]writeSample(nil), f.samples...)
	return f.path, f.posixOp, f.before, samples
}

func printableText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, value := range string(data) {
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			return false
		}
	}
	return true
}

func previewValue(preview contentPreview) string {
	if !preview.exists {
		return "∅"
	}
	switch preview.kind {
	case "directory":
		return "<directory>"
	case "symlink":
		return fmt.Sprintf("symlink(%q)",
			truncateRunes(string(preview.data), contentDisplayRunes))
	case "file":
		if !printableText(preview.data) {
			return fmt.Sprintf("<binary,%dB>", preview.size)
		}
		return strconv.Quote(
			truncateRunes(string(preview.data), contentDisplayRunes))
	case "unreadable":
		return "<unreadable>"
	default:
		return "<special>"
	}
}

func sampleValue(data []byte) string {
	if len(data) == 0 {
		return `""`
	}
	if !printableText(data) {
		return fmt.Sprintf("<binary,%dB>", len(data))
	}
	return strconv.Quote(truncateRunes(string(data), contentDisplayRunes))
}

func summarizeContent(
	before, after contentPreview,
	samples []writeSample,
) string {
	if before.exists != after.exists || before.kind != after.kind ||
		before.size != after.size || !bytes.Equal(before.data, after.data) {
		return previewValue(before) + " -> " + previewValue(after)
	}
	if len(samples) != 0 {
		first := samples[0]
		if first.length > 0 && len(first.before) == 0 && len(first.after) == 0 {
			return fmt.Sprintf("@%d <not sampled> -> <changed,%dB>",
				first.offset, first.length)
		}
		return fmt.Sprintf("@%d %s -> %s",
			first.offset, sampleValue(first.before), sampleValue(first.after))
	}
	return "-"
}

func summarizeWriteOp(path, fallback string, samples []writeSample) string {
	if len(samples) == 0 {
		return fallback
	}
	var total int
	offset := samples[0].offset
	var calls int
	for _, sample := range samples {
		total += sample.length
		if sample.calls == 0 {
			calls++
		} else {
			calls += sample.calls
		}
		if sample.offset < offset {
			offset = sample.offset
		}
	}
	return fmt.Sprintf("write(%q, offset=%d, total=%d, calls=%d)",
		path, offset, total, calls)
}

func formatOpenFlags(flags uint32) string {
	parts := make([]string, 0, 8)
	switch flags & syscall.O_ACCMODE {
	case syscall.O_WRONLY:
		parts = append(parts, "O_WRONLY")
	case syscall.O_RDWR:
		parts = append(parts, "O_RDWR")
	default:
		parts = append(parts, "O_RDONLY")
	}
	for _, value := range []struct {
		flag uint32
		name string
	}{
		{syscall.O_APPEND, "O_APPEND"},
		{syscall.O_CREAT, "O_CREAT"},
		{syscall.O_EXCL, "O_EXCL"},
		{syscall.O_TRUNC, "O_TRUNC"},
		{syscall.O_SYNC, "O_SYNC"},
	} {
		if flags&value.flag != 0 {
			parts = append(parts, value.name)
		}
	}
	return strings.Join(parts, "|")
}
