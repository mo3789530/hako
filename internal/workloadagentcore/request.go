package workloadagentcore

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

const (
	maxInvocationSourceBytes   = 40 << 20
	maxInvocationTaskBytes     = 64 << 10
	maxInvocationBodyBytes     = 56 << 20
	maxInvocationResponseBytes = 12 << 20
)

var commitDigestPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var sourceDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// InvocationRequest contains only immutable attempt pins, bounded task text,
// and the digest-verified source archive. Callers must derive it from trusted
// Hako state and an authorized source materializer; it is not an auth token.
type InvocationRequest struct {
	Command       workloadjournal.Command
	RepositoryID  int64
	CommitSHA     string
	SourceSHA256  string
	Task          string
	SourceArchive []byte
}

type runtimeInvocationEnvelope struct {
	SchemaVersion int                `json:"schema_version"`
	Attempt       runtimeAttemptPins `json:"attempt"`
	Task          string             `json:"task"`
	SourceArchive string             `json:"source_archive_base64"`
}

type runtimeAttemptPins struct {
	TenantID        string `json:"tenant_id"`
	RunID           string `json:"run_id"`
	AttemptID       string `json:"attempt_id"`
	ResourcePlaneID string `json:"resource_plane_id"`
	Generation      int64  `json:"generation"`
	RepositoryID    int64  `json:"repository_id"`
	CommitSHA       string `json:"commit_sha"`
	SourceSHA256    string `json:"source_sha256"`
}

// EncodeInvocationRequest validates and serializes the exact versioned request
// accepted by coding-agent/src/hako_coding_agent/runtime.py. It always verifies
// the archive bytes against the already authorized source digest.
func EncodeInvocationRequest(input InvocationRequest) ([]byte, error) {
	if _, err := SessionID(input.Command); err != nil || input.RepositoryID < 1 ||
		!commitDigestPattern.MatchString(input.CommitSHA) || !sourceDigestPattern.MatchString(input.SourceSHA256) ||
		len(input.Task) == 0 || len(input.Task) > maxInvocationTaskBytes || !utf8.ValidString(input.Task) ||
		len(input.SourceArchive) == 0 || len(input.SourceArchive) > maxInvocationSourceBytes {
		return nil, errors.New("AgentCore invocation request is invalid")
	}
	digest := sha256.Sum256(input.SourceArchive)
	actual := hex.EncodeToString(digest[:])
	if subtle.ConstantTimeCompare([]byte(actual), []byte(input.SourceSHA256)) != 1 {
		return nil, errors.New("AgentCore source archive digest mismatch")
	}
	encoded := base64.StdEncoding.EncodeToString(input.SourceArchive)
	// Bound before allocating the JSON body as well as after encoding.
	if len(encoded) > maxInvocationBodyBytes {
		return nil, errors.New("AgentCore source archive exceeds request limit")
	}
	envelope := runtimeInvocationEnvelope{SchemaVersion: 1,
		Attempt: runtimeAttemptPins{TenantID: input.Command.TenantID, RunID: input.Command.WorkloadRunID,
			AttemptID: input.Command.AttemptID, ResourcePlaneID: input.Command.ResourcePlaneID,
			Generation: input.Command.Generation, RepositoryID: input.RepositoryID,
			CommitSHA: input.CommitSHA, SourceSHA256: input.SourceSHA256},
		Task: input.Task, SourceArchive: encoded}
	body, err := json.Marshal(envelope)
	if err != nil || len(body) > maxInvocationBodyBytes {
		return nil, errors.New("AgentCore invocation request exceeds contract")
	}
	return body, nil
}
