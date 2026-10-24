// Package workspacestorage provisions Workspace-scoped EFS Access Points.
// It does not mount filesystems in a MicroVM; that requires the guest runtime
// and a reviewed mount/bootstrap contract.
package workspacestorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	idPattern               = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	fileSystemPattern       = regexp.MustCompile(`^fs-[0-9a-f]{8,40}$`)
	accessPointPattern      = regexp.MustCompile(`^fsap-[0-9a-f]{8,40}$`)
	regionPattern           = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)+$`)
	workspaceRoleARNPattern = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):iam::[0-9]{12}:role/hako/workspaces/(hako-ws-[a-f0-9]{32})$`)
)

type EFSAPI interface {
	CreateAccessPoint(context.Context, *efs.CreateAccessPointInput, ...func(*efs.Options)) (*efs.CreateAccessPointOutput, error)
	DescribeAccessPoints(context.Context, *efs.DescribeAccessPointsInput, ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error)
	DeleteAccessPoint(context.Context, *efs.DeleteAccessPointInput, ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error)
}

type Identity struct {
	TenantID        domain.TenantID
	WorkspaceID     domain.WorkspaceID
	ResourcePlaneID domain.ResourcePlaneID
}

// Binding is safe to retain in the Resource Plane journal: it identifies the
// AP and mountpoint but contains no credentials or AWS secret material.
type Binding struct {
	FileSystemID     string `json:"file_system_id"`
	Region           string `json:"region"`
	AccessPointID    string `json:"access_point_id"`
	ExecutionRoleARN string `json:"execution_role_arn,omitempty"`
	RootPath         string `json:"root_path"`
	UID              uint32 `json:"uid"`
	GID              uint32 `json:"gid"`
}

type ManagedAccessPoint struct {
	Identity Identity `json:"identity"`
	Binding  Binding  `json:"binding"`
}

type EFS struct {
	api          EFSAPI
	fileSystemID string
	region       string
}

// Validate checks a journaled binding before it is consumed by another
// Resource Plane component. It intentionally validates shape, not live AWS
// state; ownership must still be checked through DescribeAccessPoints.
func (b Binding) Validate() error {
	if !fileSystemPattern.MatchString(b.FileSystemID) || !regionPattern.MatchString(b.Region) || !accessPointPattern.MatchString(b.AccessPointID) ||
		!regexp.MustCompile(`^/hako/[a-f0-9]{16}/[a-f0-9]{16}/[a-f0-9]{16}$`).MatchString(b.RootPath) ||
		b.UID < 100000 || b.GID != b.UID {
		return errors.New("Workspace EFS binding is invalid")
	}
	if b.ExecutionRoleARN != "" && !workspaceRoleARNPattern.MatchString(b.ExecutionRoleARN) {
		return errors.New("Workspace execution role binding is invalid")
	}
	return nil
}

// ValidateFor binds a journaled EFS mapping to one exact Resource Plane,
// Tenant, and Workspace identity.
func (b Binding) ValidateFor(identity Identity) error {
	if !validIdentity(identity) || b.Validate() != nil {
		return errors.New("Workspace EFS binding identity is invalid")
	}
	expectedRoot := "/hako/" + shortHash(string(identity.ResourcePlaneID)) + "/" + shortHash(string(identity.TenantID)) + "/" + shortHash(string(identity.WorkspaceID))
	expectedUID := stablePOSIXID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	if b.RootPath != expectedRoot || b.UID != expectedUID || b.GID != expectedUID {
		return errors.New("Workspace EFS binding does not belong to the requested identity")
	}
	if b.ExecutionRoleARN != "" {
		name, err := ExecutionRoleName(identity)
		matches := workspaceRoleARNPattern.FindStringSubmatch(b.ExecutionRoleARN)
		if err != nil || matches == nil || matches[2] != name {
			return errors.New("Workspace execution role binding does not belong to the requested identity")
		}
	}
	return nil
}

func NewEFS(api EFSAPI, fileSystemID, region string) (*EFS, error) {
	fileSystemID = strings.TrimSpace(fileSystemID)
	region = strings.TrimSpace(region)
	if api == nil || !fileSystemPattern.MatchString(fileSystemID) || !regionPattern.MatchString(region) {
		return nil, errors.New("EFS API, valid Workspace file system ID, and AWS region are required")
	}
	return &EFS{api: api, fileSystemID: fileSystemID, region: region}, nil
}

