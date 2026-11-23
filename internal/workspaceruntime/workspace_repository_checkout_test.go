package workspaceruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

const workspaceCheckoutSecretTestToken = "ghs_workspace_token_never_archive_this"

type workspaceCheckoutProviderFake struct {
	grant WorkspaceRepositoryCheckoutGrant
	err   error
	calls int
}

func (f *workspaceCheckoutProviderFake) WorkspaceCheckoutToken(_ context.Context, identity githubbroker.WorkspaceCheckoutIdentity) (WorkspaceRepositoryCheckoutGrant, error) {
	f.calls++
	if identity.TenantID != "tenant_test" || identity.WorkspaceID != "ws_test" || identity.ResourcePlaneID != "rp-test" {
		return WorkspaceRepositoryCheckoutGrant{}, errors.New("wrong identity")
	}
	return f.grant, f.err
}

type workspaceCheckoutTreeFake struct {
	tree  WorkspaceRepositoryCheckoutTree
	calls int
	grant string
}

func (f *workspaceCheckoutTreeFake) Checkout(_ context.Context, _ resourcecontroller.Command, grant WorkspaceRepositoryCheckoutGrant) (WorkspaceRepositoryCheckoutTree, error) {
	f.calls++
	f.grant = grant.Token
	return f.tree, nil
}

type workspaceCheckoutGuestFake struct {
	err        error
	archive    []byte
	repository githubapp.RepositoryAccess
	commit     string
	calls      int
	localEdit  bool
}

func (f *workspaceCheckoutGuestFake) Install(_ context.Context, _ resourcecontroller.Command, _ string, repo githubapp.RepositoryAccess, commit string, source workloadsource.SourceArchive) error {
	f.calls++
	f.localEdit = false
	f.repository = repo
	f.commit = commit
	data, err := io.ReadAll(source.Body)
	if err != nil {
		return err
	}
	f.archive = data
	return f.err
}

type workspaceCheckoutReportFake struct {
	reports []WorkspaceRepositoryCheckoutReport
	err     error
}

func (f *workspaceCheckoutReportFake) Report(_ context.Context, report WorkspaceRepositoryCheckoutReport) error {
	f.reports = append(f.reports, report)
	return f.err
}

func workspaceCheckoutCommand() resourcecontroller.Command {
	return resourcecontroller.Command{SchemaVersion: 2, WorkspaceRevision: 1, OperationID: "op_test", TenantID: "tenant_test", WorkspaceID: "ws_test",
		ResourcePlaneID: "rp-test", Type: domain.OperationEnsureRunning, CreatedAt: time.Now().UTC(), GitHubInstallationID: 81, GitHubRepositoryID: 82,
		GitHubOwner: "acme", GitHubRepository: "demo", GitHubDefaultBranch: "main"}
}

