package healthreport

import (
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
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type queueAttributesStub struct {
	output *sqs.GetQueueAttributesOutput
	err    error
	input  *sqs.GetQueueAttributesInput
}

func (s *queueAttributesStub) GetQueueAttributes(_ context.Context, input *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	s.input = input
	return s.output, s.err
}

func TestSQSQueueProbeChecksOnlyQueueMetadata(t *testing.T) {
	client := &queueAttributesStub{output: &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
		string(sqstypes.QueueAttributeNameQueueArn): "arn:aws:sqs:ap-northeast-1:123456789012:rp-commands",
	}}}
	probe := SQSQueueProbe("command-queue", "https://sqs.ap-northeast-1.amazonaws.com/123456789012/rp-commands", client)
	if err := probe.Check(context.Background()); err != nil {
		t.Fatalf("queue probe failed: %v", err)
	}
	if client.input == nil || client.input.QueueUrl == nil || *client.input.QueueUrl == "" || len(client.input.AttributeNames) != 1 || client.input.AttributeNames[0] != sqstypes.QueueAttributeNameQueueArn {
		t.Fatalf("probe requested unexpected queue data: %+v", client.input)
	}
}

type microVMImageStub struct {
	output *lambdamicrovms.GetMicrovmImageVersionOutput
	err    error
	input  *lambdamicrovms.GetMicrovmImageVersionInput
}

func (s *microVMImageStub) GetMicrovmImageVersion(_ context.Context, input *lambdamicrovms.GetMicrovmImageVersionInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmImageVersionOutput, error) {
	s.input = input
	return s.output, s.err
}

func TestMicroVMImageProbeChecksExactActiveImageVersion(t *testing.T) {
	const imageARN = "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-workloads"
	const imageVersion = "42"
	client := &microVMImageStub{output: &lambdamicrovms.GetMicrovmImageVersionOutput{
		ImageArn: aws.String(imageARN), ImageVersion: aws.String(imageVersion),
		Status: types.MicrovmImageVersionStatusActive, State: types.MicrovmImageVersionStateSuccessful,
	}}
	if err := MicroVMImageProbe("microvm-image", imageARN, imageVersion, client).Check(context.Background()); err != nil {
		t.Fatalf("active image probe failed: %v", err)
	}
	if client.input == nil || aws.ToString(client.input.ImageIdentifier) != imageARN || aws.ToString(client.input.ImageVersion) != imageVersion {
		t.Fatalf("probe did not query the immutable configured image version: %+v", client.input)
	}
}

func TestMicroVMImageProbeFailsClosed(t *testing.T) {
	active := &lambdamicrovms.GetMicrovmImageVersionOutput{
		ImageArn: aws.String("configured-image"), ImageVersion: aws.String("7"),
		Status: types.MicrovmImageVersionStatusActive, State: types.MicrovmImageVersionStateSuccessful,
	}
	for name, stub := range map[string]*microVMImageStub{
		"api error":        {err: errors.New("sensitive provider details")},
		"missing response": {},
		"inactive":         {output: &lambdamicrovms.GetMicrovmImageVersionOutput{ImageArn: active.ImageArn, ImageVersion: active.ImageVersion, State: active.State, Status: types.MicrovmImageVersionStatusInactive}},
		"build incomplete": {output: &lambdamicrovms.GetMicrovmImageVersionOutput{ImageArn: active.ImageArn, ImageVersion: active.ImageVersion, State: types.MicrovmImageVersionStateInProgress, Status: active.Status}},
		"wrong image":      {output: &lambdamicrovms.GetMicrovmImageVersionOutput{ImageArn: aws.String("other-image"), ImageVersion: active.ImageVersion, State: active.State, Status: active.Status}},
	} {
		t.Run(name, func(t *testing.T) {
			err := MicroVMImageProbe("microvm-image", "configured-image", "7", stub).Check(context.Background())
			if err == nil || strings.Contains(err.Error(), "sensitive provider details") {
				t.Fatalf("probe did not safely reject unavailable image: %v", err)
			}
		})
	}
	if err := MicroVMImageProbe("microvm-image", "", "7", &microVMImageStub{}).Check(context.Background()); err == nil {
		t.Fatal("probe accepted an empty image ARN")
	}
}

type networkConnectorStub struct {
	output *lambdacore.GetNetworkConnectorOutput
	err    error
	input  *lambdacore.GetNetworkConnectorInput
}

func (s *networkConnectorStub) GetNetworkConnector(_ context.Context, input *lambdacore.GetNetworkConnectorInput, _ ...func(*lambdacore.Options)) (*lambdacore.GetNetworkConnectorOutput, error) {
	s.input = input
	return s.output, s.err
}

