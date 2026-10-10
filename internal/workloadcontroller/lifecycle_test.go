package workloadcontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type journalDB struct {
	mu       sync.Mutex
	rows     map[string]workloadjournal.Snapshot
	casErr   error
	casCalls int
	failOn   int
}

func key(id workloadjournal.Command) string {
	return id.TenantID + "/" + id.WorkloadRunID + "/" + id.AttemptID
}
func (s *journalDB) Create(_ context.Context, j workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(identity(j))
	if _, ok := s.rows[k]; ok {
		return workloadjournal.Snapshot{}, workloadjournal.ErrConflict
	}
	x := workloadjournal.Snapshot{Journal: j, ETag: "1"}
	s.rows[k] = x
	return x, nil
}
func (s *journalDB) Get(_ context.Context, tenant, run, attempt string) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.rows[tenant+"/"+run+"/"+attempt]
	if !ok {
		return x, workloadjournal.ErrNotFound
	}
	return x, nil
}
func (s *journalDB) CompareAndSwap(_ context.Context, old workloadjournal.Snapshot, next workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.casCalls++
	if s.failOn > 0 && s.casCalls == s.failOn {
		return old, errors.New("checkpoint interrupted")
	}
	if s.casErr != nil {
		return old, s.casErr
	}
	k := key(identity(old.Journal))
	if s.rows[k].ETag != old.ETag {
		return old, workloadjournal.ErrConflict
	}
	if err := workloadjournal.ValidateUpdate(old, next); err != nil {
		return old, err
	}
	x := workloadjournal.Snapshot{Journal: next, ETag: fmt.Sprint(next.Revision)}
	s.rows[k] = x
	return x, nil
}

type resolveFunc func(context.Context, workloadjournal.Command) (Specification, error)

func (r resolveFunc) Resolve(ctx context.Context, id workloadjournal.Command) (Specification, error) {
	return r(ctx, id)
}

// fakeMicroVM models the mandatory idempotency and cancellation fence. It is
// deliberately not a proof that any actual provider offers these primitives.
type fakeMicroVM struct {
	mu          sync.Mutex
	handles     map[string]string
	fenced      map[string]bool
	inflight    int
	launches    int
	unknownOnce bool
	cleanupErr  error
	cancelErr   error
	entered     chan struct{}
	release     chan struct{}
	result      workloadjobs.WorkloadResult
	resultErr   error
}

func (r *fakeMicroVM) GetStatus(context.Context, workloadjournal.Command) (workloadjournal.ExecutionState, error) {
	if r.result.Status == "" {
		return workloadjournal.Running, nil
	}
	return workloadjournal.ExecutionState(r.result.Status), nil
}
func (r *fakeMicroVM) CollectResult(context.Context, workloadjournal.Command) (workloadjobs.WorkloadResult, error) {
	return r.result, r.resultErr
}

