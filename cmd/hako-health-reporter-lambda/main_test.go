package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	efs "github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/lambdacore"
	coretypes "github.com/aws/aws-sdk-go-v2/service/lambdacore/types"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mo3789530/hako/internal/healthreport"
)

type healthSecretStub struct {
	value string
	err   error
}

func (s healthSecretStub) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.value == "" {
		return &secretsmanager.GetSecretValueOutput{}, nil
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(s.value)}, nil
}

type healthQueueStub struct{ err error }

func (s healthQueueStub) GetQueueAttributes(_ context.Context, input *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
		string(sqstypes.QueueAttributeNameQueueArn): "arn:aws:sqs:ap-northeast-1:123456789012:queue",
	}}, nil
}

type healthMicroVMStub struct{ err error }

func (s healthMicroVMStub) GetMicrovmImageVersion(_ context.Context, input *lambdamicrovms.GetMicrovmImageVersionInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmImageVersionOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &lambdamicrovms.GetMicrovmImageVersionOutput{
		ImageArn: input.ImageIdentifier, ImageVersion: input.ImageVersion,
		Status: types.MicrovmImageVersionStatusActive, State: types.MicrovmImageVersionStateSuccessful,
	}, nil
}

type healthNetworkConnectorStub struct{ err error }

func (s healthNetworkConnectorStub) GetNetworkConnector(_ context.Context, input *lambdacore.GetNetworkConnectorInput, _ ...func(*lambdacore.Options)) (*lambdacore.GetNetworkConnectorOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &lambdacore.GetNetworkConnectorOutput{Arn: input.Identifier, State: coretypes.NetworkConnectorStateActive}, nil
}

type healthControllerStub struct{ err error }

func (s healthControllerStub) GetFunctionConfiguration(_ context.Context, input *lambdasvc.GetFunctionConfigurationInput, _ ...func(*lambdasvc.Options)) (*lambdasvc.GetFunctionConfigurationOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &lambdasvc.GetFunctionConfigurationOutput{FunctionArn: input.FunctionName, State: lambdatypes.StateActive}, nil
}

type healthEFSStub struct{ err error }

func (s healthEFSStub) DescribeFileSystems(_ context.Context, input *efs.DescribeFileSystemsInput, _ ...func(*efs.Options)) (*efs.DescribeFileSystemsOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &efs.DescribeFileSystemsOutput{FileSystems: []efstypes.FileSystemDescription{{FileSystemId: input.FileSystemId,
		LifeCycleState: efstypes.LifeCycleStateAvailable, Encrypted: aws.Bool(true), NumberOfMountTargets: 1}}}, nil
}

type healthSenderStub struct {
	result healthreport.Result
	err    error
}

func (s *healthSenderStub) Send(_ context.Context, _ string, result healthreport.Result) error {
	s.result = result
	return s.err
}

func TestReportOnceEmitsMetricsAndReportsThroughInjectedSender(t *testing.T) {
	secret := "not-a-real-secret-but-32-bytes-minimum"
	var output bytes.Buffer
	sender := &healthSenderStub{}
	err := reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{}, healthNetworkConnectorStub{}, healthSecretStub{value: secret}, &output,
		func(endpoint, planeID string, key []byte) (reportSender, error) {
			if endpoint != "https://cp.hako.example" || planeID != "rp-test" || string(key) != secret {
				t.Fatal("sender factory received incorrect configuration")
			}
			return sender, nil
		})
	if err != nil || sender.result.Status != "healthy" || !strings.Contains(output.String(), "HealthReportHeartbeat") {
		t.Fatalf("reportOnce success: result=%+v metrics=%s err=%v", sender.result, output.String(), err)
	}
}

func TestReportOnceSurfacesOutageButEmitsSanitizedMetric(t *testing.T) {
	var output bytes.Buffer
	sender := &healthSenderStub{err: errors.New("sensitive endpoint response")}
	err := reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{}, healthNetworkConnectorStub{}, healthSecretStub{value: "not-a-real-secret-but-32-bytes-minimum"}, &output,
		func(string, string, []byte) (reportSender, error) { return sender, nil })
	if err == nil || strings.Contains(err.Error(), "sensitive endpoint response") || strings.Contains(output.String(), "sensitive endpoint response") || !strings.Contains(output.String(), "ReporterFailures") {
		t.Fatalf("reporter outage handling leaked details or missed metric: err=%v output=%s", err, output.String())
	}
}

