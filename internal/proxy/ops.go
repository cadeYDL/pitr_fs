package proxy

import (
	"context"
	"fmt"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func command(op, path string) string {
	return fmt.Sprintf("%s:%s", op, path)
}

func (n *Node) Create(
	ctx context.Context,
	name string,
	flags, mode uint32,
	out *fuse.EntryOut,
) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := n.root.visiblePath(n, name)
	var file *trackedFile
	errno = n.versionedHook(ctx, path, command("create", path), 0,
		func() syscall.Errno {
			var base fs.FileHandle
			inode, base, fuseFlags, errno = n.LoopbackNode.Create(
				ctx, name, flags, mode, out)
			if errno == 0 {
				file = n.root.newFile(base, flags|syscall.O_WRONLY)
				if file == nil {
					return syscall.EIO
				}
				fh = file
				// 可写句柄必须绕过上层内核页缓存,否则小写入可能在 auto
				// 窗口关闭后才下发到 JuiceFS,无法确定 history 归属。
				fuseFlags |= fuse.FOPEN_DIRECT_IO
			}
			return errno
		})
	return inode, fh, fuseFlags, errno
}

func (n *Node) Mkdir(
	ctx context.Context,
	name string,
	mode uint32,
	out *fuse.EntryOut,
) (inode *fs.Inode, errno syscall.Errno) {
	path := n.root.visiblePath(n, name)
	errno = n.versionedHook(ctx, path, command("mkdir", path), 0,
		func() syscall.Errno {
			inode, errno = n.LoopbackNode.Mkdir(ctx, name, mode, out)
			return errno
		})
	return inode, errno
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	path := n.root.visiblePath(n, name)
	return n.versionedHook(ctx, path, command("unlink", path), 0,
		func() syscall.Errno {
			return n.LoopbackNode.Unlink(ctx, name)
		})
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	path := n.root.visiblePath(n, name)
	return n.versionedHook(ctx, path, command("rmdir", path), 0,
		func() syscall.Errno {
			return n.LoopbackNode.Rmdir(ctx, name)
		})
}

// Rename 按目标路径选择 active txn。跨 scope 时归属 dst,与执行计划约定一致。
func (n *Node) Rename(
	ctx context.Context,
	name string,
	newParent fs.InodeEmbedder,
	newName string,
	flags uint32,
) syscall.Errno {
	destination, ok := newParent.(*Node)
	if !ok {
		return syscall.EXDEV
	}
	path := destination.root.visiblePath(destination, newName)
	return n.versionedHook(ctx, path, command("rename", path), 0,
		func() syscall.Errno {
			return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
		})
}

func (n *Node) Symlink(
	ctx context.Context,
	target, name string,
	out *fuse.EntryOut,
) (inode *fs.Inode, errno syscall.Errno) {
	path := n.root.visiblePath(n, name)
	errno = n.versionedHook(ctx, path, command("symlink", path), 0,
		func() syscall.Errno {
			inode, errno = n.LoopbackNode.Symlink(ctx, target, name, out)
			return errno
		})
	return inode, errno
}

func (n *Node) Link(
	ctx context.Context,
	target fs.InodeEmbedder,
	name string,
	out *fuse.EntryOut,
) (inode *fs.Inode, errno syscall.Errno) {
	path := n.root.visiblePath(n, name)
	errno = n.versionedHook(ctx, path, command("link", path), 0,
		func() syscall.Errno {
			inode, errno = n.LoopbackNode.Link(ctx, target, name, out)
			return errno
		})
	return inode, errno
}

func (n *Node) Open(
	ctx context.Context,
	flags uint32,
) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := n.root.visiblePath(n)
	open := func() syscall.Errno {
		var base fs.FileHandle
		base, fuseFlags, errno = n.LoopbackNode.Open(ctx, flags)
		if errno == 0 {
			file := n.root.newFile(base, flags)
			if file == nil {
				return syscall.EIO
			}
			fh = file
			if file.writable {
				fuseFlags |= fuse.FOPEN_DIRECT_IO
			}
		}
		return errno
	}
	if flags&(syscall.O_TRUNC|syscall.O_CREAT) == 0 {
		errno = open()
		return fh, fuseFlags, errno
	}

	errno = n.versionedHook(ctx, path, command("open", path), 0, open)
	return fh, fuseFlags, errno
}

