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
	if _, err := fmt.Fprintf(output, "Deleting Workspace %s in Tenant %s is irreversible. Type %q to continue: ", workspaceID, tenantID, workspaceID); err != nil {
		return false, errors.New("Workspace deletion confirmation could not be displayed")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64), 256)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, errors.New("Workspace deletion confirmation could not be read")
		}
		return false, nil
	}
	return strings.TrimSpace(scanner.Text()) == workspaceID, nil
}
