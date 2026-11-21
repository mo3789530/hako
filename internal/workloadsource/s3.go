package workloadsource

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

var (
	sourceBucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	sourcePrefixPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	sourceIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

type S3SourceArchivePutAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type S3SourceArchiveGetAPI interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type S3SourceArchiveStager struct {
	client   S3SourceArchivePutAPI
	bucket   string
	prefix   string
	kmsKey   string
	maxBytes int64
}

func NewS3SourceArchiveStager(client S3SourceArchivePutAPI, bucket, prefix, kmsKeyID string, maxBytes int64) (*S3SourceArchiveStager, error) {
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	kmsKeyID = strings.TrimSpace(kmsKeyID)
	if client == nil || !sourceBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") ||
		(prefix != "" && (!sourcePrefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.Contains(prefix, "//"))) ||
		strings.ContainsAny(kmsKeyID, "\r\n\x00") || maxBytes < 0 || maxBytes > DefaultMaxArchiveBytes {
		return nil, errors.New("S3 source stager configuration is invalid")
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxArchiveBytes
	}
	return &S3SourceArchiveStager{client: client, bucket: bucket, prefix: prefix, kmsKey: kmsKeyID, maxBytes: maxBytes}, nil
}

// StageCheckout stores a deterministic, credential-free tar archive beneath
// the exact Tenant/Run/attempt prefix using server-side encryption and the
// archive's SHA-256 as both key component and S3 checksum.
func (s *S3SourceArchiveStager) StageCheckout(ctx context.Context, id workloadjournal.Command, checkoutPath string) (MaterializedSource, error) {
	if s == nil || s.client == nil || ctx == nil || !validSourceIdentity(id) || strings.TrimSpace(checkoutPath) == "" {
		return MaterializedSource{}, ErrArchive
	}
	archive, err := CreateArchive(ctx, checkoutPath, s.maxBytes)
	if err != nil {
		return MaterializedSource{}, ErrArchive
	}
	defer archive.Close()
	path := fmt.Sprintf("%s/%s/%s/%d/source/%s.tar", id.TenantID, id.WorkloadRunID, id.AttemptID, id.Generation, archive.SHA256())
	// The artifact prefix intentionally stops at the Attempt. Logs and source
	// objects can then share the same Tenant/Run/attempt IAM boundary.
	attemptPrefix := fmt.Sprintf("%s/%s/%s/", id.TenantID, id.WorkloadRunID, id.AttemptID)
	if s.prefix != "" {
		path = s.prefix + "/" + path
		attemptPrefix = s.prefix + "/" + attemptPrefix
	}
	checksum, err := base64SHA256(archive.SHA256())
	if err != nil {
		return MaterializedSource{}, ErrArchive
	}
	input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(path), Body: archive.Reader(), ContentLength: aws.Int64(archive.Size()),
		ContentType: aws.String("application/x-tar"), ChecksumSHA256: aws.String(checksum), ServerSideEncryption: types.ServerSideEncryptionAes256,
		Metadata: map[string]string{"hako-tenant-id": id.TenantID, "hako-workload-run-id": id.WorkloadRunID,
			"hako-attempt-id": id.AttemptID, "hako-attempt-generation": fmt.Sprintf("%d", id.Generation),
			"hako-source-sha256": archive.SHA256(), "hako-source-format": "tar-v1"}}
	if s.kmsKey != "" {
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(s.kmsKey)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return MaterializedSource{}, errors.New("source archive upload failed")
	}
	return MaterializedSource{Digest: archive.SHA256(), ArtifactPrefix: "s3://" + s.bucket + "/" + attemptPrefix}, nil
}

func validSourceIdentity(id workloadjournal.Command) bool {
	return sourceIdentityPattern.MatchString(id.TenantID) && sourceIdentityPattern.MatchString(id.WorkloadRunID) &&
		sourceIdentityPattern.MatchString(id.AttemptID) && sourceIdentityPattern.MatchString(id.ResourcePlaneID) && id.Generation > 0
}

