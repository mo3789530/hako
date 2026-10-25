package awse2e

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	regionPattern    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	runIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,47}$`)
)

// Config contains the explicit account allowlist and opt-in for a future AWS
// integration E2E. The validation itself makes no AWS calls.
type Config struct {
	Enabled                bool
	Region                 string
	RunID                  string
	ControlPlaneProfile    string
	ResourcePlaneProfile   string
	ControlPlaneAccountID  string
	ResourcePlaneAccountID string
	ManagementAccountID    string
	ProductionAccountIDs   []string
}

func (c Config) Validate() error {
	if !c.Enabled {
		return errors.New("AWS E2E is disabled; set HAKO_AWS_E2E_ENABLED=true to opt in")
	}
	if !regionPattern.MatchString(strings.TrimSpace(c.Region)) {
		return errors.New("AWS E2E region is missing or malformed")
	}
	if !runIDPattern.MatchString(strings.TrimSpace(c.RunID)) {
		return errors.New("AWS E2E run ID must be 3-48 lowercase letters, digits, or hyphens")
	}
	for label, value := range map[string]string{
		"Control Plane profile":  c.ControlPlaneProfile,
		"Resource Plane profile": c.ResourcePlaneProfile,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("AWS E2E %s is required", label)
		}
	}
	for label, value := range map[string]string{
		"Control Plane account":  c.ControlPlaneAccountID,
		"Resource Plane account": c.ResourcePlaneAccountID,
		"Management account":     c.ManagementAccountID,
	} {
		if !accountIDPattern.MatchString(strings.TrimSpace(value)) {
			return fmt.Errorf("AWS E2E %s ID must contain exactly 12 digits", label)
		}
	}
	if c.ControlPlaneAccountID == c.ResourcePlaneAccountID {
		return errors.New("Control Plane and Resource Plane AWS accounts must be different")
	}
	if c.ControlPlaneAccountID == c.ManagementAccountID || c.ResourcePlaneAccountID == c.ManagementAccountID {
		return errors.New("AWS E2E refuses to use the Organizations Management Account")
	}
	if len(c.ProductionAccountIDs) == 0 {
		return errors.New("AWS E2E production account denylist is required")
	}
	seen := make(map[string]struct{}, len(c.ProductionAccountIDs))
	for _, accountID := range c.ProductionAccountIDs {
		accountID = strings.TrimSpace(accountID)
		if !accountIDPattern.MatchString(accountID) {
			return errors.New("AWS E2E production account denylist contains a malformed account ID")
		}
		if _, ok := seen[accountID]; ok {
			continue
		}
		seen[accountID] = struct{}{}
		if accountID == c.ControlPlaneAccountID || accountID == c.ResourcePlaneAccountID {
			return errors.New("AWS E2E refuses to use a production account")
		}
	}
	return nil
}

// ValidateCallerAccounts compares STS identity results with the explicitly
// configured allowlist. Call it again immediately before any future mutation.
func (c Config) ValidateCallerAccounts(controlPlaneAccountID, resourcePlaneAccountID string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if controlPlaneAccountID != c.ControlPlaneAccountID {
		return errors.New("Control Plane caller account does not match the approved account ID")
	}
	if resourcePlaneAccountID != c.ResourcePlaneAccountID {
		return errors.New("Resource Plane caller account does not match the approved account ID")
	}
	return nil
}

// ValidateRunTags only permits inspection/cleanup of resources carrying the
// exact Hako ownership and current run identity.
func ValidateRunTags(tags map[string]string, runID, plane string) error {
	if !runIDPattern.MatchString(runID) {
		return errors.New("invalid AWS E2E run ID")
	}
	if plane != "control" && plane != "resource" {
		return errors.New("invalid AWS E2E plane")
	}
	if tags["hako:managed-by"] != "hako" || tags["hako:e2e-run-id"] != runID || tags["hako:plane"] != plane {
		return errors.New("AWS resource is not owned by this Hako E2E run")
	}
	return nil
}
