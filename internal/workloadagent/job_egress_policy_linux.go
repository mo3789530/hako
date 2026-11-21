//go:build linux

package workloadagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

const jobEgressChain = "HAKO_JOB_EGRESS"

var errJobEgressPolicy = errors.New("Job egress policy could not be enforced")

type jobEgressCommandRunner interface {
	Run(context.Context, string, ...string) error
}

type execJobEgressCommandRunner struct{}

func (execJobEgressCommandRunner) Run(ctx context.Context, binary string, args ...string) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	if err := command.Run(); err != nil {
		return errJobEgressPolicy
	}
	return nil
}

// LinuxJobEgressPolicy allows only loopback traffic from the fixed Job process
// identity and the rootless containerd user namespace. All other IPv4/IPv6
// OUTPUT traffic from those identities is rejected. This is defense in depth;
// the Resource Plane VPC connector/firewall remains responsible for the
// MicroVM's outer network boundary.
type LinuxJobEgressPolicy struct {
	runner jobEgressCommandRunner
}

func (p LinuxJobEgressPolicy) Enforce(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || os.Geteuid() != 0 {
		return errJobEgressPolicy
	}
	runner := p.runner
	if runner == nil {
		runner = execJobEgressCommandRunner{}
	}
	for _, binary := range []string{"/usr/sbin/iptables", "/usr/sbin/ip6tables"} {
		if err := enforceJobEgressFamily(ctx, runner, binary); err != nil {
			return errJobEgressPolicy
		}
	}
	return nil
}

func enforceJobEgressFamily(ctx context.Context, runner jobEgressCommandRunner, binary string) error {
	base := []string{"-w", "5", "-t", "filter"}
	chainArgs := append(append([]string(nil), base...), "-S", jobEgressChain)
	if err := runner.Run(ctx, binary, chainArgs...); err != nil {
		createArgs := append(append([]string(nil), base...), "-N", jobEgressChain)
		if err := runner.Run(ctx, binary, createArgs...); err != nil {
			return errJobEgressPolicy
		}
	}
	flushArgs := append(append([]string(nil), base...), "-F", jobEgressChain)
	if err := runner.Run(ctx, binary, flushArgs...); err != nil {
		return errJobEgressPolicy
	}
	mappedUIDRange := fmt.Sprintf("%d-%d", jobRuntimeSubIDBase, jobRuntimeSubIDBase+jobRuntimeSubIDSize-1)
	for _, uid := range []string{"65532", fmt.Sprint(jobRuntimeUID), mappedUIDRange} {
		loopbackArgs := append(append([]string(nil), base...), "-A", jobEgressChain,
			"-m", "owner", "--uid-owner", uid, "-o", "lo", "-j", "ACCEPT")
		if err := runner.Run(ctx, binary, loopbackArgs...); err != nil {
			return errJobEgressPolicy
		}
		denyArgs := append(append([]string(nil), base...), "-A", jobEgressChain,
			"-m", "owner", "--uid-owner", uid, "-j", "REJECT")
		if err := runner.Run(ctx, binary, denyArgs...); err != nil {
			return errJobEgressPolicy
		}
	}
	returnArgs := append(append([]string(nil), base...), "-A", jobEgressChain, "-j", "RETURN")
	if err := runner.Run(ctx, binary, returnArgs...); err != nil {
		return errJobEgressPolicy
	}
	checkArgs := append(append([]string(nil), base...), "-C", "OUTPUT", "-j", jobEgressChain)
	if err := runner.Run(ctx, binary, checkArgs...); err != nil {
		insertArgs := append(append([]string(nil), base...), "-I", "OUTPUT", "1", "-j", jobEgressChain)
		if err := runner.Run(ctx, binary, insertArgs...); err != nil {
			return errJobEgressPolicy
		}
	}
	if err := runner.Run(ctx, binary, checkArgs...); err != nil {
		return errJobEgressPolicy
	}
	return nil
}