func base64SHA256(hexDigest string) (string, error) {
	if len(hexDigest) != 64 {
		return "", ErrArchive
	}
	data := make([]byte, 32)
	for index := range data {
		var value byte
		for _, char := range hexDigest[index*2 : index*2+2] {
			digit, ok := unhex(byte(char))
			if !ok {
				return "", ErrArchive
			}
			value = value<<4 | digit
		}
		data[index] = value
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func unhex(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	default:
		return 0, false
	}
}

type S3SourceArchiveReader struct {
	client   S3SourceArchiveGetAPI
	bucket   string
	prefix   string
	maxBytes int64
}

const MaxAgentProposalArchiveBytes = 8 << 20

type AgentProposalArchiveIdentity struct {
	TenantID      string
	WorkloadRunID string
	AttemptID     string
	SHA256        string
}

// AgentProposalArchiveReader is deliberately identity-based; callers cannot
// supply an arbitrary S3 bucket, key, or storage reference.
type AgentProposalArchiveReader interface {
	ReadAgentProposalArchive(context.Context, AgentProposalArchiveIdentity) ([]byte, error)
}

type S3AgentProposalArchiveReader struct {
	client S3SourceArchiveGetAPI
	bucket string
	prefix string
}

func NewS3AgentProposalArchiveReader(client S3SourceArchiveGetAPI, bucket, prefix string) (*S3AgentProposalArchiveReader, error) {
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if client == nil || !sourceBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") ||
		(prefix != "" && (!sourcePrefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.Contains(prefix, "//"))) {
		return nil, errors.New("S3 Agent proposal reader configuration is invalid")
	}
	return &S3AgentProposalArchiveReader{client: client, bucket: bucket, prefix: prefix}, nil
}

func (r *S3AgentProposalArchiveReader) ReadAgentProposalArchive(ctx context.Context, identity AgentProposalArchiveIdentity) ([]byte, error) {
	if r == nil || r.client == nil || ctx == nil || !sourceIdentityPattern.MatchString(identity.TenantID) ||
		!sourceIdentityPattern.MatchString(identity.WorkloadRunID) || !sourceIdentityPattern.MatchString(identity.AttemptID) ||
		!sourceDigestPattern.MatchString(identity.SHA256) {
		return nil, ErrArchive
	}
	key := fmt.Sprintf("%s/%s/%s/agent-changes/%s.zip", identity.TenantID, identity.WorkloadRunID, identity.AttemptID, identity.SHA256)
	if r.prefix != "" {
		key = r.prefix + "/" + key
	}
	output, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil || output == nil || output.Body == nil {
		return nil, errors.New("Agent proposal archive unavailable")
	}
	defer output.Body.Close()
	if output.ContentLength == nil || aws.ToInt64(output.ContentLength) < 1 || aws.ToInt64(output.ContentLength) > MaxAgentProposalArchiveBytes ||
		aws.ToString(output.ContentType) != "application/zip" || output.Metadata["hako-tenant-id"] != identity.TenantID ||
		output.Metadata["hako-workload-run-id"] != identity.WorkloadRunID || output.Metadata["hako-attempt-id"] != identity.AttemptID ||
		output.Metadata["hako-sha256"] != identity.SHA256 || output.Metadata["hako-artifact-kind"] != "agent-changes" {
		return nil, ErrArchive
	}
	data, err := io.ReadAll(io.LimitReader(output.Body, MaxAgentProposalArchiveBytes+1))
	if err != nil || int64(len(data)) != aws.ToInt64(output.ContentLength) || len(data) > MaxAgentProposalArchiveBytes {
		clear(data)
		return nil, ErrArchive
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != identity.SHA256 {
		clear(data)
		return nil, ErrArchive
	}
	return data, nil
}

func NewS3SourceArchiveReader(client S3SourceArchiveGetAPI, bucket, prefix string, maxBytes int64) (*S3SourceArchiveReader, error) {
	bucket = strings.TrimSpace(bucket)
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if client == nil || !sourceBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") ||
		(prefix != "" && (!sourcePrefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.Contains(prefix, "//"))) ||
		maxBytes < 0 || maxBytes > DefaultMaxArchiveBytes {
		return nil, errors.New("S3 source reader configuration is invalid")
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxArchiveBytes
	}
	return &S3SourceArchiveReader{client: client, bucket: bucket, prefix: prefix, maxBytes: maxBytes}, nil
}

func (r *S3SourceArchiveReader) OpenSourceArchive(ctx context.Context, id workloadjournal.Command, artifactPrefix, digest string) (SourceArchive, error) {
	if r == nil || r.client == nil || ctx == nil || !validSourceIdentity(id) || !sourceDigestPattern.MatchString(digest) {
		return SourceArchive{}, ErrArchive
	}
	base := fmt.Sprintf("%s/%s/%s/", id.TenantID, id.WorkloadRunID, id.AttemptID)
	if r.prefix != "" {
		base = r.prefix + "/" + base
	}
	expectedPrefix := "s3://" + r.bucket + "/" + base
	if artifactPrefix != expectedPrefix {
		return SourceArchive{}, ErrArchive
	}
	key := fmt.Sprintf("%s/%s/%s/%d/source/%s.tar", id.TenantID, id.WorkloadRunID, id.AttemptID, id.Generation, digest)
	if r.prefix != "" {
		key = r.prefix + "/" + key
	}
	output, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(key)})
	if err != nil || output == nil || output.Body == nil {
		return SourceArchive{}, errors.New("source archive unavailable")
	}
	if output.ContentLength == nil || aws.ToInt64(output.ContentLength) < 1 || aws.ToInt64(output.ContentLength) > r.maxBytes ||
		aws.ToString(output.ContentType) != "application/x-tar" || output.Metadata["hako-tenant-id"] != id.TenantID ||
		output.Metadata["hako-workload-run-id"] != id.WorkloadRunID || output.Metadata["hako-attempt-id"] != id.AttemptID ||
		output.Metadata["hako-attempt-generation"] != fmt.Sprintf("%d", id.Generation) || output.Metadata["hako-source-sha256"] != digest ||
		output.Metadata["hako-source-format"] != "tar-v1" {
		_ = output.Body.Close()
		return SourceArchive{}, ErrArchive
	}
	return SourceArchive{Body: output.Body, Size: aws.ToInt64(output.ContentLength)}, nil
}
