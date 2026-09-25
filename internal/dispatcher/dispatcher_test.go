package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/outbox"
)

type fakeStore struct {
	events     []outbox.Event
	published  []string
	retryTimes []time.Time
}

func (s *fakeStore) Claim(context.Context, int, time.Duration, time.Time) ([]outbox.Event, error) {
	return s.events, nil
}
func (s *fakeStore) MarkPublished(_ context.Context, event outbox.Event, _ time.Time) error {
	s.published = append(s.published, event.ID)
	return nil
}
func (s *fakeStore) ReleaseAfterFailure(_ context.Context, _ outbox.Event, at time.Time) error {
	s.retryTimes = append(s.retryTimes, at)
	return nil
}

type fakePublisher struct {
	queueURLs []string
	err       error
}

func (p *fakePublisher) Publish(_ context.Context, queueURL string, _ outbox.Event) error {
	p.queueURLs = append(p.queueURLs, queueURL)
	return p.err
}

func TestRunOnceRoutesAndAcknowledgesCommittedCommand(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_1", AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"operation_id":"op_1","resource_plane_id":"rp_tokyo"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "https://sqs.ap-northeast-1.amazonaws.com/123456789012/hako-rp-tokyo"}, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run dispatcher: %v", err)
	}
	if stats != (Stats{Claimed: 1, Published: 1}) || len(store.published) != 1 || store.published[0] != "evt_1" || len(publisher.queueURLs) != 1 {
		t.Fatalf("unexpected dispatch outcome: stats=%+v published=%v queues=%v", stats, store.published, publisher.queueURLs)
	}
}

func TestRunOnceReleasesFailedPublicationWithBoundedBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_1", AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Attempt: 3,
		Payload: []byte(`{"schema_version":1,"operation_id":"op_1","resource_plane_id":"rp_tokyo"}`),
	}}}
	publisher := &fakePublisher{err: errors.New("queue unavailable")}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "queue-url"}, Config{
		Now: func() time.Time { return now }, BaseBackoff: 2 * time.Second, MaxBackoff: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "queue unavailable") {
		t.Fatalf("expected queue failure, got %v", err)
	}
	if stats != (Stats{Claimed: 1, Failed: 1}) || len(store.retryTimes) != 1 || !store.retryTimes[0].Equal(now.Add(5*time.Second)) || len(store.published) != 0 {
		t.Fatalf("unexpected retry handling: stats=%+v retry=%v published=%v", stats, store.retryTimes, store.published)
	}
}

func TestRunOnceRejectsInvalidOrUnmappedCommand(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{events: []outbox.Event{{
		ID: "evt_1", AggregateType: "operation", AggregateID: "op_1", EventType: "operation.requested", Attempt: 1,
		Payload: []byte(`{"schema_version":1,"operation_id":"op_1","resource_plane_id":"rp_missing"}`),
	}}}
	publisher := &fakePublisher{}
	dispatcher, err := New(store, publisher, map[string]string{"rp_tokyo": "queue-url"}, Config{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := dispatcher.RunOnce(context.Background())
	if err == nil || stats.Failed != 1 || len(publisher.queueURLs) != 0 || len(store.retryTimes) != 1 {
		t.Fatalf("unmapped Resource Plane command must be deferred: stats=%+v err=%v", stats, err)
	}
}
