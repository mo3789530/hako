// Package workloadagentcore contains the AgentCore coding-agent backend.
// It is not connected to dispatch until its execution and cleanup gates exist.
package workloadagentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

var identityPart = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SessionID maps a durable dispatch identity to an opaque AgentCore session.
// The caller must obtain the command from the trusted journal, not from an
// unverified invocation payload. Persist the mapping before calling AWS.
func SessionID(command workloadjournal.Command) (string, error) {
	if !identityPart.MatchString(command.TenantID) || !identityPart.MatchString(command.WorkloadRunID) ||
		!identityPart.MatchString(command.AttemptID) || !identityPart.MatchString(command.ResourcePlaneID) ||
		!identityPart.MatchString(command.IdempotencyToken) || command.Generation < 1 {
		return "", errors.New("invalid AgentCore attempt identity")
	}
	// A versioned JSON array gives unambiguous field boundaries and avoids
	// exposing tenant or repository identifiers in AWS session metadata.
	identity, err := json.Marshal([]any{"hako-agentcore-session-v1", command.TenantID, command.WorkloadRunID,
		command.AttemptID, command.Generation, command.ResourcePlaneID, command.IdempotencyToken})
	if err != nil {
		return "", errors.New("encode AgentCore attempt identity")
	}
	digest := sha256.Sum256(identity)
	return "hako-" + hex.EncodeToString(digest[:]), nil
}
