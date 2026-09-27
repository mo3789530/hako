package workloadjobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mo3789530/hako/internal/domain"
)

type putObjectStub struct {
	input *s3.PutObjectInput
	err   error
	calls int
}

func (s *putObjectStub) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.calls++
	s.input = input
	return &s3.PutObjectOutput{}, s.err
}

func TestS3ArtifactWriterStoresTenantScopedEncryptedContent(t *testing.T) {
	logs := []byte("test output with secrets already redacted\n")
	digest := sha256.Sum256(logs)
	digestHex := hex.EncodeToString(digest[:])
	client := &putObjectStub{}
	writer, err := NewS3ArtifactWriter(client, "hako-artifacts-example", "/hako/factory/", "arn:aws:kms:ap-northeast-1:123456789012:key/abc")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := writer.PutRedactedLogs(context.Background(), domain.TenantID("tenant-1"), domain.WorkloadRunID("run_1"), logs, digestHex)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "hako/factory/tenant-1/run_1/logs/" + digestHex + ".txt"
	if ref != "s3://hako-artifacts-example/"+wantKey || client.calls != 1 {
		t.Fatalf("ref=%q calls=%d", ref, client.calls)
	}
	body, err := io.ReadAll(client.input.Body)
	if err != nil || string(body) != string(logs) {
		t.Fatalf("uploaded body=%q error=%v", body, err)
	}
	if *client.input.Bucket != "hako-artifacts-example" || *client.input.Key != wantKey || *client.input.ContentLength != int64(len(logs)) {
		t.Fatalf("unexpected S3 request: %+v", client.input)
	}
	if client.input.ServerSideEncryption != types.ServerSideEncryptionAwsKms || client.input.SSEKMSKeyId == nil ||
		*client.input.SSEKMSKeyId != "arn:aws:kms:ap-northeast-1:123456789012:key/abc" {
		t.Fatalf("SSE-KMS not configured: %+v", client.input)
	}
	if *client.input.ChecksumSHA256 != base64.StdEncoding.EncodeToString(digest[:]) || client.input.Metadata["hako-redacted"] != "true" ||
		client.input.Metadata["hako-tenant-id"] != "tenant-1" || client.input.Metadata["hako-sha256"] != digestHex {
		t.Fatalf("checksum/metadata missing: %+v", client.input)
	}
	if _, err := writer.PutRedactedLogs(context.Background(), "tenant-1", "run_1", logs, digestHex); err != nil || *client.input.Key != wantKey {
		t.Fatalf("retry not deterministic: request=%+v error=%v", client.input, err)
	}
}

func TestS3ArtifactWriterDefaultsToSSES3AndRejectsUnsafeInput(t *testing.T) {
	client := &putObjectStub{}
	writer, err := NewS3ArtifactWriter(client, "hako-artifacts-example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte("ok")
	digest := sha256.Sum256(logs)
	if _, err := writer.PutRedactedLogs(context.Background(), "tenant", "run", logs, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if client.input.ServerSideEncryption != types.ServerSideEncryptionAes256 || client.input.SSEKMSKeyId != nil {
		t.Fatalf("expected SSE-S3 by default: %+v", client.input)
	}
	for _, input := range []struct {
		bucket string
		prefix string
	}{
		{bucket: "bad_bucket"},
		{bucket: "hako-artifacts-example", prefix: "../escape"},
		{bucket: "hako-artifacts-example", prefix: "one//two"},
	} {
		if _, err := NewS3ArtifactWriter(client, input.bucket, input.prefix, ""); err == nil {
			t.Errorf("NewS3ArtifactWriter(%+v) succeeded", input)
		}
	}
	if _, err := writer.PutRedactedLogs(context.Background(), "../tenant", "run", logs, hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("unsafe Tenant path was accepted")
	}
	if _, err := writer.PutRedactedLogs(context.Background(), "tenant", "run", logs, "not-a-sha"); err == nil {
		t.Fatal("invalid digest was accepted")
	}
}

func TestS3ArtifactWriterReturnsUploadErrors(t *testing.T) {
	client := &putObjectStub{err: errors.New("access denied")}
	writer, err := NewS3ArtifactWriter(client, "hako-artifacts-example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	logs := []byte("ok")
	digest := sha256.Sum256(logs)
	if _, err := writer.PutRedactedLogs(context.Background(), "tenant", "run", logs, hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("PutRedactedLogs() swallowed S3 error")
	}
}
