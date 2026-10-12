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

var _ AttemptLogWriter = (*S3AttemptLogWriter)(nil)