// Ensure creates one POSIX-confined Access Point per Tenant/Workspace. EFS's
// client token is deterministic so retries of an ambiguous CreateAccessPoint
// request return the same AP instead of creating an orphan.
func (e *EFS) Ensure(ctx context.Context, identity Identity) (Binding, error) {
	if e == nil || ctx == nil || ctx.Err() != nil || !validIdentity(identity) {
		return Binding{}, errors.New("Workspace EFS identity or context is invalid")
	}
	root := "/hako/" + shortHash(string(identity.ResourcePlaneID)) + "/" + shortHash(string(identity.TenantID)) + "/" + shortHash(string(identity.WorkspaceID))
	uid := stablePOSIXID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	gid := uid
	token := hashToken(e.fileSystemID + ":" + string(identity.ResourcePlaneID) + ":" + string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	existing, err := e.DiscoverManaged(ctx)
	if err != nil {
		return Binding{}, err
	}
	var matched *Binding
	for _, point := range existing {
		if point.Identity != identity {
			continue
		}
		if matched != nil {
			return Binding{}, errors.New("multiple EFS Access Points claim the same Workspace identity")
		}
		binding := point.Binding
		matched = &binding
	}
	if matched != nil {
		if matched.RootPath != root || matched.UID != uid || matched.GID != gid {
			return Binding{}, errors.New("existing Workspace EFS Access Point does not match the expected isolation configuration")
		}
		return *matched, nil
	}
	output, err := e.api.CreateAccessPoint(ctx, &efs.CreateAccessPointInput{
		ClientToken: aws.String(token), FileSystemId: aws.String(e.fileSystemID),
		PosixUser: &types.PosixUser{Uid: aws.Int64(int64(uid)), Gid: aws.Int64(int64(gid))},
		RootDirectory: &types.RootDirectory{Path: aws.String(root), CreationInfo: &types.CreationInfo{
			OwnerUid: aws.Int64(int64(uid)), OwnerGid: aws.Int64(int64(gid)), Permissions: aws.String("0700"),
		}},
		Tags: []types.Tag{
			{Key: aws.String("hako:managed-by"), Value: aws.String("hako")},
			{Key: aws.String("hako:tenant-id"), Value: aws.String(string(identity.TenantID))},
			{Key: aws.String("hako:workspace-id"), Value: aws.String(string(identity.WorkspaceID))},
			{Key: aws.String("hako:resource-plane-id"), Value: aws.String(string(identity.ResourcePlaneID))},
		},
	})
	if err != nil {
		return Binding{}, fmt.Errorf("create Workspace EFS Access Point: %w", err)
	}
	if output == nil || !accessPointPattern.MatchString(aws.ToString(output.AccessPointId)) ||
		aws.ToString(output.FileSystemId) != e.fileSystemID || aws.ToString(output.ClientToken) != token ||
		!matchesDescription(output.PosixUser, output.RootDirectory, output.Tags, identity, root, uid, gid) {
		return Binding{}, errors.New("EFS returned an Access Point that does not match the requested Workspace isolation")
	}
	return Binding{FileSystemID: e.fileSystemID, Region: e.region, AccessPointID: aws.ToString(output.AccessPointId), RootPath: root, UID: uid, GID: gid}, nil
}

// DiscoverManaged lists only Hako-tagged Access Points on this exact
// filesystem. Operators can compare these identities with durable Workspace
// bindings to find orphans; discovery never adopts or deletes an AP.
func (e *EFS) DiscoverManaged(ctx context.Context) ([]ManagedAccessPoint, error) {
	if e == nil || ctx == nil || ctx.Err() != nil {
		return nil, errors.New("Workspace EFS discovery context is invalid")
	}
	var found []ManagedAccessPoint
	var next *string
	seenTokens := map[string]struct{}{}
	for {
		output, err := e.api.DescribeAccessPoints(ctx, &efs.DescribeAccessPointsInput{FileSystemId: aws.String(e.fileSystemID), MaxResults: aws.Int32(100), NextToken: next})
		if err != nil {
			return nil, fmt.Errorf("discover Workspace EFS Access Points: %w", err)
		}
		if output == nil {
			return nil, errors.New("EFS returned an empty Access Point discovery response")
		}
		for _, point := range output.AccessPoints {
			if !hasTag(point.Tags, "hako:managed-by", "hako") {
				continue
			}
			identity, ok := identityFromTags(point.Tags)
			if !ok || aws.ToString(point.FileSystemId) != e.fileSystemID || !validIdentity(identity) {
				return nil, errors.New("Hako-tagged EFS Access Point has incomplete ownership metadata")
			}
			root := "/hako/" + shortHash(string(identity.ResourcePlaneID)) + "/" + shortHash(string(identity.TenantID)) + "/" + shortHash(string(identity.WorkspaceID))
			uid := stablePOSIXID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
			if !accessPointPattern.MatchString(aws.ToString(point.AccessPointId)) || !matchesDescription(point.PosixUser, point.RootDirectory, point.Tags, identity, root, uid, uid) {
				return nil, errors.New("Hako-tagged EFS Access Point failed ownership or POSIX isolation checks")
			}
			found = append(found, ManagedAccessPoint{Identity: identity, Binding: Binding{FileSystemID: e.fileSystemID, Region: e.region, AccessPointID: aws.ToString(point.AccessPointId), RootPath: root, UID: uid, GID: uid}})
		}
		if output.NextToken == nil || strings.TrimSpace(*output.NextToken) == "" {
			return found, nil
		}
		if _, duplicate := seenTokens[*output.NextToken]; duplicate {
			return nil, errors.New("EFS Access Point discovery returned a repeated pagination token")
		}
		seenTokens[*output.NextToken] = struct{}{}
		next = output.NextToken
	}
}

// DeleteOwned is idempotent and refuses to delete an AP unless its filesystem,
// Workspace tags, root path, and POSIX identity match the expected binding.
// Callers must first stop/unmount the MicroVM and durably tombstone its runtime.
func (e *EFS) DeleteOwned(ctx context.Context, identity Identity, binding Binding) error {
	if e == nil || ctx == nil || ctx.Err() != nil || !validIdentity(identity) ||
		binding.FileSystemID != e.fileSystemID || binding.Region != e.region || binding.ValidateFor(identity) != nil {
		return errors.New("Workspace EFS deletion identity or binding is invalid")
	}
	output, err := e.api.DescribeAccessPoints(ctx, &efs.DescribeAccessPointsInput{AccessPointId: aws.String(binding.AccessPointID)})
	if err != nil {
		return fmt.Errorf("verify Workspace EFS Access Point ownership: %w", err)
	}
	if output == nil || len(output.AccessPoints) == 0 {
		return nil
	}
	if len(output.AccessPoints) != 1 {
		return errors.New("EFS returned an ambiguous Access Point description")
	}
	point := output.AccessPoints[0]
	if aws.ToString(point.FileSystemId) != binding.FileSystemID ||
		!matchesDescription(point.PosixUser, point.RootDirectory, point.Tags, identity, binding.RootPath, binding.UID, binding.GID) {
		return errors.New("refusing to delete an Access Point not owned by this Workspace")
	}
	if _, err := e.api.DeleteAccessPoint(ctx, &efs.DeleteAccessPointInput{AccessPointId: aws.String(binding.AccessPointID)}); err != nil {
		return fmt.Errorf("delete Workspace EFS Access Point: %w", err)
	}
	return nil
}

func validIdentity(identity Identity) bool {
	return idPattern.MatchString(string(identity.TenantID)) && idPattern.MatchString(string(identity.WorkspaceID)) &&
		idPattern.MatchString(string(identity.ResourcePlaneID))
}

func identityFromTags(tags []types.Tag) (Identity, bool) {
	values := map[string]string{}
	for _, tag := range tags {
		key := aws.ToString(tag.Key)
		if key == "hako:tenant-id" || key == "hako:workspace-id" || key == "hako:resource-plane-id" {
			if _, exists := values[key]; exists {
				return Identity{}, false
			}
			values[key] = aws.ToString(tag.Value)
		}
	}
	identity := Identity{TenantID: domain.TenantID(values["hako:tenant-id"]), WorkspaceID: domain.WorkspaceID(values["hako:workspace-id"]), ResourcePlaneID: domain.ResourcePlaneID(values["hako:resource-plane-id"])}
	return identity, len(values) == 3
}

func hasTag(tags []types.Tag, key, value string) bool {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == key && aws.ToString(tag.Value) == value {
			return true
		}
	}
	return false
}

