package mount

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type JuiceFS struct {
	Binary     string
	MetaURL    string
	MountPoint string
	LogOutput  io.Writer

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan error
	managed bool
}

// GC 使用 JuiceFS 原生对象适配层做 mark/compact/delete，因此 file、S3、
// OSS 等后端共享同一套安全语义。调用方负责与写窗口串行化。
func (m *JuiceFS) GC(ctx context.Context, threads int) error {
	m.mu.Lock()
	if err := m.validate(); err != nil {
		m.mu.Unlock()
		return err
	}
	binary, metaURL, output := m.Binary, m.MetaURL, m.LogOutput
	m.mu.Unlock()
	if threads <= 0 {
		threads = 4
	}
	cmd := exec.CommandContext(ctx, binary,
		"gc", "--compact", "--delete", "--threads", strconv.Itoa(threads),
		metaURL)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("juicefs gc: %w", err)
	}
	return nil
}

func (m *JuiceFS) validate() error {
	if m.Binary == "" {
		m.Binary = "juicefs"
	}
	if m.MetaURL == "" {
		return errors.New("JuiceFS meta URL 不能为空")
	}
	if m.MountPoint == "" || !filepath.IsAbs(m.MountPoint) {
		return fmt.Errorf("JuiceFS mountpoint 必须是绝对路径:%q", m.MountPoint)
	}
	if m.LogOutput == nil {
		m.LogOutput = os.Stderr
	}
	return nil
}

func (m *JuiceFS) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		return errors.New("JuiceFS mount 已启动")
	}
	if err := m.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(m.MountPoint, 0o755); err != nil {
		return fmt.Errorf("创建 JuiceFS mountpoint: %w", err)
	}
	mounted, err := IsMountPoint(m.MountPoint)
	if err != nil {
		return err
	}
	if mounted {
		m.managed = false
		return nil
	}

	cmd := exec.Command(
		m.Binary,
		"mount",
		"--no-bgjob",
		"--no-usage-report",
		"--backup-meta", "0",
		// pitr_revert 直接原子更新 JuiceFS 元数据表。关闭元数据缓存,
		// 保证存储过程提交后下一次 lookup/getattr 立即读到回放结果。
		"--attr-cache", "0",
		"--entry-cache", "0",
		"--dir-entry-cache", "0",
		"--negative-entry-cache", "0",
		m.MetaURL,
		m.MountPoint,
	)
	cmd.Stdout = m.LogOutput
	cmd.Stderr = m.LogOutput
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 juicefs mount: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	m.cmd = cmd
	m.done = done
	m.managed = true

	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			m.cmd = nil
			m.done = nil
			m.managed = false
			if err == nil {
				err = errors.New("juicefs mount 在就绪前退出")
			}
			return fmt.Errorf("juicefs mount: %w", err)
		case <-ticker.C:
			ready, statErr := IsMountPoint(m.MountPoint)
			if statErr != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				return statErr
			}
			if ready {
				return nil
			}
		case <-timeout.C:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return fmt.Errorf("juicefs mount %s 30 秒内未就绪", m.MountPoint)
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return ctx.Err()
		}
	}
}

func (m *JuiceFS) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.managed || m.cmd == nil {
		return nil
	}

	umount := exec.CommandContext(ctx, m.Binary, "umount", "--force", m.MountPoint)
	umount.Stdout = m.LogOutput
	umount.Stderr = m.LogOutput
	umountErr := umount.Run()
	if umountErr != nil {
		_ = m.cmd.Process.Signal(syscall.SIGTERM)
	}

	select {
	case waitErr := <-m.done:
		m.cmd = nil
		m.done = nil
		m.managed = false
		if waitErr != nil {
			var exitErr *exec.ExitError
			// FUSE 进程在正常 umount 后通常收到信号退出。
			if !errors.As(waitErr, &exitErr) {
				return fmt.Errorf("等待 juicefs mount 退出: %w", waitErr)
			}
		}
		if umountErr != nil {
			return fmt.Errorf("juicefs umount: %w", umountErr)
		}
		return nil
	case <-ctx.Done():
		_ = m.cmd.Process.Kill()
		return fmt.Errorf("等待 juicefs umount: %w", ctx.Err())
	}
}

func IsMountPoint(target string) (bool, error) {
	cleaned, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("读取 mountinfo: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountpoint := strings.ReplaceAll(fields[4], `\040`, " ")
		if filepath.Clean(mountpoint) == filepath.Clean(cleaned) {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("扫描 mountinfo: %w", err)
	}
	return false, nil
}