func (n *Node) Write(
	ctx context.Context,
	f fs.FileHandle,
	data []byte,
	off int64,
) (written uint32, errno syscall.Errno) {
	file, ok := tracked(f)
	if !ok {
		return 0, syscall.EBADF
	}
	path := n.root.visiblePath(n)
	errno = n.versionedHook(ctx, path, command("write", path), file.id,
		func() syscall.Errno {
			written, errno = file.LoopbackFile.Write(ctx, data, off)
			return errno
		})
	return written, errno
}

func (n *Node) Setattr(
	ctx context.Context,
	f fs.FileHandle,
	in *fuse.SetAttrIn,
	out *fuse.AttrOut,
) syscall.Errno {
	path := n.root.visiblePath(n)
	op := "setattr"
	if _, ok := in.GetSize(); ok {
		op = "truncate"
	}
	var fd uint64
	if file, ok := tracked(f); ok {
		fd = file.id
	}
	return n.versionedHook(ctx, path, command(op, path), fd,
		func() syscall.Errno {
			return n.LoopbackNode.Setattr(ctx, f, in, out)
		})
}

func (n *Node) Setxattr(
	ctx context.Context,
	attr string,
	data []byte,
	flags uint32,
) syscall.Errno {
	path := n.root.visiblePath(n)
	return n.versionedHook(ctx, path, command("setxattr", path), 0,
		func() syscall.Errno {
			return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
		})
}

func (n *Node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	path := n.root.visiblePath(n)
	return n.versionedHook(ctx, path, command("removexattr", path), 0,
		func() syscall.Errno {
			return n.LoopbackNode.Removexattr(ctx, attr)
		})
}

func (n *Node) Allocate(
	ctx context.Context,
	f fs.FileHandle,
	off, size uint64,
	mode uint32,
) syscall.Errno {
	file, ok := tracked(f)
	if !ok {
		return syscall.EBADF
	}
	path := n.root.visiblePath(n)
	return n.versionedHook(ctx, path, command("fallocate", path), file.id,
		func() syscall.Errno {
			return file.LoopbackFile.Allocate(ctx, off, size, mode)
		})
}

// Flush 为已有变更的 close 提供兜底打点。只有该 fd 已经由 Create、Write、
// Setattr 或 Allocate 关联 auto 时才重开窗口,避免只读/空写 close 产生空版本。
// 一期的可写句柄使用 direct I/O,因此 writable mmap 会由内核明确拒绝。
func (n *Node) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	file, ok := tracked(f)
	if !ok {
		if flusher, ok := f.(fs.FileFlusher); ok {
			return flusher.Flush(ctx)
		}
		return 0
	}
	if !file.writable {
		return file.LoopbackFile.Flush(ctx)
	}
	if _, versioned := n.root.loadFD(file.id); !versioned {
		return file.LoopbackFile.Flush(ctx)
	}
	path := n.root.visiblePath(n)
	return n.versionedHook(ctx, path, command("flush", path), file.id,
		func() syscall.Errno {
			return file.LoopbackFile.Flush(ctx)
		})
}

func (n *Node) CopyFileRange(
	ctx context.Context,
	fhIn fs.FileHandle,
	offIn uint64,
	out *fs.Inode,
	fhOut fs.FileHandle,
	offOut, length, flags uint64,
) (count uint32, errno syscall.Errno) {
	source, sourceOK := tracked(fhIn)
	destination, destinationOK := tracked(fhOut)
	if !sourceOK || !destinationOK {
		return 0, syscall.EBADF
	}
	destinationNode, ok := out.Operations().(*Node)
	if !ok {
		return 0, syscall.EXDEV
	}
	path := destinationNode.root.visiblePath(destinationNode)
	errno = destinationNode.versionedHook(
		ctx, path, command("copy_file_range", path), destination.id,
		func() syscall.Errno {
			count, errno = n.LoopbackNode.CopyFileRange(
				ctx,
				source.LoopbackFile,
				offIn,
				out,
				destination.LoopbackFile,
				offOut,
				length,
				flags,
			)
			return errno
		})
	return count, errno
}
