package healthreport

import (
	"context"
	"errors"
	"regexp"
	"strings"

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

type QueueAttributesAPI interface {
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

// SQSQueueProbe verifies that a named Resource Plane queue is reachable using
// the reporter role and that AWS returns its stable QueueArn. It does not read
// or expose message bodies.
func SQSQueueProbe(name, queueURL string, client QueueAttributesAPI) Probe {
	return Probe{
		Name: name, Timeout: DefaultProbeTimeout,
		Check: func(ctx context.Context) error {
			if strings.TrimSpace(queueURL) == "" || client == nil {
				return errors.New("queue probe is not configured")
			}
			output, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
				QueueUrl: &queueURL, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
			})
			if err != nil || output == nil || strings.TrimSpace(output.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]) == "" {
				return errors.New("queue attributes unavailable")
			}
			return nil
		},
	}
}

type MicroVMImageAPI interface {
	GetMicrovmImageVersion(context.Context, *lambdamicrovms.GetMicrovmImageVersionInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmImageVersionOutput, error)
}

// MicroVMImageProbe verifies the exact immutable image version configured for
// this Resource Plane is readable and launchable. It does not list account
// resources or infer allocation capacity.
func MicroVMImageProbe(name, imageARN, imageVersion string, client MicroVMImageAPI) Probe {
	return Probe{
		Name: name, Timeout: DefaultProbeTimeout,
		Check: func(ctx context.Context) error {
			if strings.TrimSpace(imageARN) == "" || strings.TrimSpace(imageVersion) == "" || client == nil {
				return errors.New("MicroVM image probe is not configured")
			}
			output, err := client.GetMicrovmImageVersion(ctx, &lambdamicrovms.GetMicrovmImageVersionInput{
				ImageIdentifier: aws.String(imageARN), ImageVersion: aws.String(imageVersion),
			})
			if err != nil || output == nil || aws.ToString(output.ImageArn) != imageARN || aws.ToString(output.ImageVersion) != imageVersion ||
				output.Status != types.MicrovmImageVersionStatusActive || output.State != types.MicrovmImageVersionStateSuccessful {
				return errors.New("configured MicroVM image version is unavailable")
			}
			return nil
		},
	}
}

type NetworkConnectorAPI interface {
	GetNetworkConnector(context.Context, *lambdacore.GetNetworkConnectorInput, ...func(*lambdacore.Options)) (*lambdacore.GetNetworkConnectorOutput, error)
}

// NetworkConnectorProbe checks one explicitly configured VPC egress connector
// by ARN. It never lists connectors or returns VPC configuration details.
func NetworkConnectorProbe(name, connectorARN string, client NetworkConnectorAPI) Probe {
	return Probe{
		Name: name, Timeout: DefaultProbeTimeout,
		Check: func(ctx context.Context) error {
			if strings.TrimSpace(connectorARN) == "" || client == nil {
				return errors.New("network connector probe is not configured")
			}
			output, err := client.GetNetworkConnector(ctx, &lambdacore.GetNetworkConnectorInput{Identifier: aws.String(connectorARN)})
			if err != nil || output == nil || aws.ToString(output.Arn) != connectorARN || output.State != coretypes.NetworkConnectorStateActive ||
				(output.LastUpdateStatus != "" && output.LastUpdateStatus != coretypes.NetworkConnectorLastUpdateStatusSuccessful) {
				return errors.New("configured VPC network connector is unavailable")
			}
			return nil
		},
	}
}

type ControllerFunctionAPI interface {
	GetFunctionConfiguration(context.Context, *lambdasvc.GetFunctionConfigurationInput, ...func(*lambdasvc.Options)) (*lambdasvc.GetFunctionConfigurationOutput, error)
}

// ControllerFunctionProbe checks only the exact configured Resource Plane
// worker function and requires it to be invocable and fully updated.
func ControllerFunctionProbe(name, functionARN string, client ControllerFunctionAPI) Probe {
	return Probe{
		Name: name, Timeout: DefaultProbeTimeout,
		Check: func(ctx context.Context) error {
			if strings.TrimSpace(functionARN) == "" || client == nil {
				return errors.New("controller function probe is not configured")
			}
			output, err := client.GetFunctionConfiguration(ctx, &lambdasvc.GetFunctionConfigurationInput{FunctionName: &functionARN})
			if err != nil || output == nil || aws.ToString(output.FunctionArn) != functionARN || output.State != lambdatypes.StateActive ||
				(output.LastUpdateStatus != "" && output.LastUpdateStatus != lambdatypes.LastUpdateStatusSuccessful) {
				return errors.New("configured Resource Plane controller is unavailable")
			}
			return nil
		},
	}
}

type EFSFileSystemAPI interface {
	DescribeFileSystems(context.Context, *efs.DescribeFileSystemsInput, ...func(*efs.Options)) (*efs.DescribeFileSystemsOutput, error)
}

var efsFileSystemIDPattern = regexp.MustCompile(`^fs-[0-9a-f]{8,40}$`)
var efsFileSystemARNPattern = regexp.MustCompile(`^arn:aws:elasticfilesystem:[a-z0-9-]+:[0-9]{12}:file-system/(fs-[0-9a-f]{8,40})$`)

// EFSFileSystemProbe checks one explicitly configured filesystem. It verifies
// availability, encryption, and at least one mount target; it does not prove
// a MicroVM can mount the filesystem or access an individual Access Point.
func EFSFileSystemProbe(name, fileSystemID string, client EFSFileSystemAPI) Probe {
	return Probe{
		Name: name, Timeout: DefaultProbeTimeout,
		Check: func(ctx context.Context) error {
			if client == nil || (!efsFileSystemIDPattern.MatchString(fileSystemID) && !efsFileSystemARNPattern.MatchString(fileSystemID)) {
				return errors.New("EFS file system probe is not configured")
			}
			output, err := client.DescribeFileSystems(ctx, &efs.DescribeFileSystemsInput{FileSystemId: &fileSystemID})
			if err != nil || output == nil || len(output.FileSystems) != 1 {
				return errors.New("configured EFS file system is unavailable")
			}
			filesystem := output.FileSystems[0]
			wantID := fileSystemID
			if matches := efsFileSystemARNPattern.FindStringSubmatch(fileSystemID); matches != nil {
				wantID = matches[1]
			}
			if aws.ToString(filesystem.FileSystemId) != wantID || filesystem.LifeCycleState != efstypes.LifeCycleStateAvailable ||
				!aws.ToBool(filesystem.Encrypted) || filesystem.NumberOfMountTargets < 1 {
				return errors.New("configured EFS file system is not ready")
			}
			return nil
		},
	}
}
