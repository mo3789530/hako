// Package healthreport runs bounded Resource Plane capability probes and
// aggregates them into a report suitable for the authenticated API ingress.
package healthreport

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
)

const (
	DefaultProbeTimeout = 3 * time.Second
	MaxProbeTimeout     = 30 * time.Second
	MaxProbeCount       = 32
)

var ErrInvalidProbeSet = errors.New("invalid Resource Plane health probe set")

var probeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Probe checks one placement-relevant Resource Plane capability. The check
// must honor its context; it should return only an error and never include
// credentials or response bodies in the error text.
type Probe struct {
	Name    string
	Timeout time.Duration
	Check   func(context.Context) error
}

// Result is a machine health report. Failed probe names are emitted instead
// of arbitrary error strings to avoid persisting sensitive provider details.
type Result struct {
	Status     resourceplanehealth.Status
	Reason     string
	ObservedAt time.Time
}

type probeResult struct {
	name string
	err  error
}

// Run executes probes concurrently, applying a timeout to each. A partial
// failure is degraded; no passing probes is unhealthy. A later all-success
// run naturally yields healthy and acts as recovery.
func Run(ctx context.Context, probes []Probe, now time.Time) (Result, error) {
	if ctx == nil || len(probes) == 0 || len(probes) > MaxProbeCount || now.IsZero() {
		return Result{}, ErrInvalidProbeSet
	}
	seen := make(map[string]struct{}, len(probes))
	maxTimeout := time.Duration(0)
	for _, probe := range probes {
		if !probeNamePattern.MatchString(probe.Name) || probe.Check == nil {
			return Result{}, ErrInvalidProbeSet
		}
		if _, exists := seen[probe.Name]; exists {
			return Result{}, ErrInvalidProbeSet
		}
		seen[probe.Name] = struct{}{}
		if probe.Timeout == 0 {
			probe.Timeout = DefaultProbeTimeout
		}
		if probe.Timeout < 0 || probe.Timeout > MaxProbeTimeout {
			return Result{}, ErrInvalidProbeSet
		}
		timeout := probe.Timeout
		if timeout == 0 {
			timeout = DefaultProbeTimeout
		}
		if timeout > maxTimeout {
			maxTimeout = timeout
		}
	}
	results := make(chan probeResult, len(probes))
	for _, probe := range probes {
		probe := probe
		timeout := probe.Timeout
		if timeout == 0 {
			timeout = DefaultProbeTimeout
		}
		go func() {
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			results <- probeResult{name: probe.Name, err: probe.Check(probeCtx)}
		}()
	}

	failed := make([]string, 0, len(probes))
	completed := make(map[string]struct{}, len(probes))
	timer := time.NewTimer(maxTimeout)
	defer timer.Stop()
collect:
	for len(completed) < len(probes) {
		select {
		case result := <-results:
			completed[result.name] = struct{}{}
			if result.err != nil {
				failed = append(failed, result.name)
			}
		case <-ctx.Done():
			for _, probe := range probes {
				if _, ok := completed[probe.Name]; !ok {
					failed = append(failed, probe.Name)
				}
			}
			break collect
		case <-timer.C:
			for _, probe := range probes {
				if _, ok := completed[probe.Name]; !ok {
					failed = append(failed, probe.Name)
				}
			}
			break collect
		}
	}
	sort.Strings(failed)
	status := resourceplanehealth.Healthy
	if len(failed) == len(probes) {
		status = resourceplanehealth.Unhealthy
	} else if len(failed) > 0 {
		status = resourceplanehealth.Degraded
	}
	reason := "all configured capability probes passed"
	if len(failed) > 0 {
		reason = fmt.Sprintf("failed capability probes: %s", strings.Join(failed, ","))
	}
	return Result{Status: status, Reason: reason, ObservedAt: now.UTC()}, nil
}
