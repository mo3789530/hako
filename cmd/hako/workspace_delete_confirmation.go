package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

func authorizeWorkspaceDeletion(yes bool, input io.Reader, output io.Writer, tenantID, workspaceID string) (bool, error) {
	if yes {
		return true, nil
	}
	return confirmWorkspaceDeletion(input, output, tenantID, workspaceID)
}

func confirmWorkspaceDeletion(input io.Reader, output io.Writer, tenantID, workspaceID string) (bool, error) {
	if input == nil || output == nil || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(workspaceID) == "" {
		return false, errors.New("Workspace deletion confirmation is not configured")
	}
	prompt := fmt.Sprintf("Deleting Workspace %s in Tenant %s is irreversible. Type %q to continue: ", workspaceID, tenantID, workspaceID)
	return confirmExactTarget(input, output, prompt, workspaceID)
}

func authorizeWorkspaceServiceRemoval(yes bool, input io.Reader, output io.Writer, tenantID, workspaceID, serviceName string) (bool, error) {
	if yes {
		return true, nil
	}
	if input == nil || output == nil || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(serviceName) == "" {
		return false, errors.New("Workspace Service removal confirmation is not configured")
	}
	prompt := fmt.Sprintf("Removing Service %s in Workspace %s (Tenant %s) deletes its data. Type %q to continue: ", serviceName, workspaceID, tenantID, serviceName)
	return confirmExactTarget(input, output, prompt, serviceName)
}

func confirmExactTarget(input io.Reader, output io.Writer, prompt, expected string) (bool, error) {
	if input == nil || output == nil || strings.TrimSpace(expected) == "" {
		return false, errors.New("confirmation is not configured")
	}
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return false, errors.New("confirmation prompt could not be displayed")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64), 256)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, errors.New("confirmation could not be read")
		}
		return false, nil
	}
	return strings.TrimSpace(scanner.Text()) == expected, nil
}
