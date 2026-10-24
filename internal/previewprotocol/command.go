// Package previewprotocol defines the versioned Control Plane to Resource
// Plane command envelope for Preview runtime generations.
package previewprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	AggregateType   = "repository_preview_runtime"
	ActionProvision = "provision"
	ActionCleanup   = "cleanup"
)

var (
	workspaceIDPattern   = regexp.MustCompile(`^prvws_[a-f0-9]{32}$`)
	resourcePlanePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	claimIDPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	headSHAPattern       = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
)

type Command struct {
	SchemaVersion      int       `json:"schema_version"`
	Action             string    `json:"action"`
	TenantID           string    `json:"tenant_id"`
	InstallationID     int64     `json:"installation_id"`
	RepositoryID       int64     `json:"repository_id"`
	PullRequestNumber  int64     `json:"pull_request_number"`
	Generation         int64     `json:"generation"`
	RuntimeWorkspaceID string    `json:"runtime_workspace_id"`
	ResourcePlaneID    string    `json:"resource_plane_id"`
	ClaimID            string    `json:"claim_id"`
	ClaimAttempt       int       `json:"claim_attempt"`
	LeaseExpiresAt     time.Time `json:"lease_expires_at"`
	HeadSHA            string    `json:"head_sha,omitempty"`
	HeadRef            string    `json:"head_ref,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
}

type RuntimeBinding struct {
	MicroVMID      string `json:"microvm_id"`
	GatewayRouteID string `json:"gateway_route_id"`
}

// Result is a provider-neutral acknowledgement. The Control Plane must fence
// every field against its current generation and active claim before applying
// any binding or retry transition.
type Result struct {
	SchemaVersion      int             `json:"schema_version"`
	Action             string          `json:"action"`
	TenantID           string          `json:"tenant_id"`
	InstallationID     int64           `json:"installation_id"`
	RepositoryID       int64           `json:"repository_id"`
	PullRequestNumber  int64           `json:"pull_request_number"`
	Generation         int64           `json:"generation"`
	RuntimeWorkspaceID string          `json:"runtime_workspace_id"`
	ResourcePlaneID    string          `json:"resource_plane_id"`
	ClaimID            string          `json:"claim_id"`
	ClaimAttempt       int             `json:"claim_attempt"`
	LeaseExpiresAt     time.Time       `json:"lease_expires_at"`
	Status             string          `json:"status"`
	ErrorCode          string          `json:"error_code,omitempty"`
	Binding            *RuntimeBinding `json:"binding,omitempty"`
	CompletedAt        time.Time       `json:"completed_at"`
}

func EventType(action string) string { return AggregateType + "." + action }

// Validate binds the serialized command to its Outbox event identity.
func (command Command) Validate(eventType, aggregateID string) error {
	if command.SchemaVersion != 1 || (command.Action != ActionProvision && command.Action != ActionCleanup) ||
		eventType != EventType(command.Action) || command.TenantID == "" || command.InstallationID <= 0 ||
		command.RepositoryID <= 0 || command.PullRequestNumber <= 0 || command.Generation <= 0 ||
		!workspaceIDPattern.MatchString(command.RuntimeWorkspaceID) || command.RuntimeWorkspaceID != aggregateID ||
		!resourcePlanePattern.MatchString(command.ResourcePlaneID) || !claimIDPattern.MatchString(command.ClaimID) ||
		command.ClaimAttempt < 1 || command.LeaseExpiresAt.IsZero() || command.ExpiresAt.IsZero() {
		return errors.New("Preview runtime command has an unsupported or inconsistent envelope")
	}
	switch command.Action {
	case ActionProvision:
		if !headSHAPattern.MatchString(command.HeadSHA) || command.HeadRef == "" || len(command.HeadRef) > 1024 || strings.ContainsAny(command.HeadRef, "\x00\r\n") {
			return errors.New("Preview provision command has an invalid pinned head")
		}
	case ActionCleanup:
		if command.HeadSHA != "" || command.HeadRef != "" {
			return errors.New("Preview cleanup command must not include a source head")
		}
	}
	return nil
}

func Encode(command Command) ([]byte, error) {
	if err := command.Validate(EventType(command.Action), command.RuntimeWorkspaceID); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("encode Preview runtime command: %w", err)
	}
	return payload, nil
}

func Decode(payload []byte, eventType, aggregateID string) (Command, error) {
	var command Command
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return Command{}, fmt.Errorf("decode Preview runtime command: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Command{}, errors.New("Preview runtime command must contain exactly one JSON object")
	}
	if err := command.Validate(eventType, aggregateID); err != nil {
		return Command{}, err
	}
	return command, nil
}

func (result Result) Validate() error {
	if result.SchemaVersion != 1 || (result.Action != ActionProvision && result.Action != ActionCleanup) ||
		result.TenantID == "" || result.InstallationID <= 0 || result.RepositoryID <= 0 || result.PullRequestNumber <= 0 || result.Generation <= 0 ||
		!workspaceIDPattern.MatchString(result.RuntimeWorkspaceID) || !resourcePlanePattern.MatchString(result.ResourcePlaneID) ||
		!claimIDPattern.MatchString(result.ClaimID) || result.ClaimAttempt < 1 || result.LeaseExpiresAt.IsZero() || result.CompletedAt.IsZero() {
		return errors.New("Preview runtime result has an unsupported or incomplete envelope")
	}
	if result.Status == "succeeded" {
		if result.ErrorCode != "" || (result.Action == ActionProvision && (result.Binding == nil || !validProviderID(result.Binding.MicroVMID) || !validProviderID(result.Binding.GatewayRouteID))) ||
			(result.Action == ActionCleanup && result.Binding != nil) {
			return errors.New("successful Preview runtime result has an invalid binding")
		}
		return nil
	}
	if result.Status != "failed" || result.Binding != nil {
		return errors.New("Preview runtime result has an unsupported status")
	}
	switch result.Action {
	case ActionProvision:
		switch result.ErrorCode {
		case "claim_lease_expired", "preview_expired", "provider_unavailable", "runtime_profile_missing", "microvm_launch_failed", "gateway_route_publish_failed", "invalid_runtime_binding":
			return nil
		}
	case ActionCleanup:
		switch result.ErrorCode {
		case "claim_lease_expired", "provider_unavailable", "gateway_route_remove_failed", "microvm_terminate_failed":
			return nil
		}
	}
	return errors.New("Preview runtime result has an unsupported error code")
}

func EncodeResult(result Result) ([]byte, error) {
	if err := result.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode Preview runtime result: %w", err)
	}
	return payload, nil
}

func DecodeResult(payload []byte) (Result, error) {
	var result Result
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode Preview runtime result: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("Preview runtime result must contain exactly one JSON object")
	}
	if err := result.Validate(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func validProviderID(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' || char == ':') {
			return false
		}
	}
	return true
}
