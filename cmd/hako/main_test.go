package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
)

type cliTestCredentialGetter string

func (g cliTestCredentialGetter) Get(service, account string) (string, error) {
	if g == "" {
		return "", errors.New("credential not found")
	}
	if service != auth.KeyringService || account != auth.KeyringAccount {
		return "", errors.New("unexpected keyring lookup")
	}
	encoded, err := json.Marshal(auth.TokenSet{AccessToken: string(g), TokenType: "Bearer", ExpiresIn: 3600, StoredAt: time.Now().UTC()})
	return string(encoded), err
}

func TestAPIHealthCLIRequiresSavedTokenAndCallsProtectedRoute(t *testing.T) {
	oldGetter := cliCredentialGetter
	t.Cleanup(func() { cliCredentialGetter = oldGetter })

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/health" {
			t.Errorf("health CLI request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cli-health-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	t.Setenv("HAKO_API_URL", server.URL)

	cliCredentialGetter = cliTestCredentialGetter("cli-health-token")
	if err := apiHealth(); err != nil {
		t.Fatalf("apiHealth with saved token: %v", err)
	}
	if requests != 1 {
		t.Fatalf("health request count = %d, want 1", requests)
	}

	cliCredentialGetter = cliTestCredentialGetter("")
	if err := apiHealth(); err == nil || !strings.Contains(err.Error(), "run hako login") {
		t.Fatalf("apiHealth without saved token = %v, want login error", err)
	}
	if requests != 1 {
		t.Fatalf("unauthenticated health request reached API: count=%d", requests)
	}

	t.Setenv("HAKO_API_URL", "")
	cliCredentialGetter = cliTestCredentialGetter("cli-health-token")
	if err := apiHealth(); err == nil || !strings.Contains(err.Error(), "HAKO_API_URL is required") {
		t.Fatalf("apiHealth without API URL = %v, want configuration error", err)
	}
	if requests != 1 {
		t.Fatalf("missing API URL unexpectedly called server: count=%d", requests)
	}
}

func TestWorkspaceServiceHealthRequiresCurrentRuntimeObservation(t *testing.T) {
	tests := []struct {
		name    string
		service domain.Service
		want    string
	}{
		{name: "current healthy report", service: domain.Service{DesiredState: "running", ObservedState: "healthy", DesiredRevision: 3, ObservedRevision: 3, ObservedRuntimeGeneration: 8}, want: "healthy"},
		{name: "current unhealthy report", service: domain.Service{DesiredState: "running", ObservedState: "unhealthy", DesiredRevision: 3, ObservedRevision: 3, ObservedRuntimeGeneration: 8}, want: "unhealthy"},
		{name: "current generation failure", service: domain.Service{DesiredState: "running", ObservedState: "failed", DesiredRevision: 3, ObservedRevision: 2, ObservedRuntimeGeneration: 8}, want: "failed"},
		{name: "revision not observed", service: domain.Service{DesiredState: "running", ObservedState: "healthy", DesiredRevision: 3, ObservedRevision: 2, ObservedRuntimeGeneration: 8}, want: "pending"},
		{name: "prior runtime generation", service: domain.Service{DesiredState: "running", ObservedState: "healthy", DesiredRevision: 3, ObservedRevision: 3}, want: "pending"},
		{name: "not desired running", service: domain.Service{DesiredState: "stopped", ObservedState: "stopped", DesiredRevision: 3, ObservedRevision: 3, ObservedRuntimeGeneration: 8}, want: "n/a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := workspaceServiceHealth(test.service); got != test.want {
				t.Fatalf("workspaceServiceHealth() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCLIUsageListsWorkspaceServiceCommands(t *testing.T) {
	var output bytes.Buffer
	if err := writeCLIUsage(&output); err != nil {
		t.Fatalf("writeCLIUsage: %v", err)
	}
	for _, command := range []string{
		"hako add <tenant-id> <workspace-id> postgres|redis",
		"hako services <tenant-id> <workspace-id>",
		"hako service usage <tenant-id> <workspace-id>",
		"hako service start|stop|remove|rotate-credentials",
	} {
		if !strings.Contains(output.String(), command) {
			t.Errorf("CLI usage does not list %q", command)
		}
	}
}

func TestParseTenantQuotaSetArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantYes    bool
		wantErr    bool
		wantLimits api.TenantQuotaLimits
	}{
		{name: "required limits", args: []string{"tenant_1", "8", "4"}, wantLimits: api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4}},
		{name: "queued backlog flag", args: []string{"tenant_1", "8", "4", "--queued-workload-runs", "2"}, wantLimits: api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4, QueuedWorkloadRunsLimit: 2}},
		{name: "compute limits", args: []string{"tenant_1", "8", "4", "--workspace-compute-cpu-millicores", "4000", "--workspace-compute-memory-mib", "8192"}, wantLimits: api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4, WorkspaceComputeMilliCPULimit: 4000, WorkspaceComputeMemoryMiBLimit: 8192}},
		{name: "clear compute limits", args: []string{"tenant_1", "8", "4", "--no-workspace-compute-limits"}, wantLimits: api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4, WorkspaceComputeMilliCPULimit: -1, WorkspaceComputeMemoryMiBLimit: -1}},
		{name: "artifact and yes", args: []string{"tenant_1", "8", "4", "20", "--yes"}, wantYes: true, wantLimits: api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4, ArtifactBytesLimit: 20 << 30}},
		{name: "all limits", args: []string{"tenant_1", "8", "4", "20", "2", "1200", "3", "12"}, wantLimits: api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4, ArtifactBytesLimit: 20 << 30, WorkspaceServiceMemoryBytesLimit: 2 << 30, WorkspaceServiceMilliCPULimit: 1200, PreviewRunsLimit: 3, PreviewLifetimeSecondsLimit: 12 * 3600}},
		{name: "yes must be final", args: []string{"tenant_1", "8", "4", "--yes", "20"}, wantErr: true},
		{name: "reject unsafe limit", args: []string{"tenant_1", "11", "4"}, wantErr: true},
		{name: "reject queue above concurrency", args: []string{"tenant_1", "8", "3", "--queued-workload-runs", "4"}, wantErr: true},
		{name: "reject incomplete Service pair", args: []string{"tenant_1", "8", "4", "20", "2"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, skipConfirmation, err := parseTenantQuotaSetArgs(test.args)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseTenantQuotaSetArgs() error=%v, want error=%t", err, test.wantErr)
			}
			if err == nil && (skipConfirmation != test.wantYes || got != test.wantLimits) {
				t.Fatalf("parseTenantQuotaSetArgs() = (%+v, %t), want (%+v, %t)", got, skipConfirmation, test.wantLimits, test.wantYes)
			}
		})
	}
}

