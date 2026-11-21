package workloadjournal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestJournalFreezesSpecificationAndResult(t *testing.T) {
	j, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	j.ExecutionState = Succeeded
	j.CleanupState = CleanupPending
	j.SpecificationHash = strings.Repeat("a", 64)
	j.ResultPayload = json.RawMessage(`{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":1,"resource_plane_id":"rp_1","status":"succeeded"}`)
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"specification", "result", "publication"} {
		t.Run(field, func(t *testing.T) {
			old := j
			old.ResultPublished = true
			next := old
			next.Revision++
			switch field {
			case "specification":
				next.SpecificationHash = strings.Repeat("b", 64)
			case "result":
				next.ResultPayload = append([]byte{' '}, j.ResultPayload...)
			case "publication":
				next.ResultPublished = false
			}
			if err := ValidateUpdate(Snapshot{Journal: old, ETag: "etag"}, next); !errors.Is(err, ErrConflict) {
				t.Fatal("durable metadata mutated", err)
			}
		})
	}
	for _, field := range []string{"hash", "missing_payload", "wrong_tenant", "nonterminal", "oversized"} {
		t.Run(field, func(t *testing.T) {
			next := j
			switch field {
			case "hash":
				next.SpecificationHash = "bad"
			case "missing_payload":
				next.ResultPublished = true
				next.ResultPayload = nil
			case "wrong_tenant":
				next.ResultPayload = json.RawMessage(strings.ReplaceAll(string(j.ResultPayload), "tenant_1", "tenant_other"))
			case "nonterminal":
				next.ExecutionState = Running
				next.CleanupState = CleanupNotStarted
			case "oversized":
				next.ResultPayload = make([]byte, 32769)
			}
			if err := next.Validate(); err == nil {
				t.Fatal("invalid journal result accepted")
			}
		})
	}
}

func TestFrozenSpecificationHashAndImmutability(t *testing.T) {
	j, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	j.SpecificationPayload = json.RawMessage(`{"source_digest":"fixed"}`)
	digest := sha256.Sum256(j.SpecificationPayload)
	j.SpecificationHash = hex.EncodeToString(digest[:])
	data, err := Encode(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(data); err != nil {
		t.Fatal("frozen payload lost on round trip", err)
	}
	next := j
	next.SpecificationPayload = json.RawMessage(`{"source_digest":"changed"}`)
	next.Revision++
	if err := next.Validate(); err == nil {
		t.Fatal("modified payload accepted with old hash")
	}
	digest = sha256.Sum256(next.SpecificationPayload)
	next.SpecificationHash = hex.EncodeToString(digest[:])
	if err := ValidateUpdate(Snapshot{Journal: j, ETag: "etag"}, next); !errors.Is(err, ErrConflict) {
		t.Fatal("rewrote frozen payload and hash", err)
	}
}
