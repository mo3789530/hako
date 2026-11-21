package artifactretention

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/domain"
)

type memoryStore struct {
	artifacts []domain.WorkloadArtifact
	listErr   error
	deleteErr error
	removed   []domain.WorkloadArtifact
}

func (s *memoryStore) ListExpired(_ context.Context, _ time.Time, limit int) ([]domain.WorkloadArtifact, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if len(s.artifacts) > limit {
		return s.artifacts[:limit], nil
	}
	return s.artifacts, nil
}

func (s *memoryStore) DeleteExpired(_ context.Context, artifact domain.WorkloadArtifact, _ time.Time) (bool, error) {
	if s.deleteErr != nil {
		return false, s.deleteErr
	}
	s.removed = append(s.removed, artifact)
	return true, nil
}

type memoryObjects struct {
	inputs []*s3.DeleteObjectInput
	err    error
}

func (s *memoryObjects) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	s.inputs = append(s.inputs, input)
	return &s3.DeleteObjectOutput{}, s.err
}

func expiredArtifact(id, ref string) domain.WorkloadArtifact {
	expires := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return domain.WorkloadArtifact{ID: id, TenantID: "tenant_test", RunID: "run_test", StorageRef: ref, ExpiresAt: &expires}
}

func TestWorkerDeletesObjectBeforeReleasingQuota(t *testing.T) {
	artifact := expiredArtifact("artifact_1", "s3://hako-artifacts/workloads/tenant/run/log.txt")
	store := &memoryStore{artifacts: []domain.WorkloadArtifact{artifact}}
	objects := &memoryObjects{}
	worker, err := New(store, objects, Config{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	report, err := worker.RunOnce(context.Background(), time.Now().UTC())
	if err != nil || report != (Report{Candidates: 1, Deleted: 1}) {
		t.Fatalf("cleanup report = %+v err=%v", report, err)
	}
	if len(objects.inputs) != 1 || *objects.inputs[0].Bucket != "hako-artifacts" || *objects.inputs[0].Key != "workloads/tenant/run/log.txt" {
		t.Fatalf("S3 delete request = %+v", objects.inputs)
	}
	if len(store.removed) != 1 || store.removed[0].ID != artifact.ID {
		t.Fatalf("metadata removed after object deletion = %+v", store.removed)
	}
}

func TestWorkerKeepsQuotaWhenObjectDeleteFails(t *testing.T) {
	artifact := expiredArtifact("artifact_1", "s3://hako-artifacts/workloads/log.txt")
	artifact.CorrelationID = "req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	store := &memoryStore{artifacts: []domain.WorkloadArtifact{artifact}}
	objects := &memoryObjects{err: errors.New("sensitive-provider-detail")}
	var logs bytes.Buffer
	worker, err := New(store, objects, Config{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 1,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	report, err := worker.RunOnce(context.Background(), time.Now())
	if err == nil || report.Failures != 1 || len(store.removed) != 0 {
		t.Fatalf("failed delete must retain metadata/quota: report=%+v err=%v removed=%+v", report, err, store.removed)
	}
	if !strings.Contains(logs.String(), "correlation_id=req_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") ||
		!strings.Contains(logs.String(), "workload_run_id=run_test") || strings.Contains(logs.String(), "sensitive-provider-detail") ||
		strings.Contains(logs.String(), "s3://") {
		t.Fatalf("artifact cleanup log must correlate without raw errors or storage refs: %s", logs.String())
	}
	if strings.Contains(err.Error(), "sensitive-provider-detail") || strings.Contains(err.Error(), artifact.ID) || strings.Contains(err.Error(), "s3://") {
		t.Fatalf("artifact cleanup return error leaked provider or artifact details: %v", err)
	}
}

func TestWorkerRejectsObjectsOutsideConfiguredScope(t *testing.T) {
	refs := []string{
		"s3://other-bucket/workloads/log.txt",
		"s3://hako-artifacts/other/log.txt",
		"s3://user@hako-artifacts/workloads/log.txt",
		"s3://hako-artifacts/workloads/../secret.txt",
		"s3://hako-artifacts/workloads/%2e%2e/secret.txt",
		"s3://hako-artifacts/workloads/log.txt?versionId=secret",
	}
	for index, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			store := &memoryStore{artifacts: []domain.WorkloadArtifact{expiredArtifact("artifact_1", ref)}}
			objects := &memoryObjects{}
			worker, err := New(store, objects, Config{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			report, err := worker.RunOnce(context.Background(), time.Now())
			if err == nil || report.Failures != 1 || len(objects.inputs) != 0 || len(store.removed) != 0 {
				t.Fatalf("out-of-scope ref %d was not retained: report=%+v err=%v deletes=%d metadata=%d", index, report, err, len(objects.inputs), len(store.removed))
			}
		})
	}
}

func TestWorkerDoesNotReleaseQuotaWhenMetadataFenceFails(t *testing.T) {
	store := &memoryStore{artifacts: []domain.WorkloadArtifact{expiredArtifact("artifact_1", "s3://hako-artifacts/workloads/log.txt")}, deleteErr: errors.New("sensitive-database-detail")}
	objects := &memoryObjects{}
	worker, err := New(store, objects, Config{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	report, err := worker.RunOnce(context.Background(), time.Now())
	if err == nil || report.Failures != 1 || len(objects.inputs) != 1 || len(store.removed) != 0 {
		t.Fatalf("metadata error result = %+v err=%v objects=%d removed=%d", report, err, len(objects.inputs), len(store.removed))
	}
	if strings.Contains(err.Error(), "sensitive-database-detail") || strings.Contains(err.Error(), "artifact_1") {
		t.Fatalf("artifact metadata error leaked provider or object identity: %v", err)
	}
}

func TestWorkerSanitizesListingErrorsAndRejectsOversizedBatch(t *testing.T) {
	listingWorker, err := New(&memoryStore{listErr: errors.New("sensitive-database-detail")}, &memoryObjects{}, Config{
		Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = listingWorker.RunOnce(context.Background(), time.Now())
	if !errors.Is(err, ErrListExpiredArtifacts) || strings.Contains(err.Error(), "sensitive-database-detail") {
		t.Fatalf("listing error was not sanitized: %v", err)
	}
	oversizedStore := oversizedArtifactStore{}
	oversizedWorker, err := New(oversizedStore, &memoryObjects{}, Config{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = oversizedWorker.RunOnce(context.Background(), time.Now())
	if !errors.Is(err, ErrInvalidArtifactBatch) {
		t.Fatalf("oversized listing should fail with a bounded class: %v", err)
	}
}

type oversizedArtifactStore struct{}

func (oversizedArtifactStore) ListExpired(context.Context, time.Time, int) ([]domain.WorkloadArtifact, error) {
	return []domain.WorkloadArtifact{{ID: "one"}, {ID: "two"}}, nil
}

func (oversizedArtifactStore) DeleteExpired(context.Context, domain.WorkloadArtifact, time.Time) (bool, error) {
	return false, nil
}

func TestNewRequiresNarrowConfiguredBucketPrefixAndBatch(t *testing.T) {
	store, objects := &memoryStore{}, &memoryObjects{}
	for _, config := range []Config{
		{Bucket: "hako-artifacts", KeyPrefix: "", BatchSize: 1},
		{Bucket: "hako-artifacts", KeyPrefix: "../", BatchSize: 1},
		{Bucket: "hako-artifacts", KeyPrefix: "workloads", BatchSize: 1},
		{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: MaxBatchSize + 1},
	} {
		if _, err := New(store, objects, config); err == nil {
			t.Errorf("unsafe cleanup config accepted: %+v", config)
		}
	}
	if _, err := New(nil, objects, Config{Bucket: "hako-artifacts", KeyPrefix: "workloads/", BatchSize: 1}); err == nil {
		t.Fatal("missing store accepted")
	}
}
