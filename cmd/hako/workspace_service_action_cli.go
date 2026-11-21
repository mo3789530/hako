package main

import (
	"fmt"
	"io"

	"github.com/mo3789530/hako/internal/domain"
)

// runWorkspaceServiceActionCLI handles a parsed `service <action>` command.
// It returns the CLI exit status so confirmation and request ordering can be
// tested without invoking process exit or reaching a real API.
func runWorkspaceServiceActionCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if stdout == nil || stderr == nil {
		return 2
	}
	if len(args) < 4 || len(args) > 5 {
		_, _ = fmt.Fprintln(stderr, "usage: hako service <action> <tenant-id> <workspace-id> <instance-name> [--yes]")
		return 2
	}
	action, tenantID, workspaceID, serviceName := args[0], args[1], args[2], args[3]
	options, err := parseWorkspaceServiceActionOptions(action, args[4:])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hako service %s: %v\nusage: hako service %s <tenant-id> <workspace-id> <instance-name> [--yes]\n", action, err, action)
		return 2
	}
	requestAction := func() (domain.Service, error) {
		return requestWorkspaceServiceAction(action, tenantID, workspaceID, serviceName)
	}
	var service domain.Service
	if action == "remove" {
		confirmedService, accepted, err := runConfirmedAction(
			func() (bool, error) {
				return authorizeWorkspaceServiceRemoval(options.yes, stdin, stderr, tenantID, workspaceID, serviceName)
			}, requestAction)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hako service remove: %v\n", err)
			return 1
		}
		if !accepted {
			if _, err := fmt.Fprintln(stderr, "Workspace Service removal cancelled."); err != nil {
				return 1
			}
			return 0
		}
		service = confirmedService
	} else {
		service, err = requestAction()
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hako service %s: %v\n", action, err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Service %s %s accepted (desired=%s observed=%s)\n", service.Name, action, service.DesiredState, service.ObservedState); err != nil {
		_, _ = fmt.Fprintf(stderr, "hako service %s: write output: %v\n", action, err)
		return 1
	}
	return 0
}
