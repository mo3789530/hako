package main

import (
	"reflect"
	"testing"
)

func TestParseWorkspaceExecArgs(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantTenant    string
		wantWorkspace string
		wantTimeout   int
		wantArgv      []string
		wantErr       bool
	}{
		{name: "default timeout", args: []string{"tenant_1", "ws_1", "--", "go", "test", "./..."}, wantTenant: "tenant_1", wantWorkspace: "ws_1", wantArgv: []string{"go", "test", "./..."}},
		{name: "separate timeout", args: []string{"tenant_1", "ws_1", "--timeout", "45s", "--", "go", "test"}, wantTenant: "tenant_1", wantWorkspace: "ws_1", wantTimeout: 45000, wantArgv: []string{"go", "test"}},
		{name: "inline timeout", args: []string{"tenant_1", "ws_1", "--timeout=2m30s", "--", "go", "test"}, wantTenant: "tenant_1", wantWorkspace: "ws_1", wantTimeout: 150000, wantArgv: []string{"go", "test"}},
		{name: "missing separator", args: []string{"tenant_1", "ws_1", "go", "test"}, wantErr: true},
		{name: "empty command", args: []string{"tenant_1", "ws_1", "--"}, wantErr: true},
		{name: "unknown option", args: []string{"tenant_1", "ws_1", "--env", "X=1", "--", "go"}, wantErr: true},
		{name: "duplicate timeout", args: []string{"tenant_1", "ws_1", "--timeout", "1s", "--timeout", "2s", "--", "go"}, wantErr: true},
		{name: "too short", args: []string{"tenant_1", "ws_1", "--timeout", "99ms", "--", "go"}, wantErr: true},
		{name: "too long", args: []string{"tenant_1", "ws_1", "--timeout", "5m1s", "--", "go"}, wantErr: true},
		{name: "submillisecond", args: []string{"tenant_1", "ws_1", "--timeout", "100.1ms", "--", "go"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant, workspace, timeout, argv, err := parseWorkspaceExecArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseWorkspaceExecArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tenant != tt.wantTenant || workspace != tt.wantWorkspace || timeout != tt.wantTimeout || !reflect.DeepEqual(argv, tt.wantArgv) {
				t.Fatalf("parseWorkspaceExecArgs() = (%q, %q, %d, %#v), want (%q, %q, %d, %#v)", tenant, workspace, timeout, argv, tt.wantTenant, tt.wantWorkspace, tt.wantTimeout, tt.wantArgv)
			}
		})
	}
}
