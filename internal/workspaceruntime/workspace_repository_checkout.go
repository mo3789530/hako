package workspaceruntime

import (
	"context"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/repositorycheckout"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacesession"
)

var ErrWorkspaceRepositoryCheckout = errors.New("Workspace repository checkout failed")
var workspaceRepositoryPlanePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type WorkspaceRepositoryCredentialProvider interface {
	WorkspaceCheckoutToken(context.Context, githubbroker.WorkspaceCheckoutIdentity) (WorkspaceRepositoryCheckoutGrant, error)
}

type WorkspaceRepositoryCheckoutGrant struct {
	Token      string
	ExpiresAt  time.Time
	Repository githubapp.RepositoryAccess
	CommitSHA  string
}

type WorkspaceRepositoryBrokerCredentialProvider struct {
	Provider interface {
		WorkspaceCheckoutToken(context.Context, githubbroker.WorkspaceCheckoutIdentity) (githubbroker.WorkspaceCheckoutGrant, error)
	}
}

func (p WorkspaceRepositoryBrokerCredentialProvider) WorkspaceCheckoutToken(ctx context.Context, identity githubbroker.WorkspaceCheckoutIdentity) (WorkspaceRepositoryCheckoutGrant, error) {
	if p.Provider == nil {
		return WorkspaceRepositoryCheckoutGrant{}, ErrWorkspaceRepositoryCheckout
	}
	grant, err := p.Provider.WorkspaceCheckoutToken(ctx, identity)
	if err != nil {
		return WorkspaceRepositoryCheckoutGrant{}, err
	}
	return WorkspaceRepositoryCheckoutGrant{Token: grant.Token(), ExpiresAt: grant.ExpiresAt(), Repository: grant.Repository(), CommitSHA: grant.CommitSHA()}, nil
}

type WorkspaceRepositoryReportSink interface {
	Report(context.Context, WorkspaceRepositoryCheckoutReport) error
}

type WorkspaceRepositoryGuestInstaller interface {
	Install(context.Context, resourcecontroller.Command, string, githubapp.RepositoryAccess, string, workloadsource.SourceArchive) error
}

type WorkspaceRepositoryCheckoutTree struct {
	Path    string
	Cleanup func() error
}

type WorkspaceRepositoryCheckout interface {
	Checkout(context.Context, resourcecontroller.Command, WorkspaceRepositoryCheckoutGrant) (WorkspaceRepositoryCheckoutTree, error)
}

type WorkspaceRepositoryCheckoutWorkflow struct {
	credentials WorkspaceRepositoryCredentialProvider
	checkout    WorkspaceRepositoryCheckout
	guest       WorkspaceRepositoryGuestInstaller
	reports     WorkspaceRepositoryReportSink
	now         func() time.Time
}

// WorkspaceRepositoryRuntime inserts first-repository checkout after the
// underlying runtime has started the initial Workspace and before the
// Resource Controller reports the operation as successful.
type WorkspaceRepositoryRuntime struct {
	runtime  resourcecontroller.Runtime
	workflow *WorkspaceRepositoryCheckoutWorkflow
}

func NewWorkspaceRepositoryRuntime(runtime resourcecontroller.Runtime, workflow *WorkspaceRepositoryCheckoutWorkflow) (*WorkspaceRepositoryRuntime, error) {
	if runtime == nil {
		return nil, errors.New("Workspace repository runtime is required")
	}
	return &WorkspaceRepositoryRuntime{runtime: runtime, workflow: workflow}, nil
}

func (r *WorkspaceRepositoryRuntime) Execute(ctx context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	if r == nil || r.runtime == nil {
		return domain.ObservedWorkspaceFailed, ErrWorkspaceRepositoryCheckout
	}
	state, err := r.runtime.Execute(ctx, command)
	if err != nil || command.Type != domain.OperationEnsureRunning || command.GitHubRepositoryID == 0 || state != domain.ObservedWorkspaceRunning {
		return state, err
	}
	if r.workflow == nil {
		return domain.ObservedWorkspaceFailed, ErrWorkspaceRepositoryCheckout
	}
	reporter, ok := r.runtime.(resourcecontroller.ResourceBindingReporter)
	if !ok {
		return domain.ObservedWorkspaceFailed, ErrWorkspaceRepositoryCheckout
	}
	kind, microVMID, generation, err := reporter.ResourceBinding(ctx, command)
	if err != nil || kind != "lambda_microvm" || !validWorkspaceMicroVMID(microVMID) || generation < 1 {
		return domain.ObservedWorkspaceFailed, ErrWorkspaceRepositoryCheckout
	}
	if err := r.workflow.Execute(ctx, command, microVMID); err != nil {
		return domain.ObservedWorkspaceFailed, err
	}
	return state, nil
}

