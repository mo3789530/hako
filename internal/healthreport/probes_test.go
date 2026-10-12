package healthreport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
)

func TestRunAggregatesPartialFailureAndRecoveryWithoutLeakingError(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	fail := true
	probes := []Probe{
		{Name: "controller", Check: func(context.Context) error { return nil }},
		{Name: "provider", Check: func(context.Context) error {
			if fail {
				return errors.New("secret endpoint token must not leak")
			}
			return nil
		}},
	}
	partial, err := Run(context.Background(), probes, now)
	if err != nil || partial.Status != resourceplanehealth.Degraded || partial.ObservedAt != now || !strings.Contains(partial.Reason, "provider") || strings.Contains(partial.Reason, "secret") {
		t.Fatalf("partial probe result = %+v, %v", partial, err)
	}
	fail = false
	recovered, err := Run(context.Background(), probes, now.Add(time.Minute))
	if err != nil || recovered.Status != resourceplanehealth.Healthy || !strings.Contains(recovered.Reason, "all configured") {
		t.Fatalf("recovered probe result = %+v, %v", recovered, err)
	}
}

func TestRunMarksAllProbeFailuresUnhealthyAndEnforcesTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := Run(ctx, []Probe{
		{Name: "queue", Timeout: 25 * time.Millisecond, Check: func(context.Context) error { return errors.New("queue unavailable") }},
		{Name: "provider", Timeout: 25 * time.Millisecond, Check: func(context.Context) error { return errors.New("provider unavailable") }},
	}, time.Now().UTC())
	if err != nil || result.Status != resourceplanehealth.Unhealthy {
		t.Fatalf("all failed probes result = %+v, %v", result, err)
	}

	started := time.Now()
	block := make(chan struct{})
	go func() {
		select {
		case <-block:
		case <-time.After(time.Second):
		}
	}()
	timed, err := Run(context.Background(), []Probe{{Name: "hung", Timeout: 25 * time.Millisecond, Check: func(context.Context) error {
		<-block
		return nil
	}}}, time.Now().UTC())
	close(block)
	if err != nil || timed.Status != resourceplanehealth.Unhealthy || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("timeout was not bounded: result=%+v err=%v duration=%s", timed, err, time.Since(started))
	}
}

func TestRunRejectsInvalidProbeDefinitions(t *testing.T) {
	now := time.Now().UTC()
	for name, probes := range map[string][]Probe{
		"empty":        nil,
		"duplicate":    {{Name: "api", Check: func(context.Context) error { return nil }}, {Name: "api", Check: func(context.Context) error { return nil }}},
		"missing func": {{Name: "api"}},
		"long timeout": {{Name: "api", Timeout: MaxProbeTimeout + time.Second, Check: func(context.Context) error { return nil }}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Run(context.Background(), probes, now); !errors.Is(err, ErrInvalidProbeSet) {
				t.Fatalf("Run error = %v, want ErrInvalidProbeSet", err)
			}
		})
	}
}
