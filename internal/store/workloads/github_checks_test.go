package workloads

import (
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestGitHubCheckStateMapsWorkloadRunStates(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		state      domain.WorkloadRunState
		status     string
		conclusion string
	}{
		{domain.WorkloadPending, "queued", ""},
		{domain.WorkloadQueued, "queued", ""},
		{domain.WorkloadProvisioning, "in_progress", ""},
		{domain.WorkloadRunning, "in_progress", ""},
		{domain.WorkloadSucceeded, "completed", "success"},
		{domain.WorkloadFailed, "completed", "failure"},
		{domain.WorkloadCancelled, "completed", "cancelled"},
		{domain.WorkloadTimedOut, "completed", "timed_out"},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			run := domain.WorkloadRun{Kind: domain.WorkloadJob, State: test.state, CreatedAt: now}
			if test.status == "completed" {
				run.CompletedAt = &now
			}
			state, err := githubCheckStateForRun(run)
			if err != nil || state.status != test.status || state.conclusion != test.conclusion {
				t.Fatalf("state mapping = %+v, %v; want status=%q conclusion=%q", state, err, test.status, test.conclusion)
			}
		})
	}
	if _, err := githubCheckStateForRun(domain.WorkloadRun{State: "unknown"}); err == nil {
		t.Fatal("unsupported Run state was mapped to a GitHub Check")
	}
}