func (r *WorkspaceRepositoryRuntime) ResourceBinding(ctx context.Context, command resourcecontroller.Command) (string, string, int64, error) {
	if r == nil || r.runtime == nil {
		return "", "", 0, ErrWorkspaceRepositoryCheckout
	}
	reporter, ok := r.runtime.(resourcecontroller.ResourceBindingReporter)
	if !ok {
		return "", "", 0, ErrWorkspaceRepositoryCheckout
	}
	return reporter.ResourceBinding(ctx, command)
}

func NewWorkspaceRepositoryCheckoutWorkflow(credentials WorkspaceRepositoryCredentialProvider, checkout WorkspaceRepositoryCheckout,
	guest WorkspaceRepositoryGuestInstaller, reports WorkspaceRepositoryReportSink, now func() time.Time) (*WorkspaceRepositoryCheckoutWorkflow, error) {
	if credentials == nil || checkout == nil || guest == nil || reports == nil {
		return nil, errors.New("Workspace repository checkout dependencies are required")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &WorkspaceRepositoryCheckoutWorkflow{credentials: credentials, checkout: checkout, guest: guest, reports: reports, now: now}, nil
}

// Execute resolves the Tenant-pinned Broker grant, fetches exactly its commit,
// transfers a credential-free archive to the Guest and records the result.
func (w *WorkspaceRepositoryCheckoutWorkflow) Execute(ctx context.Context, command resourcecontroller.Command, microVMID string) error {
	if w == nil || ctx == nil || ctx.Err() != nil || command.Type != domain.OperationEnsureRunning || command.SchemaVersion != 2 || command.WorkspaceRevision < 1 ||
		command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" || command.GitHubInstallationID < 1 || command.GitHubRepositoryID < 1 ||
		!validWorkspaceMicroVMID(microVMID) || !validCheckoutRepositoryPart(command.GitHubOwner, 39) || !validCheckoutRepositoryPart(command.GitHubRepository, 100) || command.GitHubDefaultBranch == "" {
		return ErrWorkspaceRepositoryCheckout
	}
	identity := githubbroker.WorkspaceCheckoutIdentity{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID, ResourcePlaneID: command.ResourcePlaneID}
	grant, err := w.credentials.WorkspaceCheckoutToken(ctx, identity)
	if err != nil || !validWorkspaceRepositoryGrant(grant, command, w.now()) {
		_ = w.report(ctx, command, "", "failed")
		return ErrWorkspaceRepositoryCheckout
	}
	tree, err := w.checkout.Checkout(ctx, command, grant)
	if err != nil || strings.TrimSpace(tree.Path) == "" || tree.Cleanup == nil {
		if tree.Cleanup != nil {
			_ = tree.Cleanup()
		}
		_ = w.report(ctx, command, grant.CommitSHA, "failed")
		return ErrWorkspaceRepositoryCheckout
	}
	archive, err := workloadsource.CreateWorkspaceRepositoryArchive(ctx, tree.Path, workloadsource.DefaultMaxArchiveBytes)
	if err != nil || archive == nil {
		_ = tree.Cleanup()
		_ = w.report(ctx, command, grant.CommitSHA, "failed")
		return ErrWorkspaceRepositoryCheckout
	}
	source := workloadsource.SourceArchive{Body: io.NopCloser(archive.Reader()), Size: archive.Size(), SHA256: archive.SHA256()}
	installErr := w.guest.Install(ctx, command, microVMID, grant.Repository, grant.CommitSHA, source)
	closeErr := source.Body.Close()
	archiveErr := archive.Close()
	cleanupErr := tree.Cleanup()
	if installErr != nil || closeErr != nil || archiveErr != nil || cleanupErr != nil {
		_ = w.report(ctx, command, grant.CommitSHA, "failed")
		return ErrWorkspaceRepositoryCheckout
	}
	if err := w.report(ctx, command, grant.CommitSHA, "ready"); err != nil {
		return ErrWorkspaceRepositoryCheckout
	}
	return nil
}

func validWorkspaceMicroVMID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_-.:/", r)) {
			return false
		}
	}
	return true
}

type workspaceRepositoryMicroVMAPI interface {
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
}

type LambdaWorkspaceRepositoryGuestInstaller struct {
	plane     domain.ResourcePlaneID
	region    string
	origin    string
	api       workspaceRepositoryMicroVMAPI
	tokens    workspaceagent.WorkspaceAgentTokenProvider
	tickets   workspaceagent.WorkspaceAgentTicketIssuer
	websocket workspaceagent.WorkspaceWebSocketDialer
}

