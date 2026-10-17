// Package workspaceruntime implements the Workspace-specific Lambda MicroVM
// lifecycle adapter. It is intentionally separate from Workload Job runtime.
package workspaceruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

const MaxMicroVMRuntime = 8 * time.Hour

var (
	ErrSpecification = errors.New("Workspace Lambda MicroVM specification is invalid or unsupported")
	ErrProviderState = errors.New("Lambda MicroVM returned an unsupported lifecycle state")
	arnPattern       = regexp.MustCompile(`^arn:aws:[a-z0-9-]*:[a-z0-9-]*:[0-9]{12}:[a-z0-9-]+[:/][A-Za-z0-9._:/-]+$`)
)

type LambdaAPI interface {
	RunMicrovm(context.Context, *lambdamicrovms.RunMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error)
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	SuspendMicrovm(context.Context, *lambdamicrovms.SuspendMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.SuspendMicrovmOutput, error)
	ResumeMicrovm(context.Context, *lambdamicrovms.ResumeMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error)
	TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
}

// LaunchSpec is resolved from trusted Resource Plane configuration. It is
// frozen in the journal before RunMicrovm and is never accepted from a command.
type LaunchSpec struct {
	RuntimeClass             string   `json:"runtime_class"`
	ImageIdentifier          string   `json:"image_identifier"`
	ImageVersion             string   `json:"image_version"`
	ExecutionRoleARN         string   `json:"execution_role_arn"`
	EgressNetworkConnectors  []string `json:"egress_network_connectors"`
	IngressNetworkConnectors []string `json:"ingress_network_connectors"`
	MaximumDurationInSeconds int32    `json:"maximum_duration_in_seconds"`
	AutoResumeEnabled        bool     `json:"auto_resume_enabled"`
	MaxIdleDurationSeconds   int32    `json:"max_idle_duration_seconds"`
	SuspendedDurationSeconds int32    `json:"suspended_duration_seconds"`
	RunHookPayload           string   `json:"run_hook_payload,omitempty"`
}

type SpecResolver interface {
	Resolve(context.Context, resourcecontroller.Command) (LaunchSpec, error)
}

// StorageProvisioner owns the durable Workspace EFS mapping. The provisioner
// must not expose filesystem credentials or secret material to the guest.
type StorageProvisioner interface {
	Ensure(context.Context, workspacestorage.Identity) (workspacestorage.Binding, error)
	DeleteOwned(context.Context, workspacestorage.Identity, workspacestorage.Binding) error
}

type Config struct {
	SupportedRuntimeClasses map[string]bool
	PollInterval            time.Duration
	PollTimeout             time.Duration
}

