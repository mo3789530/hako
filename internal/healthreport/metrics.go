package healthreport

import (
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
)

const MetricNamespace = "Hako/ResourcePlaneHealth"

type MetricDatum struct {
	Name      string
	Value     float64
	Unit      string
	Timestamp time.Time
}

// MetricsForAttempt returns the documented custom-metric contract for one
// probe/report cycle. Publish these independently from the Control Plane
// request so that report-ingress outages remain observable.
func MetricsForAttempt(result Result, reportErr error) []MetricDatum {
	now := result.ObservedAt.UTC()
	failedProbeCount := 0
	if strings.HasPrefix(result.Reason, "failed capability probes: ") {
		failed := strings.TrimPrefix(result.Reason, "failed capability probes: ")
		if failed != "" {
			failedProbeCount = len(strings.Split(failed, ","))
		}
	}
	metrics := []MetricDatum{{Name: "ProbeFailures", Value: float64(failedProbeCount), Unit: "Count", Timestamp: now}}
	if result.Status == resourceplanehealth.Unhealthy {
		metrics = append(metrics, MetricDatum{Name: "HealthUnhealthy", Value: 1, Unit: "Count", Timestamp: now})
	} else {
		metrics = append(metrics, MetricDatum{Name: "HealthUnhealthy", Value: 0, Unit: "Count", Timestamp: now})
	}
	if reportErr != nil {
		return append(metrics, MetricDatum{Name: "ReporterFailures", Value: 1, Unit: "Count", Timestamp: now})
	}
	return append(metrics, MetricDatum{Name: "HealthReportHeartbeat", Value: 1, Unit: "Count", Timestamp: now})
}
