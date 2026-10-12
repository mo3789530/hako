package workloadcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type publishFunc func(context.Context, workloadjobs.WorkloadResult) error

func (p publishFunc) Publish(ctx context.Context, r workloadjobs.WorkloadResult) error {
	return p(ctx, r)
}

func successfulResult(t *testing.T, c *Controller) workloadjobs.WorkloadResult {
	t.Helper()
	cmd, err := Decode(envelope(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.resolver.Resolve(context.Background(), cmd.identity())
	if err != nil {
		t.Fatal(err)
	}
	return workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1",
		CommitSHA: s.CommitSHA, Status: domain.WorkloadSucceeded, CompletedAt: c.acceptor.now()}
}

func TestResultRetryUsesDurablePayload(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	r.result = successfulResult(t, c)
	sendErr := errors.New("queue unavailable")
	attempts := 0
	var sent []byte
	p := publishFunc(func(_ context.Context, result workloadjobs.WorkloadResult) error {
		attempts++
		b, _ := json.Marshal(result)
		if sent != nil && string(b) != string(sent) {
			t.Error("result changed on retry")
		}
		sent = b
		if attempts == 1 {
			return sendErr
		}
		return nil
	})
	s, err := c.Poll(context.Background(), data, p)
	if !errors.Is(err, sendErr) || s.Journal.ResultPublished || len(s.Journal.ResultPayload) == 0 || s.Journal.ExecutionState != workloadjournal.Succeeded {
		t.Fatalf("result checkpoint = %+v, %v", s, err)
	}
	// Recovery must not consult provider result or resolver after checkpoint.
	r.resultErr = errors.New("provider unavailable")
	c.resolver = resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) {
		return Specification{}, errors.New("resolver unavailable")
	})
	s, err = c.Poll(context.Background(), data, p)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Journal.ResultPublished || s.Journal.CleanupState != workloadjournal.CleanupConfirmed || len(r.handles) != 0 {
		t.Fatal("result/cleanup recovery failed")
	}
	if _, err := c.Poll(context.Background(), data, p); err != nil || attempts != 2 {
		t.Fatal("published result sent again")
	}
}

func TestPollUsesFrozenSpecificationAfterResolverRevoked(t *testing.T) {
	c, _, runtime, _ := setupController(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	runtime.result = successfulResult(t, c)
	c.resolver = resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) {
		return Specification{}, errors.New("repository binding revoked")
	})
	var published int
	report, err := c.Poll(context.Background(), data, publishFunc(func(context.Context, workloadjobs.WorkloadResult) error {
		published++
		return nil
	}))
	if err != nil || !report.Journal.ResultPublished || report.Journal.CleanupState != workloadjournal.CleanupConfirmed || published != 1 {
		t.Fatalf("frozen specification did not finish recovery after revocation: report=%+v published=%d err=%v", report, published, err)
	}
}

func TestCrashAfterSendReplaysSameResult(t *testing.T) {
	c, db, r, _ := setupController(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	r.result = successfulResult(t, c)
	writes := 0
	failure := errors.New("lost journal acknowledgement")
	p := publishFunc(func(context.Context, workloadjobs.WorkloadResult) error { writes++; db.casErr = failure; return nil })
	s, err := c.Poll(context.Background(), data, p)
	if !errors.Is(err, failure) || s.Journal.ResultPublished {
		t.Fatal("marked publication despite failed checkpoint")
	}
	db.casErr = nil
	p = publishFunc(func(context.Context, workloadjobs.WorkloadResult) error { writes++; return nil })
	if _, err := c.Poll(context.Background(), data, p); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatal("did not replay durable result")
	}
}

func TestCollectFailurePreservesRunning(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	r.result = successfulResult(t, c)
	r.resultErr = errors.New("artifact upload failed")
	s, err := c.Poll(context.Background(), data, publishFunc(func(context.Context, workloadjobs.WorkloadResult) error {
		t.Fatal("published failed artifact collection")
		return nil
	}))
	if err == nil || s.Journal.ExecutionState != workloadjournal.Running || len(s.Journal.ResultPayload) != 0 {
		t.Fatal("lost recoverable result collection")
	}
}

