//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type workspaceJournalReaderStub struct {
	snapshot workspacejournal.Snapshot
	err      error
	got      [3]string
	calls    int
}

func (s *workspaceJournalReaderStub) Get(_ context.Context, tenant domain.TenantID, workspace domain.WorkspaceID, plane domain.ResourcePlaneID) (workspacejournal.Snapshot, error) {
	s.got = [3]string{string(tenant), string(workspace), string(plane)}
	s.calls++
	return s.snapshot, s.err
}

type workspaceMicroVMReaderStub struct {
	input  *lambdamicrovms.GetMicrovmInput
	output *lambdamicrovms.GetMicrovmOutput
	err    error
	calls  int
}

func (s *workspaceMicroVMReaderStub) GetMicrovm(_ context.Context, input *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	s.input = input
	s.calls++
	return s.output, s.err
}

func TestWorkspaceLambdaMicroVMDirectoryResolvesCurrentDurableBinding(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &AgentTicketIssuer{KeyID: "directory-agent-key", PrivateKey: private, TTL: time.Minute}
	claims := workspacesession.Claims{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	journal := newRunningWorkspaceJournal(t, claims, "microvm-1")
	store := &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: journal, ETag: `"running-v1"`}}
	api := &workspaceMicroVMReaderStub{output: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
		State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws")}}
	tokens := &gatewayTokenProviderStub{token: "ephemeral-token"}
	stream := &gatewayTestStream{}
	ws := &gatewayWebSocketStub{stream: stream}
	directory := &WorkspaceLambdaMicroVMStreamDirectory{ResourcePlaneID: "rp-1", Journal: store, MicroVMs: api, Tokens: tokens, Tickets: issuer,
		Region: "us-east-1", Origin: "https://gateway.hako.test", WebSocket: ws}
	opened, err := directory.OpenShell(context.Background(), claims)
	if err != nil || opened != stream {
		t.Fatalf("OpenShell() stream=%v err=%v", opened, err)
	}
	if store.calls != 1 || store.got != [3]string{"tenant-1", "workspace-1", "rp-1"} || api.calls != 1 ||
		aws.ToString(api.input.MicrovmIdentifier) != "microvm-1" || tokens.vmID != "microvm-1" || ws.endpoint != aws.ToString(api.output.Endpoint) {
		t.Fatalf("directory lookup binding mismatch: store=%+v API=%+v token=%+v socket=%+v", store, api, tokens, ws)
	}
	if _, err := VerifyAgentTicket(ws.ticket, map[string]ed25519.PublicKey{"directory-agent-key": public}, AgentTicket{
		UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane,
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell}, time.Now().UTC()); err != nil {
		t.Fatalf("directory-issued Agent Ticket is invalid: %v", err)
	}
	vscodeClaims := claims
	vscodeClaims.Permissions = []workspacesession.Permission{workspacesession.PermissionVSCode}
	ideStream, err := directory.OpenVSCode(context.Background(), vscodeClaims)
	if err != nil || ideStream != stream || ws.permission != workspacesession.PermissionVSCode {
		t.Fatalf("OpenVSCode() stream=%v route permission=%q err=%v", ideStream, ws.permission, err)
	}
	if _, err := VerifyAgentTicket(ws.ticket, map[string]ed25519.PublicKey{"directory-agent-key": public}, AgentTicket{
		UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane,
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionVSCode}, time.Now().UTC()); err != nil {
		t.Fatalf("directory-issued VS Code Agent Ticket is invalid: %v", err)
	}
	sshClaims := claims
	sshClaims.Permissions = []workspacesession.Permission{workspacesession.PermissionSSHProxy}
	sshStream, err := directory.OpenPort(context.Background(), sshClaims, workspacesession.PermissionSSHProxy, 22)
	if err != nil || sshStream != stream || ws.permission != workspacesession.PermissionSSHProxy {
		t.Fatalf("OpenPort(22) stream=%v route permission=%q err=%v", sshStream, ws.permission, err)
	}
	if _, err := VerifyAgentTicket(ws.ticket, map[string]ed25519.PublicKey{"directory-agent-key": public}, AgentTicket{
		UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane,
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionSSHProxy}, time.Now().UTC()); err != nil {
		t.Fatalf("directory-issued SSH Agent Ticket is invalid: %v", err)
	}
	forwardClaims := claims
	forwardClaims.Permissions = []workspacesession.Permission{workspacesession.PermissionPortForward}
	forwardStream, err := directory.OpenPort(context.Background(), forwardClaims, workspacesession.PermissionPortForward, 5432)
	if err != nil || forwardStream != stream || ws.permission != workspacesession.PermissionPortForward || ws.port != 5432 {
		t.Fatalf("OpenPort(5432) stream=%v permission=%q port=%d err=%v", forwardStream, ws.permission, ws.port, err)
	}
	if _, err := VerifyAgentTicket(ws.ticket, map[string]ed25519.PublicKey{"directory-agent-key": public}, AgentTicket{
		UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane,
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionPortForward, Port: 5432}, time.Now().UTC()); err != nil {
		t.Fatalf("directory-issued port-forward Agent Ticket is invalid: %v", err)
	}
	if _, err := directory.OpenPort(context.Background(), sshClaims, workspacesession.PermissionSSHProxy, 2222); err == nil {
		t.Fatal("directory accepted arbitrary port through SSH-proxy permission")
	}
	foreignPlane := claims
	foreignPlane.ResourcePlane = "rp-other"
	if _, err := directory.OpenShell(context.Background(), foreignPlane); err == nil || store.calls != 4 {
		t.Fatalf("directory accepted foreign Resource Plane claim or performed lookup: err=%v store calls=%d", err, store.calls)
	}
}