func TestMergeTenantQuotaDefaults(t *testing.T) {
	current := api.TenantQuotaLimits{QueuedWorkloadRunsLimit: 4, ArtifactBytesLimit: 10 << 30, WorkspaceServiceMemoryBytesLimit: 1 << 30,
		WorkspaceServiceMilliCPULimit: 900, PreviewRunsLimit: 2, PreviewLifetimeSecondsLimit: 3600, WorkspaceComputeMilliCPULimit: 3000, WorkspaceComputeMemoryMiBLimit: 6000}
	target := mergeTenantQuotaDefaults(api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4}, current)
	if target.QueuedWorkloadRunsLimit != 4 || target.ArtifactBytesLimit != current.ArtifactBytesLimit || target.WorkspaceServiceMemoryBytesLimit != current.WorkspaceServiceMemoryBytesLimit ||
		target.WorkspaceServiceMilliCPULimit != current.WorkspaceServiceMilliCPULimit || target.PreviewRunsLimit != current.PreviewRunsLimit || target.PreviewLifetimeSecondsLimit != current.PreviewLifetimeSecondsLimit ||
		target.WorkspaceComputeMilliCPULimit != current.WorkspaceComputeMilliCPULimit || target.WorkspaceComputeMemoryMiBLimit != current.WorkspaceComputeMemoryMiBLimit {
		t.Fatalf("omitted settings should retain existing values: %+v", target)
	}
}

