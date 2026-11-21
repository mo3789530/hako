package workloadjobs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mo3789530/hako/internal/domain"
)

var (
	s3BucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3PathPartPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	artifactIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type S3PutObjectAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3ArtifactWriter stores redacted logs under deterministic, Tenant-scoped
// keys. Credentials are supplied by the AWS SDK default credential chain;
// callers should use the workload's IAM role, not static keys.
type S3ArtifactWriter struct {
	client S3PutObjectAPI
	bucket string
	prefix string
	kmsKey string
}

func NewS3ArtifactWriter(client S3PutObjectAPI, bucket, prefix, kmsKeyID string) (*S3ArtifactWriter, error) {
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if client == nil || !s3BucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") {
		return nil, errors.New("S3 client and valid artifact bucket are required")
	}
	if prefix != "" && (!s3PathPartPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.Contains(prefix, "//")) {
		return nil, errors.New("artifact prefix contains an unsafe path")
	}
	if strings.ContainsAny(kmsKeyID, "\r\n\x00") {
		return nil, errors.New("KMS key ID is invalid")
	}
	return &S3ArtifactWriter{client: client, bucket: bucket, prefix: prefix, kmsKey: strings.TrimSpace(kmsKeyID)}, nil
}

func (w *S3ArtifactWriter) PutRedactedLogs(ctx context.Context, tenantID domain.TenantID, runID domain.WorkloadRunID, logs []byte, digest string) (string, error) {
	if w == nil || w.client == nil || ctx == nil || !artifactIDPattern.MatchString(string(tenantID)) ||
		!artifactIDPattern.MatchString(string(runID)) || !sha256Pattern.MatchString(digest) {
		return "", errors.New("S3 writer, context, Tenant, Workload Run, and lowercase SHA-256 are required")
	}
	path := fmt.Sprintf("%s/%s/logs/%s.txt", tenantID, runID, digest)
	if w.prefix != "" {
		path = w.prefix + "/" + path
	}
	decodedDigest, _ := hex.DecodeString(digest)
	input := &s3.PutObjectInput{
		Bucket: aws.String(w.bucket), Key: aws.String(path), Body: bytes.NewReader(logs),
		ContentLength: aws.Int64(int64(len(logs))), ContentType: aws.String("text/plain; charset=utf-8"),
		ChecksumSHA256:       aws.String(base64.StdEncoding.EncodeToString(decodedDigest)),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
		Metadata:             map[string]string{"hako-tenant-id": string(tenantID), "hako-workload-run-id": string(runID), "hako-sha256": digest, "hako-redacted": "true"},
	}
	if w.kmsKey != "" {
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(w.kmsKey)
	}
	if _, err := w.client.PutObject(ctx, input); err != nil {
		return "", fmt.Errorf("put redacted Workload logs to S3: %w", err)
	}
	return "s3://" + w.bucket + "/" + path, nil
}