func TestResultTenantAndArtifactScope(t *testing.T) {
	for _, test := range []string{"tenant", "generation", "commit", "unredacted", "path", "deadline", "encoded_path", "query"} {
		t.Run(test, func(t *testing.T) {
			c, _, r, _ := setupController(t)
			data := envelope(t, nil)
			old := c.resolver
			c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
				s, err := old.Resolve(ctx, id)
				s.ArtifactPrefix = "s3://artifacts/logs/tenant_1/run_1/attempt_1/"
				return s, err
			})
			if _, err := c.Ensure(context.Background(), data); err != nil {
				t.Fatal(err)
			}
			r.result = successfulResult(t, c)
			r.result.LogArtifact = &workloadjobs.WorkloadLogArtifact{StorageRef: "s3://artifacts/logs/tenant_1/run_1/attempt_1/log.txt", SHA256: strings.Repeat("d", 64), Redacted: true}
			switch test {
			case "tenant":
				r.result.TenantID = "tenant_other"
			case "generation":
				r.result.Generation++
			case "commit":
				r.result.CommitSHA = strings.Repeat("e", 40)
			case "unredacted":
				r.result.LogArtifact.Redacted = false
			case "path":
				r.result.LogArtifact.StorageRef = "s3://artifacts/logs/tenant_other/run_1/attempt_1/log.txt"
			case "deadline":
				r.result.CompletedAt = r.result.CompletedAt.Add(25 * 60 * 60 * 1e9)
			case "encoded_path":
				r.result.LogArtifact.StorageRef = "s3://artifacts/logs/tenant_1/run_1/attempt_1/%2e%2e/log.txt"
			case "query":
				r.result.LogArtifact.StorageRef += "?token=secret"
			}
			s, err := c.Poll(context.Background(), data, publishFunc(func(context.Context, workloadjobs.WorkloadResult) error {
				t.Fatal("unsafe result published")
				return nil
			}))
			if !errors.Is(err, ErrSpecification) || len(s.Journal.ResultPayload) != 0 {
				t.Fatal("unsafe result checkpointed", err)
			}
		})
	}
}

func TestPollCancelledAndTimedOut(t *testing.T) {
	for _, expired := range []bool{false, true} {
		c, _, _, now := setupController(t)
		data := envelope(t, nil)
		if _, err := c.Ensure(context.Background(), data); err != nil {
			t.Fatal(err)
		}
		want := domain.WorkloadCancelled
		if expired {
			*now = now.Add(24 * 60 * 60 * 1e9)
			want = domain.WorkloadTimedOut
		} else if _, err := c.Cancel(context.Background(), data); err != nil {
			t.Fatal(err)
		}
		p := publishFunc(func(_ context.Context, r workloadjobs.WorkloadResult) error {
			if r.Status != want {
				t.Errorf("status %s want %s", r.Status, want)
			}
			return nil
		})
		s, err := c.Poll(context.Background(), data, p)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Journal.ResultPublished || s.Journal.CleanupState != workloadjournal.CleanupConfirmed {
			t.Fatal("terminal result not delivered")
		}
	}
}

func TestPollAdvancesLaunchAndRunning(t *testing.T) {
	c, _, _, _ := setupController(t)
	data := envelope(t, nil)
	p := publishFunc(func(context.Context, workloadjobs.WorkloadResult) error {
		t.Fatal("published before terminal")
		return nil
	})
	s, err := c.Poll(context.Background(), data, p)
	if err != nil || s.Journal.ExecutionState != workloadjournal.Running {
		t.Fatal(s, err)
	}
	if _, err := c.Poll(context.Background(), data, p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Poll(context.Background(), data, nil); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal(err)
	}
}
