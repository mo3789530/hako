package healthreport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
)

const (
	DefaultReportRequestTimeout = 5 * time.Second
	maxReportResponseBytes      = 4 << 10
)

var resourcePlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var reportIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Client sends signed health reports to the Hako Control Plane. It owns a
// copy of the per-Plane key and never includes it in errors or logs.
type Client struct {
	endpoint      *url.URL
	resourcePlane string
	key           []byte
	httpClient    *http.Client
	now           func() time.Time
}

type ClientConfig struct {
	Endpoint      string
	ResourcePlane string
	Key           []byte
	HTTPClient    *http.Client
	Now           func() time.Time
}

type reportResponse struct {
	Accepted bool `json:"accepted"`
}

func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("health report endpoint must be an HTTPS origin")
	}
	if endpoint.Path != "" && endpoint.Path != "/" || !resourcePlaneIDPattern.MatchString(config.ResourcePlane) || len(config.Key) < 32 {
		return nil, errors.New("invalid health report client configuration")
	}
	endpoint.Path = ""
	endpoint.RawPath = ""
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultReportRequestTimeout}
	}
	// A signed report must never be redirected to a different host/path.
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		endpoint: endpoint, resourcePlane: config.ResourcePlane,
		key: append([]byte(nil), config.Key...), httpClient: &clientCopy, now: now,
	}, nil
}

// NewReportID generates a random identifier safe for the report ingress.
func NewReportID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate Resource Plane health report ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// Send submits one report. Reusing the same report ID and payload after a
// transport failure is safe: the server treats exact duplicates as success.
func (c *Client) Send(ctx context.Context, reportID string, result Result) error {
	if c == nil || c.endpoint == nil || ctx == nil || !reportIDPattern.MatchString(reportID) || result.ObservedAt.IsZero() {
		return errors.New("invalid Resource Plane health report")
	}
	cleanReason, err := validateResult(result)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Status     string    `json:"status"`
		Reason     string    `json:"reason"`
		ObservedAt time.Time `json:"observed_at"`
	}{string(result.Status), cleanReason, result.ObservedAt.UTC()})
	if err != nil {
		return fmt.Errorf("encode Resource Plane health report: %w", err)
	}
	reportURL := *c.endpoint
	reportURL.Path = "/v1/internal/resource-planes/" + c.resourcePlane + "/health"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, reportURL.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Resource Plane health report request: %w", err)
	}
	timestamp := strconv.FormatInt(c.now().UTC().Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hako-Timestamp", timestamp)
	request.Header.Set("X-Hako-Report-ID", reportID)
	request.Header.Set("X-Hako-Signature", sign(c.key, request.Method, request.URL.EscapedPath(), timestamp, reportID, body))
	sendCtx, cancel := context.WithTimeout(request.Context(), DefaultReportRequestTimeout)
	defer cancel()
	request = request.WithContext(sendCtx)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send Resource Plane health report: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxReportResponseBytes))
		return fmt.Errorf("Hako health report endpoint returned HTTP %d", response.StatusCode)
	}
	var resultBody reportResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReportResponseBytes))
	if err := decoder.Decode(&resultBody); err != nil {
		return errors.New("Hako health report endpoint returned an invalid response")
	}
	return nil
}

func validateResult(result Result) (string, error) {
	return resourceplanehealth.Validate(result.Status, result.Reason)
}

func sign(key []byte, method, path, timestamp, reportID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, path, timestamp, reportID, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
