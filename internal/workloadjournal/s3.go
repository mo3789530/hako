package workloadjournal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type s3API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type s3Lister interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type S3Store struct {
	client s3API
	bucket string
	prefix string
}

func NewS3Store(client s3API, bucket, prefix string) (*S3Store, error) {
	if client == nil || strings.TrimSpace(bucket) == "" {
		return nil, errors.New("S3 client and Workload journal bucket are required")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if strings.Contains(prefix, "..") || strings.ContainsAny(prefix, "\\\x00") {
		return nil, errors.New("Workload journal prefix is invalid")
	}
	return &S3Store{client: client, bucket: strings.TrimSpace(bucket), prefix: prefix}, nil
}

func (s *S3Store) Create(ctx context.Context, journal Journal) (Snapshot, error) {
	if journal.Revision != 1 || journal.ExecutionState != Accepted || journal.CleanupState != CleanupNotStarted {
		return Snapshot{}, errors.New("new Workload journal must start at revision 1 in accepted state")
	}
	body, err := Encode(journal)
	if err != nil {
		return Snapshot{}, err
	}
	key, err := s.key(journal.TenantID, journal.WorkloadRunID, journal.AttemptID)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key,
		Body: strings.NewReader(string(body)), ContentType: stringPtr("application/json"), IfNoneMatch: stringPtr("*")})
	if err != nil {
		return Snapshot{}, mapConditionalError(err)
	}
	if output == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		return Snapshot{}, errors.New("S3 did not return a Workload journal ETag")
	}
	return Snapshot{Journal: journal, ETag: *output.ETag}, nil
}

func (s *S3Store) Get(ctx context.Context, tenantID, runID, attemptID string) (Snapshot, error) {
	key, err := s.key(tenantID, runID, attemptID)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var apiError smithy.APIError
		if errors.As(err, &apiError) && apiError.ErrorCode() == "NoSuchKey" {
			return Snapshot{}, ErrNotFound
		}
		return Snapshot{}, fmt.Errorf("read Workload attempt journal: %w", err)
	}
	if output == nil || output.Body == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		return Snapshot{}, errors.New("S3 Workload journal response is incomplete")
	}
	defer output.Body.Close()
	data, err := io.ReadAll(io.LimitReader(output.Body, (64<<10)+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read Workload journal body: %w", err)
	}
	journal, err := Decode(data)
	if err != nil {
		return Snapshot{}, err
	}
	if journal.TenantID != tenantID || journal.WorkloadRunID != runID || journal.AttemptID != attemptID {
		return Snapshot{}, errors.New("S3 Workload journal key does not match its payload")
	}
	return Snapshot{Journal: journal, ETag: *output.ETag}, nil
}

func (s *S3Store) CompareAndSwap(ctx context.Context, current Snapshot, next Journal) (Snapshot, error) {
	if err := ValidateUpdate(current, next); err != nil {
		return Snapshot{}, err
	}
	body, err := Encode(next)
	if err != nil {
		return Snapshot{}, err
	}
	key, err := s.key(next.TenantID, next.WorkloadRunID, next.AttemptID)
	if err != nil {
		return Snapshot{}, err
	}
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key,
		Body: strings.NewReader(string(body)), ContentType: stringPtr("application/json"), IfMatch: &current.ETag})
	if err != nil {
		return Snapshot{}, mapConditionalError(err)
	}
	if output == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		return Snapshot{}, errors.New("S3 did not return a Workload journal ETag")
	}
	return Snapshot{Journal: next, ETag: *output.ETag}, nil
}

// ListRecoveryPage returns a bounded page of valid attempt journals that may
// need reconciliation. Listing is deliberately available only below an
// explicit journal prefix, so a recovery worker cannot scan unrelated bucket
// objects. Callers should start a fresh scan on each scheduled invocation;
// continuation tokens are for pagination within one bounded invocation.
func (s *S3Store) ListRecoveryPage(ctx context.Context, continuationToken string, limit int32) ([]Snapshot, string, error) {
	lister, ok := s.client.(s3Lister)
	if !ok || s.prefix == "" || limit < 1 || limit > 1000 || len(continuationToken) > 4096 {
		return nil, "", errors.New("Workload journal recovery listing is not configured")
	}
	prefix := s.prefix + "/"
	input := &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix, MaxKeys: &limit}
	if continuationToken != "" {
		input.ContinuationToken = &continuationToken
	}
	output, err := lister.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, "", fmt.Errorf("list Workload recovery journals: %w", err)
	}
	if output == nil {
		return nil, "", errors.New("S3 returned an empty Workload recovery listing")
	}
	items := make([]Snapshot, 0, len(output.Contents))
	for _, object := range output.Contents {
		if object.Key == nil {
			return nil, "", errors.New("S3 returned a Workload journal without a key")
		}
		tenantID, runID, attemptID, err := s.identityFromKey(*object.Key)
		if err != nil {
			return nil, "", err
		}
		snapshot, err := s.Get(ctx, tenantID, runID, attemptID)
		if err != nil {
			return nil, "", err
		}
		if recoverable(snapshot.Journal) {
			items = append(items, snapshot)
		}
	}
	var next string
	if aws.ToBool(output.IsTruncated) {
		if output.NextContinuationToken == nil || *output.NextContinuationToken == "" {
			return nil, "", errors.New("truncated S3 Workload recovery listing has no continuation token")
		}
		next = *output.NextContinuationToken
	}
	return items, next, nil
}

func (s *S3Store) identityFromKey(key string) (string, string, string, error) {
	prefix := s.prefix + "/"
	if !strings.HasPrefix(key, prefix) {
		return "", "", "", errors.New("S3 Workload recovery key is outside the configured prefix")
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".json") {
		return "", "", "", errors.New("S3 returned a malformed Workload recovery key")
	}
	attemptID := strings.TrimSuffix(parts[2], ".json")
	if _, err := s.key(parts[0], parts[1], attemptID); err != nil {
		return "", "", "", err
	}
	return parts[0], parts[1], attemptID, nil
}

func recoverable(j Journal) bool {
	if !terminal(j.ExecutionState) {
		return true
	}
	return !j.ResultPublished || j.CleanupState != CleanupConfirmed
}

func (s *S3Store) key(tenantID, runID, attemptID string) (string, error) {
	if !idPattern.MatchString(tenantID) || !idPattern.MatchString(runID) || !idPattern.MatchString(attemptID) {
		return "", errors.New("Workload journal identity is invalid")
	}
	key := tenantID + "/" + runID + "/" + attemptID + ".json"
	if s.prefix != "" {
		key = s.prefix + "/" + key
	}
	return key, nil
}

func mapConditionalError(err error) error {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return fmt.Errorf("%w: %s", ErrConflict, apiError.ErrorCode())
		}
	}
	return fmt.Errorf("write Workload attempt journal: %w", err)
}

func stringPtr(value string) *string { return &value }