func TestNetworkConnectorProbeChecksExactActiveConnector(t *testing.T) {
	const connectorARN = "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:hako-private-egress"
	client := &networkConnectorStub{output: &lambdacore.GetNetworkConnectorOutput{
		Arn: aws.String(connectorARN), State: coretypes.NetworkConnectorStateActive,
		LastUpdateStatus: coretypes.NetworkConnectorLastUpdateStatusSuccessful,
	}}
	if err := NetworkConnectorProbe("vpc-egress", connectorARN, client).Check(context.Background()); err != nil {
		t.Fatalf("active connector probe failed: %v", err)
	}
	if client.input == nil || aws.ToString(client.input.Identifier) != connectorARN {
		t.Fatalf("probe did not query exact connector ARN: %+v", client.input)
	}
}

func TestNetworkConnectorProbeFailsClosed(t *testing.T) {
	active := &lambdacore.GetNetworkConnectorOutput{Arn: aws.String("connector-arn"), State: coretypes.NetworkConnectorStateActive}
	for name, stub := range map[string]*networkConnectorStub{
		"api error":        {err: errors.New("sensitive network configuration")},
		"missing response": {},
		"inactive":         {output: &lambdacore.GetNetworkConnectorOutput{Arn: active.Arn, State: coretypes.NetworkConnectorStateInactive}},
		"failed update":    {output: &lambdacore.GetNetworkConnectorOutput{Arn: active.Arn, State: active.State, LastUpdateStatus: coretypes.NetworkConnectorLastUpdateStatusFailed}},
		"wrong connector":  {output: &lambdacore.GetNetworkConnectorOutput{Arn: aws.String("another-connector"), State: active.State}},
	} {
		t.Run(name, func(t *testing.T) {
			err := NetworkConnectorProbe("vpc-egress", "connector-arn", stub).Check(context.Background())
			if err == nil || strings.Contains(err.Error(), "sensitive network configuration") {
				t.Fatalf("probe did not safely reject unavailable connector: %v", err)
			}
		})
	}
	if err := NetworkConnectorProbe("vpc-egress", "", &networkConnectorStub{}).Check(context.Background()); err == nil {
		t.Fatal("probe accepted an empty connector ARN")
	}
}

func TestSQSQueueProbeFailsClosed(t *testing.T) {
	for name, stub := range map[string]*queueAttributesStub{
		"api error":    {err: errors.New("queue unavailable")},
		"missing arn":  {output: &sqs.GetQueueAttributesOutput{Attributes: map[string]string{}}},
		"nil response": {},
	} {
		t.Run(name, func(t *testing.T) {
			probe := SQSQueueProbe("result-queue", "queue-url", stub)
			if err := probe.Check(context.Background()); err == nil {
				t.Fatal("probe accepted an unavailable queue")
			}
		})
	}
	if err := SQSQueueProbe("result-queue", "", &queueAttributesStub{}).Check(context.Background()); err == nil {
		t.Fatal("probe accepted an empty queue URL")
	}
}

type controllerFunctionStub struct {
	output *lambdasvc.GetFunctionConfigurationOutput
	err    error
	input  *lambdasvc.GetFunctionConfigurationInput
}

func (s *controllerFunctionStub) GetFunctionConfiguration(_ context.Context, input *lambdasvc.GetFunctionConfigurationInput, _ ...func(*lambdasvc.Options)) (*lambdasvc.GetFunctionConfigurationOutput, error) {
	s.input = input
	return s.output, s.err
}

func TestControllerFunctionProbeChecksExactActiveFunction(t *testing.T) {
	const functionARN = "arn:aws:lambda:ap-northeast-1:123456789012:function:hako-resource-controller"
	client := &controllerFunctionStub{output: &lambdasvc.GetFunctionConfigurationOutput{
		FunctionArn: aws.String(functionARN), State: lambdatypes.StateActive, LastUpdateStatus: lambdatypes.LastUpdateStatusSuccessful,
	}}
	if err := ControllerFunctionProbe("controller", functionARN, client).Check(context.Background()); err != nil {
		t.Fatalf("active controller probe failed: %v", err)
	}
	if client.input == nil || aws.ToString(client.input.FunctionName) != functionARN {
		t.Fatalf("probe did not query the exact configured controller: %+v", client.input)
	}
}

