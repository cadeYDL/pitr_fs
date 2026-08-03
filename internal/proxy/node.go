package proxy

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

type Node struct {
	fs.LoopbackNode
	root *Loopback
}

type trackedFile struct {
	*fs.LoopbackFile
	id        uint64
	writable  bool
	released  atomic.Bool
	discarded atomic.Bool
	mutated   atomic.Bool

	auditMu sync.Mutex
	path    string
	posixOp string
	before  contentPreview
	samples []writeSample
}

func tracked(f fs.FileHandle) (*trackedFile, bool) {
	value, ok := f.(*trackedFile)
	return value, ok
}

func (n *Node) Release(ctx context.Context, f fs.FileHandle) syscall.Errno {
	if file, ok := tracked(f); ok {
		return n.closeWritableWindow(ctx, file)
	}
	if releaser, ok := f.(fs.FileReleaser); ok {
		return releaser.Release(ctx)
	}
	return 0
}
