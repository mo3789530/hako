package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

var cliCredentialStore auth.CredentialStore = auth.OSKeyring{}
var cliCredentialGetter auth.CredentialGetter = auth.OSKeyring{}
var cliBrowserOpener auth.BrowserOpener = openBrowser

func main() {
	switch {
	case len(os.Args) >= 4 && os.Args[1] == "model" && os.Args[2] == "credential":
		result, err := manageModelCredentialCLI(os.Args[3:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako model credential: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 3 && os.Args[1] == "factory" && os.Args[2] == "validate":
		if err := validateFactoryManifestCLI(os.Args[3:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako factory validate: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 4 && os.Args[1] == "catalog" && os.Args[2] == "deploy-policy":
		if err := runSoftwareCatalogDeployPolicyCLI(os.Args[3:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako catalog deploy-policy: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 3 && os.Args[1] == "catalog":
		if err := runSoftwareCatalogCLI(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako catalog: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 5 && os.Args[1] == "github" && os.Args[2] == "repository" && os.Args[3] == "factory":
		result, err := manageGitHubRepositoryFactoryCLI(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github repository factory: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 5 && os.Args[1] == "github" && os.Args[2] == "repository" && os.Args[3] == "preview":
		result, err := manageGitHubRepositoryPreviewCLI(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github repository preview: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 5 && os.Args[1] == "github" && os.Args[2] == "repository" && os.Args[3] == "previews":
		if len(os.Args) >= 5 && os.Args[4] == "open" {
			if err := openGitHubRepositoryPreview(context.Background(), os.Args[5:], os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "hako github repository previews open: %v\n", err)
				os.Exit(1)
			}
			break
		}
		if len(os.Args) >= 5 && os.Args[4] == "forward" {
			if err := forwardGitHubRepositoryPreview(context.Background(), os.Args[5:], os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "hako github repository previews forward: %v\n", err)
				os.Exit(1)
			}
			break
		}
		if len(os.Args) >= 5 && os.Args[4] == "logs" {
			if err := readGitHubRepositoryPreviewLogs(os.Args[5:], os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "hako github repository previews logs: %v\n", err)
				os.Exit(1)
			}
			break
		}
		if len(os.Args) >= 6 && os.Args[4] == "session" {
			if os.Args[5] == "revoke" {
				if err := revokeGitHubRepositoryPreviewSessionCLI(os.Args[6:]); err != nil {
					fmt.Fprintf(os.Stderr, "hako github repository previews session revoke: %v\n", err)
					os.Exit(1)
				}
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"revoked": true})
				break
			}
			result, err := createGitHubRepositoryPreviewSessionCLI(os.Args[5:])
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako github repository previews session create: %v\n", err)
				os.Exit(1)
			}
			_ = json.NewEncoder(os.Stdout).Encode(result)
			break
		}
		result, err := listGitHubRepositoryPreviewsCLI(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github repository previews: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 5 && os.Args[1] == "github" && os.Args[2] == "repository" && os.Args[3] == "runner":
		if len(os.Args) >= 5 && os.Args[4] == "policy" {
			result, err := manageGitHubRepositoryActionsRunnerWorkflowPolicyCLI(os.Args[4:])
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako github repository runner policy: %v\n", err)
				os.Exit(1)
			}
			_ = json.NewEncoder(os.Stdout).Encode(result)
			break
		}
		result, err := manageGitHubRepositoryActionsRunnerCLI(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github repository runner: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 5 && os.Args[1] == "github" && os.Args[2] == "repository" && os.Args[3] == "agent-policy":
		result, err := manageGitHubRepositoryAgentWritePolicyCLI(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github repository agent-policy: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 5 && os.Args[1] == "github" && os.Args[2] == "repository" && os.Args[3] == "agent-model-access":
		result, err := manageGitHubRepositoryAgentModelAccessCLI(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github repository agent-model-access: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) >= 4 && os.Args[1] == "github" && os.Args[2] == "app":
		result, key, err := manageGitHubAppCLI(os.Args[3:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako github app: %v\n", err)
			if key != "" {
				fmt.Fprintf(os.Stderr, "Retry identical add with --idempotency-key %s\n", key)
			}
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case len(os.Args) == 2 && os.Args[1] == "login":
		if err := login(); err != nil {
			fmt.Fprintf(os.Stderr, "hako login: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "Login complete. Credentials are stored in the operating system credential store.")
	case len(os.Args) == 3 && os.Args[1] == "api" && os.Args[2] == "health":
		if err := apiHealth(); err != nil {
			fmt.Fprintf(os.Stderr, "hako api health: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "Hako API is healthy.")
	case len(os.Args) == 4 && os.Args[1] == "shell":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		fmt.Fprintf(os.Stderr, "Connecting to Workspace %s. Press Ctrl-C to disconnect.\n", os.Args[3])
		if err := workspaceShell(ctx, os.Args[2], os.Args[3], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako shell: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 6 && os.Args[1] == "exec":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		tenantID, workspaceID, timeoutMillis, argv, err := parseWorkspaceExecArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "usage: hako exec <tenant-id> <workspace-id> [--timeout <duration>] -- <command> [args...]")
			os.Exit(2)
		}
		if err := workspaceExecWithTimeout(ctx, tenantID, workspaceID, argv, timeoutMillis, os.Stdout, os.Stderr, nil, nil); err != nil {
			var exitErr workspaceExecExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.code)
			}
			fmt.Fprintf(os.Stderr, "hako exec: %v\n", err)
			os.Exit(1)
		}
	case (len(os.Args) == 4 || len(os.Args) == 5) && os.Args[1] == "open":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := workspaceIDE(ctx, os.Args[2], os.Args[3], optionalArg(os.Args, 4), os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "hako open: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) == 4 && os.Args[1] == "ssh-proxy":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := workspaceSSHProxy(ctx, os.Args[2], os.Args[3], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako ssh-proxy: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) == 6 && os.Args[1] == "cp":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		fmt.Fprintf(os.Stderr, "Copying to Workspace %s...\n", os.Args[3])
		if err := copyWorkspacePath(ctx, os.Args[2], os.Args[3], os.Args[4], os.Args[5], false); err != nil {
			fmt.Fprintf(os.Stderr, "hako cp: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) == 7 && os.Args[1] == "cp" && os.Args[2] == "--from":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		fmt.Fprintf(os.Stderr, "Copying from Workspace %s...\n", os.Args[4])
		if err := copyWorkspacePath(ctx, os.Args[3], os.Args[4], os.Args[5], os.Args[6], true); err != nil {
			fmt.Fprintf(os.Stderr, "hako cp: %v\n", err)
			os.Exit(1)
		}
	case (len(os.Args) == 5 || len(os.Args) == 6) && os.Args[1] == "forward":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := workspaceForward(ctx, os.Args[2], os.Args[3], os.Args[4], optionalArg(os.Args, 5), os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "hako forward: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) == 4 && os.Args[1] == "tenant" && os.Args[2] == "membership":
		membership, err := tenantMembership(os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant membership: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Tenant %s: %s (user %s)\n", membership.TenantID, membership.Role, membership.UserID)
	case len(os.Args) == 3 && os.Args[1] == "usage":
		usage, err := tenantUsage(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako usage: %v\n", err)
			os.Exit(1)
		}
		if err := writeTenantUsage(os.Stdout, os.Args[2], usage); err != nil {
			fmt.Fprintf(os.Stderr, "hako usage: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) == 5 && os.Args[1] == "tenant" && os.Args[2] == "quota" && os.Args[3] == "get":
		limits, err := getTenantQuotaLimits(os.Args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant quota get: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(limits)
	case len(os.Args) >= 7 && len(os.Args) <= 24 && os.Args[1] == "tenant" && os.Args[2] == "quota" && os.Args[3] == "set":
		target, skipConfirmation, err := parseTenantQuotaSetArgs(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant quota set: %v\n", err)
			os.Exit(2)
		}
		limits, err := executeTenantQuotaUpdate(os.Args[4], target, skipConfirmation, os.Stdin, os.Stderr,
			getTenantQuotaLimits, tenantUsage, func(tenantID string, target api.TenantQuotaLimits) (api.TenantQuotaLimits, error) {
				return setTenantQuotaLimits(tenantID, target.WorkspacesLimit, target.WorkloadRunsLimit,
					target.QueuedWorkloadRunsLimit,
					target.ArtifactBytesLimit, target.WorkspaceServiceMemoryBytesLimit, target.WorkspaceServiceMilliCPULimit,
					target.PreviewRunsLimit, target.PreviewLifetimeSecondsLimit,
					target.WorkspaceComputeMilliCPULimit, target.WorkspaceComputeMemoryMiBLimit)
			})
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant quota set: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(limits)
	case len(os.Args) == 5 && os.Args[1] == "tenant" && os.Args[2] == "placement-policy" && os.Args[3] == "get":
		policy, err := tenantPlacementPolicy(os.Args[4], nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant placement-policy get: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(policy)
	case len(os.Args) == 6 && os.Args[1] == "tenant" && os.Args[2] == "placement-policy" && os.Args[3] == "set":
		var policy api.TenantPlacementPolicy
		if err := json.Unmarshal([]byte(os.Args[5]), &policy); err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant placement-policy set: invalid JSON: %v\n", err)
			os.Exit(2)
		}
		policy.TenantID = os.Args[4]
		updated, err := tenantPlacementPolicy(os.Args[4], &policy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant placement-policy set: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(updated)
	case len(os.Args) == 5 && os.Args[1] == "tenant" && os.Args[2] == "workspace-idle-policy" && os.Args[3] == "get":
		policy, err := tenantWorkspaceIdlePolicy(os.Args[4], nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant workspace-idle-policy get: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(policy)
	case len(os.Args) == 7 && os.Args[1] == "tenant" && os.Args[2] == "workspace-idle-policy" && os.Args[3] == "set":
		enabled, err := strconv.ParseBool(os.Args[5])
		if err != nil {
			fmt.Fprintln(os.Stderr, "hako tenant workspace-idle-policy set: enabled must be true or false")
			os.Exit(2)
		}
		graceSeconds, err := strconv.Atoi(os.Args[6])
		if err != nil {
			fmt.Fprintln(os.Stderr, "hako tenant workspace-idle-policy set: grace period must be seconds")
			os.Exit(2)
		}
		policy := &api.TenantWorkspaceIdlePolicy{TenantID: os.Args[4], Enabled: enabled, GracePeriodSeconds: graceSeconds}
		updated, err := tenantWorkspaceIdlePolicy(os.Args[4], policy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant workspace-idle-policy set: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(updated)
	case len(os.Args) >= 3 && os.Args[1] == "workload" && os.Args[2] == "create":
		run, key, err := createWorkloadCLI(os.Args[3:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako workload create: %v\n", err)
			if key != "" {
				fmt.Fprintf(os.Stderr, "Retry the same request with --idempotency-key %s\n", key)
			}
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workload Run %s accepted (state=%s, request-key=%s)\n", run.ID, run.State, key)
	case len(os.Args) >= 5 && os.Args[1] == "workload" && os.Args[2] == "artifact" && os.Args[3] == "download":
		if err := downloadWorkloadArtifactCLI(os.Args[4:]); err != nil {
			fmt.Fprintf(os.Stderr, "hako workload artifact download: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 5 && os.Args[1] == "workload" && os.Args[2] == "changes" && os.Args[3] == "download":
		if err := downloadAgentChangesCLI(os.Args[4:]); err != nil {
			fmt.Fprintf(os.Stderr, "hako workload changes download: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 4 && os.Args[1] == "workload" && os.Args[2] == "changes" && os.Args[3] == "inspect":
		if err := inspectAgentChangesCLI(os.Args[4:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako workload changes inspect: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 4 && os.Args[1] == "workload" && os.Args[2] == "publication" && os.Args[3] == "show":
		if err := showAgentPublicationCLI(os.Args[4:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako workload publication show: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 4 && len(os.Args) <= 6 && os.Args[1] == "workload" && os.Args[2] == "list":
		limit, offset, err := parseCLIPage(os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako workload list: %v\n", err)
			os.Exit(2)
		}
		page, err := listWorkloadRuns(os.Args[3], limit, offset)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako workload list: %v\n", err)
			os.Exit(1)
		}
		for _, run := range page.Items {
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", run.ID, run.Kind, run.DesiredState, run.State)
		}
		fmt.Fprintf(os.Stdout, "Showing %d of %d workload runs (limit=%d offset=%d)\n", len(page.Items), page.Total, page.Limit, page.Offset)
	case len(os.Args) == 5 && os.Args[1] == "workload" && os.Args[2] == "show":
		run, err := getWorkloadRun(os.Args[3], os.Args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako workload show: %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(run)
	case len(os.Args) == 5 && os.Args[1] == "workload" && os.Args[2] == "cancel":
		run, err := cancelWorkloadRun(os.Args[3], os.Args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako workload cancel: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workload Run %s cancellation accepted (desired=%s observed=%s)\n", run.ID, run.DesiredState, run.State)
	case (len(os.Args) == 5 || len(os.Args) == 6) && os.Args[1] == "add":
		name := ""
		if len(os.Args) == 6 {
			name = os.Args[5]
		}
		service, err := addWorkspaceService(os.Args[2], os.Args[3], os.Args[4], name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Service %s accepted (catalog=%s desired=%s observed=%s)\n", service.Name, service.CatalogVersion, service.DesiredState, service.ObservedState)
	case len(os.Args) == 4 && os.Args[1] == "services":
		result, err := listWorkspaceServices(os.Args[2], os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako services: %v\n", err)
			os.Exit(1)
		}
		if err := writeWorkspaceServiceList(os.Stdout, result.Items); err != nil {
			fmt.Fprintf(os.Stderr, "hako services: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) == 5 && os.Args[1] == "service" && os.Args[2] == "usage":
		usage, err := workspaceServiceResourceUsage(os.Args[3], os.Args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako service usage: %v\n", err)
			os.Exit(1)
		}
		if err := writeWorkspaceServiceResourceUsage(os.Stdout, os.Args[4], usage); err != nil {
			fmt.Fprintf(os.Stderr, "hako service usage: %v\n", err)
			os.Exit(1)
		}
	case len(os.Args) >= 6 && len(os.Args) <= 7 && os.Args[1] == "service":
		action := os.Args[2]
		options, err := parseWorkspaceServiceActionOptions(action, os.Args[6:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako service %s: %v\nusage: hako service %s <tenant-id> <workspace-id> <instance-name> [--yes]\n", action, err, action)
			os.Exit(2)
		}
		requestAction := func() (domain.Service, error) {
			return requestWorkspaceServiceAction(action, os.Args[3], os.Args[4], os.Args[5])
		}
		var service domain.Service
		if action == "remove" {
			confirmedService, accepted, err := runConfirmedAction(
				func() (bool, error) {
					return authorizeWorkspaceServiceRemoval(options.yes, os.Stdin, os.Stderr, os.Args[3], os.Args[4], os.Args[5])
				}, requestAction)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako service remove: %v\n", err)
				os.Exit(1)
			}
			if !accepted {
				fmt.Fprintln(os.Stderr, "Workspace Service removal cancelled.")
				return
			}
			service = confirmedService
		} else {
			service, err = requestAction()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako service %s: %v\n", action, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Service %s %s accepted (desired=%s observed=%s)\n", service.Name, action, service.DesiredState, service.ObservedState)
	case len(os.Args) >= 3 && len(os.Args) <= 5 && os.Args[1] == "list":
		limit, offset, err := parseCLIPage(os.Args[3:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako list: %v\n", err)
			os.Exit(2)
		}
		result, err := listWorkspaces(os.Args[2], limit, offset)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako list: %v\n", err)
			os.Exit(1)
		}
		for _, item := range result.Items {
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", item.Workspace.ID, item.Workspace.Name, item.Status.DesiredState, item.Status.ObservedState)
		}
		fmt.Fprintf(os.Stdout, "Showing %d of %d workspaces (limit=%d offset=%d)\n", len(result.Items), result.Total, result.Limit, result.Offset)
	case len(os.Args) == 4 && os.Args[1] == "get":
		workspace, err := getWorkspace(os.Args[2], os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako get: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\n", workspace.Workspace.ID, workspace.Workspace.Name, workspace.Status.DesiredState, workspace.Status.ObservedState)
	case len(os.Args) >= 4 && os.Args[1] == "wait":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := workspaceWaitCLI(ctx, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "hako wait: %v\n", err)
			os.Exit(1)
		}
	case (len(os.Args) == 4 || len(os.Args) == 6) && os.Args[1] == "create":
		idempotencyKey := ""
		if len(os.Args) == 6 {
			if os.Args[4] != "--idempotency-key" {
				fmt.Fprintln(os.Stderr, "usage: hako create <tenant-id> <workspace-name> [--idempotency-key <key>]")
				os.Exit(2)
			}
			idempotencyKey = os.Args[5]
		} else {
			generatedKey, err := idgen.New("req_")
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako create: %v\n", err)
				os.Exit(1)
			}
			idempotencyKey = generatedKey
		}
		created, err := createWorkspace(os.Args[2], os.Args[3], idempotencyKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako create: %v\nRetry the same request with --idempotency-key %s\n", err, idempotencyKey)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workspace %s accepted: %s (desired=%s observed=%s, operation=%s, request-key=%s)\n", created.Name, created.ID, created.DesiredState, created.ObservedState, created.OperationID, idempotencyKey)
	case len(os.Args) >= 4 && len(os.Args) <= 7 && (os.Args[1] == "suspend" || os.Args[1] == "resume" || os.Args[1] == "delete"):
		action, tenantID, workspaceID := os.Args[1], os.Args[2], os.Args[3]
		options, err := parseWorkspaceActionOptions(action, os.Args[4:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako %s: %v\nusage: hako %s <tenant-id> <workspace-id> [--idempotency-key <key>] [--wait] [--yes]\n", action, err, action)
			os.Exit(2)
		}
		idempotencyKey := options.idempotencyKey
		if idempotencyKey == "" {
			generatedKey, err := idgen.New("req_")
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako %s: %v\n", action, err)
				os.Exit(1)
			}
			idempotencyKey = generatedKey
		}
		requestAction := func() (api.WorkspaceActionResult, error) {
			return workspaceAction(action, tenantID, workspaceID, idempotencyKey)
		}
		var result api.WorkspaceActionResult
		if action == "delete" {
			confirmedResult, accepted, err := runConfirmedAction(
				func() (bool, error) {
					return authorizeWorkspaceDeletion(options.yes, os.Stdin, os.Stderr, tenantID, workspaceID)
				}, requestAction)
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako delete: %v\n", err)
				os.Exit(1)
			}
			if !accepted {
				fmt.Fprintln(os.Stderr, "Workspace deletion cancelled.")
				return
			}
			result = confirmedResult
		} else {
			result, err = requestAction()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako %s: %v\nRetry the same request with --idempotency-key %s\n", action, err, idempotencyKey)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workspace %s accepted: desired=%s observed=%s (operation=%s state=%s, request-key=%s)\n", result.Workspace.Workspace.ID, result.Workspace.Status.DesiredState, result.Workspace.Status.ObservedState, result.OperationID, result.OperationState, idempotencyKey)
		if options.wait {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := workspaceWaitCLI(ctx, []string{tenantID, workspaceID, "--state", "running"}, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "hako resume: operation %s was accepted, but Workspace readiness was not confirmed: %v\n", result.OperationID, err)
				os.Exit(1)
			}
		}
	default:
		_ = writeCLIUsage(os.Stderr)
		os.Exit(2)
	}
}

func writeCLIUsage(output io.Writer) error {
	_, err := fmt.Fprint(output, `usage: hako <command>

Authentication and Tenant:
  hako login
  hako api health
  hako tenant membership <tenant-id>
  hako tenant quota get|set <tenant-id> ...
  hako usage <tenant-id>

Workspace:
  hako create <tenant-id> <workspace-name> [--idempotency-key <key>]
  hako list <tenant-id> [limit] [offset]
  hako get|wait|shell|open|ssh-proxy <tenant-id> <workspace-id> ...
  hako cp <tenant-id> <workspace-id> <local-path> <workspace-path>
  hako cp --from <tenant-id> <workspace-id> <workspace-path> <local-path>
  hako forward <tenant-id> <workspace-id> <remote-port> [local-port]
  hako suspend|resume <tenant-id> <workspace-id> [--idempotency-key <key>] [--wait]
  hako delete <tenant-id> <workspace-id> [--idempotency-key <key>] [--yes]

Workspace Services:
  hako add <tenant-id> <workspace-id> postgres|redis [instance-name]
  hako services <tenant-id> <workspace-id>
  hako service usage <tenant-id> <workspace-id>
  hako service start|stop|remove|rotate-credentials <tenant-id> <workspace-id> <instance-name>

Workloads:
  hako workload create --tenant <id> --installation <id> --repository <id> --commit <sha> [options]
  hako workload list <tenant-id> [limit] [offset]
  hako workload show|cancel <tenant-id> <run-id>
  hako workload publication show <tenant-id> <agent-run-id>
`)
	return err
}

func writeWorkspaceServiceList(output io.Writer, items []domain.Service) error {
	if _, err := fmt.Fprintln(output, "NAME\tCATALOG\tDESIRED\tOBSERVED\tREVISION\tRUNTIME_GEN\tHEALTH"); err != nil {
		return err
	}
	for _, service := range items {
		generation := "-"
		if service.ObservedRuntimeGeneration > 0 {
			generation = strconv.FormatInt(service.ObservedRuntimeGeneration, 10)
		}
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\n", service.Name, service.CatalogVersion,
			service.DesiredState, service.ObservedState, service.DesiredRevision, service.ObservedRevision, generation,
			workspaceServiceHealth(service)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(output, "Showing %d services\n", len(items))
	return err
}

func workspaceServiceHealth(service domain.Service) string {
	if service.DesiredState != "running" {
		return "n/a"
	}
	if service.ObservedRuntimeGeneration < 1 {
		return "pending"
	}
	if service.ObservedState == "failed" {
		return "failed"
	}
	if service.DesiredRevision < 1 || service.ObservedRevision != service.DesiredRevision {
		return "pending"
	}
	switch service.ObservedState {
	case "healthy", "unhealthy":
		return service.ObservedState
	default:
		return "pending"
	}
}

func optionalArg(args []string, index int) string {
	if index >= 0 && index < len(args) {
		return args[index]
	}
	return ""
}

func workspaceForward(ctx context.Context, tenantID, workspaceID, remotePortText, localPortText string, stderr io.Writer) error {
	if ctx == nil || stderr == nil {
		return errors.New("forward context and status output are required")
	}
	remotePort, err := strconv.ParseUint(remotePortText, 10, 16)
	if err != nil || remotePort == 0 {
		return errors.New("remote port must be between 1 and 65535")
	}
	localPort := remotePort
	if localPortText != "" {
		parsed, parseErr := strconv.ParseUint(localPortText, 10, 16)
		if parseErr != nil {
			return errors.New("local port must be between 0 and 65535")
		}
		localPort = parsed
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := os.Getenv("HAKO_API_URL"), os.Getenv("HAKO_GATEWAY_URL")
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	if gatewayURL == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(localPort, 10)))
	if err != nil {
		return fmt.Errorf("listen on local loopback port: %w", err)
	}
	defer listener.Close()
	fmt.Fprintf(stderr, "Forwarding 127.0.0.1:%d to Workspace %s:%d. Press Ctrl-C to stop.\n", listener.Addr().(*net.TCPAddr).Port, workspaceID, remotePort)
	return serveWorkspaceForward(ctx, listener, tenantID, workspaceID, uint16(remotePort), apiURL, gatewayURL, accessToken, stderr, nil, nil)
}

func workspaceIDE(ctx context.Context, tenantID, workspaceID, localPortText string, stderr io.Writer) error {
	if ctx == nil || stderr == nil || tenantID == "" || workspaceID == "" {
		return errors.New("Workspace IDE context, identity, and status output are required")
	}
	localPort := uint64(0)
	if localPortText != "" {
		parsed, err := strconv.ParseUint(localPortText, 10, 16)
		if err != nil {
			return errors.New("local port must be between 0 and 65535")
		}
		localPort = parsed
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := os.Getenv("HAKO_API_URL"), os.Getenv("HAKO_GATEWAY_URL")
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	if gatewayURL == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(localPort, 10)))
	if err != nil {
		return fmt.Errorf("listen on local loopback port: %w", err)
	}
	defer listener.Close()
	localAddr := listener.Addr().(*net.TCPAddr)
	ideURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(localAddr.Port)) + "/"
	fmt.Fprintf(stderr, "Opening Workspace IDE at %s. Press Ctrl-C to stop.\n", ideURL)
	if cliBrowserOpener != nil {
		if err := cliBrowserOpener(ideURL); err != nil {
			return fmt.Errorf("open Workspace IDE in browser: %w", err)
		}
	}
	return serveWorkspaceTunnel(ctx, listener, tenantID, workspaceID, workspacesession.PermissionVSCode, 0,
		apiURL, gatewayURL, accessToken, stderr, nil, nil)
}

func openGitHubRepositoryPreview(ctx context.Context, args []string, stderr io.Writer) error {
	return openGitHubRepositoryPreviewWith(ctx, args, stderr, nil, nil, cliBrowserOpener)
}

func openGitHubRepositoryPreviewWith(ctx context.Context, args []string, stderr io.Writer, apiHTTPClient *http.Client,
	gatewayTLSConfig *tls.Config, browserOpener func(string) error) error {
	if ctx == nil || stderr == nil {
		return errors.New("Preview open context and status output are required")
	}
	input, err := parseGitHubRepositoryPreviewOpenCLI(args)
	if err != nil {
		return err
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := os.Getenv("HAKO_API_URL"), os.Getenv("HAKO_GATEWAY_URL")
	if apiURL == "" || gatewayURL == "" {
		return errors.New("HAKO_API_URL and HAKO_GATEWAY_URL are required")
	}
	localPort, _ := strconv.ParseUint(input.localPort, 10, 16)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(localPort, 10)))
	if err != nil {
		return fmt.Errorf("listen on local loopback port: %w", err)
	}
	defer listener.Close()
	localAddr := listener.Addr().(*net.TCPAddr)
	previewURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(localAddr.Port)) + "/"
	fmt.Fprintf(stderr, "Opening Preview IDE at %s. Press Ctrl-C to stop.\n", previewURL)
	if browserOpener != nil {
		if err := browserOpener(previewURL); err != nil {
			return fmt.Errorf("open Preview IDE in browser: %w", err)
		}
	}
	return serveWorkspaceTunnelWithSession(ctx, listener, workspacesession.PermissionVSCode, 0, gatewayURL, stderr, apiHTTPClient, gatewayTLSConfig,
		func(sessionCtx context.Context) (workspaceTunnelSession, func(context.Context) error, error) {
			session, err := api.CreateRepositoryPreviewSession(sessionCtx, apiURL, accessToken, input.tenantID,
				input.installationID, input.repositoryID, input.pullRequest,
				[]workspacesession.Permission{workspacesession.PermissionVSCode}, apiHTTPClient)
			if err != nil {
				return workspaceTunnelSession{}, nil, err
			}
			workspaceID := string(session.WorkspaceID)
			return workspaceTunnelSession{ID: string(session.SessionID), WorkspaceID: workspaceID, Ticket: session.Ticket}, func(revokeCtx context.Context) error {
				return api.RevokeRepositoryPreviewSession(revokeCtx, apiURL, accessToken, input.tenantID,
					input.installationID, input.repositoryID, input.pullRequest, string(session.SessionID), apiHTTPClient)
			}, nil
		})
}

func forwardGitHubRepositoryPreview(ctx context.Context, args []string, stderr io.Writer) error {
	return forwardGitHubRepositoryPreviewWith(ctx, args, stderr, nil, nil, cliBrowserOpener)
}

func forwardGitHubRepositoryPreviewWith(ctx context.Context, args []string, stderr io.Writer, apiHTTPClient *http.Client,
	gatewayTLSConfig *tls.Config, browserOpener func(string) error) error {
	if ctx == nil || stderr == nil {
		return errors.New("Preview forward context and status output are required")
	}
	input, err := parseGitHubRepositoryPreviewForwardCLI(args)
	if err != nil {
		return err
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := os.Getenv("HAKO_API_URL"), os.Getenv("HAKO_GATEWAY_URL")
	if apiURL == "" || gatewayURL == "" {
		return errors.New("HAKO_API_URL and HAKO_GATEWAY_URL are required")
	}
	localPort, _ := strconv.ParseUint(input.localPort, 10, 16)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(localPort, 10)))
	if err != nil {
		return fmt.Errorf("listen on local loopback port: %w", err)
	}
	defer listener.Close()
	localAddr := listener.Addr().(*net.TCPAddr)
	previewURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(localAddr.Port)) + "/"
	fmt.Fprintf(stderr, "Opening Preview application at %s. Press Ctrl-C to stop.\n", previewURL)
	if browserOpener != nil {
		if err := browserOpener(previewURL); err != nil {
			return fmt.Errorf("open Preview application in browser: %w", err)
		}
	}
	return serveWorkspaceTunnelWithSession(ctx, listener, workspacesession.PermissionPortForward, input.remotePort, gatewayURL, stderr, apiHTTPClient, gatewayTLSConfig,
		func(sessionCtx context.Context) (workspaceTunnelSession, func(context.Context) error, error) {
			session, err := api.CreateRepositoryPreviewSession(sessionCtx, apiURL, accessToken, input.tenantID,
				input.installationID, input.repositoryID, input.pullRequest,
				[]workspacesession.Permission{workspacesession.PermissionPortForward}, apiHTTPClient)
			if err != nil {
				return workspaceTunnelSession{}, nil, err
			}
			workspaceID := string(session.WorkspaceID)
			return workspaceTunnelSession{ID: string(session.SessionID), WorkspaceID: workspaceID, Ticket: session.Ticket}, func(revokeCtx context.Context) error {
				return api.RevokeRepositoryPreviewSession(revokeCtx, apiURL, accessToken, input.tenantID,
					input.installationID, input.repositoryID, input.pullRequest, string(session.SessionID), apiHTTPClient)
			}, nil
		})
}

func readGitHubRepositoryPreviewLogs(args []string, stdout io.Writer) error {
	return readGitHubRepositoryPreviewLogsWith(context.Background(), args, stdout, nil, nil)
}

func readGitHubRepositoryPreviewLogsWith(ctx context.Context, args []string, stdout io.Writer, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) error {
	if ctx == nil || stdout == nil {
		return errors.New("Preview log context and output are required")
	}
	input, err := parseGitHubRepositoryPreviewLogsCLI(args)
	if err != nil {
		return err
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := os.Getenv("HAKO_API_URL"), os.Getenv("HAKO_GATEWAY_URL")
	if apiURL == "" || gatewayURL == "" {
		return errors.New("HAKO_API_URL and HAKO_GATEWAY_URL are required")
	}
	sessionCtx, cancelSession := context.WithTimeout(ctx, 15*time.Second)
	defer cancelSession()
	session, err := api.CreateRepositoryPreviewSession(sessionCtx, apiURL, accessToken, input.tenantID,
		input.installationID, input.repositoryID, input.pullRequest,
		[]workspacesession.Permission{workspacesession.PermissionPreviewLogs}, apiHTTPClient)
	if err != nil {
		return err
	}
	defer func() {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = api.RevokeRepositoryPreviewSession(revokeCtx, apiURL, accessToken, input.tenantID,
			input.installationID, input.repositoryID, input.pullRequest, string(session.SessionID), apiHTTPClient)
	}()
	connection, err := workspacegateway.Dial(ctx, gatewayURL, session.WorkspaceID, workspacesession.PermissionPreviewLogs, 0,
		session.Ticket, gatewayTLSConfig)
	if err != nil {
		return err
	}
	defer connection.Close()
	var lengthBytes [4]byte
	if _, err := io.ReadFull(connection, lengthBytes[:]); err != nil {
		return errors.New("Preview log stream is unavailable")
	}
	length := binary.BigEndian.Uint32(lengthBytes[:])
	if length > uint32(workspaceprotocol.PreviewLogsMaxBytes) {
		return errors.New("Preview log response exceeds the configured limit")
	}
	logs := make([]byte, int(length))
	if _, err := io.ReadFull(connection, logs); err != nil {
		return errors.New("Preview log stream ended before the complete bounded response")
	}
	return writePreviewLogs(stdout, logs)
}

func writePreviewLogs(stdout io.Writer, logs []byte) error {
	if stdout == nil {
		return errors.New("Preview log output is required")
	}
	written, err := stdout.Write(logs)
	if err != nil {
		return errors.New("write Preview logs")
	}
	if written != len(logs) {
		return io.ErrShortWrite
	}
	return nil
}

func serveWorkspaceForward(ctx context.Context, listener net.Listener, tenantID, workspaceID string, remotePort uint16, apiURL, gatewayURL, accessToken string,
	stderr io.Writer, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) error {
	return serveWorkspaceTunnel(ctx, listener, tenantID, workspaceID, workspacesession.PermissionPortForward, remotePort,
		apiURL, gatewayURL, accessToken, stderr, apiHTTPClient, gatewayTLSConfig)
}

func serveWorkspaceTunnel(ctx context.Context, listener net.Listener, tenantID, workspaceID string, permission workspacesession.Permission, remotePort uint16,
	apiURL, gatewayURL, accessToken string, stderr io.Writer, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) error {
	if ctx == nil || listener == nil || stderr == nil || tenantID == "" || workspaceID == "" || apiURL == "" || gatewayURL == "" || accessToken == "" {
		return errors.New("Workspace tunnel listener, identity, endpoints, credentials, context, and output are required")
	}
	return serveWorkspaceTunnelWithSession(ctx, listener, permission, remotePort, gatewayURL, stderr, apiHTTPClient, gatewayTLSConfig,
		func(ctx context.Context) (workspaceTunnelSession, func(context.Context) error, error) {
			session, err := api.CreateWorkspaceSession(ctx, apiURL, accessToken, tenantID, workspaceID,
				[]workspacesession.Permission{permission}, apiHTTPClient)
			if err != nil {
				return workspaceTunnelSession{}, nil, err
			}
			return workspaceTunnelSession{ID: string(session.SessionID), WorkspaceID: workspaceID, Ticket: session.Ticket}, func(revokeCtx context.Context) error {
				return api.RevokeWorkspaceSession(revokeCtx, apiURL, accessToken, tenantID, workspaceID, string(session.SessionID), apiHTTPClient)
			}, nil
		})
}

func serveWorkspaceTunnelWithSession(ctx context.Context, listener net.Listener, permission workspacesession.Permission, remotePort uint16,
	gatewayURL string, stderr io.Writer, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config, createSession workspaceTunnelSessionFactory) error {
	if ctx == nil || listener == nil || stderr == nil || gatewayURL == "" || createSession == nil {
		return errors.New("Workspace tunnel listener, endpoints, session factory, context, and output are required")
	}
	switch permission {
	case workspacesession.PermissionPortForward:
		if remotePort == 0 {
			return errors.New("remote port-forward port is required")
		}
	case workspacesession.PermissionVSCode:
		if remotePort != 0 {
			return errors.New("VS Code tunnel cannot select a remote port")
		}
	default:
		return errors.New("unsupported Workspace tunnel permission")
	}
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	const maxForwardConnections = 16
	semaphore := make(chan struct{}, maxForwardConnections)
	var workers sync.WaitGroup
	var stderrMu sync.Mutex
	for {
		localConn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				workers.Wait()
				return nil
			}
			return fmt.Errorf("accept local forwarded connection: %w", err)
		}
		select {
		case semaphore <- struct{}{}:
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-semaphore }()
				if err := workspaceTunnelConnectionWithSession(ctx, localConn, permission, remotePort, gatewayURL, apiHTTPClient, gatewayTLSConfig, createSession); err != nil && ctx.Err() == nil {
					stderrMu.Lock()
					fmt.Fprintf(stderr, "forwarded connection failed: %v\n", err)
					stderrMu.Unlock()
				}
			}()
		case <-ctx.Done():
			_ = localConn.Close()
		}
	}
}

func forwardWorkspaceConnection(ctx context.Context, localConn net.Conn, tenantID, workspaceID string, remotePort uint16,
	apiURL, gatewayURL, accessToken string, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) (returnErr error) {
	return workspaceTunnelConnection(ctx, localConn, tenantID, workspaceID, workspacesession.PermissionPortForward, remotePort,
		apiURL, gatewayURL, accessToken, apiHTTPClient, gatewayTLSConfig)
}

func workspaceTunnelConnection(ctx context.Context, localConn net.Conn, tenantID, workspaceID string, permission workspacesession.Permission, remotePort uint16,
	apiURL, gatewayURL, accessToken string, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) (returnErr error) {
	return workspaceTunnelConnectionWithSession(ctx, localConn, permission, remotePort, gatewayURL, apiHTTPClient, gatewayTLSConfig,
		func(ctx context.Context) (workspaceTunnelSession, func(context.Context) error, error) {
			session, err := api.CreateWorkspaceSession(ctx, apiURL, accessToken, tenantID, workspaceID,
				[]workspacesession.Permission{permission}, apiHTTPClient)
			if err != nil {
				return workspaceTunnelSession{}, nil, err
			}
			return workspaceTunnelSession{ID: string(session.SessionID), WorkspaceID: workspaceID, Ticket: session.Ticket}, func(revokeCtx context.Context) error {
				return api.RevokeWorkspaceSession(revokeCtx, apiURL, accessToken, tenantID, workspaceID, string(session.SessionID), apiHTTPClient)
			}, nil
		})
}

type workspaceTunnelSession struct {
	ID          string
	WorkspaceID string
	Ticket      string
}

type workspaceTunnelSessionFactory func(context.Context) (workspaceTunnelSession, func(context.Context) error, error)

func workspaceTunnelConnectionWithSession(ctx context.Context, localConn net.Conn, permission workspacesession.Permission, remotePort uint16,
	gatewayURL string, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config, createSession workspaceTunnelSessionFactory) (returnErr error) {
	if ctx == nil || localConn == nil || createSession == nil {
		return errors.New("Workspace tunnel connection and session factory are required")
	}
	defer localConn.Close()
	session, revoke, err := createSession(ctx)
	if err != nil {
		return fmt.Errorf("request Workspace tunnel session: %w", err)
	}
	if revoke != nil {
		defer func() {
			revokeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := revoke(revokeCtx); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("revoke Workspace tunnel session: %w", err)
			}
		}()
	}
	if session.ID == "" || session.WorkspaceID == "" || session.Ticket == "" || revoke == nil {
		return errors.New("Workspace tunnel session is incomplete")
	}
	stream, err := workspacegateway.Dial(ctx, gatewayURL, domain.WorkspaceID(session.WorkspaceID), permission, remotePort,
		session.Ticket, gatewayTLSConfig)
	if err != nil {
		return err
	}
	defer stream.Close()
	stopClose := context.AfterFunc(ctx, func() {
		_ = localConn.Close()
		_ = stream.Close()
	})
	defer stopClose()
	finished := make(chan error, 2)
	go func() { _, err := io.Copy(stream, localConn); finished <- err }()
	go func() { _, err := io.Copy(localConn, stream); finished <- err }()
	if err := <-finished; err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		_ = localConn.Close()
		_ = stream.Close()
		<-finished
		return fmt.Errorf("relay port-forward stream: %w", err)
	}
	_ = localConn.Close()
	_ = stream.Close()
	<-finished
	return nil
}

func workspaceShell(ctx context.Context, tenantID, workspaceID string, stdin io.Reader, stdout io.Writer) error {
	return workspaceShellWith(ctx, tenantID, workspaceID, stdin, stdout, nil, nil)
}

func workspaceShellWith(ctx context.Context, tenantID, workspaceID string, stdin io.Reader, stdout io.Writer, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) (returnErr error) {
	if ctx == nil || stdin == nil || stdout == nil {
		return errors.New("shell input, output, and context are required")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	gatewayURL := os.Getenv("HAKO_GATEWAY_URL")
	if gatewayURL == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	shell := &workspaceShellReconnect{ctx: ctx, apiURL: apiURL, accessToken: accessToken, tenantID: tenantID,
		workspaceID: workspaceID, gatewayURL: gatewayURL, apiHTTPClient: apiHTTPClient, gatewayTLSConfig: gatewayTLSConfig}
	if err := shell.connect(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("request Workspace shell session: %w", err)
	}
	defer func() {
		if err := shell.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close Workspace terminal: %w", err)
		}
	}()
	restoreTerminal, err := enableWorkspaceTerminal(stdin, stdout, shell.Resize)
	if err != nil {
		return fmt.Errorf("prepare local terminal: %w", err)
	}
	defer restoreTerminal()
	stopClose := context.AfterFunc(ctx, shell.interrupt)
	defer stopClose()
	go func() {
		if _, err := io.Copy(shell, stdin); err != nil {
			if ctx.Err() == nil {
				_ = shell.Close()
			}
			return
		}
		// A terminal EOF (Ctrl-D) is delivered to the PTY as EOT, letting the
		// shell exit naturally while its final output still drains to stdout.
		_, _ = shell.Write([]byte{4})
		shell.finishInput()
	}()
	if _, err := io.Copy(stdout, shell); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("Workspace shell stream: %w", err)
	}
	return nil
}

// workspaceShellStream converts byte-oriented CLI reads and writes into
// bounded, explicitly typed WebSocket messages shared with hako-agent.
type workspaceShellStream struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	decoder   workspaceprotocol.Decoder
	read      []byte
	sessionID string
	ended     bool
}

// workspaceShellReconnect keeps the remote PTY alive while renewing the
// short-lived Hako session and one-use Gateway ticket after a dropped socket.
type workspaceShellReconnect struct {
	ctx              context.Context
	apiURL           string
	accessToken      string
	tenantID         string
	workspaceID      string
	gatewayURL       string
	apiHTTPClient    *http.Client
	gatewayTLSConfig *tls.Config

	mu         sync.Mutex
	current    *workspaceShellStream
	apiSession string
	terminalID string
	closed     bool
	inputDone  bool
}

func (s *workspaceShellReconnect) connect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectLocked()
}

func (s *workspaceShellReconnect) connectLocked() error {
	if s.closed || s.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	hakoSession, err := api.CreateWorkspaceSession(s.ctx, s.apiURL, s.accessToken, s.tenantID, s.workspaceID,
		[]workspacesession.Permission{workspacesession.PermissionShell}, s.apiHTTPClient)
	if err != nil {
		return err
	}
	connection, err := workspacegateway.Dial(s.ctx, s.gatewayURL, domain.WorkspaceID(s.workspaceID), workspacesession.PermissionShell, 0,
		hakoSession.Ticket, s.gatewayTLSConfig)
	if err != nil {
		s.revokeLocked(string(hakoSession.SessionID))
		return err
	}
	stream := &workspaceShellStream{conn: connection}
	if err := stream.attach(s.terminalID); err != nil {
		_ = stream.Close()
		s.revokeLocked(string(hakoSession.SessionID))
		return err
	}
	s.current = stream
	s.apiSession = string(hakoSession.SessionID)
	s.terminalID = stream.sessionID
	return nil
}

func (s *workspaceShellReconnect) reconnect(failed *workspaceShellStream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return io.ErrClosedPipe
	}
	if s.inputDone {
		return io.EOF
	}
	if failed != nil && s.current != failed {
		return nil // Another concurrent reader/writer already reconnected.
	}
	if s.current != nil {
		_ = s.current.Close()
		s.current = nil
	}
	if s.apiSession != "" {
		s.revokeLocked(s.apiSession)
		s.apiSession = ""
	}
	var lastErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 8; attempt++ {
		if err := s.connectLocked(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		case <-timer.C:
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("reconnect Workspace terminal after 8 attempts: %w", lastErr)
}

func (s *workspaceShellReconnect) Read(target []byte) (int, error) {
	for {
		s.mu.Lock()
		stream, closed, inputDone := s.current, s.closed, s.inputDone
		s.mu.Unlock()
		if closed || stream == nil {
			return 0, io.ErrClosedPipe
		}
		n, err := stream.Read(target)
		if err == nil || s.ctx.Err() != nil || inputDone || stream.ended {
			return n, err
		}
		if reconnectErr := s.reconnect(stream); reconnectErr != nil {
			return n, reconnectErr
		}
	}
}

func (s *workspaceShellReconnect) Write(data []byte) (int, error) {
	for {
		s.mu.Lock()
		stream, closed := s.current, s.closed
		s.mu.Unlock()
		if closed || stream == nil {
			return 0, io.ErrClosedPipe
		}
		n, err := stream.Write(data)
		if err == nil || n > 0 || s.ctx.Err() != nil {
			return n, err
		}
		if reconnectErr := s.reconnect(stream); reconnectErr != nil {
			return 0, reconnectErr
		}
	}
}

func (s *workspaceShellReconnect) Resize(columns, rows uint16) error {
	for {
		s.mu.Lock()
		stream, closed := s.current, s.closed
		s.mu.Unlock()
		if closed || stream == nil {
			return io.ErrClosedPipe
		}
		if err := stream.Resize(columns, rows); err == nil {
			return nil
		} else if s.ctx.Err() != nil {
			return err
		} else if reconnectErr := s.reconnect(stream); reconnectErr != nil {
			return reconnectErr
		}
	}
}

func (s *workspaceShellReconnect) finishInput() {
	s.mu.Lock()
	s.inputDone = true
	s.mu.Unlock()
}

func (s *workspaceShellReconnect) interrupt() {
	s.mu.Lock()
	stream := s.current
	s.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

func (s *workspaceShellReconnect) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream, sessionID := s.current, s.apiSession
	s.current, s.apiSession = nil, ""
	s.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
	if sessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return api.RevokeWorkspaceSession(ctx, s.apiURL, s.accessToken, s.tenantID, s.workspaceID, sessionID, s.apiHTTPClient)
}

func (s *workspaceShellReconnect) revokeLocked(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = api.RevokeWorkspaceSession(ctx, s.apiURL, s.accessToken, s.tenantID, s.workspaceID, sessionID, s.apiHTTPClient)
}

func (s *workspaceShellStream) attach(sessionID string) error {
	frame, err := workspaceprotocol.EncodeAttach(sessionID)
	if err != nil {
		return err
	}
	if err := websocket.Message.Send(s.conn, frame); err != nil {
		return err
	}
	var response []byte
	if err := websocket.Message.Receive(s.conn, &response); err != nil {
		return err
	}
	decoded, err := workspaceprotocol.Decode(response)
	if err != nil || decoded.Session == "" || decoded.Attach || decoded.Resize || len(decoded.Data) != 0 {
		return errors.New("invalid Workspace terminal session response")
	}
	s.sessionID = decoded.Session
	return nil
}

func (s *workspaceShellStream) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		size := len(data)
		if limit := workspaceprotocol.MaxFrame - workspaceprotocol.HeaderSize; size > limit {
			size = limit
		}
		frame, err := workspaceprotocol.EncodeData(data[:size])
		if err != nil {
			return written, err
		}
		s.mu.Lock()
		err = websocket.Message.Send(s.conn, frame)
		s.mu.Unlock()
		if err != nil {
			return written, err
		}
		written += size
		data = data[size:]
	}
	return written, nil
}

func (s *workspaceShellStream) Read(target []byte) (int, error) {
	if len(target) == 0 {
		return 0, nil
	}
	for len(s.read) == 0 {
		if s.ended {
			return 0, io.EOF
		}
		var chunk []byte
		if err := websocket.Message.Receive(s.conn, &chunk); err != nil {
			return 0, err
		}
		frames, err := s.decoder.Feed(chunk)
		if err != nil {
			return 0, fmt.Errorf("decode Workspace shell frame: %w", err)
		}
		for _, decoded := range frames {
			if decoded.Attach || decoded.Session != "" {
				return 0, errors.New("unexpected terminal control frame from Workspace")
			}
			if decoded.End {
				s.ended = true
				continue
			}
			if decoded.Resize {
				return 0, errors.New("unexpected terminal resize frame from Workspace")
			}
			s.read = append(s.read, decoded.Data...)
		}
	}
	n := copy(target, s.read)
	s.read = s.read[n:]
	return n, nil
}

func (s *workspaceShellStream) Resize(columns, rows uint16) error {
	frame, err := workspaceprotocol.EncodeResize(columns, rows)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return websocket.Message.Send(s.conn, frame)
}

func (s *workspaceShellStream) Close() error { return s.conn.Close() }

// workspaceSSHProxy is an OpenSSH ProxyCommand transport. It tunnels raw SSH
// bytes over TLS/WSS to the fixed Workspace-local 127.0.0.1:22 endpoint.
func workspaceSSHProxy(ctx context.Context, tenantID, workspaceID string, stdin io.Reader, stdout io.Writer) error {
	return workspaceSSHProxyWith(ctx, tenantID, workspaceID, stdin, stdout, nil, nil)
}

func workspaceSSHProxyWith(ctx context.Context, tenantID, workspaceID string, stdin io.Reader, stdout io.Writer, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) (returnErr error) {
	if ctx == nil || stdin == nil || stdout == nil {
		return errors.New("SSH proxy input, output, and context are required")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := os.Getenv("HAKO_API_URL"), os.Getenv("HAKO_GATEWAY_URL")
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	if gatewayURL == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	session, err := api.CreateWorkspaceSession(ctx, apiURL, accessToken, tenantID, workspaceID,
		[]workspacesession.Permission{workspacesession.PermissionSSHProxy}, apiHTTPClient)
	if err != nil {
		return fmt.Errorf("request Workspace SSH proxy session: %w", err)
	}
	defer func() {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := api.RevokeWorkspaceSession(revokeCtx, apiURL, accessToken, tenantID, workspaceID, string(session.SessionID), apiHTTPClient); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("revoke Workspace SSH proxy session: %w", err)
		}
	}()
	connection, err := workspacegateway.Dial(ctx, gatewayURL, domain.WorkspaceID(workspaceID), workspacesession.PermissionSSHProxy, 0,
		session.Ticket, gatewayTLSConfig)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer connection.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	go func() {
		_, _ = io.Copy(connection, stdin)
		_ = connection.Close()
	}()
	if _, err := io.Copy(stdout, connection); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("Workspace SSH proxy stream: %w", err)
	}
	return nil
}

func login() error {
	config := auth.Config{
		Domain:      os.Getenv("HAKO_COGNITO_DOMAIN"),
		ClientID:    os.Getenv("HAKO_COGNITO_CLIENT_ID"),
		CallbackURL: os.Getenv("HAKO_COGNITO_CALLBACK_URL"),
	}
	return runLogin(config, cliCredentialStore, cliBrowserOpener, func(url string) {
		fmt.Fprintln(os.Stderr, "Open this URL to sign in:")
		fmt.Fprintln(os.Stderr, url)
	})
}

func runLogin(config auth.Config, store auth.CredentialStore, browser auth.BrowserOpener, announce auth.URLAnnouncer) error {
	return auth.Login(context.Background(), config, store, browser, announce)
}

func apiHealth() error {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	return api.Health(context.Background(), apiURL, accessToken, nil)
}

func tenantMembership(tenantID string) (api.TenantMembership, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.TenantMembership{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.TenantMembership{}, errors.New("HAKO_API_URL is required")
	}
	return api.GetTenantMembership(context.Background(), apiURL, accessToken, tenantID, nil)
}

func tenantPlacementPolicy(tenantID string, update *api.TenantPlacementPolicy) (api.TenantPlacementPolicy, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.TenantPlacementPolicy{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.TenantPlacementPolicy{}, errors.New("HAKO_API_URL is required")
	}
	if update == nil {
		return api.GetTenantPlacementPolicy(context.Background(), apiURL, accessToken, tenantID, nil)
	}
	update.TenantID = tenantID
	return api.PutTenantPlacementPolicy(context.Background(), apiURL, accessToken, *update, nil)
}

func tenantWorkspaceIdlePolicy(tenantID string, update *api.TenantWorkspaceIdlePolicy) (api.TenantWorkspaceIdlePolicy, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.TenantWorkspaceIdlePolicy{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.TenantWorkspaceIdlePolicy{}, errors.New("HAKO_API_URL is required")
	}
	if update == nil {
		return api.GetTenantWorkspaceIdlePolicy(context.Background(), apiURL, accessToken, tenantID, nil)
	}
	update.TenantID = tenantID
	return api.PutTenantWorkspaceIdlePolicy(context.Background(), apiURL, accessToken, *update, nil)
}

func createWorkspace(tenantID, name, idempotencyKey string) (api.WorkspaceCreateResult, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkspaceCreateResult{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkspaceCreateResult{}, errors.New("HAKO_API_URL is required")
	}
	return api.CreateWorkspace(context.Background(), apiURL, accessToken, tenantID, name, idempotencyKey, nil)
}

func listWorkspaces(tenantID string, limit, offset int) (api.WorkspaceListResult, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkspaceListResult{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkspaceListResult{}, errors.New("HAKO_API_URL is required")
	}
	return api.ListWorkspaces(context.Background(), apiURL, accessToken, tenantID, limit, offset, nil)
}

func tenantUsage(tenantID string) (api.TenantUsage, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.TenantUsage{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.TenantUsage{}, errors.New("HAKO_API_URL is required")
	}
	return api.GetTenantUsage(context.Background(), apiURL, accessToken, tenantID, nil)
}

func getTenantQuotaLimits(tenantID string) (api.TenantQuotaLimits, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.TenantQuotaLimits{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.TenantQuotaLimits{}, errors.New("HAKO_API_URL is required")
	}
	return api.GetTenantQuotaLimits(context.Background(), apiURL, accessToken, tenantID, nil)
}

func setTenantQuotaLimits(tenantID string, workspacesLimit, workloadRunsLimit, queuedWorkloadRunsLimit int16, artifactBytesLimit, serviceMemoryBytesLimit, serviceMilliCPULimit int64, previewRunsLimit int16, previewLifetimeSeconds, workspaceComputeMilliCPULimit, workspaceComputeMemoryMiBLimit int64) (api.TenantQuotaLimits, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.TenantQuotaLimits{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.TenantQuotaLimits{}, errors.New("HAKO_API_URL is required")
	}
	clearWorkspaceComputeLimits := workspaceComputeMilliCPULimit < 0 || workspaceComputeMemoryMiBLimit < 0
	if queuedWorkloadRunsLimit == 0 || artifactBytesLimit == 0 || serviceMemoryBytesLimit == 0 || serviceMilliCPULimit == 0 || previewRunsLimit == 0 || previewLifetimeSeconds == 0 || workspaceComputeMilliCPULimit == 0 || workspaceComputeMemoryMiBLimit == 0 {
		current, err := api.GetTenantQuotaLimits(context.Background(), apiURL, accessToken, tenantID, nil)
		if err != nil {
			return api.TenantQuotaLimits{}, fmt.Errorf("read current Tenant quota settings: %w", err)
		}
		if artifactBytesLimit == 0 {
			artifactBytesLimit = current.ArtifactBytesLimit
		}
		if queuedWorkloadRunsLimit == 0 {
			queuedWorkloadRunsLimit = current.QueuedWorkloadRunsLimit
			if queuedWorkloadRunsLimit > workloadRunsLimit {
				queuedWorkloadRunsLimit = workloadRunsLimit
			}
		}
		if serviceMemoryBytesLimit == 0 {
			serviceMemoryBytesLimit = current.WorkspaceServiceMemoryBytesLimit
		}
		if serviceMilliCPULimit == 0 {
			serviceMilliCPULimit = current.WorkspaceServiceMilliCPULimit
		}
		if previewRunsLimit == 0 {
			previewRunsLimit = current.PreviewRunsLimit
		}
		if previewLifetimeSeconds == 0 {
			previewLifetimeSeconds = current.PreviewLifetimeSecondsLimit
		}
		if workspaceComputeMilliCPULimit == 0 {
			workspaceComputeMilliCPULimit = current.WorkspaceComputeMilliCPULimit
		}
		if workspaceComputeMemoryMiBLimit == 0 {
			workspaceComputeMemoryMiBLimit = current.WorkspaceComputeMemoryMiBLimit
		}
	}
	if clearWorkspaceComputeLimits {
		workspaceComputeMilliCPULimit, workspaceComputeMemoryMiBLimit = 0, 0
	}
	return api.SetTenantQuotaLimits(context.Background(), apiURL, accessToken, tenantID,
		api.TenantQuotaLimits{WorkspacesLimit: workspacesLimit, WorkloadRunsLimit: workloadRunsLimit, QueuedWorkloadRunsLimit: queuedWorkloadRunsLimit,
			PreviewRunsLimit: previewRunsLimit, PreviewLifetimeSecondsLimit: previewLifetimeSeconds, ArtifactBytesLimit: artifactBytesLimit,
			WorkspaceServiceMemoryBytesLimit: serviceMemoryBytesLimit, WorkspaceServiceMilliCPULimit: serviceMilliCPULimit,
			WorkspaceComputeMilliCPULimit: workspaceComputeMilliCPULimit, WorkspaceComputeMemoryMiBLimit: workspaceComputeMemoryMiBLimit}, nil)
}

func parseTenantQuotaSetArgs(args []string) (api.TenantQuotaLimits, bool, error) {
	if len(args) < 3 {
		return api.TenantQuotaLimits{}, false, errors.New("usage: hako tenant quota set <tenant-id> <workspaces 1-10> <workload-runs 1-5> [artifact-storage-GiB 1-100 [service-memory-GiB 1-2 service-millicpu 1-1500 [preview-runs 1-5 preview-lifetime-hours 1-24]]] [--queued-workload-runs 1-5] [--workspace-compute-cpu-millicores 1-80000] [--workspace-compute-memory-mib 1-163840] [--no-workspace-compute-limits] [--yes]")
	}
	skipConfirmation := false
	queuedWorkloadRuns := int64(0)
	workspaceComputeCPU, workspaceComputeMemory := int64(0), int64(0)
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--queued-workload-runs" {
			if i+1 >= len(args) || queuedWorkloadRuns != 0 {
				return api.TenantQuotaLimits{}, false, errors.New("--queued-workload-runs requires one value")
			}
			value, err := strconv.ParseInt(args[i+1], 10, 16)
			if err != nil || value < 1 || value > 5 {
				return api.TenantQuotaLimits{}, false, errors.New("queued Workload Run limit must be 1-5")
			}
			queuedWorkloadRuns = value
			i++
			continue
		}
		if args[i] == "--workspace-compute-cpu-millicores" || args[i] == "--workspace-compute-memory-mib" {
			if i+1 >= len(args) {
				return api.TenantQuotaLimits{}, false, fmt.Errorf("%s requires one value", args[i])
			}
			value, err := strconv.ParseInt(args[i+1], 10, 64)
			maximum := int64(80000)
			if args[i] == "--workspace-compute-memory-mib" {
				maximum = 163840
			}
			if err != nil || value < 1 || value > maximum {
				return api.TenantQuotaLimits{}, false, fmt.Errorf("%s must be 1-%d", args[i], maximum)
			}
			if args[i] == "--workspace-compute-cpu-millicores" {
				if workspaceComputeCPU != 0 {
					return api.TenantQuotaLimits{}, false, errors.New("Workspace compute CPU limit was supplied more than once")
				}
				workspaceComputeCPU = value
			} else {
				if workspaceComputeMemory != 0 {
					return api.TenantQuotaLimits{}, false, errors.New("Workspace compute memory limit was supplied more than once")
				}
				workspaceComputeMemory = value
			}
			i++
			continue
		}
		if args[i] == "--no-workspace-compute-limits" {
			if workspaceComputeCPU != 0 || workspaceComputeMemory != 0 {
				return api.TenantQuotaLimits{}, false, errors.New("cannot combine --no-workspace-compute-limits with explicit compute limits")
			}
			workspaceComputeCPU, workspaceComputeMemory = -1, -1
			continue
		}
		filtered = append(filtered, args[i])
	}
	args = filtered
	if args[len(args)-1] == "--yes" {
		skipConfirmation = true
		args = args[:len(args)-1]
	}
	if len(args) != 3 && len(args) != 4 && len(args) != 6 && len(args) != 8 {
		return api.TenantQuotaLimits{}, false, errors.New("invalid quota arguments; optional positional settings must be supplied in artifact, Service, then Preview order")
	}
	if args[0] == "" {
		return api.TenantQuotaLimits{}, false, errors.New("Tenant ID is required")
	}
	workspaces, err := strconv.ParseInt(args[1], 10, 16)
	if err != nil || workspaces < 1 || workspaces > 10 {
		return api.TenantQuotaLimits{}, false, errors.New("Workspace limit must be 1-10")
	}
	workloads, err := strconv.ParseInt(args[2], 10, 16)
	if err != nil || workloads < 1 || workloads > 5 {
		return api.TenantQuotaLimits{}, false, errors.New("Workload Run limit must be 1-5")
	}
	if queuedWorkloadRuns > 0 && queuedWorkloadRuns > workloads {
		return api.TenantQuotaLimits{}, false, errors.New("queued Workload Run limit cannot exceed total Workload Run limit")
	}
	limits := api.TenantQuotaLimits{TenantID: args[0], WorkspacesLimit: int16(workspaces), WorkloadRunsLimit: int16(workloads)}
	limits.WorkspaceComputeMilliCPULimit = workspaceComputeCPU
	limits.WorkspaceComputeMemoryMiBLimit = workspaceComputeMemory
	if queuedWorkloadRuns > 0 {
		limits.QueuedWorkloadRunsLimit = int16(queuedWorkloadRuns)
	}
	if len(args) >= 4 {
		gib, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil || gib < 1 || gib > 100 {
			return api.TenantQuotaLimits{}, false, errors.New("artifact storage limit must be 1-100 GiB")
		}
		limits.ArtifactBytesLimit = gib * (1 << 30)
	}
	if len(args) >= 6 {
		memoryGiB, memoryErr := strconv.ParseInt(args[4], 10, 64)
		milliCPU, cpuErr := strconv.ParseInt(args[5], 10, 64)
		if memoryErr != nil || memoryGiB < 1 || memoryGiB > 2 || cpuErr != nil || milliCPU < 1 || milliCPU > 1500 {
			return api.TenantQuotaLimits{}, false, errors.New("Service limits must be memory 1-2 GiB and CPU 1-1500 millicores")
		}
		limits.WorkspaceServiceMemoryBytesLimit = memoryGiB * (1 << 30)
		limits.WorkspaceServiceMilliCPULimit = milliCPU
	}
	if len(args) == 8 {
		previewRuns, runsErr := strconv.ParseInt(args[6], 10, 16)
		lifetimeHours, lifetimeErr := strconv.ParseInt(args[7], 10, 64)
		if runsErr != nil || previewRuns < 1 || previewRuns > 5 || lifetimeErr != nil || lifetimeHours < 1 || lifetimeHours > 24 {
			return api.TenantQuotaLimits{}, false, errors.New("Preview limits must be 1-5 runs and 1-24 hours")
		}
		limits.PreviewRunsLimit = int16(previewRuns)
		limits.PreviewLifetimeSecondsLimit = lifetimeHours * 3600
	}
	return limits, skipConfirmation, nil
}

func mergeTenantQuotaDefaults(target, current api.TenantQuotaLimits) api.TenantQuotaLimits {
	if target.WorkspaceComputeMilliCPULimit == 0 {
		target.WorkspaceComputeMilliCPULimit = current.WorkspaceComputeMilliCPULimit
	}
	if target.WorkspaceComputeMemoryMiBLimit == 0 {
		target.WorkspaceComputeMemoryMiBLimit = current.WorkspaceComputeMemoryMiBLimit
	}
	if target.QueuedWorkloadRunsLimit == 0 {
		target.QueuedWorkloadRunsLimit = current.QueuedWorkloadRunsLimit
		if target.QueuedWorkloadRunsLimit > target.WorkloadRunsLimit {
			target.QueuedWorkloadRunsLimit = target.WorkloadRunsLimit
		}
	}
	if target.ArtifactBytesLimit == 0 {
		target.ArtifactBytesLimit = current.ArtifactBytesLimit
	}
	if target.WorkspaceServiceMemoryBytesLimit == 0 {
		target.WorkspaceServiceMemoryBytesLimit = current.WorkspaceServiceMemoryBytesLimit
	}
	if target.WorkspaceServiceMilliCPULimit == 0 {
		target.WorkspaceServiceMilliCPULimit = current.WorkspaceServiceMilliCPULimit
	}
	if target.PreviewRunsLimit == 0 {
		target.PreviewRunsLimit = current.PreviewRunsLimit
	}
	if target.PreviewLifetimeSecondsLimit == 0 {
		target.PreviewLifetimeSecondsLimit = current.PreviewLifetimeSecondsLimit
	}
	return target
}

func executeTenantQuotaUpdate(tenantID string, target api.TenantQuotaLimits, skipConfirmation bool, input io.Reader, output io.Writer,
	readLimits func(string) (api.TenantQuotaLimits, error), readUsage func(string) (api.TenantUsage, error),
	update func(string, api.TenantQuotaLimits) (api.TenantQuotaLimits, error)) (api.TenantQuotaLimits, error) {
	if tenantID == "" || readLimits == nil || readUsage == nil || update == nil {
		return api.TenantQuotaLimits{}, errors.New("Tenant quota update dependencies are required")
	}
	current, err := readLimits(tenantID)
	if err != nil {
		return api.TenantQuotaLimits{}, fmt.Errorf("read current quota: %w", err)
	}
	usage, err := readUsage(tenantID)
	if err != nil {
		return api.TenantQuotaLimits{}, fmt.Errorf("read current usage: %w", err)
	}
	target = mergeTenantQuotaDefaults(target, current)
	if !skipConfirmation {
		confirmed, err := confirmTenantQuotaUpdate(input, output, current, usage, target)
		if err != nil {
			return api.TenantQuotaLimits{}, fmt.Errorf("confirmation failed: %w", err)
		}
		if !confirmed {
			return api.TenantQuotaLimits{}, errors.New("quota update cancelled")
		}
	}
	return update(tenantID, target)
}

func confirmTenantQuotaUpdate(input io.Reader, output io.Writer, current api.TenantQuotaLimits, usage api.TenantUsage, target api.TenantQuotaLimits) (bool, error) {
	if input == nil || output == nil {
		return false, errors.New("confirmation input and output are required; pass --yes for automation")
	}
	if target.WorkspaceComputeMilliCPULimit < 0 || target.WorkspaceComputeMemoryMiBLimit < 0 {
		target.WorkspaceComputeMilliCPULimit, target.WorkspaceComputeMemoryMiBLimit = 0, 0
	}
	if _, err := fmt.Fprintf(output, "Tenant %s quota update (current usage → requested limit):\n  Workspaces: %d → %d (current limit %d)\n  Workload Runs: %d → %d (current limit %d)\n  Queued Workload Runs: %d → %d (current limit %d)\n  Preview Runs: %d → %d (current limit %d)\n  Artifact bytes: %d used + %d reserved → %d (current limit %d)\n  Service memory bytes: current limit %d → %d\n  Service millicores: current limit %d → %d\n  Workspace compute reservations: %dmCPU / %dMiB used (%d profiles missing) → %dmCPU / %dMiB limits\nApply this quota change? [y/N] ",
		target.TenantID, usage.WorkspacesUsed, target.WorkspacesLimit, current.WorkspacesLimit,
		usage.WorkloadRunsUsed, target.WorkloadRunsLimit, current.WorkloadRunsLimit,
		usage.QueuedWorkloadRunsUsed, target.QueuedWorkloadRunsLimit, current.QueuedWorkloadRunsLimit,
		usage.PreviewRunsUsed, target.PreviewRunsLimit, current.PreviewRunsLimit,
		usage.ArtifactBytesUsed, usage.ArtifactBytesReserved, target.ArtifactBytesLimit, current.ArtifactBytesLimit,
		current.WorkspaceServiceMemoryBytesLimit, target.WorkspaceServiceMemoryBytesLimit,
		current.WorkspaceServiceMilliCPULimit, target.WorkspaceServiceMilliCPULimit,
		usage.WorkspaceComputeCPUMilliUsed, usage.WorkspaceComputeMemoryMiBUsed, usage.WorkspaceComputeProfilesMissing,
		target.WorkspaceComputeMilliCPULimit, target.WorkspaceComputeMemoryMiBLimit); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.TrimSpace(answer)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}

func writeTenantUsage(output io.Writer, tenantID string, usage api.TenantUsage) error {
	if output == nil || tenantID == "" || usage.WorkspacesUsed < 0 || usage.WorkspacesLimit < 1 {
		return errors.New("Tenant quota usage is invalid")
	}
	if usage.WorkloadRunsUsed < 0 || usage.WorkloadRunsLimit < 1 || usage.QueuedWorkloadRunsUsed < 0 || usage.QueuedWorkloadRunsLimit < 1 || usage.QueuedWorkloadRunsLimit > usage.WorkloadRunsLimit || usage.PreviewRunsUsed < 0 || usage.PreviewRunsLimit < 1 || usage.PreviewLifetimeLimit < 300 || usage.PreviewLifetimeLimit > 86400 || usage.ArtifactBytesUsed < 0 || usage.ArtifactBytesReserved < 0 || usage.ArtifactBytesLimit < 1 || usage.WorkspaceComputeCPUMilliUsed < 0 || usage.WorkspaceComputeMemoryMiBUsed < 0 || usage.WorkspaceComputeProfilesMissing < 0 {
		return errors.New("Tenant quota usage is invalid")
	}
	_, err := fmt.Fprintf(output, "Tenant %s quota: Workspaces %d/%d reserved; concurrent Workload Runs %d/%d; queued Workload Runs %d/%d; Previews %d/%d (max lifetime %s); artifacts %d stored + %d reserved / %d bytes; Workspace compute reservations %dmCPU/%dMiB (%d profiles missing) / %dmCPU/%dMiB limits\n",
		tenantID, usage.WorkspacesUsed, usage.WorkspacesLimit, usage.WorkloadRunsUsed, usage.WorkloadRunsLimit,
		usage.QueuedWorkloadRunsUsed, usage.QueuedWorkloadRunsLimit,
		usage.PreviewRunsUsed, usage.PreviewRunsLimit, time.Duration(usage.PreviewLifetimeLimit)*time.Second,
		usage.ArtifactBytesUsed, usage.ArtifactBytesReserved, usage.ArtifactBytesLimit,
		usage.WorkspaceComputeCPUMilliUsed, usage.WorkspaceComputeMemoryMiBUsed, usage.WorkspaceComputeProfilesMissing,
		usage.WorkspaceComputeCPUMilliLimit, usage.WorkspaceComputeMemoryMiBLimit)
	return err
}

func workspaceServiceResourceUsage(tenantID, workspaceID string) (api.WorkspaceServiceResourceUsage, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkspaceServiceResourceUsage{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkspaceServiceResourceUsage{}, errors.New("HAKO_API_URL is required")
	}
	return api.GetWorkspaceServiceResourceUsage(context.Background(), apiURL, accessToken, tenantID, workspaceID, nil)
}

func writeWorkspaceServiceResourceUsage(output io.Writer, workspaceID string, usage api.WorkspaceServiceResourceUsage) error {
	if output == nil || workspaceID == "" || usage.MemoryBytesUsed < 0 || usage.MemoryBytesLimit < 1 ||
		usage.MilliCPUUsed < 0 || usage.MilliCPULimit < 1 {
		return errors.New("Workspace Service resource usage is invalid")
	}
	_, err := fmt.Fprintf(output, "Workspace %s Service requests: memory %d/%d bytes; CPU %d/%d millicores (catalog reservations, not measured usage)\n",
		workspaceID, usage.MemoryBytesUsed, usage.MemoryBytesLimit, usage.MilliCPUUsed, usage.MilliCPULimit)
	return err
}

func getWorkspace(tenantID, workspaceID string) (api.WorkspaceView, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkspaceView{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkspaceView{}, errors.New("HAKO_API_URL is required")
	}
	return api.GetWorkspace(context.Background(), apiURL, accessToken, tenantID, workspaceID, nil)
}

func addWorkspaceService(tenantID, workspaceID, catalogName, name string) (domain.Service, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return domain.Service{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return domain.Service{}, errors.New("HAKO_API_URL is required")
	}
	return api.AddWorkspaceService(context.Background(), apiURL, accessToken, tenantID, workspaceID,
		api.AddWorkspaceServiceRequest{CatalogName: catalogName, Name: name}, nil)
}

func listWorkspaceServices(tenantID, workspaceID string) (api.WorkspaceServiceListResult, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkspaceServiceListResult{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkspaceServiceListResult{}, errors.New("HAKO_API_URL is required")
	}
	return api.ListWorkspaceServices(context.Background(), apiURL, accessToken, tenantID, workspaceID, nil)
}

func requestWorkspaceServiceAction(action, tenantID, workspaceID, serviceName string) (domain.Service, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return domain.Service{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return domain.Service{}, errors.New("HAKO_API_URL is required")
	}
	return api.RequestWorkspaceServiceAction(context.Background(), apiURL, accessToken, tenantID, workspaceID, serviceName, action, nil)
}

func workspaceAction(action, tenantID, workspaceID, idempotencyKey string) (api.WorkspaceActionResult, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkspaceActionResult{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkspaceActionResult{}, errors.New("HAKO_API_URL is required")
	}
	return api.RequestWorkspaceAction(context.Background(), apiURL, accessToken, tenantID, workspaceID, action, idempotencyKey, nil)
}

func listWorkloadRuns(tenantID string, limit, offset int) (api.WorkloadRunListResult, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.WorkloadRunListResult{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return api.WorkloadRunListResult{}, errors.New("HAKO_API_URL is required")
	}
	return api.ListWorkloadRuns(context.Background(), apiURL, accessToken, tenantID, limit, offset, nil)
}

func getWorkloadRun(tenantID, runID string) (domain.WorkloadRun, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return domain.WorkloadRun{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return domain.WorkloadRun{}, errors.New("HAKO_API_URL is required")
	}
	return api.GetWorkloadRun(context.Background(), apiURL, accessToken, tenantID, runID, nil)
}

func cancelWorkloadRun(tenantID, runID string) (domain.WorkloadRun, error) {
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return domain.WorkloadRun{}, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return domain.WorkloadRun{}, errors.New("HAKO_API_URL is required")
	}
	return api.CancelWorkloadRun(context.Background(), apiURL, accessToken, tenantID, runID, nil)
}

func createWorkloadCLI(args []string) (domain.WorkloadRun, string, error) {
	var run domain.WorkloadRun
	flags := flag.NewFlagSet("hako workload create", flag.ContinueOnError)
	tenantID := flags.String("tenant", "", "Hako Tenant ID")
	installationID := flags.Int64("installation", 0, "Tenant GitHub App Installation ID")
	repositoryID := flags.Int64("repository", 0, "GitHub Repository ID registered to the Tenant")
	commitSHA := flags.String("commit", "", "Immutable lowercase Git commit SHA")
	ref := flags.String("ref", "", "Git ref metadata (does not select the commit)")
	timeoutSeconds := flags.Int64("timeout-seconds", 0, "Job timeout in seconds (default 1800, maximum 86400)")
	idempotencyKey := flags.String("idempotency-key", "", "Stable request key to safely retry this create")
	if err := flags.Parse(args); err != nil {
		return run, "", err
	}
	if flags.NArg() != 0 || *tenantID == "" || *installationID <= 0 || *repositoryID <= 0 || *commitSHA == "" {
		return run, "", errors.New("required flags: --tenant, --installation, --repository, and --commit")
	}
	key := *idempotencyKey
	if key == "" {
		generatedKey, err := idgen.New("req_")
		if err != nil {
			return run, "", fmt.Errorf("generate idempotency key: %w", err)
		}
		key = generatedKey
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return run, key, err
	}
	apiURL := os.Getenv("HAKO_API_URL")
	if apiURL == "" {
		return run, key, errors.New("HAKO_API_URL is required")
	}
	request := api.WorkloadRunCreateRequest{GitHubInstallationID: *installationID, GitHubRepositoryID: *repositoryID,
		CommitSHA: *commitSHA, Ref: *ref, TimeoutSeconds: *timeoutSeconds}
	run, err = api.CreateWorkloadRun(context.Background(), apiURL, accessToken, *tenantID, key, request, nil)
	return run, key, err
}

func parseCLIPage(args []string) (int, int, error) {
	if len(args) > 2 {
		return 0, 0, errors.New("usage: hako list <tenant-id> [limit] [offset]")
	}
	limit, offset := 20, 0
	if len(args) > 0 {
		parsed, err := strconv.Atoi(args[0])
		if err != nil {
			return 0, 0, errors.New("limit must be an integer")
		}
		limit = parsed
	}
	if len(args) > 1 {
		parsed, err := strconv.Atoi(args[1])
		if err != nil {
			return 0, 0, errors.New("offset must be an integer")
		}
		offset = parsed
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
		return 0, 0, errors.New("limit must be 1-100 and offset must be 0-1000000")
	}
	return limit, offset, nil
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "linux", "freebsd", "openbsd", "netbsd":
		command = exec.Command("xdg-open", target)
	default:
		return errors.New("automatic browser launch is not supported on this operating system")
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
