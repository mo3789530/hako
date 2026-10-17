package actionsrunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

type RunnerJobClaimer interface {
	ClaimNext(context.Context, string, time.Time, time.Duration) (githubwebhook.ActionsRunnerJob, bool, error)
}

type RunnerStartupPublisher interface {
	Publish(context.Context, githubwebhook.ActionsRunnerJob) error
}

type RunnerStartupDispatcherConfig struct {
	BatchSize     int
	LeaseDuration time.Duration
	Now           func() time.Time
	NewClaimID    func() (string, error)
}

type RunnerStartupDispatcher struct {
	claimer   RunnerJobClaimer
	publisher RunnerStartupPublisher
	config    RunnerStartupDispatcherConfig
}

type RunnerStartupDispatchStats struct {
	Claimed   int
	Published int
	Failed    int
}

func NewRunnerStartupDispatcher(claimer RunnerJobClaimer, publisher RunnerStartupPublisher, config RunnerStartupDispatcherConfig) (*RunnerStartupDispatcher, error) {
	if claimer == nil || publisher == nil {
		return nil, errors.New("runner claim source and startup publisher are required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 10
	}
	if config.BatchSize < 1 || config.BatchSize > 100 {
		return nil, errors.New("runner startup dispatch batch size must be 1-100")
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 10 * time.Minute
	}
	if config.LeaseDuration < 5*time.Second || config.LeaseDuration > 10*time.Minute {
		return nil, errors.New("runner claim lease must be 5 seconds to 10 minutes")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.NewClaimID == nil {
		config.NewClaimID = newRunnerClaimID
	}
	return &RunnerStartupDispatcher{claimer: claimer, publisher: publisher, config: config}, nil
}

// DispatchOnce leases admitted jobs and publishes only their opaque lookup key.
// A failed SendMessage leaves the bounded DB lease to expire; a successful
// publish is at-least-once and the broker's claim transition fences duplicates.
func (d *RunnerStartupDispatcher) DispatchOnce(ctx context.Context) (RunnerStartupDispatchStats, error) {
	var stats RunnerStartupDispatchStats
	if d == nil || ctx == nil || d.claimer == nil || d.publisher == nil {
		return stats, errors.New("runner startup dispatcher and context are required")
	}
	seenClaimIDs := make(map[string]struct{}, d.config.BatchSize)
	for range d.config.BatchSize {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		claimID, err := d.config.NewClaimID()
		if err != nil || !validClaimID(claimID) {
			stats.Failed++
			return stats, errors.New("create valid runner claim identity")
		}
		if _, duplicate := seenClaimIDs[claimID]; duplicate {
			stats.Failed++
			return stats, errors.New("runner claim identity generator returned a duplicate")
		}
		seenClaimIDs[claimID] = struct{}{}
		now := d.config.Now().UTC()
		job, found, err := d.claimer.ClaimNext(ctx, claimID, now, d.config.LeaseDuration)
		if err != nil {
			stats.Failed++
			return stats, fmt.Errorf("claim admitted Actions runner job: %w", err)
		}
		if !found {
			return stats, nil
		}
		stats.Claimed++
		if job.ClaimID != claimID || (job.State != "claimed" && job.State != "starting") || !job.LeaseExpires.After(now) ||
			job.IntegrationID == "" || job.JobID <= 0 || job.RunAttempt <= 0 {
			stats.Failed++
			return stats, errors.New("claim source returned an invalid runner job")
		}
		if err := d.publisher.Publish(ctx, job); err != nil {
			stats.Failed++
			return stats, fmt.Errorf("publish Actions runner startup command: %w", err)
		}
		stats.Published++
	}
	return stats, nil
}

func newRunnerClaimID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "runner-" + hex.EncodeToString(random[:]), nil
}

func validClaimID(value string) bool {
	if len(value) < 8 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}
