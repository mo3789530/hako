package workloadjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type ResultDelivery struct {
	Body          []byte
	ReceiptHandle string
}

type ResultQueue interface {
	Receive(context.Context, int, time.Duration) ([]ResultDelivery, error)
	Delete(context.Context, string) error
}

type WorkloadResultApplier interface {
	Handle(context.Context, WorkloadResult) (ResultOutcome, error)
}

type ResultConsumerConfig struct {
	BatchSize int
	WaitTime  time.Duration
}

type WorkloadResultConsumer struct {
	queue  ResultQueue
	apply  WorkloadResultApplier
	config ResultConsumerConfig
}

type ResultConsumerStats struct {
	Received  int
	Applied   int
	Duplicate int
	Stale     int
	Failed    int
}

func NewWorkloadResultConsumer(queue ResultQueue, apply WorkloadResultApplier, config ResultConsumerConfig) (*WorkloadResultConsumer, error) {
	if queue == nil || apply == nil {
		return nil, errors.New("Workload result queue and applier are required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 1
	}
	if config.BatchSize < 1 || config.BatchSize > 10 {
		return nil, errors.New("Workload result SQS batch size must be 1-10")
	}
	if config.WaitTime == 0 {
		config.WaitTime = 20 * time.Second
	}
	if config.WaitTime < 0 || config.WaitTime > 20*time.Second {
		return nil, errors.New("Workload result SQS long-poll wait time must be 0-20 seconds")
	}
	return &WorkloadResultConsumer{queue: queue, apply: apply, config: config}, nil
}

// RunOnce deletes each delivery only after the result transaction has durably
// applied it or identified it as a duplicate/stale generation.
func (c *WorkloadResultConsumer) RunOnce(ctx context.Context) (ResultConsumerStats, error) {
	var stats ResultConsumerStats
	deliveries, err := c.queue.Receive(ctx, c.config.BatchSize, c.config.WaitTime)
	if err != nil {
		return stats, fmt.Errorf("receive Workload results: %w", err)
	}
	stats.Received = len(deliveries)
	var failures []error
	for _, delivery := range deliveries {
		if delivery.ReceiptHandle == "" {
			stats.Failed++
			failures = append(failures, errors.New("Workload result delivery has no receipt handle"))
			continue
		}
		result, err := decodeWorkloadResult(delivery.Body)
		if err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("decode Workload result: %w", err))
			continue
		}
		outcome, err := c.apply.Handle(ctx, result)
		if err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("apply Workload result %s: %w", result.WorkloadRunID, err))
			continue
		}
		if outcome != ResultApplied && outcome != ResultDuplicate && outcome != ResultStale {
			stats.Failed++
			failures = append(failures, fmt.Errorf("apply Workload result %s: unsupported outcome %q", result.WorkloadRunID, outcome))
			continue
		}
		if err := c.queue.Delete(ctx, delivery.ReceiptHandle); err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("delete durably handled Workload result: %w", err))
			continue
		}
		switch outcome {
		case ResultApplied:
			stats.Applied++
		case ResultDuplicate:
			stats.Duplicate++
		case ResultStale:
			stats.Stale++
		}
	}
	return stats, errors.Join(failures...)
}

func decodeWorkloadResult(payload []byte) (WorkloadResult, error) {
	if len(payload) == 0 || len(payload) > 256<<10 {
		return WorkloadResult{}, errors.New("Workload result payload must be 1-256 KiB")
	}
	var result WorkloadResult
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return WorkloadResult{}, fmt.Errorf("decode result envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return WorkloadResult{}, errors.New("Workload result must contain exactly one JSON object")
	}
	if err := validateWorkloadResult(result); err != nil {
		return WorkloadResult{}, err
	}
	return result, nil
}
