package awse2e

import (
	"context"
	"errors"
	"testing"
)

func TestMutationGateRunsOnlyAfterAllFencesPass(t *testing.T) {
	baseTags := map[string]string{
		"hako:managed-by": "hako",
		"hako:e2e-run-id": "local-e2e-001",
		"hako:plane":      "resource",
	}

	tests := []struct {
		name      string
		configure func(*MutationGate, *string, *int)
		plane     string
		region    string
		tags      map[string]string
		wantErr   bool
	}{
		{name: "valid mutation", plane: "resource", region: "ap-northeast-1"},
		{name: "opt in required", plane: "resource", region: "ap-northeast-1", configure: func(g *MutationGate, _ *string, _ *int) { g.Settings.Enabled = false }, wantErr: true},
		{name: "plane required", plane: "other", region: "ap-northeast-1", wantErr: true},
		{name: "region must match", plane: "resource", region: "us-east-1", wantErr: true},
		{name: "owned tags required", plane: "resource", region: "ap-northeast-1", tags: map[string]string{"hako:managed-by": "hako"}, wantErr: true},
		{name: "account must match", plane: "resource", region: "ap-northeast-1", configure: func(_ *MutationGate, account *string, _ *int) { *account = "999999999999" }, wantErr: true},
		{name: "lookup failure", plane: "resource", region: "ap-northeast-1", configure: func(g *MutationGate, _ *string, _ *int) {
			g.Lookup = func(context.Context, string, string) (string, error) { return "", errors.New("STS unavailable") }
		}, wantErr: true},
		{name: "lookup required", plane: "resource", region: "ap-northeast-1", configure: func(g *MutationGate, _ *string, _ *int) { g.Lookup = nil }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := MutationGate{Settings: validConfig()}
			account := gate.Settings.ResourcePlaneAccountID
			lookups := 0
			gate.Lookup = func(_ context.Context, profile, region string) (string, error) {
				lookups++
				if profile != gate.Settings.ResourcePlaneProfile || region != gate.Settings.Region {
					t.Fatalf("lookup(%q, %q) got unexpected profile or region", profile, region)
				}
				return account, nil
			}
			mutations := 0
			if tt.configure != nil {
				tt.configure(&gate, &account, &lookups)
			}
			tags := tt.tags
			if tags == nil {
				tags = baseTags
			}
			err := gate.RunMutation(context.Background(), tt.plane, tt.region, tags, func(context.Context) error {
				mutations++
				return nil
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunMutation() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && mutations != 0 {
				t.Fatalf("mutation callback ran %d times after rejected request", mutations)
			}
			if !tt.wantErr && mutations != 1 {
				t.Fatalf("mutation callback ran %d times, want exactly once", mutations)
			}
			if tt.name == "valid mutation" && lookups != 1 {
				t.Fatalf("account lookup ran %d times, want exactly once", lookups)
			}
		})
	}
}

func TestMutationGateRequiresCallback(t *testing.T) {
	gate := MutationGate{
		Settings: validConfig(),
		Lookup: func(context.Context, string, string) (string, error) {
			return validConfig().ResourcePlaneAccountID, nil
		},
	}
	tags := map[string]string{"hako:managed-by": "hako", "hako:e2e-run-id": "local-e2e-001", "hako:plane": "resource"}
	if err := gate.RunMutation(context.Background(), "resource", "ap-northeast-1", tags, nil); err == nil {
		t.Fatal("RunMutation accepted a nil mutation callback")
	}
}
