package proxy

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
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

func (n *Node) protectedChild(name string) bool {
	return n != nil && n.root != nil && n.root.hiddenRootName != "" &&
		n.root.rootNode == n && name == n.root.hiddenRootName
}

func (n *Node) Lookup(
	ctx context.Context,
	name string,
	out *fuse.EntryOut,
) (*fs.Inode, syscall.Errno) {
	if n.protectedChild(name) {
		return nil, syscall.ENOENT
	}
	return n.LoopbackNode.Lookup(ctx, name, out)
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	stream, errno := n.LoopbackNode.Readdir(ctx)
	if errno != 0 || n.root == nil || n.root.hiddenRootName == "" ||
		n.root.rootNode != n {
		return stream, errno
	}
	defer stream.Close()
	entries := make([]fuse.DirEntry, 0)
	for stream.HasNext() {
		entry, nextErrno := stream.Next()
		if nextErrno != 0 {
			return nil, nextErrno
		}
		if entry.Name != n.root.hiddenRootName {
			entries = append(entries, entry)
		}
	}
	return fs.NewListDirStream(entries), 0
}

func (n *Node) OpendirHandle(
	ctx context.Context,
	_ uint32,
) (fs.FileHandle, uint32, syscall.Errno) {
	stream, errno := n.Readdir(ctx)
	if errno != 0 {
		return nil, 0, errno
	}
	return stream, 0, 0
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
