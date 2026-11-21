package actionsrunner

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
)

var (
	ErrMicroVMIdentityMismatch = errors.New("Lambda MicroVM cleanup identity mismatch")
	ErrMicroVMUnavailable      = errors.New("Lambda MicroVM cleanup unavailable")
)

type LambdaMicroVMAPI interface {
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	TerminateMicrovm(context.Context, *lambdamicrovms.TerminateMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
}

type LambdaTerminatorConfig struct {
	ResourcePlaneID string
	PollInterval    time.Duration
	PollTimeout     time.Duration
}

type SDKLambdaMicroVMTerminator struct {
	resourcePlaneID string
	api             LambdaMicroVMAPI
	pollInterval    time.Duration
	pollTimeout     time.Duration
	sleep           func(context.Context, time.Duration) error
}

func NewSDKLambdaMicroVMTerminator(config LambdaTerminatorConfig, api LambdaMicroVMAPI) (*SDKLambdaMicroVMTerminator, error) {
	if !validResourcePlaneID(config.ResourcePlaneID) || api == nil {
		return nil, ErrInvalidCleanupConfig
	}
	if config.PollInterval == 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.PollTimeout == 0 {
		config.PollTimeout = 30 * time.Second
	}
	if config.PollInterval < 10*time.Millisecond || config.PollInterval > time.Minute ||
		config.PollTimeout < config.PollInterval || config.PollTimeout > 10*time.Minute {
		return nil, ErrInvalidCleanupConfig
	}
	return &SDKLambdaMicroVMTerminator{resourcePlaneID: config.ResourcePlaneID, api: api,
		pollInterval: config.PollInterval, pollTimeout: config.PollTimeout, sleep: sleepContext}, nil
}

// Terminate only acts through the Lambda MicroVM client owned by the exact
// configured Resource Plane. It verifies provider identity and polls until the
// MicroVM is terminated, making retries after ambiguous responses idempotent.
func (t *SDKLambdaMicroVMTerminator) Terminate(ctx context.Context, resourcePlaneID, microVMID string) error {
	if t == nil || ctx == nil || resourcePlaneID != t.resourcePlaneID || strings.TrimSpace(microVMID) != microVMID || microVMID == "" || len(microVMID) > 256 {
		return ErrMicroVMIdentityMismatch
	}
	for _, r := range microVMID {
		if r < 0x21 || r == 0x7f {
			return ErrMicroVMIdentityMismatch
		}
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, t.pollTimeout)
	defer cancel()
	for {
		output, err := t.api.GetMicrovm(deadlineCtx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(microVMID)})
		if err != nil || output == nil || output.MicrovmId == nil || *output.MicrovmId != microVMID {
			return ErrMicroVMUnavailable
		}
		switch output.State {
		case types.MicrovmStateTerminated:
			return nil
		case types.MicrovmStatePending, types.MicrovmStateRunning, types.MicrovmStateSuspending, types.MicrovmStateSuspended:
			if _, err := t.api.TerminateMicrovm(deadlineCtx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(microVMID)}); err != nil {
				return ErrMicroVMUnavailable
			}
		case types.MicrovmStateTerminating:
		default:
			return ErrMicroVMUnavailable
		}
		if err := t.sleep(deadlineCtx, t.pollInterval); err != nil {
			return ErrMicroVMUnavailable
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
