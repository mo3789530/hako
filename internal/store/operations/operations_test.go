package operations

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type validationBeginner struct{}

func (validationBeginner) Begin(context.Context) (pgx.Tx, error) {
	panic("invalid input must fail before Begin")
}

func TestRequestHashAndVerifyRequest(t *testing.T) {
	request := map[string]any{"name": "api-dev", "runtime_class": "standard"}
	hash, err := RequestHash("tenant_1", domain.OperationEnsureRunning, request)
	if err != nil || hash == "" {
		t.Fatalf("request hash = %q, %v", hash, err)
	}
	op := domain.Operation{TenantID: "tenant_1", Type: domain.OperationEnsureRunning, RequestHash: hash}
	if err := VerifyRequest(op, domain.OperationEnsureRunning, request); err != nil {
		t.Fatalf("matching request rejected: %v", err)
	}
	if err := VerifyRequest(op, domain.OperationSuspend, request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different operation type should conflict, got %v", err)
	}
	if err := VerifyRequest(op, domain.OperationEnsureRunning, map[string]any{"name": "other"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different request should conflict, got %v", err)
	}
	if _, err := RequestHash("tenant_1", domain.OperationEnsureRunning, func() {}); err == nil {
		t.Fatal("unserializable normalized request should fail")
	}
	if err := VerifyRequest(op, domain.OperationEnsureRunning, func() {}); err == nil {
		t.Fatal("fingerprinting failure should be returned")
	}
}

func TestOperationMutationValidationBeforeDatabaseAccess(t *testing.T) {
	ctx := t.Context()
	pool := validationBeginner{}
	validResult := ResultInput{
		OperationID: "op_1", TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1",
		Type: domain.OperationEnsureRunning, Status: domain.OperationSucceeded,
		ObservedState: domain.ObservedWorkspaceRunning, CompletedAt: time.Now(),
	}
	resultCases := []struct {
		name   string
		mutate func(*ResultInput)
	}{
		{"missing identity", func(input *ResultInput) { input.OperationID = "" }},
		{"nonterminal status", func(input *ResultInput) { input.Status = domain.OperationRunning }},
		{"failed result wrong state", func(input *ResultInput) { input.Status = domain.OperationFailed }},
		{"successful result wrong state", func(input *ResultInput) { input.ObservedState = domain.ObservedWorkspaceFailed }},
		{"invalid payload", func(input *ResultInput) { input.Payload = json.RawMessage("{") }},
		{"missing completion timestamp", func(input *ResultInput) { input.CompletedAt = time.Time{} }},
	}
	for _, test := range resultCases {
		t.Run("result/"+test.name, func(t *testing.T) {
			input := validResult
			test.mutate(&input)
			if _, err := ApplyResult(ctx, pool, transaction.Policy{}, input); err == nil {
				t.Fatal("invalid result should be rejected")
			}
		})
	}

	validTransitionInput := TransitionInput{
		OperationID: "op_1", ExpectedStatus: domain.OperationPending, NextStatus: domain.OperationRunning,
		EventType: "operation.started", OccurredAt: time.Now(),
	}
	transitionCases := []struct {
		name   string
		mutate func(*TransitionInput)
	}{
		{"missing operation", func(input *TransitionInput) { input.OperationID = "" }},
		{"invalid state transition", func(input *TransitionInput) { input.NextStatus = domain.OperationSucceeded }},
		{"invalid payload", func(input *TransitionInput) { input.Payload = json.RawMessage("not-json") }},
		{"invalid observed state", func(input *TransitionInput) {
			state := domain.ObservedWorkspaceState("unknown")
			input.ObservedState = &state
		}},
	}
	for _, test := range transitionCases {
		t.Run("transition/"+test.name, func(t *testing.T) {
			input := validTransitionInput
			test.mutate(&input)
			if _, err := Transition(ctx, pool, transaction.Policy{}, input); err == nil {
				t.Fatal("invalid transition should be rejected")
			}
		})
	}
	if _, err := ListEvents(ctx, nil, "op_1"); err == nil {
		t.Fatal("nil transaction should be rejected")
	}
	if _, err := ListEvents(ctx, nil, ""); err == nil {
		t.Fatal("missing operation id should be rejected")
	}
}
