package workloadagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/factoryconfig"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

var (
	ErrJobServicesUnavailable = errors.New("Job Service runtime is unavailable")
	ErrJobServiceCleanup      = errors.New("Job Service cleanup is incomplete")
)

// JobServiceEngine is deliberately separate from the Workspace Service Engine:
// each network/container is ephemeral and owned by one immutable Run attempt
// and pipeline. Implementations must use an internal rootless network, bind
// published ports only to loopback, enforce catalog resource limits, and never
// accept user-provided images, mounts, commands, or host sockets.
type JobServiceEngine interface {
	EnsureNetwork(context.Context, string, string) error
	EnsureService(context.Context, string, string, string, servicecatalog.Template, map[string]string) error
	Healthy(context.Context, string, servicecatalog.HealthCheck) (bool, error)
	RemoveService(context.Context, string, string) error
	RemoveNetwork(context.Context, string, string) error
}

type JobServiceProvisioner interface {
	Start(context.Context, workloadjournal.Command, factoryconfig.ExecutionPlan) (JobServiceSession, error)
}

// JobEgressPolicy installs a host-level default-deny policy before
// repository-controlled processes run. The MicroVM connector is an outer
// boundary and does not constrain loopback or local sidecars by itself.
type JobEgressPolicy interface {
	Enforce(context.Context) error
}

type JobServiceSession interface {
	Environment() []string
	SecretValues() []string
	Close(context.Context) error
}

type CatalogJobServiceManager struct {
	engine JobServiceEngine
	random io.Reader
	now    func() time.Time
}

func NewCatalogJobServiceManager(engine JobServiceEngine, random io.Reader, now func() time.Time) (*CatalogJobServiceManager, error) {
	if engine == nil {
		return nil, ErrJobServicesUnavailable
	}
	if random == nil {
		random = rand.Reader
	}
	if now == nil {
		now = time.Now
	}
	return &CatalogJobServiceManager{engine: engine, random: random, now: now}, nil
}

func (m *CatalogJobServiceManager) Start(ctx context.Context, identity workloadjournal.Command, pipeline factoryconfig.ExecutionPlan) (JobServiceSession, error) {
	if m == nil || m.engine == nil || ctx == nil || ctx.Err() != nil || !validIdentity(identity) ||
		factoryconfig.ValidateExecutionPlan(pipeline) != nil || len(pipeline.Services) == 0 || pipeline.Network != "registry-only" {
		return nil, ErrJobServicesUnavailable
	}
	services := append([]string(nil), pipeline.Services...)
	sort.Strings(services)
	for i, name := range services {
		if _, err := servicecatalog.Resolve(name); err != nil || (i > 0 && services[i-1] == name) {
			return nil, ErrJobServicesUnavailable
		}
	}
	network, ownershipKey := jobServiceNames(identity, pipeline.Pipeline)
	containerNames := make(map[string]string, len(services))
	for _, name := range services {
		containerNames[name] = "hako-js-" + ownershipKey[:20] + "-" + name
	}
	// A process restart in the same MicroVM must not reconnect a new execution
	// to a former container's data. Clear the deterministic attempt-owned scope
	// before creating the fresh, ephemeral network.
	if err := m.remove(ctx, ownershipKey, network, containerNames); err != nil {
		return nil, ErrJobServiceCleanup
	}
	session := &catalogJobServiceSession{engine: m.engine, network: network, ownershipKey: ownershipKey,
		containers: containerNames, environments: make([]string, 0, len(services)*5), secrets: make([]string, 0, 1)}
	if err := m.engine.EnsureNetwork(ctx, network, ownershipKey); err != nil {
		return nil, errors.Join(ErrJobServicesUnavailable, session.closeAfterFailure())
	}
	for _, name := range services {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(ErrJobServicesUnavailable, session.closeAfterFailure())
		}
		template, err := servicecatalog.Resolve(name)
		if err != nil {
			return nil, errors.Join(ErrJobServicesUnavailable, session.closeAfterFailure())
		}
		secretValues, secretMap, err := m.generateSecrets(template)
		if err != nil {
			return nil, errors.Join(ErrJobServicesUnavailable, session.closeAfterFailure())
		}
		session.secrets = append(session.secrets, secretValues...)
		if err := m.engine.EnsureService(ctx, containerNames[name], network, ownershipKey, template, secretMap); err != nil {
			clearSecretMap(secretMap)
			failure := error(ErrJobServicesUnavailable)
			if errors.Is(err, ErrJobServiceCleanup) {
				failure = ErrJobServiceCleanup
			}
			return nil, errors.Join(failure, session.closeAfterFailure())
		}
		if err := m.waitHealthy(ctx, containerNames[name], template.HealthCheck); err != nil {
			clearSecretMap(secretMap)
			return nil, errors.Join(ErrJobServicesUnavailable, session.closeAfterFailure())
		}
		environment, ok := jobServiceEnvironment(name, template, secretMap)
		clearSecretMap(secretMap)
		if !ok {
			return nil, errors.Join(ErrJobServicesUnavailable, session.closeAfterFailure())
		}
		session.environments = append(session.environments, environment...)
	}
	return session, nil
}

