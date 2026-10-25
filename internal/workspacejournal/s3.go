package workspacejournal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

type s3API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

const maxListedWorkspaceJournals = 100000

// S3Store implements a Workspace-scoped journal using conditional object
// creation and ETag compare-and-swap. The backing bucket must deny object
// deletion to runtime roles so tombstones cannot be removed by a controller.
type S3Store struct {
	client s3API
	bucket string
	prefix string
}

func NewS3Store(client s3API, bucket, prefix string) (*S3Store, error) {
	if client == nil || strings.TrimSpace(bucket) == "" {
		return nil, errors.New("S3 client and Workspace journal bucket are required")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if strings.Contains(prefix, "..") || strings.ContainsAny(prefix, "\\\x00") {
		return nil, errors.New("Workspace journal prefix is invalid")
	}
	return &S3Store{client: client, bucket: strings.TrimSpace(bucket), prefix: prefix}, nil
}

func (s *S3Store) Create(ctx context.Context, journal Journal) (Snapshot, error) {
	if journal.Revision != 1 {
		return Snapshot{}, errors.New("new Workspace runtime journal must start at revision 1")
	}
	body, err := Encode(journal)
	if err != nil {
		return Snapshot{}, err
	}
	key, err := s.key(journal.TenantID, journal.WorkspaceID, journal.ResourcePlaneID)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, Body: strings.NewReader(string(body)),
		ContentType: s3StringPtr("application/json"), IfNoneMatch: s3StringPtr("*"),
	})
	if err != nil {
		return Snapshot{}, mapS3Error(err)
	}
	if output == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		return Snapshot{}, errors.New("S3 did not return a Workspace runtime journal ETag")
	}
	return Snapshot{Journal: journal, ETag: *output.ETag}, nil
}

func (s *S3Store) Get(ctx context.Context, tenantID domain.TenantID, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID) (Snapshot, error) {
	key, err := s.key(tenantID, workspaceID, resourcePlaneID)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return Snapshot{}, mapS3Error(err)
	}
	if output == nil || output.Body == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		return Snapshot{}, errors.New("S3 Workspace runtime journal response is incomplete")
	}
	defer output.Body.Close()
	data, err := io.ReadAll(io.LimitReader(output.Body, (32<<10)+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read Workspace runtime journal body: %w", err)
	}
	journal, err := Decode(data)
	if err != nil {
		return Snapshot{}, err
	}
	if journal.TenantID != tenantID || journal.WorkspaceID != workspaceID || journal.ResourcePlaneID != resourcePlaneID {
		return Snapshot{}, errors.New("S3 Workspace runtime journal key does not match payload")
	}
	return Snapshot{Journal: journal, ETag: *output.ETag}, nil
}

// ListStorageBindings enumerates journal objects under this store's configured
// prefix and returns the active EFS bindings needed by the read-only operator
// reconciliation. Confirmed delete tombstones are omitted because their AP
// cleanup has completed. The result is bounded and sorted deterministically.
func (s *S3Store) ListStorageBindings(ctx context.Context, limit int) ([]workspacestorage.WorkspaceBinding, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || limit < 1 || limit > maxListedWorkspaceJournals {
		return nil, errors.New("Workspace journal listing bounds are invalid")
	}
	prefix := ""
	if s.prefix != "" {
		prefix = s.prefix + "/"
	}
	var result []workspacestorage.WorkspaceBinding
	listed := 0
	var token *string
	seenTokens := map[string]struct{}{}
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: &s.bucket, Prefix: &prefix, MaxKeys: s3Int32Ptr(1000), ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list Workspace runtime journals: %w", err)
		}
		if page == nil {
			return nil, errors.New("S3 returned an empty Workspace journal listing")
		}
		for _, item := range page.Contents {
			listed++
			if listed > limit {
				return nil, errors.New("Workspace journal listing exceeds configured limit")
			}
			if item.Key == nil {
				return nil, errors.New("S3 Workspace journal listing contains an empty key")
			}
			identity, err := s.identityFromKey(*item.Key)
			if err != nil {
				return nil, err
			}
			snapshot, err := s.Get(ctx, identity.TenantID, identity.WorkspaceID, identity.ResourcePlaneID)
			if err != nil {
				return nil, err
			}
			journal := snapshot.Journal
			if journal.StorageBinding == nil || (journal.Tombstone && journal.CleanupState == CleanupConfirmed) {
				continue
			}
			result = append(result, workspacestorage.WorkspaceBinding{Identity: workspacestorage.Identity{
				TenantID: journal.TenantID, WorkspaceID: journal.WorkspaceID, ResourcePlaneID: journal.ResourcePlaneID,
			}, Binding: *journal.StorageBinding})
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		if page.NextContinuationToken == nil || strings.TrimSpace(*page.NextContinuationToken) == "" {
			return nil, errors.New("S3 Workspace journal listing omitted its continuation token")
		}
		if _, duplicate := seenTokens[*page.NextContinuationToken]; duplicate {
			return nil, errors.New("S3 Workspace journal listing repeated a continuation token")
		}
		seenTokens[*page.NextContinuationToken] = struct{}{}
		token = page.NextContinuationToken
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Identity.ResourcePlaneID != result[j].Identity.ResourcePlaneID {
			return result[i].Identity.ResourcePlaneID < result[j].Identity.ResourcePlaneID
		}
		if result[i].Identity.TenantID != result[j].Identity.TenantID {
			return result[i].Identity.TenantID < result[j].Identity.TenantID
		}
		return result[i].Identity.WorkspaceID < result[j].Identity.WorkspaceID
	})
	return result, nil
}

