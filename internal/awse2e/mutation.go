package awse2e

import (
	"context"
	"errors"
	"fmt"
)

// AccountLookup resolves the account currently selected by an explicit AWS
// profile. Call it immediately before each AWS resource mutation.
type AccountLookup func(context.Context, string, string) (string, error)

type MutationGate struct {
	Settings Config
	Lookup   AccountLookup
}

// RunMutation validates opt-in, region, ownership tags, and the current STS
// account identity before invoking mutate. It never retries mutations.
func (g MutationGate) RunMutation(ctx context.Context, plane, region string, tags map[string]string, mutate func(context.Context) error) error {
	if err := g.Settings.Validate(); err != nil {
		return err
	}
	if plane != "control" && plane != "resource" {
		return errors.New("invalid AWS E2E plane")
	}
	if region != g.Settings.Region {
		return errors.New("AWS resource region does not match the approved E2E region")
	}
	if mutate == nil {
		return errors.New("AWS E2E mutation callback is required")
	}
	if err := ValidateRunTags(tags, g.Settings.RunID, plane); err != nil {
		return err
	}
	if g.Lookup == nil {
		return errors.New("AWS E2E account identity lookup is required")
	}
	profile, expectedAccountID := g.Settings.ControlPlaneProfile, g.Settings.ControlPlaneAccountID
	if plane == "resource" {
		profile, expectedAccountID = g.Settings.ResourcePlaneProfile, g.Settings.ResourcePlaneAccountID
	}
	actualAccountID, err := g.Lookup(ctx, profile, region)
	if err != nil {
		return fmt.Errorf("recheck AWS E2E %s account identity: %w", plane, err)
	}
	if actualAccountID != expectedAccountID {
		return fmt.Errorf("AWS E2E %s caller account does not match the approved account ID", plane)
	}
	return mutate(ctx)
}