type LambdaWorkspaceRepositoryGuestInstallerConfig struct {
	ResourcePlaneID domain.ResourcePlaneID
	Region          string
	GatewayOrigin   string
	MicroVMs        workspaceRepositoryMicroVMAPI
	Tokens          workspaceagent.WorkspaceAgentTokenProvider
	Tickets         workspaceagent.WorkspaceAgentTicketIssuer
	WebSocket       workspaceagent.WorkspaceWebSocketDialer
}

func NewLambdaWorkspaceRepositoryGuestInstaller(config LambdaWorkspaceRepositoryGuestInstallerConfig) (*LambdaWorkspaceRepositoryGuestInstaller, error) {
	origin, err := url.Parse(strings.TrimSpace(config.GatewayOrigin))
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
		!workspaceRepositoryPlanePattern.MatchString(string(config.ResourcePlaneID)) || strings.TrimSpace(config.Region) == "" || config.MicroVMs == nil || config.Tokens == nil || config.Tickets == nil {
		return nil, ErrWorkspaceRepositoryCheckout
	}
	return &LambdaWorkspaceRepositoryGuestInstaller{plane: config.ResourcePlaneID, region: strings.TrimSpace(config.Region), origin: origin.String(), api: config.MicroVMs,
		tokens: config.Tokens, tickets: config.Tickets, websocket: config.WebSocket}, nil
}

func (i *LambdaWorkspaceRepositoryGuestInstaller) Install(ctx context.Context, command resourcecontroller.Command, microVMID string, repository githubapp.RepositoryAccess,
	commitSHA string, archive workloadsource.SourceArchive) error {
	if i == nil || ctx == nil || ctx.Err() != nil || command.ResourcePlaneID != i.plane || !validWorkspaceMicroVMID(microVMID) || !validCheckoutRepositoryPart(repository.Name, 100) ||
		!validCheckoutRepositoryPart(repository.Owner, 39) || !validWorkspaceRepositoryCommitSHA(commitSHA) || archive.Body == nil || archive.Size < 1 || archive.Size > workloadsource.DefaultMaxArchiveBytes || len(archive.SHA256) != 64 {
		return ErrWorkspaceRepositoryCheckout
	}
	vm, err := i.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(microVMID)})
	if err != nil || vm == nil || aws.ToString(vm.MicrovmId) != microVMID || aws.ToString(vm.Endpoint) == "" {
		return ErrWorkspaceRepositoryCheckout
	}
	dialer, err := workspaceagent.NewLambdaMicroVMGatewayDialer(workspaceagent.LambdaMicroVMGatewayDialerConfig{
		Endpoint: workspaceagent.WorkspaceRuntimeEndpoint{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID, ResourcePlane: command.ResourcePlaneID,
			MicroVMID: microVMID, Region: i.region, Endpoint: aws.ToString(vm.Endpoint)}, Token: i.tokens, Ticket: i.tickets, Origin: i.origin, WebSocket: i.websocket})
	if err != nil {
		return ErrWorkspaceRepositoryCheckout
	}
	claims := workspacesession.Claims{UserID: domain.UserID("hako-repository-checkout"), TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlane: command.ResourcePlaneID, Permissions: []workspacesession.Permission{workspacesession.PermissionFileTransfer}}
	stream, err := dialer.OpenFileTransfer(ctx, claims)
	if err != nil {
		return ErrWorkspaceRepositoryCheckout
	}
	installErr := workspaceagent.TransferWorkspaceRepositoryArchive(ctx, stream, archive, repository.Name, commitSHA, archive.SHA256)
	_ = stream.Close()
	if installErr != nil {
		return ErrWorkspaceRepositoryCheckout
	}
	return nil
}

func (w *WorkspaceRepositoryCheckoutWorkflow) report(ctx context.Context, command resourcecontroller.Command, commitSHA, state string) error {
	return w.reports.Report(ctx, WorkspaceRepositoryCheckoutReport{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, InstallationID: command.GitHubInstallationID, RepositoryID: command.GitHubRepositoryID,
		CommitSHA: commitSHA, State: state, ObservedAt: w.now().UTC()})
}

func validWorkspaceRepositoryGrant(grant WorkspaceRepositoryCheckoutGrant, command resourcecontroller.Command, now time.Time) bool {
	return !now.IsZero() && validWorkspaceRepositoryToken(grant.Token) && grant.ExpiresAt.After(now) &&
		grant.Repository == (githubapp.RepositoryAccess{Owner: command.GitHubOwner, Name: command.GitHubRepository}) &&
		validWorkspaceRepositoryCommitSHA(grant.CommitSHA)
}

