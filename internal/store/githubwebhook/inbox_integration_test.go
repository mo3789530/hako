//go:build integration

package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestInboxRecordIsDurableAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	inbox := Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	now := time.Now().UTC()
	delivery := githubwebhook.VerifiedDelivery{
		DeliveryID: "a1b2c3d4-e5f6-4789-8abc-def012345678",
		Event:      "pull_request", Action: "opened", Payload: []byte(`{"action":"opened","installation":{"id":12},"repository":{"id":42,"name":"api","owner":{"login":"acme"}},"pull_request":{"number":17,"state":"open","head":{"ref":"feature/example","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main"}}}`),
	}
	inserted, err := inbox.Record(ctx, delivery, now)
	if err != nil || !inserted {
		t.Fatalf("first record = inserted %v, err %v", inserted, err)
	}
	inserted, err = inbox.Record(ctx, delivery, now.Add(time.Second))
	if err != nil || inserted {
		t.Fatalf("replay = inserted %v, err %v", inserted, err)
	}
	conflicting := delivery
	conflicting.Payload = []byte(strings.Replace(string(delivery.Payload), `"action":"opened"`, `"action":"opened","changed":true`, 1))
	if _, err := inbox.Record(ctx, conflicting, now); !errors.Is(err, ErrDeliveryIDConflict) {
		t.Fatalf("delivery ID collision error = %v, want %v", err, ErrDeliveryIDConflict)
	}
	var count int
	var payload string
	var schemaVersion int
	var eventJSON string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), MIN(payload_json), MAX(event_schema_version), MIN(normalized_event_json) FROM github_webhook_deliveries WHERE delivery_id = $1`, delivery.DeliveryID).Scan(&count, &payload, &schemaVersion, &eventJSON); err != nil {
		t.Fatal(err)
	}
	if count != 1 || payload != string(delivery.Payload) || schemaVersion != githubwebhook.RepositoryEventSchemaVersion {
		t.Fatalf("persisted rows = %d schema=%d payload=%s; want one original payload and normalized schema", count, schemaVersion, payload)
	}
	var normalized githubwebhook.RepositoryEvent
	if err := json.Unmarshal([]byte(eventJSON), &normalized); err != nil || normalized.Type != "repository.pull_request.opened" || normalized.Repository == nil || normalized.Repository.GitHubID != 42 || normalized.PullRequest == nil || normalized.PullRequest.Number != 17 {
		t.Fatalf("persisted normalized event = %+v; decode error=%v", normalized, err)
	}
}