func matchesDescription(posix *types.PosixUser, root *types.RootDirectory, tags []types.Tag, identity Identity, path string, uid, gid uint32) bool {
	if posix == nil || root == nil || root.CreationInfo == nil || aws.ToInt64(posix.Uid) != int64(uid) ||
		aws.ToInt64(posix.Gid) != int64(gid) || aws.ToString(root.Path) != path ||
		aws.ToInt64(root.CreationInfo.OwnerUid) != int64(uid) || aws.ToInt64(root.CreationInfo.OwnerGid) != int64(gid) ||
		aws.ToString(root.CreationInfo.Permissions) != "0700" {
		return false
	}
	want := map[string]string{
		"hako:managed-by": "hako", "hako:tenant-id": string(identity.TenantID),
		"hako:workspace-id": string(identity.WorkspaceID), "hako:resource-plane-id": string(identity.ResourcePlaneID),
	}
	expected := make(map[string]string, len(want))
	for key, value := range want {
		expected[key] = value
	}
	seen := make(map[string]struct{}, len(want))
	for _, tag := range tags {
		key := aws.ToString(tag.Key)
		value, relevant := expected[key]
		if !relevant {
			continue
		}
		if _, duplicate := seen[key]; duplicate || aws.ToString(tag.Value) != value {
			return false
		}
		seen[key] = struct{}{}
		delete(want, key)
	}
	return len(want) == 0
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func hashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stablePOSIXID(value string) uint32 {
	sum := sha256.Sum256([]byte(value))
	// Avoid root and the low system account range while keeping a stable owner.
	return (uint32(sum[0])<<16 | uint32(sum[1])<<8 | uint32(sum[2])) + 100000
}
