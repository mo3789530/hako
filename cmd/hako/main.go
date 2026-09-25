package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/idgen"
)

var cliCredentialStore auth.CredentialStore = auth.OSKeyring{}
var cliCredentialGetter auth.CredentialGetter = auth.OSKeyring{}
var cliBrowserOpener auth.BrowserOpener = openBrowser

func main() {
	switch {
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
	case len(os.Args) == 4 && os.Args[1] == "tenant" && os.Args[2] == "membership":
		membership, err := tenantMembership(os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako tenant membership: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Tenant %s: %s (user %s)\n", membership.TenantID, membership.Role, membership.UserID)
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
	case (len(os.Args) == 4 || len(os.Args) == 6) && (os.Args[1] == "suspend" || os.Args[1] == "resume" || os.Args[1] == "delete"):
		action, tenantID, workspaceID := os.Args[1], os.Args[2], os.Args[3]
		idempotencyKey := ""
		if len(os.Args) == 6 {
			if os.Args[4] != "--idempotency-key" {
				fmt.Fprintf(os.Stderr, "usage: hako %s <tenant-id> <workspace-id> [--idempotency-key <key>]\n", action)
				os.Exit(2)
			}
			idempotencyKey = os.Args[5]
		} else {
			generatedKey, err := idgen.New("req_")
			if err != nil {
				fmt.Fprintf(os.Stderr, "hako %s: %v\n", action, err)
				os.Exit(1)
			}
			idempotencyKey = generatedKey
		}
		result, err := workspaceAction(action, tenantID, workspaceID, idempotencyKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hako %s: %v\nRetry the same request with --idempotency-key %s\n", action, err, idempotencyKey)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Workspace %s accepted: desired=%s observed=%s (operation=%s state=%s, request-key=%s)\n", result.Workspace.Workspace.ID, result.Workspace.Status.DesiredState, result.Workspace.Status.ObservedState, result.OperationID, result.OperationState, idempotencyKey)
	default:
		fmt.Fprintln(os.Stderr, "usage: hako login | hako api health | hako tenant membership <tenant-id> | hako tenant placement-policy get <tenant-id> | hako tenant placement-policy set <tenant-id> '<policy-json>' | hako create <tenant-id> <workspace-name> [--idempotency-key <key>] | hako list <tenant-id> [limit] [offset] | hako get <tenant-id> <workspace-id> | hako suspend|resume|delete <tenant-id> <workspace-id> [--idempotency-key <key>]")
		os.Exit(2)
	}
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
