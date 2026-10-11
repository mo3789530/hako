package workloadjobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/workloads"
)

// CreateWebhookRepositoryRun creates the fixed, untrusted verification Job
// for an explicitly enabled Tenant repository. It creates the queued GitHub
// Check and its delivery Outbox event inside the same caller transaction.
func CreateWebhookRepositoryRun(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, integrationID, deliveryID string, now time.Time) (bool, error) {
	if ctx == nil || tx == nil || tenantID == "" || now.IsZero() {
		return false, webhookstore.ErrUnsupportedWebhookEvent
	}
	runID, err := idgen.New("run_")
	if err != nil {
		return false, err
	}
	_, created, err := workloads.CreateOrGetFromWebhookDelivery(ctx, tx, integrationID, deliveryID, domain.WorkloadRun{
		ID: domain.WorkloadRunID(runID), TenantID: tenantID, Kind: domain.WorkloadJob,
		RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
		TimeoutSeconds: int64(workloads.DefaultTimeout / time.Second), CreatedAt: now.UTC(),
	})
	return created, err
}
