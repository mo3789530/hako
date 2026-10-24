package main

import (
	"context"
	"testing"
)

func TestOpenDatabaseRejectsAmbiguousOrMissingConfiguration(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
	t.Setenv("HAKO_DATABASE_URL", "")
	t.Setenv("HAKO_DSQL_HOST", "")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("missing DSQL host should fail validation")
	}
}

func TestShouldStartLambdaRuntime(t *testing.T) {
	tests := []struct {
		name       string
		runtimeAPI string
		httpMode   string
		want       bool
	}{
		{name: "local HTTP", want: false},
		{name: "zip Lambda runtime", runtimeAPI: "127.0.0.1:9001", want: true},
		{name: "Lambda Web Adapter image", runtimeAPI: "127.0.0.1:9001", httpMode: "true", want: false},
		{name: "HTTP mode outside Lambda", httpMode: "true", want: false},
		{name: "whitespace values", runtimeAPI: "  ", httpMode: " true ", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldStartLambdaRuntime(test.runtimeAPI, test.httpMode); got != test.want {
				t.Fatalf("shouldStartLambdaRuntime(%q, %q) = %t, want %t", test.runtimeAPI, test.httpMode, got, test.want)
			}
		})
	}
}

func TestResourcePlaneWorkspaceSessionKeyResolverIsOptionalAndValidatesARNMap(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS", "")
	resolver, err := resourcePlaneWorkspaceSessionKeyResolverFromEnv(context.Background())
	if err != nil || resolver != nil {
		t.Fatalf("unset session key map resolver=%v err=%v", resolver, err)
	}
	t.Setenv("HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS", `{"rp_a":"not-an-arn"}`)
	if _, err := resourcePlaneWorkspaceSessionKeyResolverFromEnv(context.Background()); err == nil {
		t.Fatal("invalid Resource Plane session Secret map was accepted")
	}
}

func TestResourcePlaneArtifactCleanupKeyResolverIsOptionalAndValidatesARNMap(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS", "")
	resolver, err := resourcePlaneArtifactCleanupKeyResolverFromEnv(context.Background())
	if err != nil || resolver != nil {
		t.Fatalf("unset cleanup key map resolver=%v err=%v", resolver, err)
	}
	t.Setenv("HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS", `{"rp_a":"not-an-arn"}`)
	if _, err := resourcePlaneArtifactCleanupKeyResolverFromEnv(context.Background()); err == nil {
		t.Fatal("invalid Resource Plane Artifact cleanup Secret map was accepted")
	}
}