func (s *S3Store) identityFromKey(key string) (workspacestorage.Identity, error) {
	if s.prefix != "" {
		if !strings.HasPrefix(key, s.prefix+"/") {
			return workspacestorage.Identity{}, errors.New("S3 Workspace journal key is outside the configured prefix")
		}
		key = strings.TrimPrefix(key, s.prefix+"/")
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".json") {
		return workspacestorage.Identity{}, errors.New("S3 Workspace journal key has an invalid shape")
	}
	workspaceID := strings.TrimSuffix(parts[2], ".json")
	if !idPattern.MatchString(parts[0]) || !idPattern.MatchString(parts[1]) || !idPattern.MatchString(workspaceID) {
		return workspacestorage.Identity{}, errors.New("S3 Workspace journal key has an invalid identity")
	}
	return workspacestorage.Identity{ResourcePlaneID: domain.ResourcePlaneID(parts[0]), TenantID: domain.TenantID(parts[1]), WorkspaceID: domain.WorkspaceID(workspaceID)}, nil
}

func (s *S3Store) CompareAndSwap(ctx context.Context, current Snapshot, next Journal) (Snapshot, error) {
	if err := ValidateUpdate(current, next); err != nil {
		return Snapshot{}, err
	}
	body, err := Encode(next)
	if err != nil {
		return Snapshot{}, err
	}
	key, err := s.key(next.TenantID, next.WorkspaceID, next.ResourcePlaneID)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, Body: strings.NewReader(string(body)),
		ContentType: s3StringPtr("application/json"), IfMatch: &current.ETag,
	})
	if err != nil {
		return Snapshot{}, mapS3Error(err)
	}
	if output == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		return Snapshot{}, errors.New("S3 did not return a Workspace runtime journal ETag")
	}
	return Snapshot{Journal: next, ETag: *output.ETag}, nil
}

func (s *S3Store) key(tenantID domain.TenantID, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID) (string, error) {
	if !idPattern.MatchString(string(tenantID)) || !idPattern.MatchString(string(workspaceID)) || !idPattern.MatchString(string(resourcePlaneID)) {
		return "", errors.New("Workspace runtime journal identity is invalid")
	}
	key := string(resourcePlaneID) + "/" + string(tenantID) + "/" + string(workspaceID) + ".json"
	if s.prefix != "" {
		key = s.prefix + "/" + key
	}
	return key, nil
}

func mapS3Error(err error) error {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return fmt.Errorf("%w: %s", ErrNotFound, apiError.ErrorCode())
		case "PreconditionFailed", "ConditionalRequestConflict":
			return fmt.Errorf("%w: %s", ErrConflict, apiError.ErrorCode())
		}
	}
	return fmt.Errorf("Workspace runtime journal S3 request: %w", err)
}

func s3StringPtr(value string) *string { return &value }
func s3Int32Ptr(value int32) *int32    { return &value }