type LambdaMicroVM struct {
	api      LambdaAPI
	store    workspacejournal.Store
	resolver SpecResolver
	storage  StorageProvisioner
	config   Config
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func NewLambdaMicroVM(api LambdaAPI, store workspacejournal.Store, resolver SpecResolver, config Config, now func() time.Time) (*LambdaMicroVM, error) {
	return NewLambdaMicroVMWithStorage(api, store, resolver, nil, config, now)
}

func NewLambdaMicroVMWithStorage(api LambdaAPI, store workspacejournal.Store, resolver SpecResolver, storage StorageProvisioner, config Config, now func() time.Time) (*LambdaMicroVM, error) {
	if api == nil || store == nil || resolver == nil || len(config.SupportedRuntimeClasses) == 0 {
		return nil, errors.New("Lambda MicroVM API, journal store, trusted spec resolver, and supported runtime classes are required")
	}
	for class, supported := range config.SupportedRuntimeClasses {
		if strings.TrimSpace(class) == "" || !supported {
			return nil, errors.New("only explicitly enabled Workspace runtime classes are allowed")
		}
	}
	if config.PollInterval == 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.PollTimeout == 0 {
		config.PollTimeout = 30 * time.Second
	}
	if config.PollInterval < 10*time.Millisecond || config.PollTimeout <= 0 {
		return nil, errors.New("Workspace MicroVM polling interval and timeout must be positive and bounded")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &LambdaMicroVM{api: api, store: store, resolver: resolver, storage: storage, config: config, now: now, sleep: sleepContext}, nil
}

func (r *LambdaMicroVM) Execute(ctx context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	if command.SchemaVersion != 2 || command.WorkspaceRevision < 1 || command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" || command.OperationID == "" {
		return domain.ObservedWorkspaceFailed, ErrSpecification
	}
	identity := workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: command.WorkspaceRevision,
		OperationID: command.OperationID, Type: command.Type}
	snapshot, err := r.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if errors.Is(err, workspacejournal.ErrNotFound) {
		var initial workspacejournal.Journal
		if command.Type == domain.OperationEnsureRunning {
			spec, resolveErr := r.resolver.Resolve(ctx, command)
			if resolveErr != nil || r.validateSpec(spec) != nil {
				return domain.ObservedWorkspaceFailed, ErrSpecification
			}
			var storageBinding *workspacestorage.Binding
			if r.storage != nil {
				binding, storageErr := r.storage.Ensure(ctx, storageIdentity(command))
				if storageErr != nil {
					return domain.ObservedWorkspaceFailed, storageErr
				}
				storageBinding = &binding
			}
			payload, token, resolveErr := r.launchRequestForSpec(spec, command, storageBinding)
			if resolveErr != nil {
				return domain.ObservedWorkspaceFailed, resolveErr
			}
			initial, err = workspacejournal.NewWithLaunchSpecAndStorage(identity, token, payload, storageBinding, r.now())
		} else if command.Type == domain.OperationDelete {
			initial, err = workspacejournal.New(identity, "", "", r.now())
		} else {
			return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
		}
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		snapshot, err = r.store.Create(ctx, initial)
		if errors.Is(err, workspacejournal.ErrConflict) {
			snapshot, err = r.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
		}
	}
	if err != nil {
		return domain.ObservedWorkspaceFailed, err
	}
	if snapshot.ETag == "" || snapshot.Journal.Validate() != nil || snapshot.Journal.TenantID != command.TenantID || snapshot.Journal.WorkspaceID != command.WorkspaceID || snapshot.Journal.ResourcePlaneID != command.ResourcePlaneID {
		return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
	}

	var token string
	var payload []byte
	if command.Type == domain.OperationEnsureRunning {
		if snapshot.Journal.State == workspacejournal.Terminated {
			if r.storage != nil && snapshot.Journal.StorageBinding == nil {
				return domain.ObservedWorkspaceFailed, errors.New("Workspace runtime journal has no durable EFS binding; refusing data-loss replacement")
			}
			var spec LaunchSpec
			spec, err = r.resolver.Resolve(ctx, command)
			if err == nil {
				err = r.validateSpec(spec)
			}
			if err == nil {
				payload, token, err = r.launchRequestForSpec(spec, command, snapshot.Journal.StorageBinding)
			}
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
		} else if command.WorkspaceRevision == snapshot.Journal.WorkspaceRevision && command.OperationID == snapshot.Journal.OperationID && command.Type == snapshot.Journal.OperationType {
			payload = append([]byte(nil), snapshot.Journal.LaunchSpecPayload...)
			token = snapshot.Journal.LaunchToken
		}
	}
	next, applyErr := workspacejournal.ApplyCommandWithLaunchSpec(snapshot, identity, token, payload, r.now())
	if errors.Is(applyErr, workspacejournal.ErrDuplicate) {
		applyErr = nil
	} else if applyErr == nil {
		snapshot, err = r.store.CompareAndSwap(ctx, snapshot, next)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
	}
	if applyErr != nil {
		return domain.ObservedWorkspaceFailed, applyErr
	}
	return r.reconcile(ctx, snapshot)
}

