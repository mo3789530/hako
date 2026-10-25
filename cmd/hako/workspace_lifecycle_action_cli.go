package main

import (
	"fmt"
	"io"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/idgen"
)

type workspaceReadinessWaiter func(tenantID, workspaceID string) error

// runWorkspaceLifecycleActionCLI handles suspend/resume/delete without
// process exits so request, confirmation, and progress behavior can be tested.
func runWorkspaceLifecycleActionCLI(action string, args []string, stdin io.Reader, stdout, stderr io.Writer,
	wait workspaceReadinessWaiter) int {
	if stdout == nil || stderr == nil {
		return 2
	}
	if len(args) < 2 {
		_, _ = fmt.Fprintf(stderr, "usage: hako %s <tenant-id> <workspace-id> [--idempotency-key <key>] [--wait] [--yes]\n", action)
		return 2
	}
	tenantID, workspaceID := args[0], args[1]
	options, err := parseWorkspaceActionOptions(action, args[2:])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hako %s: %v\nusage: hako %s <tenant-id> <workspace-id> [--idempotency-key <key>] [--wait] [--yes]\n", action, err, action)
		return 2
	}
	if options.wait && wait == nil {
		_, _ = fmt.Fprintln(stderr, "hako resume: Workspace readiness checker is unavailable")
		return 1
	}
	idempotencyKey := options.idempotencyKey
	if idempotencyKey == "" {
		idempotencyKey, err = idgen.New("req_")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako %s: %v\n", action, err)
			return 1
		}
	}
	requestAction := func() (api.WorkspaceActionResult, error) {
		return workspaceAction(action, tenantID, workspaceID, idempotencyKey)
	}
	var result api.WorkspaceActionResult
	if action == "delete" {
		confirmedResult, accepted, err := runConfirmedAction(
			func() (bool, error) {
				return authorizeWorkspaceDeletion(options.yes, stdin, stderr, tenantID, workspaceID)
			}, requestAction)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako delete: %v\n", err)
			return 1
		}
		if !accepted {
			_, _ = fmt.Fprintln(stderr, "Workspace deletion cancelled.")
			return 0
		}
		result = confirmedResult
	} else {
		result, err = requestAction()
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hako %s: %v\nRetry the same request with --idempotency-key %s\n", action, err, idempotencyKey)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Workspace %s accepted: desired=%s observed=%s (operation=%s state=%s, request-key=%s)\n",
		result.Workspace.Workspace.ID, result.Workspace.Status.DesiredState, result.Workspace.Status.ObservedState,
		result.OperationID, result.OperationState, idempotencyKey); err != nil {
		_, _ = fmt.Fprintf(stderr, "hako %s: write output: %v\n", action, err)
		return 1
	}
	if options.wait {
		if err := wait(tenantID, workspaceID); err != nil {
			_, _ = fmt.Fprintf(stderr, "hako resume: operation %s was accepted, but Workspace readiness was not confirmed: %v\n", result.OperationID, err)
			return 1
		}
	}
	return 0
}
