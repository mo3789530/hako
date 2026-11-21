package healthreport

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMetricsForAttemptHealthAndReporterOutage(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	result := Result{Status: "degraded", Reason: "failed capability probes: controller,provider", ObservedAt: now}
	metrics := MetricsForAttempt(result, errors.New("API unavailable"))
	if len(metrics) != 3 || metrics[0].Name != "ProbeFailures" || metrics[0].Value != 2 || metrics[1].Name != "HealthUnhealthy" || metrics[1].Value != 0 || metrics[2].Name != "ReporterFailures" || metrics[2].Value != 1 {
		t.Fatalf("degraded report outage metrics = %+v", metrics)
	}

	result.Status, result.Reason = "unhealthy", "failed capability probes: controller,provider"
	metrics = MetricsForAttempt(result, nil)
	if len(metrics) != 3 || metrics[1].Name != "HealthUnhealthy" || metrics[1].Value != 1 || metrics[2].Name != "HealthReportHeartbeat" || metrics[2].Value != 1 {
		t.Fatalf("unhealthy but delivered metrics = %+v", metrics)
	}
}

func TestEncodeEMFContainsOnlyAggregateMetrics(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	metrics := MetricsForAttempt(Result{Status: "degraded", Reason: "failed capability probes: provider", ObservedAt: now}, errors.New("secret must not leak"))
	encoded, err := EncodeEMF("rp-tokyo-01", metrics)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode EMF: %v", err)
	}
	if decoded["ResourcePlaneId"] != "rp-tokyo-01" || decoded["ProbeFailures"] != float64(1) || decoded["ReporterFailures"] != float64(1) {
		t.Fatalf("EMF values = %s", encoded)
	}
	if _, exists := decoded["Reason"]; exists || strings.Contains(string(encoded), "secret must not leak") || !strings.Contains(string(encoded), MetricNamespace) {
		t.Fatalf("EMF leaked report details or omitted namespace: %s", encoded)
	}
}
