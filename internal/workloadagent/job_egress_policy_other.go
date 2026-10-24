//go:build !linux

package workloadagent

import (
	"context"
	"errors"
)

var errJobEgressPolicy = errors.New("Job egress policy is unsupported on this platform")

type LinuxJobEgressPolicy struct{}

func (LinuxJobEgressPolicy) Enforce(context.Context) error { return errJobEgressPolicy }