func TestConfirmTenantQuotaUpdate(t *testing.T) {
	current := api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 10, WorkloadRunsLimit: 5, QueuedWorkloadRunsLimit: 5, PreviewRunsLimit: 2,
		ArtifactBytesLimit: 10 << 30, WorkspaceServiceMemoryBytesLimit: 2 << 30, WorkspaceServiceMilliCPULimit: 1500}
	usage := api.TenantUsage{WorkspacesUsed: 4, WorkloadRunsUsed: 2, QueuedWorkloadRunsUsed: 1, QueuedWorkloadRunsLimit: 5, PreviewRunsUsed: 1, ArtifactBytesUsed: 100, ArtifactBytesReserved: 200}
	target := current
	target.WorkspacesLimit = 6
	var output bytes.Buffer
	confirmed, err := confirmTenantQuotaUpdate(strings.NewReader("yes\n"), &output, current, usage, target)
	if err != nil || !confirmed {
		t.Fatalf("yes should confirm: confirmed=%t err=%v", confirmed, err)
	}
	if !strings.Contains(output.String(), "Workspaces: 4 → 6 (current limit 10)") || !strings.Contains(output.String(), "100 used + 200 reserved") {
		t.Fatalf("confirmation did not show current usage and target limits: %q", output.String())
	}
	confirmed, err = confirmTenantQuotaUpdate(strings.NewReader("n\n"), io.Discard, current, usage, target)
	if err != nil || confirmed {
		t.Fatalf("no should cancel: confirmed=%t err=%v", confirmed, err)
	}
	confirmed, err = confirmTenantQuotaUpdate(strings.NewReader(""), io.Discard, current, usage, target)
	if err != nil || confirmed {
		t.Fatalf("EOF should fail closed: confirmed=%t err=%v", confirmed, err)
	}
	if _, err := confirmTenantQuotaUpdate(nil, io.Discard, current, usage, target); err == nil {
		t.Fatal("missing stdin should require --yes or an input stream")
	}
}

func TestExecuteTenantQuotaUpdateDoesNotMutateUntilConfirmed(t *testing.T) {
	current := api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 10, WorkloadRunsLimit: 5, PreviewRunsLimit: 2,
		PreviewLifetimeSecondsLimit: 86400, ArtifactBytesLimit: 10 << 30, WorkspaceServiceMemoryBytesLimit: 2 << 30,
		WorkspaceServiceMilliCPULimit: 1500}
	usage := api.TenantUsage{WorkspacesUsed: 3, WorkspacesLimit: 10, WorkloadRunsUsed: 1, WorkloadRunsLimit: 5,
		PreviewRunsUsed: 1, PreviewRunsLimit: 2, PreviewLifetimeLimit: 86400, ArtifactBytesUsed: 10, ArtifactBytesReserved: 20,
		ArtifactBytesLimit: 10 << 30}
	target := api.TenantQuotaLimits{TenantID: "tenant_1", WorkspacesLimit: 8, WorkloadRunsLimit: 4}
	updates := 0
	readLimits := func(tenantID string) (api.TenantQuotaLimits, error) {
		if tenantID != "tenant_1" {
			t.Fatalf("read limits received Tenant %q", tenantID)
		}
		return current, nil
	}
	readUsage := func(tenantID string) (api.TenantUsage, error) {
		if tenantID != "tenant_1" {
			t.Fatalf("read usage received Tenant %q", tenantID)
		}
		return usage, nil
	}
	update := func(tenantID string, got api.TenantQuotaLimits) (api.TenantQuotaLimits, error) {
		updates++
		if tenantID != "tenant_1" || got.ArtifactBytesLimit != current.ArtifactBytesLimit || got.WorkspaceServiceMemoryBytesLimit != current.WorkspaceServiceMemoryBytesLimit {
			t.Fatalf("update did not preserve omitted existing limits: tenant=%q target=%+v", tenantID, got)
		}
		return got, nil
	}
	if _, err := executeTenantQuotaUpdate("tenant_1", target, false, strings.NewReader("n\n"), io.Discard, readLimits, readUsage, update); err == nil {
		t.Fatal("declined quota update should return an error")
	}
	if updates != 0 {
		t.Fatalf("declined quota update called mutation %d times", updates)
	}
	if _, err := executeTenantQuotaUpdate("tenant_1", target, false, strings.NewReader("yes\n"), io.Discard, readLimits, readUsage, update); err != nil {
		t.Fatalf("confirmed quota update failed: %v", err)
	}
	if updates != 1 {
		t.Fatalf("confirmed quota update called mutation %d times, want 1", updates)
	}
	if _, err := executeTenantQuotaUpdate("tenant_1", target, false, strings.NewReader(""), io.Discard, readLimits, readUsage, update); err == nil {
		t.Fatal("EOF must fail closed")
	}
	if updates != 1 {
		t.Fatalf("EOF quota update called mutation; updates=%d", updates)
	}
	if _, err := executeTenantQuotaUpdate("tenant_1", target, false, strings.NewReader("yes\n"), workspaceBasicCLIWriterError{}, readLimits, readUsage, update); err == nil {
		t.Fatal("quota update must fail when the confirmation prompt cannot be displayed")
	}
	if _, err := executeTenantQuotaUpdate("tenant_1", target, false, quotaUpdateReadError{}, io.Discard, readLimits, readUsage, update); err == nil {
		t.Fatal("quota update must fail when confirmation input cannot be read")
	}
	if updates != 1 {
		t.Fatalf("confirmation I/O failure called mutation; updates=%d", updates)
	}
	if _, err := executeTenantQuotaUpdate("tenant_1", target, true, nil, nil, readLimits, readUsage, update); err != nil {
		t.Fatalf("--yes should permit a non-interactive update: %v", err)
	}
	if updates != 2 {
		t.Fatalf("--yes update calls=%d want 2", updates)
	}
}

