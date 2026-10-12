package workloadsource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type captureSourceArchiveGet struct {
	input  *s3.GetObjectInput
	output *s3.GetObjectOutput
	err    error
	calls  int
}

func (c *captureSourceArchiveGet) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	c.calls++
	c.input = input
	return c.output, c.err
}

type closeTrackingReader struct {
	*bytes.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error { r.closed = true; return nil }

type captureSourceArchivePut struct {
	input *s3.PutObjectInput
	body  []byte
	err   error
	calls int
}

func (c *captureSourceArchivePut) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	c.calls++
	c.input = input
	if input.Body != nil {
		c.body, _ = io.ReadAll(input.Body)
	}
	return &s3.PutObjectOutput{}, c.err
}

func TestS3SourceArchiveStagerStoresDeterministicEncryptedAttemptArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &captureSourceArchivePut{}
	stager, err := NewS3SourceArchiveStager(client, "hako-source-example", "hako/source", "arn:aws:kms:ap-northeast-1:123456789012:key/abc", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 3, ResourcePlaneID: "rp_1"}
	first, err := stager.StageCheckout(context.Background(), id, root)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := aws.ToString(client.input.Key)
	firstDigest := first.Digest
	actual := sha256.Sum256(client.body)
	if first.Digest != hex.EncodeToString(actual[:]) || aws.ToString(client.input.Bucket) != "hako-source-example" ||
		first.ArtifactPrefix != "s3://hako-source-example/hako/source/tenant_1/run_1/attempt_1/" ||
		firstKey != "hako/source/tenant_1/run_1/attempt_1/3/source/"+first.Digest+".tar" ||
		aws.ToString(client.input.ContentType) != "application/x-tar" || aws.ToInt64(client.input.ContentLength) != int64(len(client.body)) ||
		aws.ToString(client.input.ChecksumSHA256) != base64.StdEncoding.EncodeToString(actual[:]) ||
		client.input.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(client.input.SSEKMSKeyId) == "" ||
		client.input.Metadata["hako-tenant-id"] != id.TenantID || client.input.Metadata["hako-workload-run-id"] != id.WorkloadRunID ||
		client.input.Metadata["hako-attempt-id"] != id.AttemptID || client.input.Metadata["hako-attempt-generation"] != "3" {
		t.Fatalf("unexpected source S3 object: result=%+v input=%+v body_sha=%x", first, client.input, actual)
	}
	if _, err := stager.StageCheckout(context.Background(), id, root); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 || aws.ToString(client.input.Key) != firstKey || firstDigest != client.input.Metadata["hako-source-sha256"] {
		t.Fatalf("retry did not use deterministic content-addressed object: calls=%d key=%q first=%q", client.calls, aws.ToString(client.input.Key), firstKey)
	}
}

func TestS3SourceArchiveStagerDefaultsToSSES3AndRejectsUnsafeConfiguration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &captureSourceArchivePut{}
	stager, err := NewS3SourceArchiveStager(client, "hako-source-example", "", "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1"}
	if _, err := stager.StageCheckout(context.Background(), id, root); err != nil || client.input.ServerSideEncryption != types.ServerSideEncryptionAes256 || client.input.SSEKMSKeyId != nil {
		t.Fatalf("default S3 encryption = %v input=%+v", err, client.input)
	}
	for _, tc := range []struct {
		bucket, prefix, kms string
		max                 int64
	}{
		{bucket: "bad_bucket"}, {bucket: "hako-source-example", prefix: "../escape"}, {bucket: "hako-source-example", prefix: "safe//path"},
		{bucket: "hako-source-example", kms: "key\nvalue"}, {bucket: "hako-source-example", max: -1}, {bucket: "hako-source-example", max: DefaultMaxArchiveBytes + 1},
	} {
		if _, err := NewS3SourceArchiveStager(client, tc.bucket, tc.prefix, tc.kms, tc.max); err == nil {
			t.Errorf("unsafe S3 source configuration accepted: %+v", tc)
		}
	}
	invalid := id
	invalid.AttemptID = "../attempt"
	previousCalls := client.calls
	if _, err := stager.StageCheckout(context.Background(), invalid, root); err == nil || client.calls != previousCalls {
		t.Fatal("invalid attempt identity was uploaded")
	}
}

