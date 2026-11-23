package githubwebhook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTenantWebhookEMFMetricsUseFixedLowCardinalityDimensions(t *testing.T) {
	var output bytes.Buffer
	metrics := NewTenantWebhookEMFMetrics(&output)
	metrics.now = func() time.Time { return time.UnixMilli(1_800_000_000_123).UTC() }
	metrics.RecordWebhookAdmission("rate_limited")
	metrics.RecordWebhookAdmission("limiter_unavailable")
	metrics.RecordWebhookAdmission("untrusted-route-value")

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected EMF record count: %d (%q)", len(lines), output.String())
	}
	for index, wantOutcome := range []string{"rate_limited", "limiter_unavailable"} {
		var record map[string]any
		if err := json.Unmarshal([]byte(lines[index]), &record); err != nil {
			t.Fatal(err)
		}
		if record["Service"] != "HakoAPI" || record["Outcome"] != wantOutcome || record["TenantWebhookAdmission"] != float64(1) {
			t.Fatalf("unexpected metric fields: %#v", record)
		}
		aws, ok := record["_aws"].(map[string]any)
		if !ok || aws["Timestamp"] != float64(1_800_000_000_123) {
			t.Fatalf("unexpected EMF metadata: %#v", record["_aws"])
		}
		metrics, ok := aws["CloudWatchMetrics"].([]any)
		if !ok || len(metrics) != 1 {
			t.Fatalf("missing CloudWatch metric definition: %#v", aws)
		}
		definition := metrics[0].(map[string]any)
		if definition["Namespace"] != tenantWebhookMetricNamespace {
			t.Fatalf("unexpected namespace: %#v", definition)
		}
		dimensions := definition["Dimensions"].([]any)
		if len(dimensions) != 1 || strings.Join([]string{dimensions[0].([]any)[0].(string), dimensions[0].([]any)[1].(string)}, ",") != "Service,Outcome" {
			t.Fatalf("request-derived dimension detected: %#v", dimensions)
		}
	}
	if strings.Contains(output.String(), "untrusted-route-value") {
		t.Fatal("metric output contains a request-derived route value")
	}
}
