package proxy

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

type Node struct {
	fs.LoopbackNode
	root *Loopback
}

type trackedFile struct {
	*fs.LoopbackFile
	id       uint64
	writable bool
}

func tracked(f fs.FileHandle) (*trackedFile, bool) {
	value, ok := f.(*trackedFile)
	return value, ok
}

func (n *Node) Release(ctx context.Context, f fs.FileHandle) syscall.Errno {
	if file, ok := tracked(f); ok {
		n.root.deleteFD(file.id)
		return file.LoopbackFile.Release(ctx)
	}
	if releaser, ok := f.(fs.FileReleaser); ok {
		return releaser.Release(ctx)
	}
	return 0
}