func validWorkspaceRepositoryToken(value string) bool {
	return len(value) >= 16 && len(value) <= 4096 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func validCheckoutRepositoryPart(value string, max int) bool {
	if value == "" || len(value) > max || value == "." || value == ".." || value[0] == '-' {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

type workspaceCheckoutCredential struct {
	tenant                   domain.TenantID
	installation, repository int64
	token                    string
}

func (c workspaceCheckoutCredential) CheckoutRunToken(_ context.Context, run domain.WorkloadRun) (string, error) {
	if run.TenantID != c.tenant || run.GitHubInstallation != c.installation || run.GitHubRepository != c.repository || !validWorkspaceRepositoryToken(c.token) {
		return "", repositorycheckout.ErrCheckoutCredentialFailure
	}
	return c.token, nil
}

type workspaceCheckoutAuthorizer struct {
	tenant                   domain.TenantID
	installation, repository int64
	access                   githubapp.RepositoryAccess
}

func (a workspaceCheckoutAuthorizer) AuthorizeRepository(_ context.Context, tenant domain.TenantID, installation, repository int64) (githubapp.RepositoryAccess, error) {
	if tenant != a.tenant || installation != a.installation || repository != a.repository || a.access.Owner == "" || a.access.Name == "" {
		return githubapp.RepositoryAccess{}, repositorycheckout.ErrCheckoutNotAuthorized
	}
	return a.access, nil
}

// GitWorkspaceRepositoryCheckout fetches the immutable commit with the
// one-Repository Broker token. GitRepositoryCheckout uses askpass over an
// inherited pipe and does not persist the token in origin or .git/config.
type GitWorkspaceRepositoryCheckout struct{ now func() time.Time }

func NewGitWorkspaceRepositoryCheckout(now func() time.Time) *GitWorkspaceRepositoryCheckout {
	if now == nil {
		now = time.Now
	}
	return &GitWorkspaceRepositoryCheckout{now: now}
}
func (g *GitWorkspaceRepositoryCheckout) Checkout(ctx context.Context, command resourcecontroller.Command, grant WorkspaceRepositoryCheckoutGrant) (WorkspaceRepositoryCheckoutTree, error) {
	if g == nil || ctx == nil || !validWorkspaceRepositoryGrant(grant, command, g.now()) {
		return WorkspaceRepositoryCheckoutTree{}, ErrWorkspaceRepositoryCheckout
	}
	now := g.now().UTC()
	run := domain.WorkloadRun{ID: domain.WorkloadRunID(command.WorkspaceID), TenantID: command.TenantID, Kind: domain.WorkloadJob,
		GitHubInstallation: command.GitHubInstallationID, GitHubRepository: command.GitHubRepositoryID,
		GitHubIntegrationID: "workspace-repository-checkout", GitHubConfigRevision: 1, GitHubCredentialGeneration: 1,
		CommitSHA: grant.CommitSHA, DesiredState: domain.WorkloadDesiredRunning, State: domain.WorkloadProvisioning, CreatedAt: now, TimeoutAt: now.Add(10 * time.Minute)}
	credentials := workspaceCheckoutCredential{tenant: command.TenantID, installation: command.GitHubInstallationID, repository: command.GitHubRepositoryID, token: grant.Token}
	legacy := repositorycheckout.CredentialProviderFunc(func(context.Context, domain.TenantID, int64, int64) (string, error) {
		return "", repositorycheckout.ErrCheckoutCredentialFailure
	})
	checkout, err := repositorycheckout.NewGitRepositoryCheckoutWithRunCredentials(
		workspaceCheckoutAuthorizer{tenant: command.TenantID, installation: command.GitHubInstallationID, repository: command.GitHubRepositoryID, access: grant.Repository}, legacy, credentials)
	if err != nil {
		return WorkspaceRepositoryCheckoutTree{}, ErrWorkspaceRepositoryCheckout
	}
	checked, err := checkout.Checkout(ctx, run)
	if err != nil {
		return WorkspaceRepositoryCheckoutTree{}, ErrWorkspaceRepositoryCheckout
	}
	return WorkspaceRepositoryCheckoutTree{Path: checked.Path, Cleanup: checked.Cleanup}, nil
}

var _ repositorycheckout.RunCredentialProvider = workspaceCheckoutCredential{}
var _ githubapp.RepositoryAuthorizer = workspaceCheckoutAuthorizer{}
