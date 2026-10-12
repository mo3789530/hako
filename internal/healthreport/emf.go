package healthreport

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var metricPlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// EncodeEMF serializes only aggregate metric values into CloudWatch Embedded
// Metric Format. It deliberately excludes probe reasons and credentials.
func EncodeEMF(resourcePlaneID string, metrics []MetricDatum) ([]byte, error) {
	if !metricPlaneIDPattern.MatchString(resourcePlaneID) || len(metrics) == 0 || len(metrics) > 16 {
		return nil, errors.New("invalid Resource Plane health metric envelope")
	}
	definitions := make([]map[string]string, 0, len(metrics))
	values := map[string]any{"ResourcePlaneId": resourcePlaneID}
	timestamp := time.Time{}
	for _, metric := range metrics {
		if metric.Name == "" || metric.Unit != "Count" || metric.Timestamp.IsZero() {
			return nil, errors.New("invalid Resource Plane health metric datum")
		}
		values[metric.Name] = metric.Value
		definitions = append(definitions, map[string]string{"Name": metric.Name, "Unit": metric.Unit})
		if metric.Timestamp.After(timestamp) {
			timestamp = metric.Timestamp
		}
	}
	values["_aws"] = map[string]any{
		"Timestamp": timestamp.UnixMilli(),
		"CloudWatchMetrics": []any{map[string]any{
			"Namespace":  MetricNamespace,
			"Dimensions": [][]string{{"ResourcePlaneId"}},
			"Metrics":    definitions,
		}},
	}
	return json.Marshal(values)
}
