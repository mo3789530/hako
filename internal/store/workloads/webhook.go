package workloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	webhookevent "github.com/mo3789530/hako/internal/githubwebhook"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
)

var ErrWebhookDeliveryRunMismatch = errors.New("Workload Run does not match its authorized GitHub webhook delivery")

// WebhookDeliveryIdempotencyKey deterministically maps an integration-scoped
// GitHub Delivery ID to a Workload request key without exposing the raw ID in
// operational tables or downstream messages.
func WebhookDeliveryIdempotencyKey(integrationID, deliveryID string) string {
	sum := sha256.Sum256([]byte(integrationID + "\x00" + deliveryID))
	return "github-delivery-" + hex.EncodeToString(sum[:])
}

// CreateOrGetFromWebhookDelivery creates a Job Run from an authenticated,
// Tenant-bound event. Trigger selection and trust/runtime policy must already
// have been approved by the caller; repository identity, ref and SHA are
// always reloaded from the durable inbox. Repeated delivery processing returns
// the same immutable Run via a stable Delivery ID idempotency key.
func CreateOrGetFromWebhookDelivery(ctx context.Context, tx pgx.Tx, integrationID, deliveryID string, run domain.WorkloadRun) (domain.WorkloadRun, bool, error) {
	if tx == nil || run.TenantID == "" || run.Kind != domain.WorkloadJob {
		return domain.WorkloadRun{}, false, ErrWebhookDeliveryRunMismatch
	}
	event, err := webhookstore.LoadAuthorizedRepositoryEvent(ctx, tx, run.TenantID, integrationID, deliveryID)
	if err != nil {
		return domain.WorkloadRun{}, false, err
	}
	if !eligibleRunTrigger(event) {
		return domain.WorkloadRun{}, false, ErrWebhookDeliveryRunMismatch
	}
	key := WebhookDeliveryIdempotencyKey(integrationID, deliveryID)
	if run.IdempotencyKey != "" && run.IdempotencyKey != key {
		return domain.WorkloadRun{}, false, ErrWebhookDeliveryRunMismatch
	}
	if run.GitHubInstallation != 0 && run.GitHubInstallation != event.InstallationID ||
		run.GitHubRepository != 0 && run.GitHubRepository != event.Repository.GitHubID ||
		run.CommitSHA != "" && run.CommitSHA != event.CommitSHA || run.Ref != "" && run.Ref != event.Ref {
		return domain.WorkloadRun{}, false, ErrWebhookDeliveryRunMismatch
	}
	run.GitHubInstallation = event.InstallationID
	run.GitHubRepository = event.Repository.GitHubID
	run.CommitSHA = event.CommitSHA
	run.Ref = event.Ref
	run.IdempotencyKey = key
	stored, created, err := CreateOrGet(ctx, tx, run)
	if err != nil {
		return domain.WorkloadRun{}, false, err
	}
	if integrationID != "shared" && stored.GitHubIntegrationID != integrationID {
		return domain.WorkloadRun{}, false, ErrWebhookDeliveryRunMismatch
	}
	if stored.GitHubInstallation != event.InstallationID || stored.GitHubRepository != event.Repository.GitHubID || stored.CommitSHA != event.CommitSHA || stored.Ref != event.Ref {
		return domain.WorkloadRun{}, false, fmt.Errorf("stored idempotent Workload Run identity differs from webhook event: %w", ErrWebhookDeliveryRunMismatch)
	}
	return stored, created, nil
}

func eligibleRunTrigger(event webhookevent.RepositoryEvent) bool {
	if event.GitHubEvent == "push" && event.Deleted {
		return false
	}
	switch event.Type {
	case "repository.push", "repository.pull_request.opened", "repository.pull_request.reopened", "repository.pull_request.synchronize", "repository.workflow_job.queued":
		return true
	default:
		return false
	}
}