func (r *LambdaMicroVM) ResourceBinding(ctx context.Context, command resourcecontroller.Command) (string, string, error) {
	snapshot, err := r.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil {
		return "", "", err
	}
	j := snapshot.Journal
	if snapshot.ETag == "" || j.Validate() != nil || j.TenantID != command.TenantID || j.WorkspaceID != command.WorkspaceID ||
		j.ResourcePlaneID != command.ResourcePlaneID || j.WorkspaceRevision != command.WorkspaceRevision || j.OperationID != command.OperationID || j.OperationType != command.Type || j.ProviderID == "" || j.Tombstone {
		return "", "", workspacejournal.ErrConflict
	}
	return "lambda_microvm", j.ProviderID, nil
}

func (r *LambdaMicroVM) reconcile(ctx context.Context, snapshot workspacejournal.Snapshot) (domain.ObservedWorkspaceState, error) {
	j := snapshot.Journal
	if j.State == workspacejournal.Deleted {
		return domain.ObservedWorkspaceDeleted, nil
	}
	if j.State == workspacejournal.Failed && !j.Tombstone {
		return domain.ObservedWorkspaceFailed, errors.New("Workspace MicroVM is in a failed state")
	}
	if j.State == workspacejournal.Provisioning && j.ProviderID == "" {
		input, err := launchInput(j)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		output, err := r.api.RunMicrovm(ctx, input)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		if output == nil || output.MicrovmId == nil || strings.TrimSpace(*output.MicrovmId) == "" {
			return domain.ObservedWorkspaceFailed, errors.New("RunMicrovm returned no MicroVM ID")
		}
		snapshot, err = r.advance(ctx, snapshot, workspacejournal.Provisioning, *output.MicrovmId, j.CleanupState)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		j = snapshot.Journal
	}
	if j.Tombstone {
		return r.delete(ctx, snapshot)
	}
	if j.ProviderID == "" {
		return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
	}

	if j.OperationType == domain.OperationEnsureRunning && j.State == workspacejournal.Running {
		return domain.ObservedWorkspaceRunning, nil
	}
	if j.OperationType == domain.OperationSuspend && j.State == workspacejournal.Suspended {
		return domain.ObservedWorkspaceSuspended, nil
	}
	if j.OperationType == domain.OperationResume && j.State == workspacejournal.Running {
		return domain.ObservedWorkspaceRunning, nil
	}

	if j.State == workspacejournal.Suspending {
		observed, err := r.get(ctx, j)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		if observed.State == types.MicrovmStateSuspended {
			snapshot, err = r.advance(ctx, snapshot, workspacejournal.Suspended, j.ProviderID, j.CleanupState)
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
			return domain.ObservedWorkspaceSuspended, nil
		}
		if observed.State == types.MicrovmStateRunning {
			if _, err := r.api.SuspendMicrovm(ctx, &lambdamicrovms.SuspendMicrovmInput{MicrovmIdentifier: aws.String(j.ProviderID)}); err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
		} else if observed.State != types.MicrovmStateSuspending {
			return domain.ObservedWorkspaceFailed, fmt.Errorf("cannot suspend MicroVM in state %q", observed.State)
		}
		return r.waitFor(ctx, snapshot, types.MicrovmStateSuspended, workspacejournal.Suspended)
	}
	if j.State == workspacejournal.Resuming {
		observed, err := r.get(ctx, j)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		if observed.State == types.MicrovmStateRunning {
			snapshot, err = r.advance(ctx, snapshot, workspacejournal.Running, j.ProviderID, j.CleanupState)
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
			return domain.ObservedWorkspaceRunning, nil
		}
		if observed.State == types.MicrovmStateSuspended {
			if _, err := r.api.ResumeMicrovm(ctx, &lambdamicrovms.ResumeMicrovmInput{MicrovmIdentifier: aws.String(j.ProviderID)}); err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
		} else if observed.State != types.MicrovmStateSuspending && observed.State != types.MicrovmStatePending {
			return domain.ObservedWorkspaceFailed, fmt.Errorf("cannot resume MicroVM in state %q", observed.State)
		}
		return r.waitFor(ctx, snapshot, types.MicrovmStateRunning, workspacejournal.Running)
	}
	if j.State == workspacejournal.Provisioning {
		return r.waitFor(ctx, snapshot, types.MicrovmStateRunning, workspacejournal.Running)
	}
	if j.State == workspacejournal.Running {
		return domain.ObservedWorkspaceRunning, nil
	}
	if j.State == workspacejournal.Suspended {
		return domain.ObservedWorkspaceSuspended, nil
	}
	return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
}

