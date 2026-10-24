package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

type workspaceExecExitError struct{ code int }

func (e workspaceExecExitError) Error() string {
	return fmt.Sprintf("Workspace command exited with status %d", e.code)
}

func workspaceExec(ctx context.Context, tenantID, workspaceID string, argv []string, stdout, stderr io.Writer) error {
	return workspaceExecWith(ctx, tenantID, workspaceID, argv, stdout, stderr, nil, nil)
}

func workspaceExecWith(ctx context.Context, tenantID, workspaceID string, argv []string, stdout, stderr io.Writer,
	apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) (returnErr error) {
	request := workspaceprotocol.ExecRequest{Argv: append([]string(nil), argv...)}
	if ctx == nil || stdout == nil || stderr == nil || workspaceprotocol.ValidateExecRequest(request) != nil ||
		!validCopyID(tenantID) || !validCopyID(workspaceID) {
		return workspaceprotocol.ErrInvalidExec
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := strings.TrimSpace(os.Getenv("HAKO_API_URL")), strings.TrimSpace(os.Getenv("HAKO_GATEWAY_URL"))
	if apiURL == "" || gatewayURL == "" {
		return errors.New("HAKO_API_URL and HAKO_GATEWAY_URL are required")
	}
	session, err := api.CreateWorkspaceSession(ctx, apiURL, accessToken, tenantID, workspaceID,
		[]workspacesession.Permission{workspacesession.PermissionExec}, apiHTTPClient)
	if err != nil {
		return fmt.Errorf("request Workspace exec session: %w", err)
	}
	defer func() {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := api.RevokeWorkspaceSession(revokeCtx, apiURL, accessToken, tenantID, workspaceID, string(session.SessionID), apiHTTPClient); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("revoke Workspace exec session: %w", err)
		}
	}()
	connection, err := workspacegateway.Dial(ctx, gatewayURL, domain.WorkspaceID(workspaceID), workspacesession.PermissionExec, 0,
		session.Ticket, gatewayTLSConfig)
	if err != nil {
		return err
	}
	defer connection.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if err := websocket.Message.Send(connection, requestJSON(request)); err != nil {
		return errors.New("send Workspace exec request")
	}
	var payload []byte
	if err := websocket.Message.Receive(connection, &payload); err != nil || len(payload) == 0 || len(payload) > 1<<20 {
		return errors.New("receive Workspace exec result")
	}
	var result workspaceprotocol.ExecResult
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || workspaceprotocol.ValidateExecResult(result) != nil {
		return errors.New("Workspace returned an invalid exec result")
	}
	if _, err := stdout.Write(result.Stdout); err != nil {
		return errors.New("write Workspace command stdout")
	}
	if _, err := stderr.Write(result.Stderr); err != nil {
		return errors.New("write Workspace command stderr")
	}
	if result.TimedOut || result.Code == "timed_out" {
		return errors.New("Workspace command timed out")
	}
	if result.Code != "" || result.ExitCode < 0 {
		return errors.New("Workspace command could not be started")
	}
	if result.ExitCode != 0 {
		return workspaceExecExitError{code: result.ExitCode}
	}
	return nil
}

func requestJSON(request workspaceprotocol.ExecRequest) []byte {
	encoded, _ := json.Marshal(request)
	return encoded
}
