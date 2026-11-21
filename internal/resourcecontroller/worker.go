package resourcecontroller

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Delivery struct {
	Body          []byte
	ReceiptHandle string
	ReceiveCount  int
}

type CommandQueue interface {
	Receive(context.Context, int, time.Duration, time.Duration) ([]Delivery, error)
	ChangeVisibility(context.Context, string, time.Duration) error
	Delete(context.Context, string) error
}

type WorkerConfig struct {
	MaxMessages         int
	WaitTime            time.Duration
	VisibilityTimeout   time.Duration
	RetryInitialBackoff time.Duration
	RetryMaxBackoff     time.Duration
}

type Worker struct {
	queue      CommandQueue
	controller *Controller
	config     WorkerConfig
}

type WorkerStats struct {
	Received int
	Handled  int
	Failed   int
}

func NewWorker(queue CommandQueue, controller *Controller, config WorkerConfig) (*Worker, error) {
	if queue == nil || controller == nil {
		return nil, errors.New("command queue and controller are required")
	}
	if config.MaxMessages == 0 {
		config.MaxMessages = 10
	}
	if config.MaxMessages < 1 || config.MaxMessages > 10 {
		return nil, errors.New("SQS worker batch size must be 1-10")
	}
	if config.WaitTime == 0 {
		config.WaitTime = 20 * time.Second
	}
	if config.WaitTime < 0 || config.WaitTime > 20*time.Second {
		return nil, errors.New("SQS long-poll wait time must be 0-20 seconds")
	}
	if config.VisibilityTimeout == 0 {
		config.VisibilityTimeout = 60 * time.Second
	}
	if config.VisibilityTimeout < time.Second || config.VisibilityTimeout > 12*time.Hour || config.VisibilityTimeout%time.Second != 0 {
		return nil, errors.New("SQS visibility timeout must be a positive whole number of seconds, at most 12 hours")
	}
	if config.RetryInitialBackoff == 0 {
		config.RetryInitialBackoff = time.Second
	}
	if config.RetryMaxBackoff == 0 {
		config.RetryMaxBackoff = time.Minute
	}
	if config.RetryInitialBackoff < time.Second || config.RetryMaxBackoff < config.RetryInitialBackoff || config.RetryMaxBackoff > 12*time.Hour {
		return nil, errors.New("SQS retry backoff must be between one second and 12 hours, with max >= initial")
	}
	return &Worker{queue: queue, controller: controller, config: config}, nil
}

// RunOnce receives one SQS batch, reports each result, and deletes a message
// only after reporting succeeds. Unhandled/poison messages remain for SQS
// redelivery and eventual queue DLQ handling.
func (w *Worker) RunOnce(ctx context.Context) (WorkerStats, error) {
	var stats WorkerStats
	deliveries, err := w.queue.Receive(ctx, w.config.MaxMessages, w.config.WaitTime, w.config.VisibilityTimeout)
	if err != nil {
		return stats, fmt.Errorf("receive Resource Plane commands: %w", err)
	}
	stats.Received = len(deliveries)
	results := make(chan deliveryResult, len(deliveries))
	for _, delivery := range deliveries {
		go func(delivery Delivery) {
			results <- w.processDelivery(ctx, delivery)
		}(delivery)
	}
	var failures []error
	for range deliveries {
		result := <-results
		if result.err != nil {
			stats.Failed++
			failures = append(failures, result.err)
		} else if result.handled {
			stats.Handled++
		}
	}
	return stats, errors.Join(failures...)
}

func (w *Worker) processDelivery(ctx context.Context, delivery Delivery) deliveryResult {
	if delivery.ReceiptHandle == "" {
		return deliveryResult{err: errors.New("SQS delivery has no receipt handle")}
	}
	workCtx, cancel := context.WithCancel(ctx)
	stopRenewal := make(chan struct{})
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		interval := w.config.VisibilityTimeout / 2
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenewal:
				return
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := w.queue.ChangeVisibility(ctx, delivery.ReceiptHandle, w.config.VisibilityTimeout); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	handleErr := w.controller.Handle(workCtx, delivery.Body)
	close(stopRenewal)
	<-renewalDone
	cancel()
	if handleErr != nil {
		backoff := w.retryBackoff(delivery.ReceiveCount)
		if err := w.queue.ChangeVisibility(ctx, delivery.ReceiptHandle, backoff); err != nil {
			return deliveryResult{err: fmt.Errorf("handle Resource Plane command: %v; set retry visibility: %w", handleErr, err)}
		}
		return deliveryResult{err: fmt.Errorf("handle Resource Plane command (retry in %s): %w", backoff, handleErr)}
	}
	if err := w.queue.Delete(ctx, delivery.ReceiptHandle); err != nil {
		return deliveryResult{err: fmt.Errorf("delete handled SQS command: %w", err)}
	}
	return deliveryResult{handled: true}
}

func (w *Worker) retryBackoff(receiveCount int) time.Duration {
	delay := w.config.RetryInitialBackoff
	for attempt := 1; attempt < receiveCount && delay < w.config.RetryMaxBackoff; attempt++ {
		if delay > w.config.RetryMaxBackoff/2 {
			return w.config.RetryMaxBackoff
		}
		delay *= 2
	}
	if delay > w.config.RetryMaxBackoff {
		return w.config.RetryMaxBackoff
	}
	return delay
}

type deliveryResult struct {
	handled bool
	err     error
}