func (r *LambdaMicroVM) delete(ctx context.Context, snapshot workspacejournal.Snapshot) (domain.ObservedWorkspaceState, error) {
	j := snapshot.Journal
	if j.StorageBinding != nil && r.storage == nil {
		return domain.ObservedWorkspaceFailed, errors.New("Workspace EFS binding exists but no storage provisioner is configured")
	}
	if j.ProviderID == "" {
		if j.AllocationGeneration == 0 {
			return domain.ObservedWorkspaceDeleted, nil
		}
		// RunMicrovm may have succeeded while its response was lost. The exact
		// immutable payload and token are replayed to recover the provider ID.
		input, err := launchInput(j)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		output, err := r.api.RunMicrovm(ctx, input)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		if output == nil || output.MicrovmId == nil || strings.TrimSpace(*output.MicrovmId) == "" {
			return domain.ObservedWorkspaceFailed, errors.New("RunMicrovm recovery returned no MicroVM ID")
		}
		var advanceErr error
		snapshot, advanceErr = r.advance(ctx, snapshot, workspacejournal.Deleting, *output.MicrovmId, workspacejournal.CleanupPending)
		if advanceErr != nil {
			return domain.ObservedWorkspaceFailed, advanceErr
		}
		j = snapshot.Journal
	}
	if j.State == workspacejournal.Failed {
		var err error
		snapshot, err = r.advance(ctx, snapshot, workspacejournal.Deleting, j.ProviderID, workspacejournal.CleanupPending)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		j = snapshot.Journal
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, r.config.PollTimeout)
	defer cancel()
	for {
		output, err := r.get(deadlineCtx, j)
		if err != nil {
			_ = r.markCleanupFailed(ctx, snapshot)
			return domain.ObservedWorkspaceFailed, err
		}
		if output.State == types.MicrovmStateTerminated {
			if j.StorageBinding != nil {
				if err := r.storage.DeleteOwned(deadlineCtx, storageIdentityFromJournal(j), *j.StorageBinding); err != nil {
					_ = r.markCleanupFailed(ctx, snapshot)
					return domain.ObservedWorkspaceFailed, err
				}
			}
			_, err := r.advance(deadlineCtx, snapshot, workspacejournal.Deleted, j.ProviderID, workspacejournal.CleanupConfirmed)
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
			return domain.ObservedWorkspaceDeleted, nil
		}
		if output.State == types.MicrovmStatePending || output.State == types.MicrovmStateRunning || output.State == types.MicrovmStateSuspending || output.State == types.MicrovmStateSuspended {
			if _, err := r.api.TerminateMicrovm(deadlineCtx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(j.ProviderID)}); err != nil {
				_ = r.markCleanupFailed(ctx, snapshot)
				return domain.ObservedWorkspaceFailed, err
			}
		} else if output.State != types.MicrovmStateTerminating {
			_ = r.markCleanupFailed(ctx, snapshot)
			return domain.ObservedWorkspaceFailed, fmt.Errorf("%w: %q", ErrProviderState, output.State)
		}
		if err := r.sleep(deadlineCtx, r.config.PollInterval); err != nil {
			_ = r.markCleanupFailed(ctx, snapshot)
			return domain.ObservedWorkspaceDeleting, err
		}
	}
}

func (r *LambdaMicroVM) markCleanupFailed(ctx context.Context, current workspacejournal.Snapshot) error {
	if current.Journal.Tombstone && current.Journal.CleanupState == workspacejournal.CleanupFailed && current.Journal.State == workspacejournal.Failed {
		return nil
	}
	next, err := workspacejournal.Advance(current, workspacejournal.Failed, current.Journal.ProviderID, workspacejournal.CleanupFailed, r.now())
	if err != nil {
		return err
	}
	_, err = r.store.CompareAndSwap(ctx, current, next)
	return err
}

