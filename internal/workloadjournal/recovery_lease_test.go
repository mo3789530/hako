package workloadjournal

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type leaseS3Stub struct {
	body string
	etag string
	seq  int
	put  *s3.PutObjectInput
}

func (s *leaseS3Stub) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if s.etag == "" || aws.ToString(input.Key) != "exact/recovery-lease.json" {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(s.body)), ETag: aws.String(s.etag)}, nil
}

func (s *leaseS3Stub) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.put = input
	if input.IfNoneMatch != nil && s.etag != "" || input.IfMatch != nil && aws.ToString(input.IfMatch) != s.etag {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	s.body = string(data)
	s.seq++
	s.etag = `"lease-` + string(rune('0'+s.seq)) + `"`
	return &s3.PutObjectOutput{ETag: aws.String(s.etag)}, nil
}

func TestS3RecoveryLeaseAcquireRenewStealAndStaleRelease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	client := &leaseS3Stub{}
	store, err := NewS3RecoveryLease(client, "bucket", "exact/recovery-lease.json")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Acquire(ctx, "owner_first_123", now, time.Minute)
	if err != nil || first.ETag != `"lease-1"` || aws.ToString(client.put.IfNoneMatch) != "*" {
		t.Fatalf("initial acquire = %+v, err=%v, conditional=%+v", first, err, client.put)
	}
	if _, err := store.Acquire(ctx, "owner_second_123", now, time.Minute); !errors.Is(err, ErrRecoveryLeaseHeld) {
		t.Fatalf("active lease was not held: %v", err)
	}
	renewed, err := store.Renew(ctx, first, now.Add(20*time.Second), time.Minute)
	if err != nil || renewed.ETag != `"lease-2"` || aws.ToString(client.put.IfMatch) != first.ETag {
		t.Fatalf("lease renewal = %+v, err=%v, conditional=%+v", renewed, err, client.put)
	}
	if err := store.Release(ctx, first, now.Add(30*time.Second)); !errors.Is(err, ErrRecoveryLeaseLost) {
		t.Fatalf("stale owner released a renewed lease: %v", err)
	}
	second, err := store.Acquire(ctx, "owner_second_123", now.Add(2*time.Minute), time.Minute)
	if err != nil || second.Owner != "owner_second_123" || aws.ToString(client.put.IfMatch) != renewed.ETag {
		t.Fatalf("expired lease takeover = %+v, err=%v, conditional=%+v", second, err, client.put)
	}
	if err := store.Release(ctx, second, now.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatalf("current owner could not release its lease: %v", err)
	}
	if _, err := store.Acquire(ctx, "owner_third_123", now.Add(2*time.Minute+2*time.Second), time.Minute); err != nil {
		t.Fatalf("released lease was not acquirable: %v", err)
	}
}

func TestS3RecoveryLeaseRejectsInvalidConfigurationAndStoredDocument(t *testing.T) {
	client := &leaseS3Stub{}
	for _, key := range []string{"", "/absolute/key", "prefix/../escape", "bad\\key"} {
		if _, err := NewS3RecoveryLease(client, "bucket", key); err == nil {
			t.Fatalf("invalid lease key %q accepted", key)
		}
	}
	store, err := NewS3RecoveryLease(client, "bucket", "exact/recovery-lease.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, tc := range []struct {
		owner string
		ttl   time.Duration
	}{
		{owner: "short", ttl: time.Minute},
		{owner: "owner_valid_123", ttl: 31 * time.Minute},
	} {
		if _, err := store.Acquire(context.Background(), tc.owner, now, tc.ttl); err == nil {
			t.Fatalf("invalid owner/TTL accepted: %+v", tc)
		}
	}
	client.etag, client.body = `"bad"`, `{"schema_version":2,"owner":"owner_valid_123","expires_at":"2026-10-12T01:01:00Z"}`
	if _, err := store.Acquire(context.Background(), "owner_next_123", now, time.Minute); err == nil {
		t.Fatal("invalid stored lease was overwritten")
	}
}
