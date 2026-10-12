package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambdacore"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mo3789530/hako/internal/healthreport"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resourcePlaneID := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_ID"))
	controlPlaneEndpoint := strings.TrimSpace(os.Getenv("HAKO_CONTROL_PLANE_ENDPOINT"))
	secretARN := strings.TrimSpace(os.Getenv("HAKO_HEALTH_REPORT_SECRET_ARN"))
	commandQueueURL := strings.TrimSpace(os.Getenv("HAKO_COMMAND_QUEUE_URL"))
	resultQueueURL := strings.TrimSpace(os.Getenv("HAKO_RESULT_QUEUE_URL"))
	imageARN := strings.TrimSpace(os.Getenv("HAKO_MICROVM_IMAGE_ARN"))
	imageVersion := strings.TrimSpace(os.Getenv("HAKO_MICROVM_IMAGE_VERSION"))
	vpcEgressConnectorARN := strings.TrimSpace(os.Getenv("HAKO_VPC_EGRESS_CONNECTOR_ARN"))
	controllerFunctionARN := strings.TrimSpace(os.Getenv("HAKO_CONTROLLER_FUNCTION_ARN"))
	workspaceFileSystemARN := strings.TrimSpace(os.Getenv("HAKO_WORKSPACE_EFS_ARN"))
	if resourcePlaneID == "" || controlPlaneEndpoint == "" || secretARN == "" || commandQueueURL == "" || resultQueueURL == "" || imageARN == "" || imageVersion == "" || vpcEgressConnectorARN == "" || controllerFunctionARN == "" || workspaceFileSystemARN == "" {
		log.Fatal("health reporter requires Resource Plane ID, Control Plane endpoint, key Secret ARN, queue URLs, fixed MicroVM image/version, VPC connector, controller function ARN, and Workspace EFS ID")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatal("configure AWS health reporter clients")
	}
	queueClient := sqs.NewFromConfig(awsCfg)
	microVMClient := lambdamicrovms.NewFromConfig(awsCfg)
	networkConnectorClient := lambdacore.NewFromConfig(awsCfg)
	controllerClient := lambdasvc.NewFromConfig(awsCfg)
	efsClient := efs.NewFromConfig(awsCfg)
	secretClient := secretsmanager.NewFromConfig(awsCfg)
	lambda.Start(func(ctx context.Context, _ events.CloudWatchEvent) error {
		extra := extraProbeConfig{controllerARN: controllerFunctionARN, controller: controllerClient, fileSystemID: workspaceFileSystemARN, efs: efsClient}
		return reportOnce(ctx, resourcePlaneID, controlPlaneEndpoint, secretARN, commandQueueURL, resultQueueURL, imageARN, imageVersion, vpcEgressConnectorARN, queueClient, microVMClient, networkConnectorClient, secretClient, os.Stdout, extra)
	})
}

type secretReader interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type reportSender interface {
	Send(context.Context, string, healthreport.Result) error
}

type extraProbeConfig struct {
	controllerARN string
	controller    healthreport.ControllerFunctionAPI
	fileSystemID  string
	efs           healthreport.EFSFileSystemAPI
}

func reportOnce(ctx context.Context, resourcePlaneID, endpoint, secretARN, commandQueueURL, resultQueueURL, imageARN, imageVersion, connectorARN string,
	queueClient healthreport.QueueAttributesAPI, microVMClient healthreport.MicroVMImageAPI, connectorClient healthreport.NetworkConnectorAPI, secrets secretReader, output interface{ Write([]byte) (int, error) }, extra ...extraProbeConfig) error {
	return reportOnceWith(ctx, resourcePlaneID, endpoint, secretARN, commandQueueURL, resultQueueURL, imageARN, imageVersion, connectorARN, queueClient, microVMClient, connectorClient, secrets, output,
		func(endpoint, resourcePlaneID string, key []byte) (reportSender, error) {
			return healthreport.NewClient(healthreport.ClientConfig{Endpoint: endpoint, ResourcePlane: resourcePlaneID, Key: key})
		}, extra...)
}