func (r *LambdaMicroVM) waitFor(ctx context.Context, snapshot workspacejournal.Snapshot, target types.MicrovmState, state workspacejournal.State) (domain.ObservedWorkspaceState, error) {
	j := snapshot.Journal
	deadlineCtx, cancel := context.WithTimeout(ctx, r.config.PollTimeout)
	defer cancel()
	for {
		output, err := r.get(deadlineCtx, j)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		if err := verifyMicroVM(j, output); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		if output.State == target {
			snapshot, err = r.advance(deadlineCtx, snapshot, state, j.ProviderID, j.CleanupState)
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
			if state == workspacejournal.Suspended {
				return domain.ObservedWorkspaceSuspended, nil
			}
			return domain.ObservedWorkspaceRunning, nil
		}
		if output.State == types.MicrovmStateTerminated || output.State == types.MicrovmStateTerminating {
			return domain.ObservedWorkspaceFailed, fmt.Errorf("MicroVM terminated before reaching %s", target)
		}
		if output.State != types.MicrovmStatePending && output.State != types.MicrovmStateRunning && output.State != types.MicrovmStateSuspending && output.State != types.MicrovmStateSuspended {
			return domain.ObservedWorkspaceFailed, fmt.Errorf("%w: %q", ErrProviderState, output.State)
		}
		if err := r.sleep(deadlineCtx, r.config.PollInterval); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
	}
}

func (r *LambdaMicroVM) get(ctx context.Context, j workspacejournal.Journal) (*lambdamicrovms.GetMicrovmOutput, error) {
	output, err := r.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(j.ProviderID)})
	if err != nil {
		return nil, err
	}
	if output == nil || output.MicrovmId == nil || *output.MicrovmId != j.ProviderID {
		return nil, errors.New("GetMicrovm returned a mismatched provider identity")
	}
	if err := verifyMicroVM(j, output); err != nil {
		return nil, err
	}
	return output, nil
}

func verifyMicroVM(j workspacejournal.Journal, output *lambdamicrovms.GetMicrovmOutput) error {
	var spec LaunchSpec
	if err := json.Unmarshal(j.LaunchSpecPayload, &spec); err != nil {
		return ErrSpecification
	}
	if output.ImageArn == nil || *output.ImageArn != spec.ImageIdentifier || output.ImageVersion == nil || *output.ImageVersion != spec.ImageVersion ||
		output.ExecutionRoleArn == nil || *output.ExecutionRoleArn != spec.ExecutionRoleARN ||
		!sameStrings(output.EgressNetworkConnectors, spec.EgressNetworkConnectors) || !sameStrings(output.IngressNetworkConnectors, spec.IngressNetworkConnectors) {
		return errors.New("Lambda MicroVM does not match its pinned Workspace allocation specification")
	}
	return nil
}

func (r *LambdaMicroVM) advance(ctx context.Context, current workspacejournal.Snapshot, state workspacejournal.State, providerID string, cleanup workspacejournal.CleanupState) (workspacejournal.Snapshot, error) {
	next, err := workspacejournal.Advance(current, state, providerID, cleanup, r.now())
	if err != nil {
		return workspacejournal.Snapshot{}, err
	}
	return r.store.CompareAndSwap(ctx, current, next)
}

func (r *LambdaMicroVM) launchRequest(ctx context.Context, command resourcecontroller.Command, storageBinding *workspacestorage.Binding) (LaunchSpec, []byte, string, error) {
	spec, err := r.resolver.Resolve(ctx, command)
	if err != nil {
		return LaunchSpec{}, nil, "", err
	}
	if err := r.validateSpec(spec); err != nil {
		return LaunchSpec{}, nil, "", err
	}
	payload, token, err := r.launchRequestForSpec(spec, command, storageBinding)
	return spec, payload, token, err
}