func (m *CatalogJobServiceManager) generateSecrets(template servicecatalog.Template) ([]string, map[string]string, error) {
	values := make([]string, 0, len(template.GeneratedSecrets))
	secrets := make(map[string]string, len(template.GeneratedSecrets))
	for _, name := range template.GeneratedSecrets {
		var random [32]byte
		if _, err := io.ReadFull(m.random, random[:]); err != nil {
			clear(random[:])
			clearSecretMap(secrets)
			return nil, nil, ErrJobServicesUnavailable
		}
		value := base64.RawURLEncoding.EncodeToString(random[:])
		clear(random[:])
		values = append(values, value)
		secrets[name] = value
	}
	return values, secrets, nil
}

func (m *CatalogJobServiceManager) waitHealthy(ctx context.Context, container string, check servicecatalog.HealthCheck) error {
	interval, err := time.ParseDuration(check.Interval)
	if err != nil || interval <= 0 {
		return ErrJobServicesUnavailable
	}
	probeTimeout, err := time.ParseDuration(check.Timeout)
	if err != nil || probeTimeout <= 0 {
		return ErrJobServicesUnavailable
	}
	deadline := m.now().Add(time.Duration(check.Retries)*interval + probeTimeout)
	for attempt := 0; attempt < check.Retries; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		healthy, probeErr := m.engine.Healthy(probeCtx, container, check)
		cancel()
		if probeErr == nil && healthy {
			return nil
		}
		if ctx.Err() != nil || !m.now().Before(deadline) {
			return ErrJobServicesUnavailable
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ErrJobServicesUnavailable
		case <-timer.C:
		}
	}
	return ErrJobServicesUnavailable
}

func (m *CatalogJobServiceManager) remove(ctx context.Context, ownershipKey, network string, containers map[string]string) error {
	var failed bool
	names := make([]string, 0, len(containers))
	for _, name := range containers {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		if err := m.engine.RemoveService(ctx, name, ownershipKey); err != nil {
			failed = true
		}
	}
	if network != "" {
		if err := m.engine.RemoveNetwork(ctx, network, ownershipKey); err != nil {
			failed = true
		}
	}
	if failed {
		return ErrJobServiceCleanup
	}
	return nil
}

type catalogJobServiceSession struct {
	mu           sync.Mutex
	engine       JobServiceEngine
	network      string
	ownershipKey string
	containers   map[string]string
	environments []string
	secrets      []string
	closed       bool
}

func (s *catalogJobServiceSession) Environment() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.environments...)
}

func (s *catalogJobServiceSession) SecretValues() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.secrets...)
}

func (s *catalogJobServiceSession) Close(ctx context.Context) error {
	if s == nil || ctx == nil || s.engine == nil {
		return ErrJobServiceCleanup
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	var failed bool
	names := make([]string, 0, len(s.containers))
	for _, name := range s.containers {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		if err := s.engine.RemoveService(ctx, name, s.ownershipKey); err != nil {
			failed = true
		}
	}
	if err := s.engine.RemoveNetwork(ctx, s.network, s.ownershipKey); err != nil {
		failed = true
	}
	for i := range s.secrets {
		s.secrets[i] = ""
	}
	s.secrets = nil
	s.environments = nil
	if failed {
		return ErrJobServiceCleanup
	}
	s.closed = true
	return nil
}

func (s *catalogJobServiceSession) closeAfterFailure() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		return ErrJobServiceCleanup
	}
	return nil
}

func jobServiceNames(identity workloadjournal.Command, pipeline string) (string, string) {
	seed := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", identity.TenantID, identity.WorkloadRunID,
		identity.AttemptID, identity.Generation, pipeline)
	digest := sha256.Sum256([]byte(seed))
	key := hex.EncodeToString(digest[:])
	return "hako-jn-" + key[:24], key
}

func jobServiceEnvironment(name string, template servicecatalog.Template, secrets map[string]string) ([]string, bool) {
	switch name {
	case "postgres":
		if len(template.GeneratedSecrets) != 1 || template.GeneratedSecrets[0] != "POSTGRES_PASSWORD" || len(secrets) != 1 {
			return nil, false
		}
		password := secrets["POSTGRES_PASSWORD"]
		return []string{"PGHOST=127.0.0.1", "PGPORT=5432", "PGUSER=postgres", "PGPASSWORD=" + password,
			"PGDATABASE=postgres", "DATABASE_URL=postgres://postgres:" + password + "@127.0.0.1:5432/postgres?sslmode=disable"}, true
	case "redis":
		if len(template.GeneratedSecrets) != 0 || len(secrets) != 0 {
			return nil, false
		}
		return []string{"REDIS_HOST=127.0.0.1", "REDIS_PORT=6379", "REDIS_ADDR=127.0.0.1:6379"}, true
	default:
		return nil, false
	}
}

func clearSecretMap(values map[string]string) {
	for key := range values {
		values[key] = ""
	}
}

var _ JobServiceProvisioner = (*CatalogJobServiceManager)(nil)