func TestS3SourceArchiveStagerReturnsSanitizedUploadError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &captureSourceArchivePut{err: errors.New("credential-bearing S3 error")}
	stager, err := NewS3SourceArchiveStager(client, "hako-source-example", "", "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1"}
	_, err = stager.StageCheckout(context.Background(), id, root)
	if err == nil || strings.Contains(err.Error(), "credential-bearing") || !strings.Contains(err.Error(), "source archive upload failed") {
		t.Fatalf("S3 error was not sanitized: %v", err)
	}
}

func TestS3SourceArchiveReaderBindsBucketPrefixIdentityGenerationAndDigest(t *testing.T) {
	data := []byte("content-addressed source archive")
	digestValue := sha256.Sum256(data)
	digest := hex.EncodeToString(digestValue[:])
	body := &closeTrackingReader{Reader: bytes.NewReader(data)}
	client := &captureSourceArchiveGet{output: &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(int64(len(data))), ContentType: aws.String("application/x-tar"),
		Metadata: map[string]string{"hako-tenant-id": "tenant_1", "hako-workload-run-id": "run_1", "hako-attempt-id": "attempt_1",
			"hako-attempt-generation": "4", "hako-source-sha256": digest, "hako-source-format": "tar-v1"}}}
	reader, err := NewS3SourceArchiveReader(client, "hako-source-example", "hako/source", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 4, ResourcePlaneID: "rp_1"}
	archive, err := reader.OpenSourceArchive(context.Background(), id, "s3://hako-source-example/hako/source/tenant_1/run_1/attempt_1/", digest)
	if err != nil || archive.Size != int64(len(data)) || archive.Body != body || aws.ToString(client.input.Key) != "hako/source/tenant_1/run_1/attempt_1/4/source/"+digest+".tar" {
		t.Fatalf("source object was not read by exact immutable key: archive=%+v key=%q err=%v", archive, aws.ToString(client.input.Key), err)
	}
	_ = archive.Body.Close()
	if !body.closed {
		t.Fatal("source body was not closed by caller")
	}
	if _, err := reader.OpenSourceArchive(context.Background(), id, "s3://other-bucket/hako/source/tenant_1/run_1/attempt_1/", digest); err == nil || client.calls != 1 {
		t.Fatal("foreign artifact prefix reached S3")
	}
}

func TestS3SourceArchiveReaderRejectsMetadataMismatchAndOversizedObjects(t *testing.T) {
	data := []byte("not trusted until digest-verified")
	digestValue := sha256.Sum256(data)
	digest := hex.EncodeToString(digestValue[:])
	for _, tc := range []struct {
		name     string
		length   int64
		metadata map[string]string
	}{
		{name: "wrong attempt", length: int64(len(data)), metadata: map[string]string{"hako-tenant-id": "tenant_1", "hako-workload-run-id": "run_1", "hako-attempt-id": "other", "hako-attempt-generation": "1", "hako-source-sha256": digest, "hako-source-format": "tar-v1"}},
		{name: "oversized", length: 1<<20 + 1, metadata: map[string]string{"hako-tenant-id": "tenant_1", "hako-workload-run-id": "run_1", "hako-attempt-id": "attempt_1", "hako-attempt-generation": "1", "hako-source-sha256": digest, "hako-source-format": "tar-v1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &closeTrackingReader{Reader: bytes.NewReader(data)}
			client := &captureSourceArchiveGet{output: &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(tc.length), ContentType: aws.String("application/x-tar"), Metadata: tc.metadata}}
			reader, err := NewS3SourceArchiveReader(client, "hako-source-example", "", 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1"}
			if _, err := reader.OpenSourceArchive(context.Background(), id, "s3://hako-source-example/tenant_1/run_1/attempt_1/", digest); err == nil || !body.closed {
				t.Fatalf("invalid S3 object accepted or not closed: err=%v closed=%v", err, body.closed)
			}
		})
	}
}