func TestWorkspaceLambdaMicroVMDirectoryFailsClosedForStateAndProviderMismatch(t *testing.T) {
	claims := workspacesession.Claims{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	base := newRunningWorkspaceJournal(t, claims, "microvm-1")
	for name, mutate := range map[string]func(*workspacejournal.Journal){
		"foreign tenant": func(j *workspacejournal.Journal) { j.TenantID = "tenant-2" },
		"no allocation":  func(j *workspacejournal.Journal) { j.ProviderID = "" },
		"tombstone":      func(j *workspacejournal.Journal) { j.Tombstone = true },
	} {
		t.Run(name, func(t *testing.T) {
			j := base
			mutate(&j)
			store := &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: j, ETag: `"etag"`}}
			api := &workspaceMicroVMReaderStub{output: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"), State: types.MicrovmStateRunning,
				Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws")}}
			tokens := &gatewayTokenProviderStub{token: "token"}
			ws := &gatewayWebSocketStub{stream: &gatewayTestStream{}}
			directory := &WorkspaceLambdaMicroVMStreamDirectory{ResourcePlaneID: "rp-1", Journal: store, MicroVMs: api, Tokens: tokens,
				Tickets: &AgentTicketIssuer{KeyID: "k1", PrivateKey: makePrivateKey(t), TTL: time.Minute}, Region: "us-east-1",
				Origin: "https://gateway.hako.test", WebSocket: ws}
			if _, err := directory.OpenShell(context.Background(), claims); err == nil {
				t.Fatal("accepted invalid Workspace runtime journal")
			}
			if api.calls != 0 || tokens.calls != 0 || ws.calls != 0 {
				t.Fatalf("invalid journal reached provider: API=%d token=%d dial=%d", api.calls, tokens.calls, ws.calls)
			}
		})
	}
	for name, vm := range map[string]*lambdamicrovms.GetMicrovmOutput{
		"foreign provider ID": {MicrovmId: aws.String("microvm-other"), State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws")},
		"suspended provider":  {MicrovmId: aws.String("microvm-1"), State: types.MicrovmStateSuspended, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws")},
		"foreign endpoint":    {MicrovmId: aws.String("microvm-1"), State: types.MicrovmStateRunning, Endpoint: aws.String("https://attacker.example")},
	} {
		t.Run(name, func(t *testing.T) {
			store := &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: base, ETag: `"etag"`}}
			api := &workspaceMicroVMReaderStub{output: vm}
			tokens := &gatewayTokenProviderStub{token: "token"}
			ws := &gatewayWebSocketStub{stream: &gatewayTestStream{}}
			directory := &WorkspaceLambdaMicroVMStreamDirectory{ResourcePlaneID: "rp-1", Journal: store, MicroVMs: api, Tokens: tokens,
				Tickets: &AgentTicketIssuer{KeyID: "k1", PrivateKey: makePrivateKey(t), TTL: time.Minute}, Region: "us-east-1",
				Origin: "https://gateway.hako.test", WebSocket: ws}
			if _, err := directory.OpenShell(context.Background(), claims); err == nil {
				t.Fatal("accepted invalid MicroVM provider observation")
			}
			if tokens.calls != 0 || ws.calls != 0 {
				t.Fatalf("invalid MicroVM observation reached auth/dial: token=%d dial=%d", tokens.calls, ws.calls)
			}
		})
	}
	store := &workspaceJournalReaderStub{err: errors.New("S3 unavailable")}
	api := &workspaceMicroVMReaderStub{}
	directory := &WorkspaceLambdaMicroVMStreamDirectory{ResourcePlaneID: "rp-1", Journal: store, MicroVMs: api, Tokens: &gatewayTokenProviderStub{},
		Tickets: &AgentTicketIssuer{KeyID: "k1", PrivateKey: makePrivateKey(t), TTL: time.Minute}, Region: "us-east-1", Origin: "https://gateway.hako.test"}
	if _, err := directory.OpenShell(context.Background(), claims); err == nil || api.calls != 0 {
		t.Fatal("journal outage did not fail closed before provider lookup")
	}
}

