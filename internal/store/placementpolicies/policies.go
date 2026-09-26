// Package placementpolicies stores Tenant-level Resource Plane constraints.
package placementpolicies

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

const MaxSelectors = 50

type Policy struct {
	TenantID             domain.TenantID `json:"tenant_id"`
	AllowedRegions       []string        `json:"allowed_regions"`
	ResourcePlaneIDs     []string        `json:"resource_plane_ids"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	MaxCostTier          string          `json:"max_cost_tier,omitempty"`
	MinimumIsolationTier string          `json:"minimum_isolation_tier,omitempty"`
	UpdatedAt            *time.Time      `json:"updated_at,omitempty"`
}

var costTierRanks = map[string]int{"low": 1, "standard": 2, "high": 3}

var isolationTierRanks = map[string]int{"shared": 1, "dedicated": 2, "isolated": 3}

func Normalize(policy Policy) (Policy, error) {
	if policy.TenantID == "" {
		return Policy{}, errors.New("Tenant ID is required")
	}
	var err error
	if policy.AllowedRegions, err = normalizeValues(policy.AllowedRegions, true); err != nil {
		return Policy{}, fmt.Errorf("allowed regions: %w", err)
	}
	if policy.ResourcePlaneIDs, err = normalizeValues(policy.ResourcePlaneIDs, false); err != nil {
		return Policy{}, fmt.Errorf("Resource Plane IDs: %w", err)
	}
	if policy.RequiredCapabilities, err = normalizeValues(policy.RequiredCapabilities, true); err != nil {
		return Policy{}, fmt.Errorf("required capabilities: %w", err)
	}
	if policy.MaxCostTier, err = normalizeTier(policy.MaxCostTier, costTierRanks); err != nil {
		return Policy{}, fmt.Errorf("maximum cost tier: %w", err)
	}
	if policy.MinimumIsolationTier, err = normalizeTier(policy.MinimumIsolationTier, isolationTierRanks); err != nil {
		return Policy{}, fmt.Errorf("minimum isolation tier: %w", err)
	}
	return policy, nil
}

// CostTierRank returns the relative operator-configured cost tier. Unknown
// values return zero so they cannot accidentally satisfy a restrictive policy.
func CostTierRank(value string) int { return costTierRanks[strings.ToLower(strings.TrimSpace(value))] }

// IsolationTierRank returns the relative isolation tier. Unknown values return
// zero so they cannot accidentally satisfy a minimum-isolation policy.
func IsolationTierRank(value string) int {
	return isolationTierRanks[strings.ToLower(strings.TrimSpace(value))]
}

func Get(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID) (Policy, error) {
	if tx == nil || tenantID == "" {
		return Policy{}, errors.New("transaction and Tenant ID are required")
	}
	policy := Policy{TenantID: tenantID, AllowedRegions: []string{}, ResourcePlaneIDs: []string{}, RequiredCapabilities: []string{}}
	var regionsJSON, planesJSON, capabilitiesJSON string
	var maxCostTier, minimumIsolationTier sql.NullString
	var updatedAt time.Time
	err := tx.QueryRow(ctx, `SELECT allowed_regions_json, resource_plane_ids_json, required_capabilities_json, max_cost_tier, minimum_isolation_tier, updated_at FROM tenant_placement_policies WHERE tenant_id = $1`, tenantID).
		Scan(&regionsJSON, &planesJSON, &capabilitiesJSON, &maxCostTier, &minimumIsolationTier, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("get Tenant placement policy: %w", err)
	}
	if err := json.Unmarshal([]byte(regionsJSON), &policy.AllowedRegions); err != nil {
		return Policy{}, fmt.Errorf("decode allowed regions: %w", err)
	}
	if err := json.Unmarshal([]byte(planesJSON), &policy.ResourcePlaneIDs); err != nil {
		return Policy{}, fmt.Errorf("decode Resource Plane IDs: %w", err)
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &policy.RequiredCapabilities); err != nil {
		return Policy{}, fmt.Errorf("decode required capabilities: %w", err)
	}
	if maxCostTier.Valid {
		policy.MaxCostTier = maxCostTier.String
	}
	if minimumIsolationTier.Valid {
		policy.MinimumIsolationTier = minimumIsolationTier.String
	}
	policy.UpdatedAt = &updatedAt
	return policy, nil
}

func Put(ctx context.Context, tx pgx.Tx, policy Policy, now time.Time) (Policy, error) {
	if tx == nil {
		return Policy{}, errors.New("transaction is required")
	}
	policy, err := Normalize(policy)
	if err != nil {
		return Policy{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	regionsJSON, err := json.Marshal(policy.AllowedRegions)
	if err != nil {
		return Policy{}, fmt.Errorf("encode allowed regions: %w", err)
	}
	planesJSON, err := json.Marshal(policy.ResourcePlaneIDs)
	if err != nil {
		return Policy{}, fmt.Errorf("encode Resource Plane IDs: %w", err)
	}
	capabilitiesJSON, err := json.Marshal(policy.RequiredCapabilities)
	if err != nil {
		return Policy{}, fmt.Errorf("encode required capabilities: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_placement_policies (tenant_id, allowed_regions_json, resource_plane_ids_json, required_capabilities_json, max_cost_tier, minimum_isolation_tier, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7) ON CONFLICT (tenant_id) DO UPDATE SET allowed_regions_json = $2, resource_plane_ids_json = $3, required_capabilities_json = $4, max_cost_tier = NULLIF($5, ''), minimum_isolation_tier = NULLIF($6, ''), updated_at = $7`,
		policy.TenantID, string(regionsJSON), string(planesJSON), string(capabilitiesJSON), policy.MaxCostTier, policy.MinimumIsolationTier, now); err != nil {
		return Policy{}, fmt.Errorf("save Tenant placement policy: %w", err)
	}
	policy.UpdatedAt = &now
	return policy, nil
}

func normalizeTier(value string, ranks map[string]int) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if _, ok := ranks[value]; !ok {
		return "", fmt.Errorf("unsupported tier %q", value)
	}
	return value, nil
}

func normalizeValues(values []string, lower bool) ([]string, error) {
	if len(values) > MaxSelectors {
		return nil, fmt.Errorf("at most %d entries are allowed", MaxSelectors)
	}
	set := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value == "" {
			return nil, errors.New("entries must not be empty")
		}
		if _, exists := set[value]; exists {
			return nil, fmt.Errorf("duplicate entry %q", value)
		}
		set[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized, nil
}
