package githubwebhook

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

const tenantWebhookMetricNamespace = "Hako/ControlPlane"

// TenantWebhookEMFMetrics writes one CloudWatch Embedded Metric Format record
// per rejected admission. It deliberately has no request-derived dimensions.
type TenantWebhookEMFMetrics struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time
}

func NewTenantWebhookEMFMetrics(out io.Writer) *TenantWebhookEMFMetrics {
	return &TenantWebhookEMFMetrics{out: out, now: time.Now}
}

func (m *TenantWebhookEMFMetrics) RecordWebhookAdmission(outcome string) {
	if m == nil || m.out == nil || (outcome != "rate_limited" && outcome != "limiter_unavailable") {
		return
	}
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	record := struct {
		AWS struct {
			Timestamp        int64 `json:"Timestamp"`
			CloudWatchMetric []struct {
				Namespace  string     `json:"Namespace"`
				Dimensions [][]string `json:"Dimensions"`
				Metrics    []struct {
					Name string `json:"Name"`
					Unit string `json:"Unit"`
				} `json:"Metrics"`
			} `json:"CloudWatchMetrics"`
		} `json:"_aws"`
		Service                string `json:"Service"`
		Outcome                string `json:"Outcome"`
		TenantWebhookAdmission int    `json:"TenantWebhookAdmission"`
	}{Service: "HakoAPI", Outcome: outcome, TenantWebhookAdmission: 1}
	record.AWS.Timestamp = now().UTC().UnixMilli()
	metric := struct {
		Namespace  string     `json:"Namespace"`
		Dimensions [][]string `json:"Dimensions"`
		Metrics    []struct {
			Name string `json:"Name"`
			Unit string `json:"Unit"`
		} `json:"Metrics"`
	}{Namespace: tenantWebhookMetricNamespace, Dimensions: [][]string{{"Service", "Outcome"}}}
	metric.Metrics = []struct {
		Name string `json:"Name"`
		Unit string `json:"Unit"`
	}{{Name: "TenantWebhookAdmission", Unit: "Count"}}
	record.AWS.CloudWatchMetric = append(record.AWS.CloudWatchMetric, metric)
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.out.Write(append(encoded, '\n'))
}
