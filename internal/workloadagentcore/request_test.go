package workloadagentcore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestEncodeInvocationRequestBindsDigestAndAllAttemptPins(t *testing.T) {
	archive := []byte("trusted source archive bytes")
	digest := sha256.Sum256(archive)
	input := InvocationRequest{
		Command: workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
			Generation: 7, ResourcePlaneID: "rp_1", IdempotencyToken: "idempotency_1"},
		RepositoryID: 42, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceSHA256: hex.EncodeToString(digest[:]), Task: "add a test", SourceArchive: archive,
	}
	body, err := EncodeInvocationRequest(input)
	if err != nil {
		t.Fatal(err)
	}
	var got runtimeInvocationEnvelope
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.Attempt != (runtimeAttemptPins{TenantID: "tenant_1", RunID: "run_1",
		AttemptID: "attempt_1", ResourcePlaneID: "rp_1", Generation: 7, RepositoryID: 42,
		CommitSHA: input.CommitSHA, SourceSHA256: input.SourceSHA256}) || got.Task != input.Task {
		t.Fatalf("runtime invocation envelope mismatch: %+v", got)
	}
	decoded, err := base64StdDecode(got.SourceArchive)
	if err != nil || string(decoded) != string(archive) {
		t.Fatalf("encoded source archive mismatch: %q, %v", decoded, err)
	}
}

func TestEncodeInvocationRequestRejectsInvalidPinsAndArchive(t *testing.T) {
	archive := []byte("source")
	digest := sha256.Sum256(archive)
	base := InvocationRequest{Command: workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1",
		AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"},
		RepositoryID: 1, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceSHA256: hex.EncodeToString(digest[:]), Task: "task", SourceArchive: archive}
	for _, mutate := range []func(*InvocationRequest){
		func(v *InvocationRequest) { v.Command.TenantID = "tenant/invalid" },
		func(v *InvocationRequest) { v.RepositoryID = 0 },
		func(v *InvocationRequest) { v.CommitSHA = "bad" },
		func(v *InvocationRequest) { v.SourceSHA256 = "bad" },
		func(v *InvocationRequest) { v.SourceArchive = []byte("changed") },
		func(v *InvocationRequest) { v.Task = string(make([]byte, maxInvocationTaskBytes+1)) },
		func(v *InvocationRequest) { v.Task = string([]byte{0xff}) },
		func(v *InvocationRequest) { v.SourceArchive = nil },
	} {
		candidate := base
		mutate(&candidate)
		if _, err := EncodeInvocationRequest(candidate); err == nil {
			t.Fatalf("accepted invalid invocation input: %+v", candidate)
		}
	}
}

func base64StdDecode(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value)
}
