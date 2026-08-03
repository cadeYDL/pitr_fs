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
	if n.protectedChild(name) {
		return nil, nil, 0, syscall.EPERM
	}
	path := n.root.visiblePath(n, name)
	var file *trackedFile
	posixOp := fmt.Sprintf("open(%q, %s, %#o)",
		path, formatOpenFlags(flags|syscall.O_CREAT), mode&0o7777)
	errno = n.versionedHook(ctx, path, command("create", path), posixOp, 0,
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
				if directWrite(flags) {
					// 普通 O_WRONLY 句柄绕过上层页缓存,防止小写入在
					// active txn 结束后才下发到 JuiceFS。
					fuseFlags |= fuse.FOPEN_DIRECT_IO
				}
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
	if n.protectedChild(name) {
		return nil, syscall.EPERM
	}
	path := n.root.visiblePath(n, name)
	posixOp := fmt.Sprintf("mkdir(%q, %#o)", path, mode)
	errno = n.versionedHook(ctx, path, command("mkdir", path), posixOp, 0,
		func() syscall.Errno {
			inode, errno = n.LoopbackNode.Mkdir(ctx, name, mode, out)
			return errno
		})
	return inode, errno
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	if n.protectedChild(name) {
		return syscall.EPERM
	}
	path := n.root.visiblePath(n, name)
	return n.versionedHook(ctx, path, command("unlink", path),
		fmt.Sprintf("unlink(%q)", path), 0,
		func() syscall.Errno {
			return n.LoopbackNode.Unlink(ctx, name)
		})
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if n.protectedChild(name) {
		return syscall.EPERM
	}
	path := n.root.visiblePath(n, name)
	return n.versionedHook(ctx, path, command("rmdir", path),
		fmt.Sprintf("rmdir(%q)", path), 0,
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
	if n.protectedChild(name) || destination.protectedChild(newName) {
		return syscall.EPERM
	}
	source := n.root.visiblePath(n, name)
	path := destination.root.visiblePath(destination, newName)
	posixOp := fmt.Sprintf("rename(%q, %q)", source, path)
	return n.versionedHook(ctx, path,
		fmt.Sprintf("rename:%s->%s", source, path), posixOp, 0,
		func() syscall.Errno {
			return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
		})
}

func (n *Node) Symlink(
	ctx context.Context,
	target, name string,
	out *fuse.EntryOut,
) (inode *fs.Inode, errno syscall.Errno) {
	if n.protectedChild(name) {
		return nil, syscall.EPERM
	}
	path := n.root.visiblePath(n, name)
	posixOp := fmt.Sprintf("symlink(%q, %q)", target, path)
	errno = n.versionedHook(ctx, path, command("symlink", path), posixOp, 0,
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
	if n.protectedChild(name) {
		return nil, syscall.EPERM
	}
	path := n.root.visiblePath(n, name)
	posixOp := fmt.Sprintf("link(<inode>, %q)", path)
	errno = n.versionedHook(ctx, path, command("link", path), posixOp, 0,
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
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		if waitErrno := n.root.waitWritable(ctx); waitErrno != 0 {
			return nil, 0, waitErrno
		}
	}
	path := n.root.visiblePath(n)
	posixOp := fmt.Sprintf("open(%q, %s)", path, formatOpenFlags(flags))
	open := func() syscall.Errno {
		var base fs.FileHandle
		base, fuseFlags, errno = n.LoopbackNode.Open(ctx, flags)
		if errno == 0 {
			file := n.root.newFile(base, flags)
			if file == nil {
				return syscall.EIO
			}
			fh = file
			if directWrite(flags) {
				fuseFlags |= fuse.FOPEN_DIRECT_IO
			}
		}
		return errno
	}
	if flags&(syscall.O_TRUNC|syscall.O_CREAT) == 0 {
		errno = open()
	} else {
		errno = n.versionedHook(
			ctx, path, command("open", path), posixOp, 0, open)
	}
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
	posixOp := fmt.Sprintf("write(%q, offset=%d, len=%d)",
		path, off, len(data))
	if keepErrno := n.keepWritableWindow(
		ctx, path, command("write", path), posixOp, file); keepErrno != 0 {
		return 0, keepErrno
	}
	sample := n.root.sampleWrite(path, data, off)
	errno = n.versionedHook(ctx, path, command("write", path), posixOp, file.id,
		func() syscall.Errno {
			written, errno = file.LoopbackFile.Write(ctx, data, off)
			return errno
		})
	if errno == 0 && written != 0 {
		file.mutated.Store(true)
		sample.length = int(written)
		if len(sample.before) > int(written) {
			sample.before = sample.before[:written]
		}
		if len(sample.after) > int(written) {
			sample.after = sample.after[:written]
		}
		file.addWriteSample(sample)
	}
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
	posixOp := fmt.Sprintf("setattr(%q)", path)
	if size, ok := in.GetSize(); ok {
		posixOp = fmt.Sprintf("truncate(%q, %d)", path, size)
	}
	var fd uint64
	file, trackedFile := tracked(f)
	if trackedFile {
		fd = file.id
		if keepErrno := n.keepWritableWindow(
			ctx, path, command(op, path), posixOp, file); keepErrno != 0 {
			return keepErrno
		}
	}
	errno := n.versionedHook(ctx, path, command(op, path), posixOp, fd,
		func() syscall.Errno {
			return n.LoopbackNode.Setattr(ctx, f, in, out)
		})
	if errno == 0 && trackedFile {
		file.mutated.Store(true)
	}
	return errno
}

func (n *Node) Setxattr(
	ctx context.Context,
	attr string,
	data []byte,
	flags uint32,
) syscall.Errno {
	path := n.root.visiblePath(n)
	posixOp := fmt.Sprintf("setxattr(%q, %q, len=%d)", path, attr, len(data))
	return n.versionedHook(ctx, path, command("setxattr", path), posixOp, 0,
		func() syscall.Errno {
			return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
		})
}

func (n *Node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	path := n.root.visiblePath(n)
	posixOp := fmt.Sprintf("removexattr(%q, %q)", path, attr)
	return n.versionedHook(ctx, path, command("removexattr", path), posixOp, 0,
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
	posixOp := fmt.Sprintf("fallocate(%q, offset=%d, len=%d, mode=%d)",
		path, off, size, mode)
	if keepErrno := n.keepWritableWindow(
		ctx, path, command("fallocate", path), posixOp, file); keepErrno != 0 {
		return keepErrno
	}
	errno := n.versionedHook(ctx, path, command("fallocate", path), posixOp, file.id,
		func() syscall.Errno {
			return file.LoopbackFile.Allocate(ctx, off, size, mode)
		})
	if errno == 0 {
		file.mutated.Store(true)
	}
	return errno
}

// Flush 为已有变更的 mmap/close 提供兜底打点。只有该 fd 已经由 Create、
// Write、Setattr 或 Allocate 关联 auto 时才重开窗口,避免只读/空写 close
// 产生空版本。支持 FUSE direct-I/O mmap 的 Linux 内核会先把脏页下沉为 Write,
// 因而沿用同一 fd auto,Flush 只负责持久化窗口的最后收口。
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
	if file.discarded.Load() || n.root.Quiescing() {
		return syscall.EBUSY
	}
	n.root.window.Lock()
	defer n.root.window.Unlock()
	if file.discarded.Load() || n.root.Quiescing() {
		return syscall.EBUSY
	}
	// auto 必须保留到 Release:JuiceFS 可能只在关闭原始底层 fd 时提交
	// 大块写的 chunk 元数据。Commit/Rollback 会短暂等待异步 Release。
	return file.LoopbackFile.Flush(ctx)
}

// Fsync 与 Flush 共用升级屏障，避免已经被丢弃的句柄在 undo
// 之后又尝试把缓存数据下沉到底层文件。
func (n *Node) Fsync(
	ctx context.Context,
	f fs.FileHandle,
	flags uint32,
) syscall.Errno {
	file, ok := tracked(f)
	if !ok {
		if syncer, ok := f.(fs.FileFsyncer); ok {
			return syncer.Fsync(ctx, flags)
		}
		return 0
	}
	if !file.writable {
		return file.LoopbackFile.Fsync(ctx, flags)
	}
	if file.discarded.Load() || n.root.Quiescing() {
		return syscall.EBUSY
	}
	n.root.window.Lock()
	defer n.root.window.Unlock()
	if file.discarded.Load() || n.root.Quiescing() {
		return syscall.EBUSY
	}
	return file.LoopbackFile.Fsync(ctx, flags)
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
	posixOp := fmt.Sprintf(
		"copy_file_range(<source>, %q, offset=%d, len=%d)",
		path, offOut, length)
	if keepErrno := destinationNode.keepWritableWindow(
		ctx, path, command("copy_file_range", path), posixOp, destination); keepErrno != 0 {
		return 0, keepErrno
	}
	errno = destinationNode.versionedHook(
		ctx, path, command("copy_file_range", path), posixOp, destination.id,
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
	if errno == 0 && count != 0 {
		destination.mutated.Store(true)
		destination.addWriteSample(writeSample{
			offset: int64(offOut), length: int(count), calls: 1,
		})
	}
	return count, errno
}
