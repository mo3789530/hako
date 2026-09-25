// Package scheduler chooses an eligible Resource Plane for a new Workspace.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/placementpolicies"
	"github.com/mo3789530/hako/internal/store/resourcecapacity"
)

var ErrNoEligibleResourcePlane = errors.New("no active Resource Plane satisfies the Workspace requirements")

const HealthFreshnessTTL = 5 * time.Minute

type Policy struct {
	TenantID             domain.TenantID
	ResourcePlaneID      domain.ResourcePlaneID
	Region               string
	RequiredCapabilities []string
}

type candidate struct {
	id                 domain.ResourcePlaneID
	region             string
	capabilities       []string
	load               int64
	capacityConfigured bool
	healthStatus       string
}

// Select chooses the least-loaded eligible active Resource Plane. Load is the
// number of placed Workspaces that have not been observed deleted. Ties are
// deterministic by region and ID. Selection runs inside the caller's create
// transaction so its Placement and Operation use the same decision.
func Select(ctx context.Context, tx pgx.Tx, policy Policy) (domain.ResourcePlaneID, error) {
	if tx == nil {
		return "", errors.New("database transaction is required")
	}
	policy.Region = strings.TrimSpace(policy.Region)
	required := make(map[string]struct{}, len(policy.RequiredCapabilities))
	for _, capability := range policy.RequiredCapabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" {
			return "", errors.New("required Resource Plane capability must not be empty")
		}
		required[capability] = struct{}{}
	}
	var tenantPolicy placementpolicies.Policy
	if policy.TenantID != "" {
		var err error
		tenantPolicy, err = placementpolicies.Get(ctx, tx, policy.TenantID)
		if err != nil {
			return "", err
		}
		for _, capability := range tenantPolicy.RequiredCapabilities {
			required[strings.ToLower(capability)] = struct{}{}
		}
	}
	healthCutoff := time.Now().UTC().Add(-HealthFreshnessTTL)
	rows, err := tx.Query(ctx, `WITH candidate_planes AS (
			SELECT rp.id, rp.region, rp.capabilities_json, COUNT(ws.workspace_id) AS load,
				rpc.resource_plane_id IS NOT NULL AS capacity_configured,
				CASE
					WHEN rph.status = 'unhealthy' THEN 'unhealthy'
					WHEN rph.updated_at < $3 THEN 'degraded'
					ELSE COALESCE(rph.status, 'healthy')
				END AS effective_health
			FROM resource_planes rp
			JOIN resource_plane_status rps ON rps.resource_plane_id = rp.id AND rps.status = 'active'
			LEFT JOIN resource_plane_capacities rpc ON rpc.resource_plane_id = rp.id
			LEFT JOIN resource_plane_health rph ON rph.resource_plane_id = rp.id
			LEFT JOIN placements p ON p.resource_plane_id = rp.id
			LEFT JOIN workspace_status ws ON ws.workspace_id = p.workspace_id AND ws.observed_state <> 'deleted'
			WHERE ($1 = '' OR rp.id = $1) AND ($2 = '' OR rp.region = $2)
			GROUP BY rp.id, rp.region, rp.capabilities_json, rpc.resource_plane_id, rph.status, rph.updated_at
		)
		SELECT id, region, capabilities_json, load, capacity_configured, effective_health
		FROM candidate_planes
		WHERE effective_health <> 'unhealthy'
		ORDER BY CASE effective_health WHEN 'healthy' THEN 0 ELSE 1 END, load, region, id`, policy.ResourcePlaneID, policy.Region, healthCutoff)
	if err != nil {
		return "", fmt.Errorf("query active Resource Plane candidates: %w", err)
	}
	defer rows.Close()
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		var rawCapabilities string
		if err := rows.Scan(&item.id, &item.region, &rawCapabilities, &item.load, &item.capacityConfigured, &item.healthStatus); err != nil {
			return "", fmt.Errorf("scan Resource Plane candidate: %w", err)
		}
		if err := json.Unmarshal([]byte(rawCapabilities), &item.capabilities); err != nil {
			return "", fmt.Errorf("decode capabilities for Resource Plane %s: %w", item.id, err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate Resource Plane candidates: %w", err)
	}
	rows.Close()
	for _, item := range candidates {
		if !matchesTenantPlane(item.id, tenantPolicy.ResourcePlaneIDs) || !matchesTenantRegion(item.region, tenantPolicy.AllowedRegions) || !containsCapabilities(item.capabilities, required) {
			continue
		}
		if item.capacityConfigured {
			available, err := resourcecapacity.Reserve(ctx, tx, item.id, time.Now().UTC())
			if err != nil {
				return "", err
			}
			if !available {
				continue
			}
		}
		return item.id, nil
	}
	return "", ErrNoEligibleResourcePlane
}

func matchesTenantPlane(id domain.ResourcePlaneID, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if string(id) == candidate {
			return true
		}
	}
	return false
}

func matchesTenantRegion(region string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(region, candidate) {
			return true
		}
	}
	return false
}

func containsCapabilities(available []string, required map[string]struct{}) bool {
	set := make(map[string]struct{}, len(available))
	for _, capability := range available {
		set[strings.ToLower(strings.TrimSpace(capability))] = struct{}{}
	}
	for capability := range required {
		if _, ok := set[capability]; !ok {
			return false
		}
	}
	return true
}
