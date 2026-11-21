package awse2e

import (
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Enabled:                true,
		Region:                 "ap-northeast-1",
		RunID:                  "local-e2e-001",
		ControlPlaneProfile:    "hako-e2e-control",
		ResourcePlaneProfile:   "hako-e2e-resource",
		ControlPlaneAccountID:  "111111111111",
		ResourcePlaneAccountID: "222222222222",
		ManagementAccountID:    "999999999999",
		ProductionAccountIDs:   []string{"333333333333", "444444444444"},
	}
}

func TestConfigPreflightFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{name: "opt in required", change: func(c *Config) { c.Enabled = false }, want: "disabled"},
		{name: "region required", change: func(c *Config) { c.Region = "" }, want: "region"},
		{name: "run ID constrained", change: func(c *Config) { c.RunID = "../prod" }, want: "run ID"},
		{name: "profile required", change: func(c *Config) { c.ResourcePlaneProfile = " " }, want: "profile"},
		{name: "different accounts", change: func(c *Config) { c.ResourcePlaneAccountID = c.ControlPlaneAccountID }, want: "different"},
		{name: "management denied", change: func(c *Config) { c.ControlPlaneAccountID = c.ManagementAccountID }, want: "Management"},
		{name: "production denied", change: func(c *Config) { c.ResourcePlaneAccountID = "333333333333" }, want: "production"},
		{name: "denylist required", change: func(c *Config) { c.ProductionAccountIDs = nil }, want: "denylist"},
		{name: "denylist malformed", change: func(c *Config) { c.ProductionAccountIDs = []string{"prod"} }, want: "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validConfig()
			tt.change(&config)
			if err := config.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestCallerAccountAndRunTagFences(t *testing.T) {
	config := validConfig()
	if err := config.ValidateCallerAccounts(config.ControlPlaneAccountID, config.ResourcePlaneAccountID); err != nil {
		t.Fatalf("valid caller identities rejected: %v", err)
	}
	if err := config.ValidateCallerAccounts("999999999999", config.ResourcePlaneAccountID); err == nil {
		t.Fatal("Management Account caller was accepted")
	}
	if err := config.ValidateCallerAccounts(config.ControlPlaneAccountID, "333333333333"); err == nil {
		t.Fatal("production Resource Plane caller was accepted")
	}

	tags := map[string]string{"hako:managed-by": "hako", "hako:e2e-run-id": config.RunID, "hako:plane": "resource"}
	if err := ValidateRunTags(tags, config.RunID, "resource"); err != nil {
		t.Fatalf("owned resource tags rejected: %v", err)
	}
	for _, tc := range []struct {
		name  string
		runID string
		plane string
		tags  map[string]string
	}{
		{name: "foreign run", runID: "other-e2e-001", plane: "resource", tags: tags},
		{name: "foreign plane", runID: config.RunID, plane: "control", tags: tags},
		{name: "missing owner", runID: config.RunID, plane: "resource", tags: map[string]string{"hako:e2e-run-id": config.RunID, "hako:plane": "resource"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateRunTags(tc.tags, tc.runID, tc.plane); err == nil {
				t.Fatal("foreign resource tags were accepted")
			}
		})
	}
}
