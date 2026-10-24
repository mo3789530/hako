package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestHandlerForwardsSQSPartialBatchResponse(t *testing.T) {
	want := events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{{ItemIdentifier: "message-1"}}}
	fake := &fakePreviewResultHandler{response: want}
	handler := newHandler(fake)
	got, err := handler(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "message-1"}}})
	if err != nil || len(got.BatchItemFailures) != 1 || got.BatchItemFailures[0].ItemIdentifier != "message-1" {
		t.Fatalf("partial batch response = %+v, %v; want %+v", got, err, want)
	}
	if !fake.called {
		t.Fatal("Preview result handler was not called")
	}
}

func TestHandlerPropagatesUnexpectedFailure(t *testing.T) {
	wantErr := errors.New("consumer unavailable")
	_, err := newHandler(&fakePreviewResultHandler{err: wantErr})(context.Background(), events.SQSEvent{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("handler error = %v, want %v", err, wantErr)
	}
}

func TestHandlerRejectsInvalidDependenciesAndCanceledContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		handler previewResultHandler
	}{
		{name: "missing handler", ctx: context.Background()},
		{name: "canceled", ctx: canceledContext(), handler: &fakePreviewResultHandler{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := newHandler(test.handler)(test.ctx, events.SQSEvent{})
			if err == nil || len(got.BatchItemFailures) != 0 {
				t.Fatalf("handler response = %+v, %v; want configuration error", got, err)
			}
		})
	}
}

type fakePreviewResultHandler struct {
	response events.SQSEventResponse
	err      error
	called   bool
}

func (h *fakePreviewResultHandler) Handle(context.Context, events.SQSEvent) (events.SQSEventResponse, error) {
	h.called = true
	return h.response, h.err
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