func TestReportOnceMarksFailedMicroVMImageProbeDegraded(t *testing.T) {
	var output bytes.Buffer
	sender := &healthSenderStub{}
	err := reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{err: errors.New("sensitive image metadata")}, healthNetworkConnectorStub{}, healthSecretStub{value: "not-a-real-secret-but-32-bytes-minimum"}, &output,
		func(string, string, []byte) (reportSender, error) { return sender, nil })
	if err != nil || sender.result.Status != "degraded" || sender.result.Reason != "failed capability probes: microvm-image" || strings.Contains(sender.result.Reason, "sensitive") || !strings.Contains(output.String(), "ProbeFailures") {
		t.Fatalf("failed image probe not safely degraded: result=%+v metrics=%s err=%v", sender.result, output.String(), err)
	}
}

func TestReportOnceMarksFailedNetworkConnectorProbeDegraded(t *testing.T) {
	var output bytes.Buffer
	sender := &healthSenderStub{}
	err := reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{}, healthNetworkConnectorStub{err: errors.New("sensitive VPC details")}, healthSecretStub{value: "not-a-real-secret-but-32-bytes-minimum"}, &output,
		func(string, string, []byte) (reportSender, error) { return sender, nil })
	if err != nil || sender.result.Status != "degraded" || sender.result.Reason != "failed capability probes: vpc-egress" || strings.Contains(sender.result.Reason, "sensitive") || !strings.Contains(output.String(), "ProbeFailures") {
		t.Fatalf("failed connector probe not safely degraded: result=%+v metrics=%s err=%v", sender.result, output.String(), err)
	}
}

func TestReportOnceIncludesControllerAndEFSProbes(t *testing.T) {
	var output bytes.Buffer
	sender := &healthSenderStub{}
	extra := extraProbeConfig{controllerARN: "arn:aws:lambda:ap-northeast-1:123456789012:function:controller", controller: healthControllerStub{},
		fileSystemID: "fs-0123456789abcdef0", efs: healthEFSStub{}}
	err := reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{}, healthNetworkConnectorStub{}, healthSecretStub{value: "not-a-real-secret-but-32-bytes-minimum"}, &output,
		func(string, string, []byte) (reportSender, error) { return sender, nil }, extra)
	if err != nil || sender.result.Status != "healthy" {
		t.Fatalf("controller/EFS probe integration failed: result=%+v err=%v", sender.result, err)
	}
	extra.controller = healthControllerStub{err: errors.New("sensitive controller configuration")}
	extra.efs = healthEFSStub{err: errors.New("sensitive EFS configuration")}
	sender = &healthSenderStub{}
	err = reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{}, healthNetworkConnectorStub{}, healthSecretStub{value: "not-a-real-secret-but-32-bytes-minimum"}, &output,
		func(string, string, []byte) (reportSender, error) { return sender, nil }, extra)
	if err != nil || sender.result.Status != "degraded" || sender.result.Reason != "failed capability probes: workload-controller,workspace-efs" || strings.Contains(sender.result.Reason, "sensitive") {
		t.Fatalf("controller/EFS failure was not sanitized: result=%+v err=%v", sender.result, err)
	}
}

func TestReportOnceHandlesKeyUnavailableWithoutSecretLeak(t *testing.T) {
	var output bytes.Buffer
	err := reportOnceWith(context.Background(), "rp-test", "https://cp.hako.example", "secret-arn", "commands-url", "results-url", "image-arn", "42", "connector-arn",
		healthQueueStub{}, healthMicroVMStub{}, healthNetworkConnectorStub{}, healthSecretStub{}, &output,
		func(string, string, []byte) (reportSender, error) {
			t.Fatal("must not build sender without a key")
			return nil, nil
		})
	if err == nil || !strings.Contains(output.String(), "ReporterFailures") || strings.Contains(err.Error(), "secret-arn") {
		t.Fatalf("missing key handling: err=%v output=%s", err, output.String())
	}
}
