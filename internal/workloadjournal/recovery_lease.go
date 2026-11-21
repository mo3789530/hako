package workloadjournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var (
	ErrRecoveryLeaseHeld = errors.New("Workload recovery sweep lease is held")
	ErrRecoveryLeaseLost = errors.New("Workload recovery sweep lease is no longer owned")
	leaseOwnerPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
)

type RecoveryLease struct {
	Owner     string
	ExpiresAt time.Time
	ETag      string
}

type leaseDocument struct {
	SchemaVersion int       `json:"schema_version"`
	Owner         string    `json:"owner"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// S3RecoveryLease uses conditional object writes as a distributed lease for a
// scheduled journal sweep. Configure an exact key separate from the journal
// listing prefix, and grant access only to that object ARN.
type S3RecoveryLease struct {
	client s3API
	bucket string
	key    string
}

func NewS3RecoveryLease(client s3API, bucket, key string) (*S3RecoveryLease, error) {
	if client == nil || strings.TrimSpace(bucket) == "" || strings.TrimSpace(key) == "" ||
		strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.ContainsAny(key, "\\\x00") {
		return nil, errors.New("S3 client, bucket, and exact recovery lease key are required")
	}
	return &S3RecoveryLease{client: client, bucket: strings.TrimSpace(bucket), key: key}, nil
}

func (s *S3RecoveryLease) Acquire(ctx context.Context, owner string, now time.Time, ttl time.Duration) (RecoveryLease, error) {
	if err := s.validate(ctx, owner, now, ttl); err != nil {
		return RecoveryLease{}, err
	}
	lease := leaseDocument{SchemaVersion: 1, Owner: owner, ExpiresAt: now.UTC().Add(ttl)}
	body, err := json.Marshal(lease)
	if err != nil {
		return RecoveryLease{}, err
	}
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &s.key,
		Body: bytes.NewReader(body), ContentType: stringPtr("application/json"), IfNoneMatch: stringPtr("*")})
	if err == nil {
		return leaseFromPut(lease, output)
	}
	if !isConditionalFailure(err) {
		return RecoveryLease{}, fmt.Errorf("create Workload recovery lease: %w", err)
	}
	current, err := s.get(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RecoveryLease{}, ErrConflict
		}
		return RecoveryLease{}, err
	}
	if current.ExpiresAt.After(now.UTC()) {
		return RecoveryLease{}, ErrRecoveryLeaseHeld
	}
	return s.replace(ctx, current, lease)
}

func (s *S3RecoveryLease) Renew(ctx context.Context, current RecoveryLease, now time.Time, ttl time.Duration) (RecoveryLease, error) {
	if err := s.validate(ctx, current.Owner, now, ttl); err != nil {
		return RecoveryLease{}, err
	}
	if current.ETag == "" || !current.ExpiresAt.After(now.UTC()) {
		return RecoveryLease{}, ErrRecoveryLeaseLost
	}
	stored, err := s.get(ctx)
	if err != nil {
		return RecoveryLease{}, err
	}
	if stored.Owner != current.Owner || stored.ETag != current.ETag || !stored.ExpiresAt.After(now.UTC()) {
		return RecoveryLease{}, ErrRecoveryLeaseLost
	}
	next := leaseDocument{SchemaVersion: 1, Owner: current.Owner, ExpiresAt: now.UTC().Add(ttl)}
	return s.replace(ctx, stored, next)
}

// Release expires the lease with an ETag-guarded update instead of deleting
// it. A stale owner therefore cannot release a lease acquired by a successor.
func (s *S3RecoveryLease) Release(ctx context.Context, current RecoveryLease, now time.Time) error {
	if ctx == nil || current.Owner == "" || current.ETag == "" || now.IsZero() {
		return ErrRecoveryLeaseLost
	}
	stored, err := s.get(ctx)
	if err != nil {
		return err
	}
	if stored.Owner != current.Owner || stored.ETag != current.ETag {
		return ErrRecoveryLeaseLost
	}
	next := leaseDocument{SchemaVersion: 1, Owner: current.Owner, ExpiresAt: now.UTC()}
	_, err = s.replace(ctx, stored, next)
	return err
}

func (s *S3RecoveryLease) validate(ctx context.Context, owner string, now time.Time, ttl time.Duration) error {
	if s == nil || ctx == nil || !leaseOwnerPattern.MatchString(owner) || now.IsZero() || ttl <= 0 || ttl > 30*time.Minute {
		return errors.New("Workload recovery lease parameters are invalid")
	}
	return ctx.Err()
}

func (s *S3RecoveryLease) replace(ctx context.Context, current RecoveryLease, next leaseDocument) (RecoveryLease, error) {
	body, err := json.Marshal(next)
	if err != nil {
		return RecoveryLease{}, err
	}
	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &s.key,
		Body: bytes.NewReader(body), ContentType: stringPtr("application/json"), IfMatch: &current.ETag})
	if err != nil {
		if isConditionalFailure(err) {
			return RecoveryLease{}, ErrRecoveryLeaseLost
		}
		return RecoveryLease{}, fmt.Errorf("update Workload recovery lease: %w", err)
	}
	return leaseFromPut(next, output)
}

func (s *S3RecoveryLease) get(ctx context.Context) (RecoveryLease, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &s.key})
	if err != nil {
		var apiError smithy.APIError
		if errors.As(err, &apiError) && apiError.ErrorCode() == "NoSuchKey" {
			return RecoveryLease{}, ErrNotFound
		}
		return RecoveryLease{}, fmt.Errorf("read Workload recovery lease: %w", err)
	}
	if output == nil || output.Body == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		return RecoveryLease{}, errors.New("S3 Workload recovery lease response is incomplete")
	}
	defer output.Body.Close()
	data, err := io.ReadAll(io.LimitReader(output.Body, 4097))
	if err != nil || len(data) > 4096 {
		return RecoveryLease{}, errors.New("S3 Workload recovery lease is unreadable")
	}
	var stored leaseDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&stored) != nil || decoder.Decode(new(any)) != io.EOF || stored.SchemaVersion != 1 ||
		!leaseOwnerPattern.MatchString(stored.Owner) || stored.ExpiresAt.IsZero() {
		return RecoveryLease{}, errors.New("S3 Workload recovery lease is invalid")
	}
	return RecoveryLease{Owner: stored.Owner, ExpiresAt: stored.ExpiresAt.UTC(), ETag: *output.ETag}, nil
}

func leaseFromPut(document leaseDocument, output *s3.PutObjectOutput) (RecoveryLease, error) {
	if output == nil || output.ETag == nil || strings.TrimSpace(*output.ETag) == "" {
		return RecoveryLease{}, errors.New("S3 did not return a Workload recovery lease ETag")
	}
	return RecoveryLease{Owner: document.Owner, ExpiresAt: document.ExpiresAt.UTC(), ETag: *output.ETag}, nil
}

func isConditionalFailure(err error) bool {
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.ErrorCode() {
	case "PreconditionFailed", "ConditionalRequestConflict":
		return true
	default:
		return false
	}
}
