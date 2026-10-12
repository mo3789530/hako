package workloadagent

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"time"
)

// FilesystemAgentConfig contains only image/Resource-Plane-owned settings.
// Repository-controlled input cannot select directories, binaries, or keys.
type FilesystemAgentConfig struct {
	SourceDirectory       string
	TrustedKeys           map[string]ed25519.PublicKey
	ProcessFactory        ProcessFactory
	MaxOutput             int
	Now                   func() time.Time
	RequireRuntimeBinding bool
}

// FilesystemAgent composes the authenticated protocol, verified source
// installer, ticket-bound fixed-path resolver, and one-attempt process backend
// used by a single Lambda MicroVM.
type FilesystemAgent struct {
	handler http.Handler
	backend *ProcessBackend
}

func NewFilesystemAgent(config FilesystemAgentConfig) (*FilesystemAgent, error) {
	resolver, err := NewFilesystemExecutionResolver(config.SourceDirectory, config.Now)
	if err != nil {
		return nil, err
	}
	sources, err := NewFilesystemSourceInstaller(config.SourceDirectory, 0)
	if err != nil {
		return nil, err
	}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: resolver, Factory: config.ProcessFactory,
		MaxOutput: config.MaxOutput, Now: config.Now})
	if err != nil {
		return nil, err
	}
	server, err := newServer(backend, sources, config.TrustedKeys, config.Now, config.RequireRuntimeBinding)
	if err != nil {
		_ = backend.Close(context.Background())
		return nil, err
	}
	handler, err := NewMicroVMLifecycleHandler(server.Handler(), server)
	if err != nil {
		_ = backend.Close(context.Background())
		return nil, err
	}
	return &FilesystemAgent{handler: handler, backend: backend}, nil
}

func (a *FilesystemAgent) Handler() http.Handler {
	if a == nil {
		return nil
	}
	return a.handler
}

func (a *FilesystemAgent) Close(ctx context.Context) error {
	if a == nil || a.backend == nil {
		return ErrExecutionPlan
	}
	return a.backend.Close(ctx)
}
