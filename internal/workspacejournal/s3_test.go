package workspacejournal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/mo3789530/hako/internal/domain"
)

type object struct {
	data []byte
	etag string
}

type conditionalS3 struct {
	mu                  sync.Mutex
	objects             map[string]object
	version             int
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

func isConflict(err error) bool {
	return errors.Is(err, ErrConflict)
}
