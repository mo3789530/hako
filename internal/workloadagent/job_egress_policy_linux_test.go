//go:build linux

package workloadagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeJobEgressRunner struct {
	chainExists bool
	jumpExists  bool
	commands    [][]string
	failOn      string
}

func (r *fakeJobEgressRunner) Run(_ context.Context, binary string, args ...string) error {
	command := append([]string{binary}, args...)
	r.commands = append(r.commands, command)
	joined := strings.Join(command, " ")
	if r.failOn != "" && strings.Contains(joined, r.failOn) {
		return errors.New("runner failure")
	}
	switch {
	case strings.Contains(joined, " -S "+jobEgressChain):
		if !r.chainExists {
			return errors.New("chain missing")
		}
	case strings.Contains(joined, " -N "+jobEgressChain):
		r.chainExists = true
	case strings.Contains(joined, " -C OUTPUT -j "+jobEgressChain):
		if !r.jumpExists {
			return errors.New("jump missing")
		}
	case strings.Contains(joined, " -I OUTPUT 1 -j "+jobEgressChain):
		r.jumpExists = true
	}
	return nil
}

func TestEnforceJobEgressFamilyInstallsLoopbackOnlyRulesIdempotently(t *testing.T) {
	for _, binary := range []string{"/usr/sbin/iptables", "/usr/sbin/ip6tables"} {
		runner := &fakeJobEgressRunner{}
		for i := 0; i < 2; i++ {
			if err := enforceJobEgressFamily(context.Background(), runner, binary); err != nil {
				t.Fatalf("EnforceJobEgressFamily(%s) error = %v", binary, err)
			}
		}
		if !runner.chainExists || !runner.jumpExists {
			t.Fatalf("policy %s was not installed: chain=%v jump=%v", binary, runner.chainExists, runner.jumpExists)
		}
		var allowLoopback, denyOther, allowMappedRange bool
		for _, command := range runner.commands {
			joined := strings.Join(command, " ")
			allowLoopback = allowLoopback || strings.Contains(joined, "--uid-owner 65532 -o lo -j ACCEPT")
			denyOther = denyOther || strings.Contains(joined, "--uid-owner 65532 -j REJECT")
			allowMappedRange = allowMappedRange || strings.Contains(joined, "--uid-owner 20000000-20065535 -j REJECT")
		}
		if !allowLoopback || !denyOther || !allowMappedRange {
			t.Fatalf("policy %s does not restrict all Job identities: loopback=%v deny=%v mapped-range=%v", binary, allowLoopback, denyOther, allowMappedRange)
		}
	}
}

func TestEnforceJobEgressFamilyFailsClosedWhenRuleCannotBeInstalled(t *testing.T) {
	runner := &fakeJobEgressRunner{failOn: "-j REJECT"}
	if err := enforceJobEgressFamily(context.Background(), runner, "/usr/sbin/ip6tables"); !errors.Is(err, errJobEgressPolicy) {
		t.Fatalf("EnforceJobEgressFamily() error = %v, want fail-closed policy error", err)
	}
}