type quotaUpdateReadError struct{}

func (quotaUpdateReadError) Read([]byte) (int, error) {
	return 0, errors.New("synthetic terminal read failure")
}

func TestWriteWorkspaceServiceListShowsRevisionGenerationAndHealth(t *testing.T) {
	var output bytes.Buffer
	services := []domain.Service{
		{ID: "svc_pg", Name: "postgres", CatalogVersion: "postgres/1.0.0", DesiredState: "running", ObservedState: "healthy", DesiredRevision: 4, ObservedRevision: 4, ObservedRuntimeGeneration: 9},
		{ID: "svc_redis", Name: "redis", CatalogVersion: "redis/1.0.0", DesiredState: "running", ObservedState: "pending", DesiredRevision: 2, ObservedRevision: 1},
	}
	if err := writeWorkspaceServiceList(&output, services); err != nil {
		t.Fatal(err)
	}
	want := "NAME\tCATALOG\tDESIRED\tOBSERVED\tREVISION\tRUNTIME_GEN\tHEALTH\n" +
		"postgres\tpostgres/1.0.0\trunning\thealthy\t4/4\t9\thealthy\n" +
		"redis\tredis/1.0.0\trunning\tpending\t2/1\t-\tpending\n" +
		"Showing 2 services\n"
	if output.String() != want {
		t.Fatalf("Workspace Service list output=%q want=%q", output.String(), want)
	}
}

func TestWriteTenantUsage(t *testing.T) {
	var output bytes.Buffer
	if err := writeTenantUsage(&output, "tenant_1", api.TenantUsage{WorkspacesUsed: 3, WorkspacesLimit: 10, WorkloadRunsUsed: 2, WorkloadRunsLimit: 5, QueuedWorkloadRunsUsed: 1, QueuedWorkloadRunsLimit: 5, PreviewRunsUsed: 1, PreviewRunsLimit: 2, PreviewLifetimeLimit: 86400, ArtifactBytesUsed: 1234, ArtifactBytesReserved: 104857600, ArtifactBytesLimit: 10737418240}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "Tenant tenant_1 quota: Workspaces 3/10 reserved; concurrent Workload Runs 2/5; queued Workload Runs 1/5; Previews 1/2 (max lifetime 24h0m0s); artifacts 1234 stored + 104857600 reserved / 10737418240 bytes; Workspace compute reservations 0mCPU/0MiB (0 profiles missing) / 0mCPU/0MiB limits\n"; got != want {
		t.Fatalf("usage output=%q want=%q", got, want)
	}
	if err := writeTenantUsage(&output, "tenant_1", api.TenantUsage{WorkspacesUsed: 11, WorkspacesLimit: 10, WorkloadRunsLimit: 5, QueuedWorkloadRunsUsed: 6, QueuedWorkloadRunsLimit: 5, PreviewRunsLimit: 2, PreviewLifetimeLimit: 86400, ArtifactBytesLimit: 10737418240}); err != nil {
		t.Fatalf("over-quota legacy Tenant usage should remain visible: %v", err)
	}
}

func TestWriteWorkspaceServiceResourceUsage(t *testing.T) {
	var output bytes.Buffer
	usage := api.WorkspaceServiceResourceUsage{MemoryBytesUsed: 1073741824, MemoryBytesLimit: 2147483648, MilliCPUUsed: 750, MilliCPULimit: 1500}
	if err := writeWorkspaceServiceResourceUsage(&output, "ws_1", usage); err != nil {
		t.Fatal(err)
	}
	want := "Workspace ws_1 Service requests: memory 1073741824/2147483648 bytes; CPU 750/1500 millicores (catalog reservations, not measured usage)\n"
	if output.String() != want {
		t.Fatalf("usage output=%q want=%q", output.String(), want)
	}
	if err := writeWorkspaceServiceResourceUsage(&output, "ws_1", api.WorkspaceServiceResourceUsage{}); err == nil {
		t.Fatal("invalid Workspace Service quota usage should be rejected")
	}
}

func TestParseCLIPage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantLimit  int
		wantOffset int
		wantError  bool
	}{
		{name: "defaults", wantLimit: 20, wantOffset: 0},
		{name: "limit only", args: []string{"50"}, wantLimit: 50},
		{name: "limit and offset", args: []string{"100", "1000000"}, wantLimit: 100, wantOffset: 1000000},
		{name: "non integer limit", args: []string{"many"}, wantError: true},
		{name: "non integer offset", args: []string{"20", "later"}, wantError: true},
		{name: "zero limit", args: []string{"0"}, wantError: true},
		{name: "limit above maximum", args: []string{"101"}, wantError: true},
		{name: "negative offset", args: []string{"20", "-1"}, wantError: true},
		{name: "offset above maximum", args: []string{"20", "1000001"}, wantError: true},
		{name: "too many arguments", args: []string{"20", "0", "extra"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit, offset, err := parseCLIPage(test.args)
			if (err != nil) != test.wantError {
				t.Fatalf("parseCLIPage(%q) error = %v, want error=%t", test.args, err, test.wantError)
			}
			if err == nil && (limit != test.wantLimit || offset != test.wantOffset) {
				t.Fatalf("parseCLIPage(%q) = (%d, %d), want (%d, %d)", test.args, limit, offset, test.wantLimit, test.wantOffset)
			}
		})
	}
}

