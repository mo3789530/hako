package workspacejournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

type object struct {
	data []byte
	etag string
}

type conditionalS3 struct {
	mu                  sync.Mutex
	objects             map[string]object
	version             int
	listPageSize        int
	writeErrAfterCommit error
}

func newConditionalS3() *conditionalS3 {
	return &conditionalS3{objects: make(map[string]object)}
}

func (s *conditionalS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.objects[*input.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(stored.data))), ETag: &stored.etag}, nil
}

func (s *conditionalS3) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, *input.Prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := 0
	if input.ContinuationToken != nil {
		parsed, err := strconv.Atoi(*input.ContinuationToken)
		if err != nil || parsed < 0 || parsed > len(keys) {
			return nil, errors.New("invalid continuation token")
		}
		start = parsed
	}
	end := len(keys)
	pageSize := int(*input.MaxKeys)
	if s.listPageSize > 0 && s.listPageSize < pageSize {
		pageSize = s.listPageSize
	}
	if end-start > pageSize {
		end = start + pageSize
	}
	contents := make([]s3types.Object, 0, end-start)
	for _, key := range keys[start:end] {
		keyCopy := key
		contents = append(contents, s3types.Object{Key: &keyCopy})
	}
	output := &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(end < len(keys))}
	if end < len(keys) {
		next := strconv.Itoa(end)
		output.NextContinuationToken = &next
	}
	return output, nil
}

func (s *conditionalS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := *input.Key
	stored, exists := s.objects[key]
	if input.IfNoneMatch != nil && *input.IfNoneMatch == "*" && exists {
		return nil, &smithy.GenericAPIError{Code: "ConditionalRequestConflict", Message: "object exists"}
	}
	if input.IfMatch != nil && (!exists || *input.IfMatch != stored.etag) {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "etag mismatch"}
	}
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	s.version++
	etag := fmt.Sprintf(`"etag-%d"`, s.version)
	s.objects[key] = object{data: data, etag: etag}
	if s.writeErrAfterCommit != nil {
		err := s.writeErrAfterCommit
		s.writeErrAfterCommit = nil
		return nil, err
	}
	return &s3.PutObjectOutput{ETag: &etag}, nil
}

func TestS3StoreUsesConditionalCreateAndCAS(t *testing.T) {
	ctx := context.Background()
	client := newConditionalS3()
	store, err := NewS3Store(client, "hako-rp-journal", "workspace-journal")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hash := DigestLaunchSpec([]byte(`{"image":"image-v1"}`))
	initial, err := New(Command{TenantID: "tenant_1", WorkspaceID: "workspace_1", ResourcePlaneID: "rp_tokyo_1",
		WorkspaceRevision: 1, OperationID: "op_1", Type: domain.OperationEnsureRunning}, "token_1", hash, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	key := "workspace-journal/rp_tokyo_1/tenant_1/workspace_1.json"
	if _, ok := client.objects[key]; !ok {
		t.Fatalf("journal was not stored under scoped key %q", key)
	}
	if _, err := store.Create(ctx, initial); err == nil {
		t.Fatal("conditional create overwrote an existing journal")
	}
	loaded, err := store.Get(ctx, initial.TenantID, initial.WorkspaceID, initial.ResourcePlaneID)
	if err != nil || loaded.Journal.OperationID != initial.OperationID {
		t.Fatalf("Get() = %+v, %v", loaded, err)
	}
	if _, err := store.Get(ctx, "tenant_1/../escape", initial.WorkspaceID, initial.ResourcePlaneID); err == nil {
		t.Fatal("unsafe identity reached S3")
	}

	next := initial
	next.Revision++
	next.State, next.ProviderID = Running, "microvm-1"
	next.UpdatedAt = now.Add(time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.CompareAndSwap(ctx, created, next)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	succeeded, conflicted := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case isConflict(err):
			conflicted++
		default:
			t.Fatalf("unexpected CAS result: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent CAS succeeded=%d conflicted=%d, want 1 each", succeeded, conflicted)
	}
	final, err := store.Get(ctx, initial.TenantID, initial.WorkspaceID, initial.ResourcePlaneID)
	if err != nil || final.Journal.State != Running || final.Journal.ProviderID != "microvm-1" {
		t.Fatalf("final checkpoint = %+v, %v", final, err)
	}
	foreign := final.Journal
	foreign.TenantID = "tenant_other"
	foreignData, err := Encode(foreign)
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.objects[key] = object{data: foreignData, etag: `"foreign"`}
	client.mu.Unlock()
	if _, err := store.Get(ctx, initial.TenantID, initial.WorkspaceID, initial.ResourcePlaneID); err == nil {
		t.Fatal("payload identity mismatch for S3 key was accepted")
	}
}

func TestS3StoreRecoversAmbiguousCreateAfterControllerRestart(t *testing.T) {
	client := newConditionalS3()
	client.writeErrAfterCommit = fmt.Errorf("connection lost after S3 commit")
	store, err := NewS3Store(client, "hako-rp-journal", "workspace-journal")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	journal, err := New(Command{TenantID: "tenant_1", WorkspaceID: "workspace_1", ResourcePlaneID: "rp_1",
		WorkspaceRevision: 1, OperationID: "op_1", Type: domain.OperationEnsureRunning}, "token_1", DigestLaunchSpec([]byte("{}")), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), journal); err == nil {
		t.Fatal("ambiguous S3 response was hidden")
	}
	// A fresh store instance models controller restart. It recovers the durable
	// operation identity rather than creating a second journal.
	restarted, err := NewS3Store(client, "hako-rp-journal", "workspace-journal")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.Get(context.Background(), journal.TenantID, journal.WorkspaceID, journal.ResourcePlaneID)
	if err != nil || loaded.Journal.OperationID != journal.OperationID || loaded.Journal.LaunchToken != journal.LaunchToken {
		t.Fatalf("journal recovery after ambiguous write = %+v, %v", loaded, err)
	}
}