func TestControllerFunctionProbeFailsClosed(t *testing.T) {
	active := &lambdasvc.GetFunctionConfigurationOutput{FunctionArn: aws.String("configured"), State: lambdatypes.StateActive}
	for name, stub := range map[string]*controllerFunctionStub{
		"api error":        {err: errors.New("sensitive function config")},
		"missing response": {},
		"inactive":         {output: &lambdasvc.GetFunctionConfigurationOutput{FunctionArn: active.FunctionArn, State: lambdatypes.StateInactive}},
		"failed update":    {output: &lambdasvc.GetFunctionConfigurationOutput{FunctionArn: active.FunctionArn, State: active.State, LastUpdateStatus: lambdatypes.LastUpdateStatusFailed}},
		"wrong function":   {output: &lambdasvc.GetFunctionConfigurationOutput{FunctionArn: aws.String("another"), State: active.State}},
	} {
		t.Run(name, func(t *testing.T) {
			err := ControllerFunctionProbe("controller", "configured", stub).Check(context.Background())
			if err == nil || strings.Contains(err.Error(), "sensitive function config") {
				t.Fatalf("probe did not safely reject unavailable controller: %v", err)
			}
		})
	}
	if err := ControllerFunctionProbe("controller", "", &controllerFunctionStub{}).Check(context.Background()); err == nil {
		t.Fatal("probe accepted an empty controller ARN")
	}
}

type efsDescriptionStub struct {
	output *efs.DescribeFileSystemsOutput
	err    error
	input  *efs.DescribeFileSystemsInput
}

func (s *efsDescriptionStub) DescribeFileSystems(_ context.Context, input *efs.DescribeFileSystemsInput, _ ...func(*efs.Options)) (*efs.DescribeFileSystemsOutput, error) {
	s.input = input
	return s.output, s.err
}

func TestEFSFileSystemProbeChecksExactEncryptedAvailableFilesystem(t *testing.T) {
	client := &efsDescriptionStub{output: &efs.DescribeFileSystemsOutput{FileSystems: []efstypes.FileSystemDescription{{
		FileSystemId: aws.String("fs-0123456789abcdef0"), LifeCycleState: efstypes.LifeCycleStateAvailable,
		Encrypted: aws.Bool(true), NumberOfMountTargets: 2,
	}}}}
	if err := EFSFileSystemProbe("workspace-efs", "fs-0123456789abcdef0", client).Check(context.Background()); err != nil {
		t.Fatalf("available encrypted EFS probe failed: %v", err)
	}
	if client.input == nil || aws.ToString(client.input.FileSystemId) != "fs-0123456789abcdef0" {
		t.Fatalf("probe did not query the exact configured EFS: %+v", client.input)
	}
}

func TestEFSFileSystemProbeFailsClosed(t *testing.T) {
	available := efstypes.FileSystemDescription{FileSystemId: aws.String("fs-0123456789abcdef0"), LifeCycleState: efstypes.LifeCycleStateAvailable,
		Encrypted: aws.Bool(true), NumberOfMountTargets: 1}
	for name, stub := range map[string]*efsDescriptionStub{
		"api error":        {err: errors.New("sensitive filesystem data")},
		"missing response": {},
		"wrong filesystem": {output: &efs.DescribeFileSystemsOutput{FileSystems: []efstypes.FileSystemDescription{{FileSystemId: aws.String("fs-fffffffffffffffff"), LifeCycleState: available.LifeCycleState, Encrypted: available.Encrypted, NumberOfMountTargets: 1}}}},
		"not available":    {output: &efs.DescribeFileSystemsOutput{FileSystems: []efstypes.FileSystemDescription{{FileSystemId: available.FileSystemId, LifeCycleState: efstypes.LifeCycleStateUpdating, Encrypted: available.Encrypted, NumberOfMountTargets: 1}}}},
		"unencrypted":      {output: &efs.DescribeFileSystemsOutput{FileSystems: []efstypes.FileSystemDescription{{FileSystemId: available.FileSystemId, LifeCycleState: available.LifeCycleState, Encrypted: aws.Bool(false), NumberOfMountTargets: 1}}}},
		"no mount target":  {output: &efs.DescribeFileSystemsOutput{FileSystems: []efstypes.FileSystemDescription{{FileSystemId: available.FileSystemId, LifeCycleState: available.LifeCycleState, Encrypted: available.Encrypted}}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := EFSFileSystemProbe("workspace-efs", "fs-0123456789abcdef0", stub).Check(context.Background())
			if err == nil || strings.Contains(err.Error(), "sensitive filesystem data") {
				t.Fatalf("probe did not safely reject unavailable EFS: %v", err)
			}
		})
	}
	if err := EFSFileSystemProbe("workspace-efs", "", &efsDescriptionStub{}).Check(context.Background()); err == nil {
		t.Fatal("probe accepted an empty EFS ID")
	}
}