func TestParseWorkspaceActionOptions(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		args      []string
		wantKey   string
		wantWait  bool
		wantYes   bool
		wantError bool
	}{
		{name: "default asynchronous resume", action: "resume"},
		{name: "resume waits for running state", action: "resume", args: []string{"--wait"}, wantWait: true},
		{name: "custom retry key", action: "resume", args: []string{"--idempotency-key", "retry-1"}, wantKey: "retry-1"},
		{name: "wait and retry key", action: "resume", args: []string{"--wait", "--idempotency-key", "retry-2"}, wantKey: "retry-2", wantWait: true},
		{name: "wait rejected for suspend", action: "suspend", args: []string{"--wait"}, wantError: true},
		{name: "wait rejected for delete", action: "delete", args: []string{"--wait"}, wantError: true},
		{name: "delete can be explicitly confirmed", action: "delete", args: []string{"--yes"}, wantYes: true},
		{name: "yes rejected for suspend", action: "suspend", args: []string{"--yes"}, wantError: true},
		{name: "yes rejected for resume", action: "resume", args: []string{"--yes"}, wantError: true},
		{name: "unknown argument rejected", action: "resume", args: []string{"extra"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseWorkspaceActionOptions(test.action, test.args)
			if (err != nil) != test.wantError {
				t.Fatalf("parseWorkspaceActionOptions(%q, %q) error=%v, want error=%t", test.action, test.args, err, test.wantError)
			}
			if err == nil && (got.idempotencyKey != test.wantKey || got.wait != test.wantWait || got.yes != test.wantYes) {
				t.Fatalf("options=%+v, want key=%q wait=%t yes=%t", got, test.wantKey, test.wantWait, test.wantYes)
			}
		})
	}
}

func TestParseWorkspaceServiceActionOptions(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		args      []string
		wantYes   bool
		wantError bool
	}{
		{name: "remove interactive confirmation", action: "remove"},
		{name: "remove automation confirmation", action: "remove", args: []string{"--yes"}, wantYes: true},
		{name: "yes rejected for start", action: "start", args: []string{"--yes"}, wantError: true},
		{name: "yes rejected for stop", action: "stop", args: []string{"--yes"}, wantError: true},
		{name: "unknown option rejected", action: "remove", args: []string{"--force"}, wantError: true},
		{name: "extra options rejected", action: "remove", args: []string{"--yes", "--yes"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseWorkspaceServiceActionOptions(test.action, test.args)
			if (err != nil) != test.wantError || (err == nil && got.yes != test.wantYes) {
				t.Fatalf("parseWorkspaceServiceActionOptions(%q, %q) = %+v, %v; want yes=%t error=%t", test.action, test.args, got, err, test.wantYes, test.wantError)
			}
		})
	}
}

func TestCreateWorkloadCLIRequiresImmutableRepositoryInputs(t *testing.T) {
	if _, _, err := createWorkloadCLI([]string{"--tenant", "tenant_1", "--installation", "1", "--repository", "2"}); err == nil {
		t.Fatal("createWorkloadCLI accepted a request without an immutable commit")
	}
	if _, _, err := createWorkloadCLI([]string{"--tenant", "tenant_1", "--installation", "1", "--repository", "2", "--commit", "abc", "extra"}); err == nil {
		t.Fatal("createWorkloadCLI accepted positional arguments")
	}
}
