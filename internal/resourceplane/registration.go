// Package resourceplane contains bootstrap configuration for Hako Resource
// Planes. The manifest carries identifiers and endpoints only; it must never
// contain AWS credentials or secret values.
package resourceplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const ManifestSchemaVersion = 1
const MaxManifestBytes = 1 << 20

var (
	resourcePlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	accountIDPattern       = regexp.MustCompile(`^[0-9]{12}$`)
	regionPattern          = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)?-[a-z]+-[0-9]+$`)
)

// Manifest describes the Resource Planes known to one Control Plane
// deployment. It is static bootstrap configuration, not a user-facing API.
type Manifest struct {
	SchemaVersion  int            `json:"schema_version"`
	ResourcePlanes []Registration `json:"resource_planes"`
}

// Registration contains non-secret placement metadata and queue endpoints.
type Registration struct {
	ID              string   `json:"id"`
	Provider        string   `json:"provider"`
	AccountID       string   `json:"account_id"`
	Region          string   `json:"region"`
	Capabilities    []string `json:"capabilities"`
	CommandQueueURL string   `json:"command_queue_url"`
	ResultQueueURL  string   `json:"result_queue_url"`
}

var supportedCapabilities = map[string]struct{}{
	"microvm":           {},
	"persistent-volume": {},
	"container":         {},
	"private-network":   {},
}

// Parse decodes and validates a versioned Resource Plane manifest. Unknown
// fields and trailing JSON are rejected to avoid silently ignoring typos.
func Parse(data []byte) (Manifest, error) {
	if len(data) == 0 || len(data) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("Resource Plane manifest must be 1-%d bytes", MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode Resource Plane manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("Resource Plane manifest must contain exactly one JSON value")
		}
		return Manifest{}, fmt.Errorf("decode trailing Resource Plane manifest data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// LoadFile reads and validates a manifest from disk.
func LoadFile(path string) (Manifest, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Manifest{}, errors.New("Resource Plane manifest path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open Resource Plane manifest: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read Resource Plane manifest: %w", err)
	}
	return Parse(data)
}

// CommandQueueURLs returns a copy of the Dispatcher routing table.
func (manifest Manifest) CommandQueueURLs() map[string]string {
	queueURLs := make(map[string]string, len(manifest.ResourcePlanes))
	for _, registration := range manifest.ResourcePlanes {
		queueURLs[registration.ID] = registration.CommandQueueURL
	}
	return queueURLs
}

// ResultQueueURLs returns a copy of the Result Consumer queue table.
func (manifest Manifest) ResultQueueURLs() map[string]string {
	queueURLs := make(map[string]string, len(manifest.ResourcePlanes))
	for _, registration := range manifest.ResourcePlanes {
		queueURLs[registration.ID] = registration.ResultQueueURL
	}
	return queueURLs
}

// Validate checks stable IDs, AWS placement metadata, endpoints, and the
// version-1 capability vocabulary. Capabilities are normalized in place.
func (manifest *Manifest) Validate() error {
	if manifest == nil {
		return errors.New("Resource Plane manifest is required")
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported Resource Plane manifest schema_version %d", manifest.SchemaVersion)
	}
	if len(manifest.ResourcePlanes) == 0 {
		return errors.New("at least one Resource Plane registration is required")
	}
	seenIDs := make(map[string]struct{}, len(manifest.ResourcePlanes))
	seenQueueURLs := make(map[string]string, len(manifest.ResourcePlanes)*2)
	for i := range manifest.ResourcePlanes {
		registration := &manifest.ResourcePlanes[i]
		if !resourcePlaneIDPattern.MatchString(registration.ID) {
			return fmt.Errorf("resource_planes[%d].id must be 1-32 lowercase letters, numbers, or hyphens", i)
		}
		if _, exists := seenIDs[registration.ID]; exists {
			return fmt.Errorf("duplicate Resource Plane ID %q", registration.ID)
		}
		seenIDs[registration.ID] = struct{}{}
		if registration.Provider != "aws" {
			return fmt.Errorf("resource_planes[%d].provider %q is unsupported; version 1 supports aws", i, registration.Provider)
		}
		if !accountIDPattern.MatchString(registration.AccountID) {
			return fmt.Errorf("resource_planes[%d].account_id must be a 12-digit AWS account ID", i)
		}
		if !regionPattern.MatchString(registration.Region) {
			return fmt.Errorf("resource_planes[%d].region is not a valid AWS Region identifier", i)
		}
		if len(registration.Capabilities) == 0 {
			return fmt.Errorf("resource_planes[%d].capabilities must not be empty", i)
		}
		seenCapabilities := make(map[string]struct{}, len(registration.Capabilities))
		for j, capability := range registration.Capabilities {
			capability = strings.ToLower(strings.TrimSpace(capability))
			if _, supported := supportedCapabilities[capability]; !supported {
				return fmt.Errorf("resource_planes[%d].capabilities[%d] %q is unsupported", i, j, capability)
			}
			if _, exists := seenCapabilities[capability]; exists {
				return fmt.Errorf("resource_planes[%d] repeats capability %q", i, capability)
			}
			seenCapabilities[capability] = struct{}{}
			registration.Capabilities[j] = capability
		}
		if _, ok := seenCapabilities["microvm"]; !ok {
			return fmt.Errorf("resource_planes[%d].capabilities must include microvm for Workspace placement", i)
		}
		if err := validateQueueURL(registration.CommandQueueURL, registration.Region, registration.AccountID); err != nil {
			return fmt.Errorf("resource_planes[%d].command_queue_url: %w", i, err)
		}
		if err := validateQueueURL(registration.ResultQueueURL, registration.Region, registration.AccountID); err != nil {
			return fmt.Errorf("resource_planes[%d].result_queue_url: %w", i, err)
		}
		if registration.CommandQueueURL == registration.ResultQueueURL {
			return fmt.Errorf("resource_planes[%d] command and result queues must be different", i)
		}
		for _, queue := range []struct{ role, url string }{
			{role: "command", url: registration.CommandQueueURL},
			{role: "result", url: registration.ResultQueueURL},
		} {
			if prior, exists := seenQueueURLs[queue.url]; exists {
				return fmt.Errorf("resource_planes[%d] %s queue URL is already assigned to %s", i, queue.role, prior)
			}
			seenQueueURLs[queue.url] = registration.ID + " " + queue.role
		}
	}
	return nil
}

func validateQueueURL(raw, region, accountID string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an HTTPS SQS queue URL without user info, query, or fragment")
	}
	if !strings.HasPrefix(parsed.Hostname(), "sqs."+region+".") {
		return errors.New("must use the registered Resource Plane Region")
	}
	path := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(path) != 2 || path[0] != accountID || path[1] == "" {
		return errors.New("must identify an SQS queue in the registered AWS account")
	}
	return nil
}