func TestS3StoreListsBoundJournalsAndKeepsUnconfirmedTombstones(t *testing.T) {
	ctx := context.Background()
	client := newConditionalS3()
	client.listPageSize = 2 // exercise continuation-token handling with a small fixture.
	store, err := NewS3Store(client, "hako-rp-journal", "workspace-journal")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	makeBinding := func(tenant, workspace, plane string) workspacestorage.Binding {
		uid := testStablePOSIXID(tenant + ":" + workspace)
		return workspacestorage.Binding{FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
			RootPath: "/hako/" + testShortHash(plane) + "/" + testShortHash(tenant) + "/" + testShortHash(workspace), UID: uid, GID: uid}
	}
	for _, item := range []struct {
		tenant, workspace, plane string
	}{
		{"tenant_b", "workspace_b", "rp_b"},
		{"tenant_a", "workspace_a", "rp_a"},
	} {
		identity := workspacestorage.Identity{TenantID: domain.TenantID(item.tenant), WorkspaceID: domain.WorkspaceID(item.workspace), ResourcePlaneID: domain.ResourcePlaneID(item.plane)}
		journal, err := NewWithLaunchSpecAndStorage(Command{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID, ResourcePlaneID: identity.ResourcePlaneID,
			WorkspaceRevision: 1, OperationID: "op_1", Type: domain.OperationEnsureRunning}, "token_1", []byte(`{"image":"pinned"}`), ptrBinding(makeBinding(item.tenant, item.workspace, item.plane)), now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(ctx, journal); err != nil {
			t.Fatal(err)
		}
	}
	withoutBinding, err := New(Command{TenantID: "tenant_no_storage", WorkspaceID: "workspace_no_storage", ResourcePlaneID: "rp_a",
		WorkspaceRevision: 1, OperationID: "op_1", Type: domain.OperationEnsureRunning}, "token_1", DigestLaunchSpec([]byte(`{}`)), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, withoutBinding); err != nil {
		t.Fatal(err)
	}
	tombstone, err := New(Command{TenantID: "tenant_deleted", WorkspaceID: "workspace_deleted", ResourcePlaneID: "rp_a",
		WorkspaceRevision: 1, OperationID: "op_1", Type: domain.OperationDelete}, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	tombstone.StorageBinding = ptrBinding(makeBinding("tenant_deleted", "workspace_deleted", "rp_a"))
	if _, err := store.Create(ctx, tombstone); err != nil {
		t.Fatal(err)
	}
	unconfirmed, err := NewWithLaunchSpecAndStorage(Command{TenantID: "tenant_pending", WorkspaceID: "workspace_pending", ResourcePlaneID: "rp_c",
		WorkspaceRevision: 1, OperationID: "op_ensure", Type: domain.OperationEnsureRunning}, "token_pending", []byte(`{"image":"pinned"}`),
		ptrBinding(makeBinding("tenant_pending", "workspace_pending", "rp_c")), now)
	if err != nil {
		t.Fatal(err)
	}
	unconfirmed.State, unconfirmed.ProviderID = Running, "microvm-pending"
	pendingSnapshot := Snapshot{Journal: unconfirmed, ETag: "etag-pending"}
	unconfirmed, err = ApplyCommandWithLaunchSpec(pendingSnapshot, Command{TenantID: unconfirmed.TenantID, WorkspaceID: unconfirmed.WorkspaceID,
		ResourcePlaneID: unconfirmed.ResourcePlaneID, WorkspaceRevision: 2, OperationID: "op_delete", Type: domain.OperationDelete}, "", nil, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pendingKey, err := store.key(unconfirmed.TenantID, unconfirmed.WorkspaceID, unconfirmed.ResourcePlaneID)
	if err != nil {
		t.Fatal(err)
	}
	pendingData, err := Encode(unconfirmed)
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.objects[pendingKey] = object{data: pendingData, etag: "etag-pending-delete"}
	client.mu.Unlock()

	bindings, err := store.ListStorageBindings(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 3 || bindings[0].Identity.ResourcePlaneID != "rp_a" || bindings[1].Identity.ResourcePlaneID != "rp_b" || bindings[2].Identity.ResourcePlaneID != "rp_c" {
		t.Fatalf("ListStorageBindings() = %+v, want active bindings and the unconfirmed tombstone, sorted", bindings)
	}
	if _, err := store.ListStorageBindings(ctx, 1); err == nil {
		t.Fatal("journal listing exceeded its configured result limit")
	}
}

func ptrBinding(binding workspacestorage.Binding) *workspacestorage.Binding { return &binding }

func testShortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func testStablePOSIXID(value string) uint32 {
	sum := sha256.Sum256([]byte(value))
	return (uint32(sum[0])<<16 | uint32(sum[1])<<8 | uint32(sum[2])) + 100000
}

func isConflict(err error) bool {
	return errors.Is(err, ErrConflict)
}
