package actionsrunner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
)

func TestLambdaMicroVMTerminatorTerminatesAndWaitsForTerminalState(t *testing.T) {
	api := &fakeLambdaAPI{microVMID: "vm-123", state: types.MicrovmStateRunning}
	terminator, err := NewSDKLambdaMicroVMTerminator(LambdaTerminatorConfig{ResourcePlaneID: "rp-tokyo-01", PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, api)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminator.Terminate(context.Background(), "rp-tokyo-01", "vm-123"); err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}
	if api.getCalls != 2 || api.terminateCalls != 1 || api.lastGetID != "vm-123" || api.lastTerminateID != "vm-123" {
		t.Fatalf("provider calls get=%d terminate=%d lastGet=%q lastTerminate=%q", api.getCalls, api.terminateCalls, api.lastGetID, api.lastTerminateID)
	}
}

func TestLambdaMicroVMTerminatorIsIdempotentForTerminatingAndTerminatedVMs(t *testing.T) {
	for _, initial := range []types.MicrovmState{types.MicrovmStateTerminating, types.MicrovmStateTerminated} {
		t.Run(string(initial), func(t *testing.T) {
			api := &fakeLambdaAPI{microVMID: "vm-123", state: initial}
			if initial == types.MicrovmStateTerminating {
				api.terminalAfterGet = 2
			}
			terminator, err := NewSDKLambdaMicroVMTerminator(LambdaTerminatorConfig{ResourcePlaneID: "rp-tokyo-01", PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, api)
			if err != nil {
				t.Fatal(err)
			}
			if err := terminator.Terminate(context.Background(), "rp-tokyo-01", "vm-123"); err != nil {
				t.Fatalf("Terminate() error = %v", err)
			}
			if api.terminateCalls != 0 {
				t.Fatalf("TerminateMicrovm called %d times for initial state %q", api.terminateCalls, initial)
			}
		})
	}
}

func TestLambdaMicroVMTerminatorFailsClosedForWrongPlaneOrProviderIdentity(t *testing.T) {
	api := &fakeLambdaAPI{microVMID: "vm-123", state: types.MicrovmStateRunning}
	terminator, err := NewSDKLambdaMicroVMTerminator(LambdaTerminatorConfig{ResourcePlaneID: "rp-tokyo-01", PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, api)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminator.Terminate(context.Background(), "rp-us-01", "vm-123"); !errors.Is(err, ErrMicroVMIdentityMismatch) {
		t.Fatalf("foreign Resource Plane error = %v", err)
	}
	if api.getCalls != 0 || api.terminateCalls != 0 {
		t.Fatal("provider was called for a foreign Resource Plane")
	}
	api.microVMID = "vm-other"
	if err := terminator.Terminate(context.Background(), "rp-tokyo-01", "vm-123"); !errors.Is(err, ErrMicroVMUnavailable) {
		t.Fatalf("provider identity mismatch error = %v", err)
	}
	if api.terminateCalls != 0 {
		t.Fatal("mismatched provider response was terminated")
	}
}

func TestLambdaMicroVMTerminatorRejectsUnknownStateAndBoundsWait(t *testing.T) {
	api := &fakeLambdaAPI{microVMID: "vm-123", state: types.MicrovmState("UNKNOWN")}
	terminator, err := NewSDKLambdaMicroVMTerminator(LambdaTerminatorConfig{ResourcePlaneID: "rp-tokyo-01", PollInterval: 10 * time.Millisecond, PollTimeout: time.Second}, api)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminator.Terminate(context.Background(), "rp-tokyo-01", "vm-123"); !errors.Is(err, ErrMicroVMUnavailable) {
		t.Fatalf("unknown state error = %v", err)
	}

	api = &fakeLambdaAPI{microVMID: "vm-123", state: types.MicrovmStateTerminating}
	terminator, err = NewSDKLambdaMicroVMTerminator(LambdaTerminatorConfig{ResourcePlaneID: "rp-tokyo-01", PollInterval: 10 * time.Millisecond, PollTimeout: 10 * time.Millisecond}, api)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminator.Terminate(context.Background(), "rp-tokyo-01", "vm-123"); !errors.Is(err, ErrMicroVMUnavailable) {
		t.Fatalf("bounded polling error = %v", err)
	}
}

type fakeLambdaAPI struct {
	microVMID        string
	state            types.MicrovmState
	getCalls         int
	terminateCalls   int
	lastGetID        string
	lastTerminateID  string
	terminalAfterGet int
}

func (f *fakeLambdaAPI) GetMicrovm(_ context.Context, input *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	f.getCalls++
	if f.terminalAfterGet > 0 && f.getCalls >= f.terminalAfterGet {
		f.state = types.MicrovmStateTerminated
	}
	if input.MicrovmIdentifier != nil {
		f.lastGetID = *input.MicrovmIdentifier
	}
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String(f.microVMID), State: f.state}, nil
}

func (f *fakeLambdaAPI) TerminateMicrovm(_ context.Context, input *lambdamicrovms.TerminateMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	f.terminateCalls++
	if input.MicrovmIdentifier != nil {
		f.lastTerminateID = *input.MicrovmIdentifier
	}
	f.state = types.MicrovmStateTerminated
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}
