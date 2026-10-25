package githubwebhook

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	webhookmodel "github.com/mo3789530/hako/internal/githubwebhook"
)

var agentIssueClaimPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

type AgentIssueCommandDelivery struct {
	IntegrationID string
	DeliveryID    string
}

// ClaimAgentIssueCommands claims a bounded set using conditional updates rather
// than row locks so the operation remains compatible with Aurora DSQL OCC.
// A worker that exits before processing can be replaced after its lease expires.
func ClaimAgentIssueCommands(ctx context.Context, tx pgx.Tx, claimID string, now time.Time, lease time.Duration, limit int) ([]AgentIssueCommandDelivery, error) {
	if ctx == nil || tx == nil || !agentIssueClaimPattern.MatchString(claimID) || now.IsZero() ||
		lease < 30*time.Second || lease > 10*time.Minute || limit < 1 || limit > 100 {
		return nil, errors.New("valid Agent Issue claim identity, lease, transaction, and batch size are required")
	}
	rows, err := tx.Query(ctx, `SELECT integration_id,delivery_id FROM github_webhook_deliveries
		WHERE event_schema_version=$1 AND event_type='issue_comment' AND action='created'
		  AND ((processing_status='received' AND agent_processing_claim_id IS NULL) OR
		       (processing_status='processing' AND agent_processing_lease_expires_at <= $2))
		ORDER BY received_at,delivery_id LIMIT $3`, webhookmodel.RepositoryEventSchemaVersion, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("select pending Agent Issue commands: %w", err)
	}
	candidates := make([]AgentIssueCommandDelivery, 0, limit)
	for rows.Next() {
		var item AgentIssueCommandDelivery
		if err := rows.Scan(&item.IntegrationID, &item.DeliveryID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pending Agent Issue command: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read pending Agent Issue commands: %w", err)
	}
	rows.Close()

	claimed := make([]AgentIssueCommandDelivery, 0, len(candidates))
	for _, item := range candidates {
		tag, err := tx.Exec(ctx, `UPDATE github_webhook_deliveries
			SET processing_status='processing',agent_processing_claim_id=$3,agent_processing_lease_expires_at=$4
			WHERE integration_id=$1 AND delivery_id=$2 AND event_schema_version=$5 AND event_type='issue_comment' AND action='created'
		  AND ((processing_status='received' AND agent_processing_claim_id IS NULL) OR
		       (processing_status='processing' AND agent_processing_lease_expires_at <= $6))`,
			item.IntegrationID, item.DeliveryID, claimID, now.UTC().Add(lease), webhookmodel.RepositoryEventSchemaVersion, now.UTC())
		if err != nil {
			return nil, fmt.Errorf("claim Agent Issue command: %w", err)
		}
		if tag.RowsAffected() == 1 {
			claimed = append(claimed, item)
		}
	}
	return claimed, nil
}