func (r *fakeMicroVM) EnsureRunning(_ context.Context, spec Specification) (string, error) {
	r.mu.Lock()
	k := key(spec.Identity)
	if r.fenced[k] {
		r.mu.Unlock()
		return "", errors.New("attempt cancelled")
	}
	r.inflight++
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- struct{}{}
		<-r.release
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight--
	if r.fenced[k] {
		return "", errors.New("attempt cancelled")
	}
	if r.handles[k] == "" {
		r.launches++
		r.handles[k] = fmt.Sprintf("vm_%d", r.launches)
	}
	if r.unknownOnce {
		r.unknownOnce = false
		return "", errors.New("unknown provider response")
	}
	return r.handles[k], nil
}
func (r *fakeMicroVM) Cancel(_ context.Context, id workloadjournal.Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelErr != nil {
		return r.cancelErr
	}
	r.fenced[key(id)] = true
	return nil
}
func (r *fakeMicroVM) Cleanup(_ context.Context, id workloadjournal.Command) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cleanupErr != nil {
		return false, r.cleanupErr
	}
	if r.inflight > 0 {
		return false, nil
	}
	delete(r.handles, key(id))
	return true, nil
}
func setupController(t *testing.T) (*Controller, *journalDB, *fakeMicroVM, *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	requestedAt := now
	deadline := now.Add(24 * time.Hour)
	db := &journalDB{rows: map[string]workloadjournal.Snapshot{}}
	r := &fakeMicroVM{handles: map[string]string{}, fenced: map[string]bool{}}
	a, err := NewAcceptor(db, "rp_1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	resolver := resolveFunc(func(_ context.Context, id workloadjournal.Command) (Specification, error) {
		return Specification{Identity: id, ImageDigest: strings.Repeat("a", 64), SourceDigest: strings.Repeat("b", 64), CommitSHA: strings.Repeat("c", 40), Deadline: deadline, RequestedAt: requestedAt}, nil
	})
	c, err := NewController(a, resolver, r)
	if err != nil {
		t.Fatal(err)
	}
	return c, db, r, &now
}
func TestLaunchRetryAfterUnknownResponse(t *testing.T) {
	c, _, r, _ := setupController(t)
	r.unknownOnce = true
	data := envelope(t, nil)
	s, err := c.Ensure(context.Background(), data)
	if err == nil || s.Journal.ExecutionState != workloadjournal.Launching || s.Journal.SpecificationHash == "" {
		t.Fatalf("missing launch checkpoint: %+v %v", s, err)
	}
	s, err = c.Ensure(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if r.launches != 1 || s.Journal.RuntimeHandle != "vm_1" || s.Journal.ExecutionState != workloadjournal.Running {
		t.Fatal("duplicated or lost allocation")
	}
	if _, err = c.Ensure(context.Background(), data); err != nil || r.launches != 1 {
		t.Fatal("running delivery re-executed")
	}
}
func TestChangedSpecificationNeverRelaunches(t *testing.T) {
	c, _, r, _ := setupController(t)
	r.unknownOnce = true
	data := envelope(t, nil)
	_, _ = c.Ensure(context.Background(), data)
	old := c.resolver
	c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
		s, err := old.Resolve(ctx, id)
		s.SourceDigest = strings.Repeat("d", 64)
		return s, err
	})
	if _, err := c.Ensure(context.Background(), data); !errors.Is(err, ErrSpecification) {
		t.Fatal(err)
	}
	if r.launches != 1 {
		t.Fatal("spec drift caused launch")
	}
}
func TestCancelledBeforeLaunch(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	s, err := c.Cancel(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Journal.CancelRequested || s.Journal.ExecutionState != workloadjournal.Cancelled || s.Journal.CleanupState != workloadjournal.CleanupConfirmed {
		t.Fatal("missing cancellation fence")
	}
	if _, err = c.Ensure(context.Background(), data); err != nil || r.launches != 0 {
		t.Fatal("cancelled attempt launched")
	}
}
func TestCancelDuringLaunch(t *testing.T) {
	c, _, r, _ := setupController(t)
	r.entered = make(chan struct{}, 1)
	r.release = make(chan struct{})
	data := envelope(t, nil)
	finished := make(chan error, 1)
	go func() { _, err := c.Ensure(context.Background(), data); finished <- err }()
	<-r.entered
	s, err := c.Cancel(context.Background(), data)
	if !errors.Is(err, ErrCleanupPending) || s.Journal.ExecutionState != workloadjournal.Cancelled || s.Journal.CleanupState == workloadjournal.CleanupConfirmed {
		t.Fatal("confirmed cleanup while launch outstanding")
	}
	close(r.release)
	if err := <-finished; err == nil {
		t.Fatal("in-flight launch escaped cancel fence")
	}
	s, err = c.RetryCleanup(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Journal.CleanupState != workloadjournal.CleanupConfirmed || r.launches != 0 {
		t.Fatal("late allocation leaked")
	}
}
func TestDeadlineAndCleanupRetry(t *testing.T) {
	c, _, r, now := setupController(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(24 * time.Hour)
	r.cleanupErr = errors.New("provider cleanup unavailable")
	s, err := c.Ensure(context.Background(), data)
	if err == nil || s.Journal.ExecutionState != workloadjournal.TimedOut || s.Journal.CleanupState != workloadjournal.CleanupFailed {
		t.Fatalf("%+v %v", s, err)
	}
	r.cleanupErr = nil
	s, err = c.RetryCleanup(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Journal.CleanupState != workloadjournal.CleanupConfirmed || len(r.handles) != 0 {
		t.Fatal("cleanup retry failed")
	}
}
func TestFailedCheckpointPreventsProviderEffect(t *testing.T) {
	c, db, r, _ := setupController(t)
	db.casErr = errors.New("journal write failed")
	if _, err := c.Ensure(context.Background(), envelope(t, nil)); err == nil {
		t.Fatal("checkpoint failure ignored")
	}
	if r.launches != 0 {
		t.Fatal("launched before checkpoint persisted")
	}
}
func TestStaleGenerationRejected(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cancel(context.Background(), envelope(t, func(c *Command) { c.Generation++ })); !errors.Is(err, workloadjournal.ErrConflict) {
		t.Fatal(err)
	}
	if len(r.fenced) != 0 {
		t.Fatal("stale command stopped current allocation")
	}
}
func TestReplacementWaitsForPredecessorCleanup(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	first, err := c.Ensure(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	previous := identity(first.Journal)
	old := c.resolver
	c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
		s, err := old.Resolve(ctx, id)
		s.Predecessor = &previous
		return s, err
	})
	next := envelope(t, func(c *Command) { c.AttemptID = "attempt_2"; c.Generation = 2; c.IdempotencyToken = "token_2" })
	if _, err := c.Ensure(context.Background(), next); !errors.Is(err, ErrCleanupPending) {
		t.Fatal(err)
	}
	if r.launches != 1 {
		t.Fatal("replacement launched before cleanup")
	}
	if _, err := c.Cancel(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if r.launches != 2 {
		t.Fatal("replacement not launched after cleanup")
	}
}
func TestControllerInvalidDependencies(t *testing.T) {
	c, _, r, _ := setupController(t)
	for _, deps := range []struct {
		a        *Acceptor
		resolver Resolver
		runtime  MicroVM
	}{{nil, c.resolver, r}, {c.acceptor, nil, r}, {c.acceptor, c.resolver, nil}} {
		if _, err := NewController(deps.a, deps.resolver, deps.runtime); err == nil {
			t.Fatal("accepted missing dependency")
		}
	}
}

func TestLaunchCheckpointCrashRecoversSameAllocation(t *testing.T) {
	c, db, r, _ := setupController(t)
	db.failOn = 2
	data := envelope(t, nil)
	if _, err := c.Ensure(context.Background(), data); err == nil {
		t.Fatal("running checkpoint failure ignored")
	}
	if r.launches != 1 {
		t.Fatal("missing initial allocation")
	}
	if _, err := c.Ensure(context.Background(), data); err != nil || r.launches != 1 {
		t.Fatal("crash recovery created duplicate", err)
	}
}

func TestCancelFailureRetainsTombstone(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	r.cancelErr = errors.New("stop unavailable")
	for i := 0; i < 2; i++ {
		s, err := c.Cancel(context.Background(), data)
		if err == nil || !s.Journal.CancelRequested || s.Journal.CleanupState != workloadjournal.CleanupFailed {
			t.Fatal("lost cancellation retry state")
		}
	}
	r.cancelErr = nil
	if _, err := c.RetryCleanup(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if r.launches != 0 {
		t.Fatal("cancelled attempt executed")
	}
}

func TestControllerInputAndResolverFailures(t *testing.T) {
	c, _, r, _ := setupController(t)
	for _, call := range []func(context.Context, []byte) (workloadjournal.Snapshot, error){c.Ensure, c.Cancel, c.RetryCleanup} {
		if _, err := call(context.Background(), []byte("{}")); !errors.Is(err, ErrInvalidCommand) {
			t.Fatal(err)
		}
	}
	if _, err := c.RetryCleanup(context.Background(), envelope(t, nil)); !errors.Is(err, ErrCleanupPending) {
		t.Fatal(err)
	}
	c.resolver = resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) {
		return Specification{}, errors.New("trusted source unavailable")
	})
	if _, err := c.Ensure(context.Background(), envelope(t, nil)); err == nil || r.launches != 0 {
		t.Fatal("resolver failure launched allocation")
	}
}

func TestInvalidSpecificationsPreventLaunch(t *testing.T) {
	for _, field := range []string{"tenant", "image", "source", "commit", "deadline", "predecessor", "missing_predecessor", "predecessor_plane"} {
		t.Run(field, func(t *testing.T) {
			c, _, r, _ := setupController(t)
			old := c.resolver
			c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
				s, err := old.Resolve(ctx, id)
				switch field {
				case "tenant":
					s.Identity.TenantID = "foreign"
				case "image":
					s.ImageDigest = "latest"
				case "source":
					s.SourceDigest = "bad"
				case "commit":
					s.CommitSHA = "main"
				case "deadline":
					s.Deadline = time.Time{}
				case "predecessor":
					s.Predecessor = &id
				case "predecessor_plane":
					p := id
					p.Generation--
					p.AttemptID = "old_attempt"
					p.ResourcePlaneID = "foreign"
					s.Predecessor = &p
				}
				return s, err
			})
			data := envelope(t, func(cmd *Command) {
				if field == "missing_predecessor" || field == "predecessor_plane" {
					cmd.Generation = 2
				}
			})
			if _, err := c.Ensure(context.Background(), data); !errors.Is(err, ErrSpecification) || r.launches != 0 {
				t.Fatal("invalid spec launched", err)
			}
		})
	}
}

func TestCancelledContextDoesNotLaunch(t *testing.T) {
	c, _, r, _ := setupController(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Ensure(ctx, envelope(t, nil)); !errors.Is(err, context.Canceled) || r.launches != 0 {
		t.Fatal("launch ignored cancelled context", err)
	}
}

func TestHandleEventType(t *testing.T) {
	c, _, r, _ := setupController(t)
	data := envelope(t, nil)
	if _, err := c.HandleEvent(context.Background(), "unknown", data); !errors.Is(err, ErrInvalidCommand) {
		t.Fatal(err)
	}
	if _, err := c.HandleEvent(context.Background(), "workload_attempt.cancel", data); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HandleEvent(context.Background(), "workload_attempt.dispatch", data); err != nil || r.launches != 0 {
		t.Fatal("out-of-order dispatch escaped cancel fence")
	}
}