func TestWorkspaceRepositoryCheckoutWorkflowArchivesPinnedGitTreeWithoutToken(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte(commit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("repo contents\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := workspaceCheckoutCommand()
	grant := WorkspaceRepositoryCheckoutGrant{Token: workspaceCheckoutSecretTestToken, ExpiresAt: time.Now().Add(time.Minute), Repository: githubapp.RepositoryAccess{Owner: command.GitHubOwner, Name: command.GitHubRepository}, CommitSHA: commit}
	provider := &workspaceCheckoutProviderFake{grant: grant}
	checkout := &workspaceCheckoutTreeFake{tree: WorkspaceRepositoryCheckoutTree{Path: repository, Cleanup: func() error { return nil }}}
	guest := &workspaceCheckoutGuestFake{}
	reports := &workspaceCheckoutReportFake{}
	workflow, err := NewWorkspaceRepositoryCheckoutWorkflow(provider, checkout, guest, reports, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.Execute(context.Background(), command, "microvm_test"); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || checkout.calls != 1 || checkout.grant != workspaceCheckoutSecretTestToken || guest.calls != 1 || guest.repository != grant.Repository || guest.commit != commit {
		t.Fatalf("checkout composition provider=%d checkout=%d guest=%+v", provider.calls, checkout.calls, guest)
	}
	if len(reports.reports) != 1 || reports.reports[0].State != "ready" || reports.reports[0].CommitSHA != commit || reports.reports[0].InstallationID != command.GitHubInstallationID {
		t.Fatalf("checkout report=%+v", reports.reports)
	}
	if bytes.Contains(guest.archive, []byte(workspaceCheckoutSecretTestToken)) {
		t.Fatal("repository archive contains the scoped installation token")
	}
	reader := tar.NewReader(bytes.NewReader(guest.archive))
	foundGitHead := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == ".git/HEAD" {
			foundGitHead = true
			data, _ := io.ReadAll(reader)
			if strings.TrimSpace(string(data)) != commit {
				t.Fatal("archived checkout has wrong HEAD")
			}
		}
	}
	if !foundGitHead {
		t.Fatal("repository archive omitted .git/HEAD")
	}
}

func TestWorkspaceRepositoryCheckoutWorkflowReportsFailedInstallAndRejectsForeignRepository(t *testing.T) {
	command := workspaceCheckoutCommand()
	commit := strings.Repeat("b", 40)
	grant := WorkspaceRepositoryCheckoutGrant{Token: workspaceCheckoutSecretTestToken, ExpiresAt: time.Now().Add(time.Minute),
		Repository: githubapp.RepositoryAccess{Owner: command.GitHubOwner, Name: command.GitHubRepository}, CommitSHA: commit}
	provider := &workspaceCheckoutProviderFake{grant: grant}
	checkout := &workspaceCheckoutTreeFake{tree: WorkspaceRepositoryCheckoutTree{Path: t.TempDir(), Cleanup: func() error { return nil }}}
	guest := &workspaceCheckoutGuestFake{err: errors.New("guest denied archive")}
	reports := &workspaceCheckoutReportFake{}
	workflow, err := NewWorkspaceRepositoryCheckoutWorkflow(provider, checkout, guest, reports, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.Execute(context.Background(), command, "microvm_test"); !errors.Is(err, ErrWorkspaceRepositoryCheckout) {
		t.Fatalf("failed guest install error=%v", err)
	}
	if len(reports.reports) != 1 || reports.reports[0].State != "failed" || reports.reports[0].CommitSHA != commit {
		t.Fatalf("failed checkout report=%+v", reports.reports)
	}

	foreign := grant
	foreign.Repository = githubapp.RepositoryAccess{Owner: "other", Name: "repository"}
	provider = &workspaceCheckoutProviderFake{grant: foreign}
	checkout = &workspaceCheckoutTreeFake{}
	reports = &workspaceCheckoutReportFake{}
	workflow, err = NewWorkspaceRepositoryCheckoutWorkflow(provider, checkout, guest, reports, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.Execute(context.Background(), command, "microvm_test"); !errors.Is(err, ErrWorkspaceRepositoryCheckout) {
		t.Fatalf("foreign repository grant error=%v", err)
	}
	if checkout.calls != 0 || len(reports.reports) != 1 || reports.reports[0].CommitSHA != "" || reports.reports[0].State != "failed" {
		t.Fatalf("foreign repository grant reached checkout: checkout=%d reports=%+v", checkout.calls, reports.reports)
	}
}

type workspaceRepositoryMicroVMFake struct{}

func (workspaceRepositoryMicroVMFake) GetMicrovm(_ context.Context, input *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	if aws.ToString(input.MicrovmIdentifier) != "microvm-test" {
		return nil, errors.New("unexpected MicroVM id")
	}
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("microvm-test"), Endpoint: aws.String("https://microvm-test.lambda-microvm.ap-northeast-1.on.aws")}, nil
}

type workspaceRepositoryTokenFake struct{}

func (workspaceRepositoryTokenFake) Create(_ context.Context, microVMID string) (string, error) {
	if microVMID != "microvm-test" {
		return "", errors.New("unexpected token target")
	}
	return "proxy-token", nil
}

type workspaceRepositoryTicketFake struct{}

func (workspaceRepositoryTicketFake) Issue(ticket workspaceagent.AgentTicket) (string, workspaceagent.AgentTicket, error) {
	return "agent-ticket", ticket, nil
}

type workspaceRepositoryStream struct {
	writes   bytes.Buffer
	response *bytes.Reader
	closed   bool
}

func (s *workspaceRepositoryStream) Write(value []byte) (int, error) { return s.writes.Write(value) }
func (s *workspaceRepositoryStream) Read(value []byte) (int, error)  { return s.response.Read(value) }
func (s *workspaceRepositoryStream) Close() error                    { s.closed = true; return nil }

type workspaceRepositoryWebSocketFake struct {
	stream                                               *workspaceRepositoryStream
	endpoint, workspace, origin, proxyToken, agentTicket string
	permission                                           workspacesession.Permission
	port                                                 uint16
}

func (f *workspaceRepositoryWebSocketFake) Dial(_ context.Context, endpoint, workspace, origin, proxyToken, agentTicket string, permission workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	f.endpoint, f.workspace, f.origin, f.proxyToken, f.agentTicket, f.permission, f.port = endpoint, workspace, origin, proxyToken, agentTicket, permission, port
	return f.stream, nil
}

func TestLambdaWorkspaceRepositoryGuestInstallerTransfersPinnedArchiveOnAuthorizedEndpoint(t *testing.T) {
	command := workspaceCheckoutCommand()
	commit := strings.Repeat("c", 40)
	var response bytes.Buffer
	if err := workspaceprotocol.WriteFileResponse(&response, workspaceprotocol.FileResponse{OK: true, Size: 12}); err != nil {
		t.Fatal(err)
	}
	stream := &workspaceRepositoryStream{response: bytes.NewReader(response.Bytes())}
	websocket := &workspaceRepositoryWebSocketFake{stream: stream}
	installer, err := NewLambdaWorkspaceRepositoryGuestInstaller(LambdaWorkspaceRepositoryGuestInstallerConfig{
		ResourcePlaneID: command.ResourcePlaneID, Region: "ap-northeast-1", GatewayOrigin: "https://gateway.example.test",
		MicroVMs: workspaceRepositoryMicroVMFake{}, Tokens: workspaceRepositoryTokenFake{}, Tickets: workspaceRepositoryTicketFake{}, WebSocket: websocket})
	if err != nil {
		t.Fatal(err)
	}
	archive := workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader([]byte("archive-body"))), Size: 12, SHA256: strings.Repeat("a", 64)}
	repository := githubapp.RepositoryAccess{Owner: "acme", Name: "demo"}
	if err := installer.Install(context.Background(), command, "microvm-test", repository, commit, archive); err != nil {
		t.Fatal(err)
	}
	request, err := workspaceprotocol.ReadFileRequest(bytes.NewReader(stream.writes.Bytes()))
	if err != nil || request.Operation != "install_repository" || request.Path != "/workspace/demo" || request.CommitSHA != commit || request.Digest != archive.SHA256 {
		t.Fatalf("Guest repository install frame=%+v err=%v", request, err)
	}
	transferred := stream.writes.Bytes()[len(stream.writes.Bytes())-len("archive-body"):]
	if string(transferred) != "archive-body" {
		t.Fatalf("Guest archive bytes=%q", transferred)
	}
	if stream.closed != true || websocket.permission != workspacesession.PermissionFileTransfer || websocket.origin != "https://gateway.example.test" || websocket.port != 0 || websocket.agentTicket != "agent-ticket" || websocket.proxyToken != "proxy-token" || websocket.workspace != "ws_test" {
		t.Fatalf("Guest transfer authorization not pinned: stream=%t websocket=%+v", stream.closed, websocket)
	}
}

type workspaceRepositoryRuntimeFake struct {
	commands     int
	commandTypes []domain.OperationType
}

func (f *workspaceRepositoryRuntimeFake) Execute(_ context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	f.commands++
	f.commandTypes = append(f.commandTypes, command.Type)
	switch command.Type {
	case domain.OperationSuspend:
		return domain.ObservedWorkspaceSuspended, nil
	case domain.OperationDelete:
		return domain.ObservedWorkspaceDeleted, nil
	default:
		return domain.ObservedWorkspaceRunning, nil
	}
}

func TestWorkspaceRepositoryCheckoutIsNotRepeatedAcrossSuspendResume(t *testing.T) {
	command := workspaceCheckoutCommand()
	commit := strings.Repeat("e", 40)
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte(commit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("pinned source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	grant := WorkspaceRepositoryCheckoutGrant{Token: workspaceCheckoutSecretTestToken, ExpiresAt: time.Now().Add(time.Minute),
		Repository: githubapp.RepositoryAccess{Owner: command.GitHubOwner, Name: command.GitHubRepository}, CommitSHA: commit}
	provider := &workspaceCheckoutProviderFake{grant: grant}
	checkout := &workspaceCheckoutTreeFake{tree: WorkspaceRepositoryCheckoutTree{Path: repository, Cleanup: func() error { return nil }}}
	guest := &workspaceCheckoutGuestFake{}
	reports := &workspaceCheckoutReportFake{}
	workflow, err := NewWorkspaceRepositoryCheckoutWorkflow(provider, checkout, guest, reports, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := &workspaceRepositoryRuntimeFake{}
	runtime, err := NewWorkspaceRepositoryRuntime(inner, workflow)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := runtime.Execute(context.Background(), command); err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("initial repository Workspace ensure state=%s err=%v", state, err)
	}
	// Model a developer edit after the initial install. Suspend/resume must
	// preserve the mounted Workspace tree and must not replay the checkout.
	guest.localEdit = true
	for _, operation := range []domain.OperationType{domain.OperationSuspend, domain.OperationResume} {
		action := command
		action.Type = operation
		action.OperationID = domain.OperationID("op_" + string(operation))
		action.WorkspaceRevision++
		state, err := runtime.Execute(context.Background(), action)
		if err != nil {
			t.Fatalf("Workspace %s state=%s err=%v", operation, state, err)
		}
		if operation == domain.OperationSuspend && state != domain.ObservedWorkspaceSuspended {
			t.Fatalf("Workspace suspend state=%s", state)
		}
	}
	if !guest.localEdit || provider.calls != 1 || checkout.calls != 1 || guest.calls != 1 || len(reports.reports) != 1 || reports.reports[0].State != "ready" {
		t.Fatalf("suspend/resume repeated checkout or lost edit: localEdit=%t broker=%d git=%d guest=%d reports=%+v",
			guest.localEdit, provider.calls, checkout.calls, guest.calls, reports.reports)
	}
}
func (*workspaceRepositoryRuntimeFake) ResourceBinding(context.Context, resourcecontroller.Command) (string, string, int64, error) {
	return "lambda_microvm", "microvm-test", 1, nil
}

func TestWorkspaceRepositoryRuntimeRunsCheckoutBeforeOperationSuccess(t *testing.T) {
	command := workspaceCheckoutCommand()
	commit := strings.Repeat("d", 40)
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "HEAD"), []byte(commit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &workspaceCheckoutProviderFake{grant: WorkspaceRepositoryCheckoutGrant{Token: workspaceCheckoutSecretTestToken, ExpiresAt: time.Now().Add(time.Minute),
		Repository: githubapp.RepositoryAccess{Owner: command.GitHubOwner, Name: command.GitHubRepository}, CommitSHA: commit}}
	checkout := &workspaceCheckoutTreeFake{tree: WorkspaceRepositoryCheckoutTree{Path: repository, Cleanup: func() error { return nil }}}
	guest := &workspaceCheckoutGuestFake{}
	reports := &workspaceCheckoutReportFake{}
	workflow, err := NewWorkspaceRepositoryCheckoutWorkflow(provider, checkout, guest, reports, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := &workspaceRepositoryRuntimeFake{}
	runtime, err := NewWorkspaceRepositoryRuntime(inner, workflow)
	if err != nil {
		t.Fatal(err)
	}
	state, err := runtime.Execute(context.Background(), command)
	if err != nil || state != domain.ObservedWorkspaceRunning || inner.commands != 1 || len(reports.reports) != 1 || reports.reports[0].State != "ready" {
		t.Fatalf("Workspace operation state=%s err=%v runtime calls=%d reports=%+v", state, err, inner.commands, reports.reports)
	}
	kind, id, generation, err := runtime.ResourceBinding(context.Background(), command)
	if err != nil || kind != "lambda_microvm" || id != "microvm-test" || generation != 1 {
		t.Fatalf("Workspace runtime binding=(%s,%s,%d) err=%v", kind, id, generation, err)
	}
}