func reportOnceWith(ctx context.Context, resourcePlaneID, endpoint, secretARN, commandQueueURL, resultQueueURL, imageARN, imageVersion, connectorARN string,
	queueClient healthreport.QueueAttributesAPI, microVMClient healthreport.MicroVMImageAPI, connectorClient healthreport.NetworkConnectorAPI, secrets secretReader, output interface{ Write([]byte) (int, error) },
	newSender func(endpoint, resourcePlaneID string, key []byte) (reportSender, error), extra ...extraProbeConfig) error {
	if ctx == nil || queueClient == nil || microVMClient == nil || connectorClient == nil || secrets == nil || output == nil || newSender == nil || strings.TrimSpace(imageARN) == "" || strings.TrimSpace(imageVersion) == "" || strings.TrimSpace(connectorARN) == "" || len(extra) > 1 {
		return errors.New("Resource Plane health reporter dependencies are required")
	}
	observedAt := time.Now().UTC()
	probes := []healthreport.Probe{
		healthreport.SQSQueueProbe("command-queue", commandQueueURL, queueClient),
		healthreport.SQSQueueProbe("result-queue", resultQueueURL, queueClient),
		healthreport.MicroVMImageProbe("microvm-image", imageARN, imageVersion, microVMClient),
		healthreport.NetworkConnectorProbe("vpc-egress", connectorARN, connectorClient),
	}
	if len(extra) == 1 {
		if strings.TrimSpace(extra[0].controllerARN) == "" || extra[0].controller == nil || strings.TrimSpace(extra[0].fileSystemID) == "" || extra[0].efs == nil {
			return errors.New("Controller and EFS health probes must be configured together")
		}
		probes = append(probes,
			healthreport.ControllerFunctionProbe("workload-controller", extra[0].controllerARN, extra[0].controller),
			healthreport.EFSFileSystemProbe("workspace-efs", extra[0].fileSystemID, extra[0].efs),
		)
	}
	result, err := healthreport.Run(ctx, probes, observedAt)
	if err != nil {
		return errors.New("Resource Plane capability probes could not be run")
	}
	secretCtx, cancelSecret := context.WithTimeout(ctx, 3*time.Second)
	keyOutput, keyErr := secrets.GetSecretValue(secretCtx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
	cancelSecret()
	if keyErr != nil || keyOutput == nil || keyOutput.SecretString == nil || len(*keyOutput.SecretString) < 32 {
		result.Status = "unhealthy"
		result.Reason = "failed capability probes: report-credentials"
		return emitMetrics(output, resourcePlaneID, healthreport.MetricsForAttempt(result, errors.New("report credentials unavailable")), errors.New("Resource Plane health report key is unavailable"))
	}
	reporter, err := newSender(endpoint, resourcePlaneID, []byte(*keyOutput.SecretString))
	if err != nil {
		result.Status = "unhealthy"
		result.Reason = "failed capability probes: reporter-configuration"
		return emitMetrics(output, resourcePlaneID, healthreport.MetricsForAttempt(result, err), errors.New("Resource Plane health reporter configuration is invalid"))
	}
	reportID, err := healthreport.NewReportID()
	if err != nil {
		return errors.New("Resource Plane health report ID generation failed")
	}
	sendErr := reporter.Send(ctx, reportID, result)
	metrics := healthreport.MetricsForAttempt(result, sendErr)
	if sendErr != nil {
		return emitMetrics(output, resourcePlaneID, metrics, errors.New("Resource Plane health report delivery failed"))
	}
	return emitMetrics(output, resourcePlaneID, metrics, nil)
}

func emitMetrics(output interface{ Write([]byte) (int, error) }, resourcePlaneID string, metrics []healthreport.MetricDatum, failure error) error {
	emf, err := healthreport.EncodeEMF(resourcePlaneID, metrics)
	if err != nil {
		return errors.New("Resource Plane health metrics are invalid")
	}
	if _, err := fmt.Fprintln(output, string(emf)); err != nil {
		return errors.New("Resource Plane health metrics could not be emitted")
	}
	return failure
}