type workspaceLeaseReporterStub struct {
	identity WorkspaceConnectionLeaseIdentity
	opened   int
	lease    *workspaceLeaseHandleStub
	err      error
}

func (s *workspaceLeaseReporterStub) Open(_ context.Context, identity WorkspaceConnectionLeaseIdentity) (WorkspaceConnectionLeaseHandle, error) {
	s.identity = identity
	s.opened++
	if s.err != nil {
		return nil, s.err
	}
	if s.lease == nil {
		s.lease = &workspaceLeaseHandleStub{}
	}
	return s.lease, nil
}

func TestWorkspaceLambdaDirectoryPreservesSharedConnectionLimit(t *testing.T) {
	claims := workspacesession.Claims{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	reporter := &workspaceLeaseReporterStub{err: workspaceLeaseHTTPError{status: 429}}
	tokens := &gatewayTokenProviderStub{}
	ws := &gatewayWebSocketStub{}
	directory := &WorkspaceLambdaMicroVMStreamDirectory{ResourcePlaneID: "rp-1",
		Journal: &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: newRunningWorkspaceJournal(t, claims, "microvm-1"), ETag: `"running"`}},
		MicroVMs: &workspaceMicroVMReaderStub{output: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
			State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws")}},
		Tokens: tokens, Tickets: &AgentTicketIssuer{KeyID: "k1", PrivateKey: makePrivateKey(t), TTL: time.Minute},
		Leases: reporter, Region: "us-east-1", Origin: "https://gateway.hako.test", WebSocket: ws}
	if _, err := directory.OpenShell(context.Background(), claims); !errors.Is(err, ErrWorkspaceConnectionLimit) {
		t.Fatalf("connection lease limit error=%v, want ErrWorkspaceConnectionLimit", err)
	}
	if tokens.calls != 0 || ws.calls != 0 {
		t.Fatalf("rate-limited request reached MicroVM auth/dial: token=%d websocket=%d", tokens.calls, ws.calls)
	}
}

type workspaceLeaseHandleStub struct{ refreshed, closed int }

func (s *workspaceLeaseHandleStub) Refresh(context.Context) error { s.refreshed++; return nil }
func (s *workspaceLeaseHandleStub) Close() error                  { s.closed++; return nil }

func TestWorkspaceLambdaDirectoryCreatesAndReleasesAllocationBoundLease(t *testing.T) {
	claims := workspacesession.Claims{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	journal := newRunningWorkspaceJournal(t, claims, "microvm-1")
	reporter := &workspaceLeaseReporterStub{}
	stream := &gatewayTestStream{}
	directory := &WorkspaceLambdaMicroVMStreamDirectory{ResourcePlaneID: "rp-1",
		Journal: &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: journal, ETag: `"running"`}},
		MicroVMs: &workspaceMicroVMReaderStub{output: &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-1"),
			State: types.MicrovmStateRunning, Endpoint: aws.String("https://microvm-1.lambda-microvm.us-east-1.on.aws")}},
		Tokens: &gatewayTokenProviderStub{token: "token"}, Tickets: &AgentTicketIssuer{KeyID: "k1", PrivateKey: makePrivateKey(t), TTL: time.Minute},
		Leases: reporter, Region: "us-east-1", Origin: "https://gateway.hako.test", WebSocket: &gatewayWebSocketStub{stream: stream}}
	opened, err := directory.OpenShell(context.Background(), claims)
	if err != nil || reporter.opened != 1 || reporter.identity != (WorkspaceConnectionLeaseIdentity{TenantID: claims.TenantID,
		WorkspaceID: claims.WorkspaceID, ResourcePlaneID: claims.ResourcePlane, MicroVMID: "microvm-1", AllocationGeneration: 1}) {
		t.Fatalf("connection lease was not bound to current allocation before stream: identity=%+v opened=%d err=%v", reporter.identity, reporter.opened, err)
	}
	if err := opened.Close(); err != nil || reporter.lease.closed != 1 {
		t.Fatalf("closing Gateway stream did not release lease: closed=%d err=%v", reporter.lease.closed, err)
	}
}

func newRunningWorkspaceJournal(t *testing.T, claims workspacesession.Claims, providerID string) workspacejournal.Journal {
	t.Helper()
	now := time.Now().UTC()
	command := workspacejournal.Command{TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlaneID: claims.ResourcePlane,
		WorkspaceRevision: 1, OperationID: "op_ensure", Type: domain.OperationEnsureRunning}
	initial, err := workspacejournal.NewWithLaunchSpec(command, "launch_token", []byte(`{}`), now)
	if err != nil {
		t.Fatal(err)
	}
	initial.Revision++
	initial.State, initial.ProviderID, initial.UpdatedAt = workspacejournal.Running, providerID, now.Add(time.Second)
	if err := initial.Validate(); err != nil {
		t.Fatalf("build running test journal: %v", err)
	}
	return initial
}

func makePrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return private
}
