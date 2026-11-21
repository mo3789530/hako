package workspacestorage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	fileSystemARNPattern  = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):elasticfilesystem:([a-z0-9-]+):([0-9]{12}):file-system/(fs-[0-9a-f]{8,40})$`)
	accessPointARNPattern = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):elasticfilesystem:([a-z0-9-]+):([0-9]{12}):access-point/(fsap-[0-9a-f]{8,40})$`)
)

// ExecutionRoleName returns a stable IAM role name that contains no raw Tenant
// or Workspace identifiers. The 128-bit identity suffix also separates
// identical Workspace IDs in different Resource Planes or Tenants.
func ExecutionRoleName(identity Identity) (string, error) {
	if !validIdentity(identity) {
		return "", errors.New("Workspace execution role identity is invalid")
	}
	sum := sha256.Sum256([]byte(string(identity.ResourcePlaneID) + "\x00" + string(identity.TenantID) + "\x00" + string(identity.WorkspaceID)))
	return "hako-ws-" + hex.EncodeToString(sum[:16]), nil
}

// ExecutionRoleTrustPolicy emits the Lambda MicroVM trust relationship. The
// execution role is assumed by Lambda, not by Hako clients or Workspace code.
func ExecutionRoleTrustPolicy() (string, error) {
	policy := struct {
		Version   string `json:"Version"`
		Statement []struct {
			Effect    string `json:"Effect"`
			Principal struct {
				Service string `json:"Service"`
			} `json:"Principal"`
			Action []string `json:"Action"`
		} `json:"Statement"`
	}{Version: "2012-10-17"}
	statement := struct {
		Effect    string `json:"Effect"`
		Principal struct {
			Service string `json:"Service"`
		} `json:"Principal"`
		Action []string `json:"Action"`
	}{Effect: "Allow", Action: []string{"sts:AssumeRole", "sts:TagSession"}}
	statement.Principal.Service = "lambda.amazonaws.com"
	policy.Statement = append(policy.Statement, statement)
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", errors.New("encode Workspace execution role trust policy")
	}
	return string(encoded), nil
}

// ExecutionRolePermissionsPolicy grants only EFS mount/write through one exact
// Access Point, denies unencrypted clients and all other APs, and deliberately
// omits ClientRootAccess and unrelated AWS permissions.
func ExecutionRolePermissionsPolicy(fileSystemARN, accessPointARN string) (string, error) {
	fs := fileSystemARNPattern.FindStringSubmatch(strings.TrimSpace(fileSystemARN))
	ap := accessPointARNPattern.FindStringSubmatch(strings.TrimSpace(accessPointARN))
	if fs == nil || ap == nil || fs[1] != ap[1] || fs[2] != ap[2] || fs[3] != ap[3] {
		return "", errors.New("Workspace execution role requires a matching exact EFS filesystem and Access Point ARN")
	}
	type statement struct {
		Sid       string         `json:"Sid"`
		Effect    string         `json:"Effect"`
		Action    []string       `json:"Action"`
		Resource  string         `json:"Resource"`
		Condition map[string]any `json:"Condition,omitempty"`
	}
	policy := struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{Version: "2012-10-17", Statement: []statement{
		{Sid: "AllowExactWorkspaceAccessPointOverTLS", Effect: "Allow", Action: []string{"elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"}, Resource: fileSystemARN,
			Condition: map[string]any{"Bool": map[string]string{"aws:SecureTransport": "true"}, "StringEquals": map[string]string{"elasticfilesystem:AccessPointArn": accessPointARN}}},
		{Sid: "DenyWorkspaceRootAccess", Effect: "Deny", Action: []string{"elasticfilesystem:ClientRootAccess"}, Resource: fileSystemARN},
		{Sid: "DenyWorkspaceAccessWithoutTLS", Effect: "Deny", Action: []string{"elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite", "elasticfilesystem:ClientRootAccess"}, Resource: fileSystemARN,
			Condition: map[string]any{"Bool": map[string]string{"aws:SecureTransport": "false"}}},
		{Sid: "DenyOtherWorkspaceAccessPoints", Effect: "Deny", Action: []string{"elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite", "elasticfilesystem:ClientRootAccess"}, Resource: fileSystemARN,
			Condition: map[string]any{"StringNotEquals": map[string]string{"elasticfilesystem:AccessPointArn": accessPointARN}}},
	}}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("encode Workspace execution role permissions policy: %w", err)
	}
	return string(encoded), nil
}
