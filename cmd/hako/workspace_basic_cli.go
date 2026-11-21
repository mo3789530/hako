package main

import (
	"fmt"
	"io"

	"github.com/mo3789530/hako/internal/idgen"
)

// runWorkspaceBasicCLI handles create/list/get separately from process exit so
// the CLI request and output contract can be exercised without subprocesses.
func runWorkspaceBasicCLI(command string, args []string, stdout, stderr io.Writer) int {
	if stdout == nil || stderr == nil {
		return 2
	}
	switch command {
	case "create":
		if len(args) != 2 && len(args) != 4 {
			_, _ = fmt.Fprintln(stderr, "usage: hako create <tenant-id> <workspace-name> [--idempotency-key <key>]")
			return 2
		}
		idempotencyKey := ""
		if len(args) == 4 {
			if args[2] != "--idempotency-key" || args[3] == "" {
				_, _ = fmt.Fprintln(stderr, "usage: hako create <tenant-id> <workspace-name> [--idempotency-key <key>]")
				return 2
			}
			idempotencyKey = args[3]
		} else {
			var err error
			idempotencyKey, err = idgen.New("req_")
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "hako create: %v\n", err)
				return 1
			}
		}
		created, err := createWorkspace(args[0], args[1], idempotencyKey)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako create: %v\nRetry the same request with --idempotency-key %s\n", err, idempotencyKey)
			return 1
		}
		if _, err := fmt.Fprintf(stdout, "Workspace %s accepted: %s (desired=%s observed=%s, operation=%s, request-key=%s)\n",
			created.Name, created.ID, created.DesiredState, created.ObservedState, created.OperationID, idempotencyKey); err != nil {
			_, _ = fmt.Fprintf(stderr, "hako create: write output: %v\n", err)
			return 1
		}
		return 0
	case "list":
		if len(args) < 1 || len(args) > 3 {
			_, _ = fmt.Fprintln(stderr, "usage: hako list <tenant-id> [limit] [offset]")
			return 2
		}
		limit, offset, err := parseCLIPage(args[1:])
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako list: %v\n", err)
			return 2
		}
		result, err := listWorkspaces(args[0], limit, offset)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako list: %v\n", err)
			return 1
		}
		for _, item := range result.Items {
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", item.Workspace.ID, item.Workspace.Name, item.Status.DesiredState, item.Status.ObservedState); err != nil {
				_, _ = fmt.Fprintf(stderr, "hako list: write output: %v\n", err)
				return 1
			}
		}
		if _, err := fmt.Fprintf(stdout, "Showing %d of %d workspaces (limit=%d offset=%d)\n", len(result.Items), result.Total, result.Limit, result.Offset); err != nil {
			_, _ = fmt.Fprintf(stderr, "hako list: write output: %v\n", err)
			return 1
		}
		return 0
	case "get":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(stderr, "usage: hako get <tenant-id> <workspace-id>")
			return 2
		}
		workspace, err := getWorkspace(args[0], args[1])
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako get: %v\n", err)
			return 1
		}
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", workspace.Workspace.ID, workspace.Workspace.Name,
			workspace.Status.DesiredState, workspace.Status.ObservedState); err != nil {
			_, _ = fmt.Fprintf(stderr, "hako get: write output: %v\n", err)
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unsupported Workspace command %q\n", command)
		return 2
	}
}