func (r *LambdaMicroVM) launchRequestForSpec(spec LaunchSpec, command resourcecontroller.Command, storageBinding *workspacestorage.Binding) ([]byte, string, error) {
	var err error
	if storageBinding != nil {
		if err := storageBinding.ValidateFor(storageIdentity(command)); err != nil {
			return nil, "", err
		}
		spec.RunHookPayload, err = workspaceMountHook(spec.RunHookPayload, *storageBinding, storageIdentity(command))
		if err != nil {
			return nil, "", err
		}
	}
	payload, err := json.Marshal(spec)
	if err != nil || len(payload) > 16<<10 {
		return nil, "", ErrSpecification
	}
	identity := strings.Join([]string{string(command.TenantID), string(command.WorkspaceID), string(command.ResourcePlaneID), fmt.Sprint(command.WorkspaceRevision), string(command.OperationID), workspacejournal.DigestLaunchSpec(payload)}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return payload, hex.EncodeToString(digest[:]), nil
}

type workspaceMountHookPayload struct {
	Hako struct {
		WorkspaceMount struct {
			TenantID        string `json:"tenant_id"`
			WorkspaceID     string `json:"workspace_id"`
			ResourcePlaneID string `json:"resource_plane_id"`
			FileSystemID    string `json:"file_system_id"`
			Region          string `json:"region"`
			AccessPointID   string `json:"access_point_id"`
			RootPath        string `json:"root_path"`
			MountPath       string `json:"mount_path"`
			UID             uint32 `json:"uid"`
			GID             uint32 `json:"gid"`
			TLS             bool   `json:"tls"`
			IAM             bool   `json:"iam"`
		} `json:"workspace_mount"`
	} `json:"hako"`
}

func workspaceMountHook(existing string, binding workspacestorage.Binding, identity workspacestorage.Identity) (string, error) {
	if !safeRunHookPayload(existing) && existing != "" {
		return "", ErrSpecification
	}
	var payload map[string]json.RawMessage
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &payload); err != nil || payload == nil {
			return "", ErrSpecification
		}
	} else {
		payload = make(map[string]json.RawMessage)
	}
	if _, exists := payload["hako"]; exists {
		return "", errors.New("Workspace Runtime profile cannot override the reserved Hako run-hook namespace")
	}
	var mount workspaceMountHookPayload
	mount.Hako.WorkspaceMount.TenantID = string(identity.TenantID)
	mount.Hako.WorkspaceMount.WorkspaceID = string(identity.WorkspaceID)
	mount.Hako.WorkspaceMount.ResourcePlaneID = string(identity.ResourcePlaneID)
	mount.Hako.WorkspaceMount.FileSystemID = binding.FileSystemID
	mount.Hako.WorkspaceMount.Region = binding.Region
	mount.Hako.WorkspaceMount.AccessPointID = binding.AccessPointID
	mount.Hako.WorkspaceMount.RootPath = binding.RootPath
	mount.Hako.WorkspaceMount.MountPath = "/workspace"
	mount.Hako.WorkspaceMount.UID = binding.UID
	mount.Hako.WorkspaceMount.GID = binding.GID
	mount.Hako.WorkspaceMount.TLS = true
	mount.Hako.WorkspaceMount.IAM = true
	encoded, err := json.Marshal(mount.Hako)
	if err != nil {
		return "", ErrSpecification
	}
	payload["hako"] = json.RawMessage(encoded)
	encoded, err = json.Marshal(payload)
	if err != nil || len(encoded) > 16<<10 || !safeRunHookPayload(string(encoded)) {
		return "", ErrSpecification
	}
	return string(encoded), nil
}

func storageIdentity(command resourcecontroller.Command) workspacestorage.Identity {
	return workspacestorage.Identity{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID, ResourcePlaneID: command.ResourcePlaneID}
}

func storageIdentityFromJournal(j workspacejournal.Journal) workspacestorage.Identity {
	return workspacestorage.Identity{TenantID: j.TenantID, WorkspaceID: j.WorkspaceID, ResourcePlaneID: j.ResourcePlaneID}
}

func (r *LambdaMicroVM) validateSpec(spec LaunchSpec) error {
	if !r.config.SupportedRuntimeClasses[spec.RuntimeClass] {
		return ErrSpecification
	}
	return ValidateLaunchSpec(spec)
}

