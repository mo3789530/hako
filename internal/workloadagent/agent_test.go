package workloadagent

import (
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
)

func TestFilesystemAgentRequiresJobServiceCapableFactoryWhenEnabled(t *testing.T) {
	for name, factory := range map[string]ProcessFactory{
		"nil factory":     nil,
		"generic factory": &fakeProcessFactory{},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewFilesystemAgent(FilesystemAgentConfig{
				AllowJobServices: true,
				ProcessFactory:   factory,
			})
			if !errors.Is(err, ErrJobServicesUnavailable) {
				t.Fatalf("NewFilesystemAgent() error = %v, want %v", err, ErrJobServicesUnavailable)
			}
		})
	}
}

func TestGoTestProcessFactoryDeclaresJobServiceComposition(t *testing.T) {
	if (GoTestProcessFactory{}).SupportsJobServices() {
		t.Fatal("factory without a Job Service provisioner reported support")
	}
	if !(GoTestProcessFactory{JobServices: fixedJobServiceProvisioner{}}).SupportsJobServices() {
		t.Fatal("factory with a Job Service provisioner did not report support")
	}
}

func TestFilesystemAgentAcceptsJobServiceCapableFactory(t *testing.T) {
	root := t.TempDir()
	agent, err := NewFilesystemAgent(FilesystemAgentConfig{
		SourceDirectory: filepath.Join(root, "source"),
		TrustedKeys: map[string]ed25519.PublicKey{
			"rp-v1": make(ed25519.PublicKey, ed25519.PublicKeySize),
		},
		ProcessFactory:   GoTestProcessFactory{JobServices: fixedJobServiceProvisioner{session: &fixedJobServiceSession{}}},
		AllowJobServices: true,
	})
	if err != nil {
		t.Fatalf("NewFilesystemAgent() error = %v", err)
	}
	if err := agent.Close(t.Context()); err != nil {
		t.Fatalf("agent.Close() error = %v", err)
	}
}
