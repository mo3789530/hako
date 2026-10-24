package previews

import (
	"context"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestCleanupRetryDelayIsExponentialAndCapped(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 3, want: 20 * time.Second},
		{attempt: 10, want: 2560 * time.Second},
		{attempt: 11, want: time.Hour},
		{attempt: 100, want: time.Hour},
	}
	for _, test := range tests {
		if got := cleanupRetryDelay(test.attempt); got != test.want {
			t.Errorf("cleanupRetryDelay(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func TestCleanupErrorCodeRejectsUnboundedProviderError(t *testing.T) {
	for _, code := range []string{"provider_unavailable", "microvm_terminate_failed", "gateway_route_remove_failed", "preview_binding_missing"} {
		if !validCleanupErrorCode(code) {
			t.Errorf("rejected fixed cleanup error code %q", code)
		}
	}
	if validCleanupErrorCode("provider timeout: secret=do-not-log") {
		t.Fatal("accepted raw provider error as a persisted cleanup category")
	}
}

func TestCleanupInputValidation(t *testing.T) {
	valid := CleanupJob{CleanupIdentity: CleanupIdentity{TenantID: "tenant_a", InstallationID: 1, RepositoryID: 2, PullRequestNumber: 3},
		ClaimID: "claim_12345678", LeaseExpiresAt: time.Now().Add(time.Minute), Attempt: 1}
	if _, _, err := RecordCleanupFailure(context.Background(), nil, valid, time.Now(), "provider_unavailable"); err == nil {
		t.Fatal("RecordCleanupFailure accepted a nil transaction")
	}
	if _, err := MarkCleaned(context.Background(), nil, valid, time.Now()); err == nil {
		t.Fatal("MarkCleaned accepted a nil transaction")
	}
	if _, _, err := RecordCleanupFailure(context.Background(), nil, valid, time.Now(), "provider timeout: secret=do-not-store"); err == nil {
		t.Fatal("RecordCleanupFailure accepted an unbounded failure category")
	}
}

func TestCleanupIdentityValidationRejectsUntrustedOrIncompleteValues(t *testing.T) {
	valid := CleanupIdentity{TenantID: domain.TenantID("tenant_a"), InstallationID: 1, RepositoryID: 2, PullRequestNumber: 3}
	if !validCleanupIdentity(valid) {
		t.Fatal("valid cleanup identity was rejected")
	}
	for _, test := range []CleanupIdentity{
		{}, {TenantID: "tenant_a", InstallationID: 1, RepositoryID: 2},
		{TenantID: "tenant_a", InstallationID: 0, RepositoryID: 2, PullRequestNumber: 3},
		{TenantID: "tenant_a", InstallationID: 1, RepositoryID: -1, PullRequestNumber: 3},
		{TenantID: "tenant_a", InstallationID: 1, RepositoryID: 2, PullRequestNumber: 0},
	} {
		if validCleanupIdentity(test) {
			t.Errorf("accepted incomplete cleanup identity %+v", test)
		}
	}
}

func TestCleanupJobValidationFencesClaimIdentity(t *testing.T) {
	valid := CleanupJob{CleanupIdentity: CleanupIdentity{TenantID: "tenant_a", InstallationID: 1, RepositoryID: 2, PullRequestNumber: 3},
		ClaimID: "claim_12345678", LeaseExpiresAt: time.Now().Add(time.Minute), Attempt: 1}
	if !validCleanupJob(valid) {
		t.Fatal("valid claimed cleanup job was rejected")
	}
	for _, mutate := range []func(*CleanupJob){
		func(job *CleanupJob) { job.ClaimID = "short" },
		func(job *CleanupJob) { job.ClaimID = "claim/with/slash" },
		func(job *CleanupJob) { job.LeaseExpiresAt = time.Time{} },
		func(job *CleanupJob) { job.Attempt = 0 },
		func(job *CleanupJob) { job.CleanupIdentity.TenantID = "" },
	} {
		invalid := valid
		mutate(&invalid)
		if validCleanupJob(invalid) {
			t.Errorf("accepted invalid claimed cleanup job %+v", invalid)
		}
	}
}
