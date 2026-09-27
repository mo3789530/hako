package workloadjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

type ScheduleRequest struct {
	SchemaVersion int                  `json:"schema_version"`
	TenantID      domain.TenantID      `json:"tenant_id"`
	WorkloadRunID domain.WorkloadRunID `json:"workload_run_id"`
}

type ScheduleDelivery struct {
	Body          []byte
	ReceiptHandle string
}

// ScheduleQueue provides the minimum SQS operations required by the worker.
// Messages must not be deleted until the handler has durably completed or
// identified a duplicate/terminal Run.
type ScheduleQueue interface {
	Receive(context.Context, int, time.Duration) ([]ScheduleDelivery, error)
	Delete(context.Context, string) error
	ChangeVisibility(context.Context, string, time.Duration) error
}

type ScheduleHandler interface {
	Handle(context.Context, ScheduleRequest) error
}

type ScheduleHandlerFunc func(context.Context, ScheduleRequest) error

func (f ScheduleHandlerFunc) Handle(ctx context.Context, request ScheduleRequest) error {
	return f(ctx, request)
}

type ConsumerConfig struct {
	BatchSize           int
	WaitTime            time.Duration
	VisibilityTimeout   time.Duration
	VisibilityHeartbeat time.Duration
}

type SchedulerConsumer struct {
	queue   ScheduleQueue
	handler ScheduleHandler
	config  ConsumerConfig
}

type ConsumerStats struct {
	Received int
	Handled  int
	Failed   int
}

func NewSchedulerConsumer(queue ScheduleQueue, handler ScheduleHandler, config ConsumerConfig) (*SchedulerConsumer, error) {
	if queue == nil || handler == nil {
		return nil, errors.New("Workload schedule queue and handler are required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = 10
	}
	if config.BatchSize < 1 || config.BatchSize > 10 {
		return nil, errors.New("Workload SQS batch size must be 1-10")
	}
	if config.WaitTime == 0 {
		config.WaitTime = 20 * time.Second
	}
	if config.WaitTime < 0 || config.WaitTime > 20*time.Second {
		return nil, errors.New("Workload SQS long-poll wait time must be 0-20 seconds")
	}
	if config.VisibilityTimeout == 0 {
		config.VisibilityTimeout = 10 * time.Minute
	}
	if config.VisibilityTimeout < time.Second || config.VisibilityTimeout > 12*time.Hour || config.VisibilityTimeout%time.Second != 0 {
		return nil, errors.New("Workload SQS visibility timeout must be whole seconds from 1 second to 12 hours")
	}
	if config.VisibilityHeartbeat == 0 {
		config.VisibilityHeartbeat = config.VisibilityTimeout / 2
	}
	if config.VisibilityHeartbeat <= 0 || config.VisibilityHeartbeat >= config.VisibilityTimeout {
		return nil, errors.New("Workload SQS visibility heartbeat must be positive and shorter than the visibility timeout")
	}
	return &SchedulerConsumer{queue: queue, handler: handler, config: config}, nil
}

// RunOnce handles a single SQS batch. Malformed, failed, and unacknowledged
// messages are deliberately left on the queue for retry/DLQ redrive.
func (c *SchedulerConsumer) RunOnce(ctx context.Context) (ConsumerStats, error) {
	var stats ConsumerStats
	deliveries, err := c.queue.Receive(ctx, c.config.BatchSize, c.config.WaitTime)
	if err != nil {
		return stats, fmt.Errorf("receive Workload schedule messages: %w", err)
	}
	stats.Received = len(deliveries)
	var failures []error
	for _, delivery := range deliveries {
		if delivery.ReceiptHandle == "" {
			stats.Failed++
			failures = append(failures, errors.New("Workload schedule delivery has no receipt handle"))
			continue
		}
		request, err := decodeScheduleRequest(delivery.Body)
		if err == nil {
			err = c.handle(ctx, delivery.ReceiptHandle, request)
		}
		if err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("handle Workload schedule message: %w", err))
			continue
		}
		if err := c.queue.Delete(ctx, delivery.ReceiptHandle); err != nil {
			stats.Failed++
			failures = append(failures, fmt.Errorf("delete handled Workload schedule message: %w", err))
			continue
		}
		stats.Handled++
	}
	return stats, errors.Join(failures...)
}

func (c *SchedulerConsumer) handle(ctx context.Context, receipt string, request ScheduleRequest) error {
	if err := c.queue.ChangeVisibility(ctx, receipt, c.config.VisibilityTimeout); err != nil {
		return fmt.Errorf("extend Workload SQS visibility: %w", err)
	}
	handlerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	heartbeatErr := make(chan error, 1)
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(c.config.VisibilityHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-handlerCtx.Done():
				return
			case <-ticker.C:
				if err := c.queue.ChangeVisibility(handlerCtx, receipt, c.config.VisibilityTimeout); err != nil {
					heartbeatErr <- fmt.Errorf("renew Workload SQS visibility: %w", err)
					cancel()
					return
				}
			}
		}
	}()
	handlerErr := c.handler.Handle(handlerCtx, request)
	cancel()
	<-heartbeatDone
	select {
	case err := <-heartbeatErr:
		return errors.Join(handlerErr, err)
	default:
		return handlerErr
	}
}

func decodeScheduleRequest(payload []byte) (ScheduleRequest, error) {
	var request ScheduleRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ScheduleRequest{}, fmt.Errorf("decode Workload schedule envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ScheduleRequest{}, errors.New("Workload schedule envelope must contain exactly one JSON object")
	}
	if request.SchemaVersion != 1 || request.TenantID == "" || request.WorkloadRunID == "" {
		return ScheduleRequest{}, errors.New("Workload schedule envelope is unsupported or incomplete")
	}
	return request, nil
}
