// Package artifactretention deletes expired Workload Artifact objects from a
// single configured S3 bucket/prefix before releasing their metadata quota.
package artifactretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/domain"
)

const MaxBatchSize = 1000

var (
	ErrListExpiredArtifacts = errors.New("Workload artifact cleanup listing failed")
	ErrInvalidArtifactBatch = errors.New("Workload artifact cleanup listing exceeded its configured bound")
	ErrArtifactCleanupItems = errors.New("one or more Workload artifact cleanup items failed")
)

type ArtifactStore interface {
	// These operations are a Control Plane API contract. A Resource Plane
	// implementation must not connect to the Control Plane DSQL database.
	ListExpired(context.Context, time.Time, int) ([]domain.WorkloadArtifact, error)
	DeleteExpired(context.Context, domain.WorkloadArtifact, time.Time) (bool, error)
}

type ObjectDeleter interface {
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

type Config struct {
	Bucket    string
	KeyPrefix string
	BatchSize int
	Logger    *slog.Logger
}

type Worker struct {
	store   ArtifactStore
	objects ObjectDeleter
	bucket  string
	prefix  string
	limit   int
	logger  *slog.Logger
}

type Report struct {
	Candidates int
	Deleted    int
	Retained   int
	Failures   int
}

func New(store ArtifactStore, objects ObjectDeleter, config Config) (*Worker, error) {
	if store == nil || objects == nil {
		return nil, errors.New("artifact store and S3 object deleter are required")
	}
	if strings.TrimSpace(config.Bucket) == "" || strings.ContainsAny(config.Bucket, "/:") {
		return nil, errors.New("artifact cleanup bucket is invalid")
	}
	if !validPrefix(config.KeyPrefix) {
		return nil, errors.New("artifact cleanup key prefix must be a clean, non-root prefix ending in slash")
	}
	if config.BatchSize < 1 || config.BatchSize > MaxBatchSize {
		return nil, fmt.Errorf("artifact cleanup batch size must be between 1 and %d", MaxBatchSize)
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Worker{store: store, objects: objects, bucket: config.Bucket, prefix: config.KeyPrefix, limit: config.BatchSize, logger: config.Logger}, nil
}

func (w *Worker) RunOnce(ctx context.Context, now time.Time) (Report, error) {
	if w == nil || ctx == nil || now.IsZero() {
		return Report{}, errors.New("artifact cleanup worker, context, and time are required")
	}
	artifacts, err := w.store.ListExpired(ctx, now.UTC(), w.limit)
	if err != nil {
		w.loggerOrDefault().ErrorContext(ctx, "Workload artifact cleanup failed", "failure_class", "list_expired_artifacts")
		return Report{}, ErrListExpiredArtifacts
	}
	if len(artifacts) > w.limit {
		return Report{}, ErrInvalidArtifactBatch
	}
	report := Report{Candidates: len(artifacts)}
	failed := false
	for _, artifact := range artifacts {
		bucket, key, err := w.objectLocation(artifact.StorageRef)
		if err != nil {
			report.Failures++
			failed = true
			w.logArtifactFailure(ctx, artifact, "invalid_storage_reference")
			continue
		}
		if _, err := w.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}); err != nil {
			report.Failures++
			failed = true
			w.logArtifactFailure(ctx, artifact, "delete_object")
			continue
		}
		removed, err := w.store.DeleteExpired(ctx, artifact, now.UTC())
		if err != nil {
			report.Failures++
			failed = true
			w.logArtifactFailure(ctx, artifact, "release_metadata")
			continue
		}
		if removed {
			report.Deleted++
		} else {
			// A concurrent cleanup may already have removed this row, or the
			// candidate may no longer match its persisted expiry/reference.
			report.Retained++
		}
	}
	if failed {
		return report, ErrArtifactCleanupItems
	}
	return report, nil
}

func (w *Worker) logArtifactFailure(ctx context.Context, artifact domain.WorkloadArtifact, failureClass string) {
	attrs := []any{"failure_class", failureClass, "artifact_id", artifact.ID, "workload_run_id", artifact.RunID}
	if artifact.CorrelationID != "" {
		attrs = append(attrs, "correlation_id", artifact.CorrelationID)
	}
	w.loggerOrDefault().ErrorContext(ctx, "Workload artifact cleanup item failed", attrs...)
}

func (w *Worker) loggerOrDefault() *slog.Logger {
	if w.logger != nil {
		return w.logger
	}
	return slog.Default()
}

func (w *Worker) objectLocation(reference string) (string, string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "s3" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", "", errors.New("invalid S3 reference")
	}
	key := strings.TrimPrefix(parsed.EscapedPath(), "/")
	decoded, err := url.PathUnescape(key)
	if err != nil || decoded == "" || strings.Contains(decoded, "\\") || path.Clean(decoded) != decoded || strings.HasPrefix(decoded, "../") || decoded == ".." {
		return "", "", errors.New("invalid S3 object key")
	}
	if parsed.Host != w.bucket || !strings.HasPrefix(decoded, w.prefix) {
		return "", "", errors.New("S3 object outside configured scope")
	}
	return w.bucket, decoded, nil
}

func validPrefix(prefix string) bool {
	if prefix == "" || prefix == "/" || !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "\\") || strings.ContainsAny(prefix, "?#%") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
