//go:build linux

package workloadagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func attachJobProcessScope(attributes *syscall.SysProcAttr, scope JobProcessResourceScope) error {
	if attributes == nil || scope == nil || scope.FileDescriptor() < 0 {
		return ErrJobResourceLimitsUnavailable
	}
	attributes.UseCgroupFD = true
	attributes.CgroupFD = scope.FileDescriptor()
	return nil
}

var ErrJobResourceLimits = errors.New("Job process resource limits could not be enforced")

// LinuxJobCgroupManager creates a cgroup v2 child under a pre-delegated root.
// The parent must have cpu and memory controllers enabled in cgroup.subtree_control.
// It never changes the host cgroup hierarchy itself.
type LinuxJobCgroupManager struct {
	Root string
}

func (m LinuxJobCgroupManager) Open(ctx context.Context, memoryMB, cpuMillis int) (JobProcessResourceScope, error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root ||
		memoryMB < 256 || memoryMB > 16384 || cpuMillis < 250 || cpuMillis > 8000 {
		return nil, ErrJobResourceLimits
	}
	rootInfo, err := os.Lstat(m.Root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrJobResourceLimits
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(m.Root, &fs); err != nil || uint64(fs.Type) != uint64(0x63677270) { // CGROUP2_SUPER_MAGIC
		return nil, ErrJobResourceLimits
	}
	controllers, err := os.ReadFile(filepath.Join(m.Root, "cgroup.subtree_control"))
	if err != nil || !containsController(string(controllers), "cpu") || !containsController(string(controllers), "memory") ||
		!containsController(string(controllers), "pids") {
		return nil, ErrJobResourceLimits
	}
	path, err := os.MkdirTemp(m.Root, "hako-job-")
	if err != nil {
		return nil, ErrJobResourceLimits
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	memoryBytes := int64(memoryMB) * (1 << 20)
	cpuQuotaMicros := int64(cpuMillis) * 100
	if writeCgroupControl(filepath.Join(path, "memory.max"), fmt.Sprintf("%d", memoryBytes)) != nil ||
		writeCgroupControl(filepath.Join(path, "cpu.max"), fmt.Sprintf("%d 100000", cpuQuotaMicros)) != nil ||
		writeCgroupControl(filepath.Join(path, "pids.max"), "256") != nil {
		return nil, ErrJobResourceLimits
	}
	group, err := os.Open(path)
	if err != nil {
		return nil, ErrJobResourceLimits
	}
	removeOnError = false
	return &linuxJobProcessScope{path: path, group: group}, nil
}

func containsController(value, name string) bool {
	for _, controller := range strings.Fields(value) {
		if controller == name || controller == "+"+name {
			return true
		}
	}
	return false
}

func writeCgroupControl(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return ErrJobResourceLimits
	}
	_, writeErr := file.WriteString(value)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return ErrJobResourceLimits
	}
	return nil
}

type linuxJobProcessScope struct {
	mu     sync.Mutex
	path   string
	group  *os.File
	closed bool
}

func (s *linuxJobProcessScope) FileDescriptor() int {
	if s == nil || s.group == nil {
		return -1
	}
	return int(s.group.Fd())
}

func (s *linuxJobProcessScope) Close(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrJobResourceLimits
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if writeCgroupControl(filepath.Join(s.path, "cgroup.kill"), "1") != nil {
		return ErrJobResourceLimits
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		populated, err := cgroupPopulated(filepath.Join(s.path, "cgroup.events"))
		if err != nil {
			return ErrJobResourceLimits
		}
		if !populated {
			break
		}
		select {
		case <-ctx.Done():
			return ErrJobResourceLimits
		case <-ticker.C:
		}
	}
	if s.group != nil {
		if err := s.group.Close(); err != nil {
			return ErrJobResourceLimits
		}
		s.group = nil
	}
	if err := os.Remove(s.path); err != nil {
		return ErrJobResourceLimits
	}
	s.closed = true
	return nil
}

func cgroupPopulated(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] != "0", nil
		}
	}
	return false, ErrJobResourceLimits
}