// ValidateLaunchSpec checks a trusted Resource Plane launch profile before it
// is used to create a MicroVM. It intentionally excludes support-policy
// selection, which belongs to the caller's enabled runtime-class set.
func ValidateLaunchSpec(spec LaunchSpec) error {
	if strings.TrimSpace(spec.RuntimeClass) == "" || !validARNService(spec.ImageIdentifier, "lambda", "microvm-image:") || !validARNService(spec.ExecutionRoleARN, "iam", "role/") ||
		!regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(spec.ImageVersion) || spec.MaximumDurationInSeconds < 1 || int64(spec.MaximumDurationInSeconds) > int64(MaxMicroVMRuntime/time.Second) ||
		spec.MaxIdleDurationSeconds < 60 || spec.SuspendedDurationSeconds < 0 || len(spec.EgressNetworkConnectors) == 0 || len(spec.EgressNetworkConnectors) > 10 || len(spec.IngressNetworkConnectors) > 10 ||
		len(spec.RunHookPayload) > 16<<10 || (spec.RunHookPayload != "" && !safeRunHookPayload(spec.RunHookPayload)) {
		return ErrSpecification
	}
	for _, connector := range append(append([]string(nil), spec.EgressNetworkConnectors...), spec.IngressNetworkConnectors...) {
		if !validARNService(connector, "lambda", "network-connector:") {
			return ErrSpecification
		}
	}
	return nil
}

func validARNService(value, service, resourcePrefix string) bool {
	if !arnPattern.MatchString(value) {
		return false
	}
	parts := strings.SplitN(value, ":", 6)
	return len(parts) == 6 && parts[2] == service && strings.HasPrefix(parts[5], resourcePrefix)
}

func safeRunHookPayload(payload string) bool {
	var value any
	decoder := json.NewDecoder(strings.NewReader(payload))
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false
	}
	return safeRunHookValue(value)
}

func safeRunHookValue(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			if strings.Contains(normalized, "secret") || strings.Contains(normalized, "token") || strings.Contains(normalized, "password") ||
				strings.Contains(normalized, "credential") || strings.Contains(normalized, "privatekey") || strings.Contains(normalized, "authorization") {
				return false
			}
			if !safeRunHookValue(child) {
				return false
			}
		}
	case []any:
		for _, child := range value {
			if !safeRunHookValue(child) {
				return false
			}
		}
	}
	return true
}

func launchInput(j workspacejournal.Journal) (*lambdamicrovms.RunMicrovmInput, error) {
	var spec LaunchSpec
	if len(j.LaunchSpecPayload) == 0 || json.Unmarshal(j.LaunchSpecPayload, &spec) != nil || workspacejournal.DigestLaunchSpec(j.LaunchSpecPayload) != j.LaunchSpecSHA256 || !workspacejournalTokenValid(j.LaunchToken) {
		return nil, ErrSpecification
	}
	idle := &types.IdlePolicy{AutoResumeEnabled: aws.Bool(spec.AutoResumeEnabled), MaxIdleDurationSeconds: aws.Int32(spec.MaxIdleDurationSeconds), SuspendedDurationSeconds: aws.Int32(spec.SuspendedDurationSeconds)}
	input := &lambdamicrovms.RunMicrovmInput{ImageIdentifier: aws.String(spec.ImageIdentifier), ImageVersion: aws.String(spec.ImageVersion), ExecutionRoleArn: aws.String(spec.ExecutionRoleARN),
		ClientToken: aws.String(j.LaunchToken), EgressNetworkConnectors: append([]string(nil), spec.EgressNetworkConnectors...), IngressNetworkConnectors: append([]string(nil), spec.IngressNetworkConnectors...),
		IdlePolicy: idle, MaximumDurationInSeconds: aws.Int32(spec.MaximumDurationInSeconds)}
	if spec.RunHookPayload != "" {
		input.RunHookPayload = aws.String(spec.RunHookPayload)
	}
	return input, nil
}

func workspacejournalTokenValid(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
