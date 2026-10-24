package workspacestorage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestExecutionRoleNameIsStableOpaqueAndWorkspaceScoped(t *testing.T) {
	identity := testIdentity()
	first, err := ExecutionRoleName(identity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExecutionRoleName(identity)
	if err != nil || first != second {
		t.Fatalf("role name is not deterministic: %q %q err=%v", first, second, err)
	}
	if len(first) > 64 || first[:8] != "hako-ws-" || first == "hako-ws-"+string(identity.WorkspaceID) {
		t.Fatalf("role name is not a bounded opaque Hako name: %q", first)
	}
	otherTenant := identity
	otherTenant.TenantID = "tenant-other"
	other, err := ExecutionRoleName(otherTenant)
	if err != nil || other == first {
		t.Fatalf("different Tenant reused Workspace execution role: %q err=%v", other, err)
	}
	otherPlane := identity
	otherPlane.ResourcePlaneID = "rp-other"
	other, err = ExecutionRoleName(otherPlane)
	if err != nil || other == first {
		t.Fatalf("different Resource Plane reused Workspace execution role: %q err=%v", other, err)
	}
}

func TestExecutionRolePoliciesAreLambdaTrustedAndExactAPOnly(t *testing.T) {
	trust, err := ExecutionRoleTrustPolicy()
	if err != nil {
		t.Fatal(err)
	}
	var trustDoc struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string `json:"Effect"`
			Principal struct {
				Service string `json:"Service"`
			} `json:"Principal"`
			Action []string `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(trust), &trustDoc); err != nil {
		t.Fatal(err)
	}
	if trustDoc.Version != "2012-10-17" || len(trustDoc.Statement) != 1 ||
		trustDoc.Statement[0].Principal.Service != "lambda.amazonaws.com" ||
		!reflect.DeepEqual(trustDoc.Statement[0].Action, []string{"sts:AssumeRole", "sts:TagSession"}) {
		t.Fatalf("unexpected MicroVM execution-role trust: %s", trust)
	}

	const fileSystemARN = "arn:aws:elasticfilesystem:us-east-1:123456789012:file-system/fs-0123456789abcdef0"
	const accessPointARN = "arn:aws:elasticfilesystem:us-east-1:123456789012:access-point/fsap-0123456789abcdef0"
	permissions, err := ExecutionRolePermissionsPolicy(fileSystemARN, accessPointARN)
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Version   string `json:"Version"`
		Statement []struct {
			Sid       string         `json:"Sid"`
			Effect    string         `json:"Effect"`
			Action    []string       `json:"Action"`
			Resource  string         `json:"Resource"`
			Condition map[string]any `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(permissions), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Version != "2012-10-17" || len(policy.Statement) != 4 {
		t.Fatalf("unexpected permission policy envelope: %s", permissions)
	}
	allow := policy.Statement[0]
	if allow.Sid != "AllowExactWorkspaceAccessPointOverTLS" || allow.Effect != "Allow" || allow.Resource != fileSystemARN ||
		!reflect.DeepEqual(allow.Action, []string{"elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"}) ||
		allow.Condition["StringEquals"] == nil || allow.Condition["Bool"] == nil {
		t.Fatalf("Workspace role allow is not exact AP/TLS-only: %+v", allow)
	}
	if strings.Contains(permissions, "ClientRootAccess\":\"Allow") || strings.Contains(permissions, "Resource\":\"*\"") || strings.Contains(permissions, "access-point/*") {
		t.Fatalf("Workspace role has root, wildcard resource, or wildcard AP access: %s", permissions)
	}
	if policy.Statement[1].Sid != "DenyWorkspaceRootAccess" || policy.Statement[1].Effect != "Deny" ||
		!reflect.DeepEqual(policy.Statement[1].Action, []string{"elasticfilesystem:ClientRootAccess"}) ||
		policy.Statement[2].Effect != "Deny" || policy.Statement[3].Effect != "Deny" ||
		policy.Statement[2].Condition["Bool"] == nil || policy.Statement[3].Condition["StringNotEquals"] == nil {
		t.Fatalf("expected explicit transport and alternate-AP denies: %+v", policy.Statement)
	}
}

func TestExecutionRolePolicyRejectsMismatchedOrMalformedARNs(t *testing.T) {
	tests := [][2]string{
		{"", "arn:aws:elasticfilesystem:us-east-1:123456789012:access-point/fsap-0123456789abcdef0"},
		{"arn:aws:elasticfilesystem:us-east-1:123456789012:file-system/fs-0123456789abcdef0", "arn:aws:elasticfilesystem:us-west-2:123456789012:access-point/fsap-0123456789abcdef0"},
		{"arn:aws:elasticfilesystem:us-east-1:123456789012:file-system/fs-0123456789abcdef0", "arn:aws-cn:elasticfilesystem:us-east-1:123456789012:access-point/fsap-0123456789abcdef0"},
	}
	for _, pair := range tests {
		if _, err := ExecutionRolePermissionsPolicy(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted mismatched or malformed ARNs: %q %q", pair[0], pair[1])
		}
	}
	if _, err := ExecutionRoleName(Identity{}); err == nil {
		t.Fatal("accepted an empty Workspace identity")
	}
}
