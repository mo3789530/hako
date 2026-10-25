package workloadagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const maxAttemptLogBytes = 16 << 20

var (
	attemptArtifactBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	attemptArtifactPathPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	attemptArtifactDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type S3AttemptLogPutAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type AttemptChangeBundleWriter interface {
	PutAgentChangeBundle(context.Context, workloadjournal.Command, []byte, string) (workloadjobs.WorkloadChangeArtifact, error)
}

// S3AttemptLogWriter stores already-redacted Agent output beneath a key scoped
// to one immutable Workload attempt. The caller must redact before calling;
// this writer validates identity, size, digest, and storage configuration.
type S3AttemptLogWriter struct {
	client S3AttemptLogPutAPI
	bucket string
	prefix string
	kmsKey string
}

func NewS3AttemptLogWriter(client S3AttemptLogPutAPI, bucket, prefix, kmsKeyID string) (*S3AttemptLogWriter, error) {
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if client == nil || !attemptArtifactBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") ||
		strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") {
		return nil, errors.New("S3 client and valid attempt artifact bucket are required")
	}
	if prefix != "" && (!attemptArtifactPathPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.Contains(prefix, "//")) {
		return nil, errors.New("attempt artifact prefix contains an unsafe path")
	}
	if strings.ContainsAny(kmsKeyID, "\r\n\x00") {
		return nil, errors.New("attempt artifact KMS key ID is invalid")
	}
	return &S3AttemptLogWriter{client: client, bucket: bucket, prefix: prefix, kmsKey: strings.TrimSpace(kmsKeyID)}, nil
}

func (w *S3AttemptLogWriter) PutAttemptLogs(ctx context.Context, id workloadjournal.Command, logs []byte, digest string) (workloadjobs.WorkloadLogArtifact, error) {
	if w == nil || w.client == nil || ctx == nil || !validIdentity(id) || len(logs) > maxAttemptLogBytes ||
		!attemptArtifactDigestPattern.MatchString(digest) {
		return workloadjobs.WorkloadLogArtifact{}, errors.New("S3 writer, context, attempt identity, bounded logs, and lowercase SHA-256 are required")
	}
	actual := sha256.Sum256(logs)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), digest) {
		return workloadjobs.WorkloadLogArtifact{}, errors.New("attempt log digest does not match content")
	}
	path := fmt.Sprintf("%s/%s/%s/logs/%s.txt", id.TenantID, id.WorkloadRunID, id.AttemptID, digest)
	if w.prefix != "" {
		path = w.prefix + "/" + path
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(w.bucket), Key: aws.String(path), Body: bytes.NewReader(logs), ContentLength: aws.Int64(int64(len(logs))),
		ContentType: aws.String("text/plain; charset=utf-8"), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(actual[:])),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
		Metadata: map[string]string{
			"hako-tenant-id": id.TenantID, "hako-workload-run-id": id.WorkloadRunID,
			"hako-attempt-id": id.AttemptID, "hako-sha256": digest, "hako-redacted": "true",
		},
	}
	if w.kmsKey != "" {
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(w.kmsKey)
	}
	if _, err := w.client.PutObject(ctx, input); err != nil {
		return workloadjobs.WorkloadLogArtifact{}, errors.New("attempt log upload failed")
	}
	return workloadjobs.WorkloadLogArtifact{StorageRef: "s3://" + w.bucket + "/" + path,
		SizeBytes: int64(len(logs)), SHA256: digest, Redacted: true}, nil
}

// S3AttemptChangeBundleWriter stores an Agent proposal archive under a
// separate attempt-scoped key. The bundle is untrusted and is never labeled
// redacted; Hako grants no implicit GitHub write permission by storing it.
type S3AttemptChangeBundleWriter struct {
	client S3AttemptLogPutAPI
	bucket string
	prefix string
	kmsKey string
}

func NewS3AttemptChangeBundleWriter(client S3AttemptLogPutAPI, bucket, prefix, kmsKeyID string) (*S3AttemptChangeBundleWriter, error) {
	logs, err := NewS3AttemptLogWriter(client, bucket, prefix, kmsKeyID)
	if err != nil {
		return nil, err
	}
	return &S3AttemptChangeBundleWriter{client: logs.client, bucket: logs.bucket, prefix: logs.prefix, kmsKey: logs.kmsKey}, nil
}

func (w *S3AttemptChangeBundleWriter) PutAgentChangeBundle(ctx context.Context, id workloadjournal.Command, bundle []byte, digest string) (workloadjobs.WorkloadChangeArtifact, error) {
	if w == nil || w.client == nil || ctx == nil || !validIdentity(id) || len(bundle) < 1 || len(bundle) > maxAgentChangeBundleBytes ||
		!attemptArtifactDigestPattern.MatchString(digest) {
		return workloadjobs.WorkloadChangeArtifact{}, errors.New("S3 writer, context, attempt identity, bounded change bundle, and lowercase SHA-256 are required")
	}
	actual := sha256.Sum256(bundle)
	if hex.EncodeToString(actual[:]) != digest {
		return workloadjobs.WorkloadChangeArtifact{}, errors.New("Agent change bundle digest does not match content")
	}
	path := fmt.Sprintf("%s/%s/%s/agent-changes/%s.zip", id.TenantID, id.WorkloadRunID, id.AttemptID, digest)
	if w.prefix != "" {
		path = w.prefix + "/" + path
	}
	input := &s3.PutObjectInput{Bucket: aws.String(w.bucket), Key: aws.String(path), Body: bytes.NewReader(bundle),
		ContentLength: aws.Int64(int64(len(bundle))), ContentType: aws.String("application/zip"),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(actual[:])), ServerSideEncryption: types.ServerSideEncryptionAes256,
		Metadata: map[string]string{"hako-tenant-id": id.TenantID, "hako-workload-run-id": id.WorkloadRunID,
			"hako-attempt-id": id.AttemptID, "hako-sha256": digest, "hako-artifact-kind": "agent-changes"}}
	if w.kmsKey != "" {
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(w.kmsKey)
	}
	if _, err := w.client.PutObject(ctx, input); err != nil {
		return workloadjobs.WorkloadChangeArtifact{}, errors.New("Agent change bundle upload failed")
	}
	return workloadjobs.WorkloadChangeArtifact{StorageRef: "s3://" + w.bucket + "/" + path, SizeBytes: int64(len(bundle)), SHA256: digest}, nil
}

var _ AttemptLogWriter = (*S3AttemptLogWriter)(nil)
var _ AttemptChangeBundleWriter = (*S3AttemptChangeBundleWriter)(nil)
