package workloadjournal

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type s3Stub struct {
	putInput *s3.PutObjectInput
	getInput *s3.GetObjectInput
	putOut   *s3.PutObjectOutput
	getOut   *s3.GetObjectOutput
	putErr   error
	getErr   error
}

func (s *s3Stub) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.putInput = input
	return s.putOut, s.putErr
}

func (s *s3Stub) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.getInput = input
	return s.getOut, s.getErr
}

func TestS3StoreUsesConditionalCreateAndCompareAndSwap(t *testing.T) {
	now := time.Now().UTC()
	journal, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", ClientToken: "client_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	client := &s3Stub{putOut: &s3.PutObjectOutput{ETag: s3StringPtr(`"etag-1"`)}}
	store, err := NewS3Store(client, "hako-journals", "workloads/rp-1")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(context.Background(), journal)
	if err != nil || created.ETag != `"etag-1"` {
		t.Fatalf("Create() = %+v, %v", created, err)
	}
	if *client.putInput.IfNoneMatch != "*" || *client.putInput.Key != "workloads/rp-1/tenant_1/run_1/attempt_1.json" {
		t.Fatalf("journal create was not conditionally scoped: %+v", client.putInput)
	}

	next := journal
	next.ExecutionState = Launching
	next.Revision++
	next.UpdatedAt = now.Add(time.Second)
	client.putOut = &s3.PutObjectOutput{ETag: s3StringPtr(`"etag-2"`)}
	updated, err := store.CompareAndSwap(context.Background(), created, next)
	if err != nil || updated.ETag != `"etag-2"` {
		t.Fatalf("CompareAndSwap() = %+v, %v", updated, err)
	}
	if client.putInput.IfMatch == nil || *client.putInput.IfMatch != `"etag-1"` || client.putInput.IfNoneMatch != nil {
		t.Fatalf("journal update was not ETag-conditional: %+v", client.putInput)
	}
	client.putErr = &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "etag changed"}
	if _, err := store.CompareAndSwap(context.Background(), created, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("lost conditional update error = %v, want ErrConflict", err)
	}
}

func TestS3StoreLoadsAndValidatesKeyAgainstJournal(t *testing.T) {
	now := time.Now().UTC()
	journal, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", ClientToken: "client_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := Encode(journal)
	client := &s3Stub{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(data))), ETag: s3StringPtr(`"etag-1"`)}}
	store, _ := NewS3Store(client, "bucket", "prefix")
	loaded, err := store.Get(context.Background(), "tenant_1", "run_1", "attempt_1")
	if err != nil || loaded.Journal.AttemptID != "attempt_1" || loaded.ETag != `"etag-1"` {
		t.Fatalf("Get() = %+v, %v", loaded, err)
	}
	if *client.getInput.Key != "prefix/tenant_1/run_1/attempt_1.json" {
		t.Fatalf("journal key = %q", *client.getInput.Key)
	}
	if _, err := store.Get(context.Background(), "tenant_1/../other", "run_1", "attempt_1"); err == nil {
		t.Fatal("unsafe identity reached S3")
	}
}

func TestS3StoreRejectsInvalidConfigurationAndMapsFailures(t *testing.T) {
	if _, err := NewS3Store(nil, "bucket", ""); err == nil {
		t.Fatal("nil S3 client accepted")
	}
	client := &s3Stub{}
	if _, err := NewS3Store(client, " ", ""); err == nil {
		t.Fatal("empty bucket accepted")
	}
	if _, err := NewS3Store(client, "bucket", "prefix/../escape"); err == nil {
		t.Fatal("unsafe prefix accepted")
	}
	store, err := NewS3Store(client, "bucket", "prefix")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	valid, err := New(Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 1, ResourcePlaneID: "rp_1", ClientToken: "client_1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	invalidStart := valid
	invalidStart.Revision = 2
	if _, err := store.Create(context.Background(), invalidStart); err == nil {
		t.Fatal("non-initial journal accepted for create")
	}
	client.putErr = &smithy.GenericAPIError{Code: "ConditionalRequestConflict", Message: "already exists"}
	if _, err := store.Create(context.Background(), valid); !errors.Is(err, ErrConflict) {
		t.Fatalf("conditional create error = %v", err)
	}
	client.putErr = nil
	if _, err := store.Create(context.Background(), valid); err == nil {
		t.Fatal("missing S3 ETag accepted")
	}

	client.getErr = &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}
	if _, err := store.Get(context.Background(), "tenant_1", "run_1", "attempt_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing journal error = %v", err)
	}
	client.getErr = errors.New("network unavailable")
	if _, err := store.Get(context.Background(), "tenant_1", "run_1", "attempt_1"); err == nil {
		t.Fatal("S3 read error was swallowed")
	}
	client.getErr = nil
	client.getOut = &s3.GetObjectOutput{}
	if _, err := store.Get(context.Background(), "tenant_1", "run_1", "attempt_1"); err == nil {
		t.Fatal("incomplete S3 response accepted")
	}
	data, _ := Encode(valid)
	wrongKeyJournal := valid
	wrongKeyJournal.AttemptID = "attempt_other"
	wrongKeyData, _ := Encode(wrongKeyJournal)
	client.getOut = &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(wrongKeyData))), ETag: s3StringPtr(`"etag"`)}
	if _, err := store.Get(context.Background(), "tenant_1", "run_1", "attempt_1"); err == nil {
		t.Fatal("journal payload for a different key accepted")
	}
	client.getOut = &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(string(data))), ETag: s3StringPtr(`"etag"`)}
	loaded, err := store.Get(context.Background(), "tenant_1", "run_1", "attempt_1")
	if err != nil {
		t.Fatal(err)
	}
	next := valid
	next.ExecutionState = Launching
	next.Revision++
	next.UpdatedAt = now.Add(time.Second)
	client.putErr = errors.New("network unavailable")
	if _, err := store.CompareAndSwap(context.Background(), loaded, next); err == nil {
		t.Fatal("S3 write error was swallowed")
	}
	client.putErr = nil
	if _, err := store.CompareAndSwap(context.Background(), Snapshot{Journal: loaded.Journal}, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS without ETag error = %v", err)
	}
	client.putOut = &s3.PutObjectOutput{}
	if _, err := store.CompareAndSwap(context.Background(), loaded, next); err == nil {
		t.Fatal("CAS response without ETag accepted")
	}
}

func s3StringPtr(value string) *string { return &value }
