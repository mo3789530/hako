//go:build !linux

package workloadagent

import "syscall"

// Non-Linux platforms cannot atomically place a child into a delegated cgroup.
// Refusing the process launch keeps local development builds portable without
// silently dropping the resource limits required for untrusted Job execution.
func attachJobProcessScope(_ *syscall.SysProcAttr, _ JobProcessResourceScope) error {
	return ErrJobResourceLimitsUnavailable
}
