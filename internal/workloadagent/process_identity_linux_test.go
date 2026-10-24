//go:build linux

package workloadagent

import (
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestCommandCredentialRequiresDedicatedNonRootIdentity(t *testing.T) {
	if got, err := commandCredential(0, 0); err != nil || got != nil {
		t.Fatalf("local default credential = %#v, %v", got, err)
	}
	for _, pair := range [][2]uint32{{65532, 0}, {0, 65532}, {999, 999}, {65532, 65531}} {
		if _, err := commandCredential(pair[0], pair[1]); err == nil {
			t.Errorf("commandCredential(%d, %d) accepted an unsafe identity", pair[0], pair[1])
		}
	}
	got, err := commandCredential(65532, 65532)
	if err != nil || got == nil || got.Uid != 65532 || got.Gid != 65532 || len(got.Groups) != 0 {
		t.Fatalf("dedicated command credential = %#v, %v", got, err)
	}
}

func TestCatalogCommandRunsAsDedicatedIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing the child UID requires root")
	}
	output := &strings.Builder{}
	err := runCatalogCommand(context.Background(), t.TempDir(), "/usr/bin/id", []string{"-u"},
		[]string{"PATH=/usr/bin:/bin"}, output, 65532, 65532)
	if err != nil || strings.TrimSpace(output.String()) != "65532" {
		t.Fatalf("catalog command identity output = %q, err=%v", output.String(), err)
	}
}

func TestCatalogCommandRejectsInvalidIdentityBeforeStart(t *testing.T) {
	output := io.Discard
	err := runCatalogCommand(context.Background(), t.TempDir(), "/should/not/run", nil,
		[]string{"PATH=/usr/bin:/bin"}, output, 65532, 0)
	if err == nil || err != ErrExecutionPlan {
		t.Fatalf("invalid identity error = %v, want ErrExecutionPlan", err)
	}
	if syscall.Geteuid() == 0 {
		// Also assert the process builder does not silently retain root when one
		// side of the requested identity is omitted.
		if credential, buildErr := commandCredential(65532, 0); buildErr == nil || credential != nil {
			t.Fatalf("partial identity produced a credential: %#v, %v", credential, buildErr)
		}
	}
}
