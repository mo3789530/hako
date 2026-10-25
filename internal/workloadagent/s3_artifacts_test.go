package workloadagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type attemptLogPutStub struct {
	input *s3.PutObjectInput
	err   error
}

func (s *attemptLogPutStub) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.input = input
	return &s3.PutObjectOutput{}, s.err
}

func TestS3AttemptLogWriterStoresRedactedLogsUnderExactAttempt(t *testing.T) {
	logs := []byte("redacted test output\n")
	digest := sha256.Sum256(logs)
	digestHex := hex.EncodeToString(digest[:])
	client := &attemptLogPutStub{}
	writer, err := NewS3AttemptLogWriter(client, "hako-attempt-artifacts-example", "hako/workloads", "arn:aws:kms:ap-northeast-1:123456789012:key/example")
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_2", Generation: 2,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idempotency_2"}
	artifact, err := writer.PutAttemptLogs(context.Background(), id, logs, digestHex)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "hako/workloads/tenant_1/run_1/attempt_2/logs/" + digestHex + ".txt"
	if artifact.StorageRef != "s3://hako-attempt-artifacts-example/"+wantKey || artifact.SizeBytes != int64(len(logs)) ||
		artifact.SHA256 != digestHex || !artifact.Redacted {
		t.Fatalf("attempt log artifact = %+v", artifact)
	}
	if got := *client.input.Key; got != wantKey {
		t.Fatalf("attempt log key = %q, want %q", got, wantKey)
	}
	body, err := io.ReadAll(client.input.Body)
	if err != nil || string(body) != string(logs) || *client.input.ContentLength != int64(len(logs)) {
		t.Fatalf("uploaded body=%q content_length=%d err=%v", body, *client.input.ContentLength, err)
	}
	if client.input.ServerSideEncryption != types.ServerSideEncryptionAwsKms || client.input.SSEKMSKeyId == nil ||
		*client.input.SSEKMSKeyId != "arn:aws:kms:ap-northeast-1:123456789012:key/example" ||
		*client.input.ChecksumSHA256 != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Fatalf("encryption/checksum missing: %+v", client.input)
	}
	metadata := client.input.Metadata
	if metadata["hako-tenant-id"] != id.TenantID || metadata["hako-workload-run-id"] != id.WorkloadRunID ||
		metadata["hako-attempt-id"] != id.AttemptID || metadata["hako-sha256"] != digestHex || metadata["hako-redacted"] != "true" {
		t.Fatalf("attempt metadata = %+v", metadata)
	}
}

func TestS3AttemptLogWriterRejectsInvalidDataBeforeUpload(t *testing.T) {
	logs := []byte("safe")
	digest := sha256.Sum256(logs)
	validDigest := hex.EncodeToString(digest[:])
	client := &attemptLogPutStub{}
	writer, err := NewS3AttemptLogWriter(client, "hako-attempt-artifacts-example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idempotency_1"}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		id     workloadjournal.Command
		logs   []byte
		digest string
	}{
		{name: "nil context", ctx: nil, id: id, logs: logs, digest: validDigest},
		{name: "invalid identity", ctx: context.Background(), id: workloadjournal.Command{}, logs: logs, digest: validDigest},
		{name: "digest mismatch", ctx: context.Background(), id: id, logs: logs, digest: strings.Repeat("a", 64)},
		{name: "oversized", ctx: context.Background(), id: id, logs: make([]byte, maxAttemptLogBytes+1), digest: validDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := writer.PutAttemptLogs(tc.ctx, tc.id, tc.logs, tc.digest); err == nil {
				t.Fatal("invalid log upload accepted")
			}
			if client.input != nil {
				t.Fatal("invalid log upload reached S3")
			}
		})
	}
}

func TestS3AttemptLogWriterSanitizesUploadErrors(t *testing.T) {
	client := &attemptLogPutStub{err: errors.New("bucket policy revealed sensitive details")}
	writer, err := NewS3AttemptLogWriter(client, "hako-attempt-artifacts-example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte("safe")
	digest := sha256.Sum256(logs)
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idempotency_1"}
	_, err = writer.PutAttemptLogs(context.Background(), id, logs, hex.EncodeToString(digest[:]))
	if err == nil || strings.Contains(err.Error(), "sensitive details") {
		t.Fatalf("upload error was not sanitized: %v", err)
	}
}

func TestS3AttemptChangeBundleWriterUsesAttemptScopedUnredactedArtifactKey(t *testing.T) {
	bundle := []byte("bounded proposal archive")
	digest := sha256.Sum256(bundle)
	digestHex := hex.EncodeToString(digest[:])
	client := &attemptLogPutStub{}
	writer, err := NewS3AttemptChangeBundleWriter(client, "hako-attempt-artifacts-example", "hako/workloads", "")
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_2", Generation: 2,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idempotency_2"}
	artifact, err := writer.PutAgentChangeBundle(context.Background(), id, bundle, digestHex)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "hako/workloads/tenant_1/run_1/attempt_2/agent-changes/" + digestHex + ".zip"
	if artifact.StorageRef != "s3://hako-attempt-artifacts-example/"+wantKey || artifact.SizeBytes != int64(len(bundle)) || artifact.SHA256 != digestHex {
		t.Fatalf("Agent change artifact = %+v", artifact)
	}
	if *client.input.Key != wantKey || client.input.ContentType == nil || *client.input.ContentType != "application/zip" ||
		client.input.ServerSideEncryption != types.ServerSideEncryptionAes256 || client.input.Metadata["hako-artifact-kind"] != "agent-changes" {
		t.Fatalf("change bundle storage key or metadata is not scoped: %+v", client.input)
	}
	if _, err := writer.PutAgentChangeBundle(context.Background(), id, bundle, strings.Repeat("a", 64)); err == nil {
		t.Fatal("change bundle with a mismatched digest was stored")
	}
}
