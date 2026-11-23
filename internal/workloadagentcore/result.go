package workloadagentcore

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrInvocationResult = errors.New("AgentCore response does not match the current attempt")

type InvocationDraft struct {
	Attempt      runtimeAttemptPins
	Status       string
	Iterations   int
	TreeDigest   string
	ProposalSHA  string
	ProposalData []byte
}

type runtimeProposalResponse struct {
	SHA256     string `json:"sha256"`
	DataBase64 string `json:"data_base64"`
}

type runtimeInvocationResponse struct {
	SchemaVersion int                      `json:"schema_version"`
	Attempt       runtimeAttemptPins       `json:"attempt"`
	DraftStatus   string                   `json:"draft_status"`
	Iterations    int                      `json:"iterations"`
	TreeDigest    string                   `json:"tree_digest"`
	Proposal      *runtimeProposalResponse `json:"proposal"`
}

// DecodeInvocationResponse treats AgentCore output as untrusted. It binds all
// returned pins to the request and verifies both the base64 and proposal
// digest before exposing proposal bytes to Hako's independent verifier.
func DecodeInvocationResponse(data []byte, expected InvocationRequest) (InvocationDraft, error) {
	if len(data) == 0 || len(data) > maxInvocationResponseBytes ||
		ValidateInvocationRequest(expected) != nil || rejectDuplicateJSONFields(data) != nil {
		return InvocationDraft{}, ErrInvocationResult
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response runtimeInvocationResponse
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF ||
		response.SchemaVersion != 1 || response.Attempt != attemptPins(expected) ||
		response.Iterations < 0 || response.Iterations > 10 || !sourceDigestPattern.MatchString(response.TreeDigest) {
		return InvocationDraft{}, ErrInvocationResult
	}
	draft := InvocationDraft{Attempt: response.Attempt, Status: response.DraftStatus,
		Iterations: response.Iterations, TreeDigest: response.TreeDigest}
	switch response.DraftStatus {
	case "no_proposal":
		if response.Proposal != nil {
			return InvocationDraft{}, ErrInvocationResult
		}
	case "proposal_ready":
		if response.Proposal == nil || !sourceDigestPattern.MatchString(response.Proposal.SHA256) || response.Proposal.DataBase64 == "" {
			return InvocationDraft{}, ErrInvocationResult
		}
		proposal, err := base64.StdEncoding.Strict().DecodeString(response.Proposal.DataBase64)
		if err != nil || len(proposal) == 0 || len(proposal) > maxInvocationResponseBytes {
			return InvocationDraft{}, ErrInvocationResult
		}
		digest := sha256.Sum256(proposal)
		actual := hex.EncodeToString(digest[:])
		if subtle.ConstantTimeCompare([]byte(actual), []byte(response.Proposal.SHA256)) != 1 {
			return InvocationDraft{}, ErrInvocationResult
		}
		// Reject malformed, protected-path, or credential-bearing archives
		// before handing bytes to an artifact store. The independent verifier
		// must still apply the proposal to the pinned base and run its profile.
		inspection, err := workloadsource.InspectAgentProposalArchive(proposal)
		if err != nil || inspection.SHA256 != actual {
			return InvocationDraft{}, ErrInvocationResult
		}
		draft.ProposalSHA, draft.ProposalData = actual, proposal
	default:
		return InvocationDraft{}, ErrInvocationResult
	}
	return draft, nil
}

func ValidateInvocationRequest(input InvocationRequest) error {
	_, err := EncodeInvocationRequest(input)
	return err
}

func attemptPins(input InvocationRequest) runtimeAttemptPins {
	return runtimeAttemptPins{TenantID: input.Command.TenantID, RunID: input.Command.WorkloadRunID,
		AttemptID: input.Command.AttemptID, ResourcePlaneID: input.Command.ResourcePlaneID,
		Generation: input.Command.Generation, RepositoryID: input.RepositoryID,
		CommitSHA: input.CommitSHA, SourceSHA256: input.SourceSHA256}
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvocationResult
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvocationResult
			}
			if _, exists := seen[key]; exists {
				return ErrInvocationResult
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvocationResult
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvocationResult
		}
	default:
		return ErrInvocationResult
	}
	return nil
}
