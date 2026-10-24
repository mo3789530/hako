package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	efsapi "github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	fakeruntime "github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspaceruntime"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

const maxWorkspaceRuntimeProfileBytes = 16 << 10

type workspaceRuntimeDeployment struct {
	JournalBucket string
	JournalPrefix string
	FileSystemID  string
	Spec          workspaceruntime.LaunchSpec
	PollInterval  time.Duration
	PollTimeout   time.Duration
}

func configuredRuntime(implementation string, awsCfg aws.Config, getenv func(string) string) (resourcecontroller.Runtime, error) {
	switch strings.TrimSpace(implementation) {
	case "fake":
		return fakeruntime.New(), nil
	case "lambda-microvm":
		config, err := configuredWorkspaceRuntime(getenv, awsCfg.Region)
		if err != nil {
			return nil, err
		}
		journal, err := workspacejournal.NewS3Store(s3.NewFromConfig(awsCfg), config.JournalBucket, config.JournalPrefix)
		if err != nil {
			return nil, errors.New("configure durable Workspace runtime journal")
		}
		resolver, err := workspaceruntime.NewStaticSpecResolver(config.Spec)
		if err != nil {
			return nil, errors.New("configure trusted Workspace launch profile")
		}
		accessPoints, err := workspacestorage.NewEFS(efsapi.NewFromConfig(awsCfg), config.FileSystemID, awsCfg.Region)
		if err != nil {
			return nil, errors.New("configure Workspace EFS Access Point manager")
		}
		roleParts := strings.SplitN(config.Spec.ExecutionRoleARN, ":", 6)
		if len(roleParts) != 6 || roleParts[2] != "iam" || roleParts[4] == "" {
			return nil, errors.New("Workspace launch profile must identify its Resource Plane account")
		}
		roles, err := workspacestorage.NewExecutionRoleProvider(iam.NewFromConfig(awsCfg), roleParts[1], roleParts[4])
		if err != nil {
			return nil, errors.New("configure Workspace-scoped Lambda execution roles")
		}
		storage, err := workspacestorage.NewWorkspaceStorage(accessPoints, roles)
		if err != nil {
			return nil, errors.New("configure combined Workspace EFS and IAM storage lifecycle")
		}
		runtime, err := workspaceruntime.NewLambdaMicroVMWithStorage(lambdamicrovms.NewFromConfig(awsCfg), journal, resolver, storage,
			workspaceruntime.Config{SupportedRuntimeClasses: map[string]bool{config.Spec.RuntimeClass: true},
				PollInterval: config.PollInterval, PollTimeout: config.PollTimeout}, nil)
		if err != nil {
			return nil, errors.New("configure Lambda MicroVM Workspace runtime")
		}
		return runtime, nil
	default:
		return nil, errors.New("HAKO_RUNTIME_IMPLEMENTATION must be explicitly set to fake or lambda-microvm")
	}
}

func configuredWorkspaceRuntime(getenv func(string) string, region string) (workspaceRuntimeDeployment, error) {
	if getenv == nil {
		return workspaceRuntimeDeployment{}, errors.New("Workspace runtime environment reader is required")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	config := workspaceRuntimeDeployment{
		JournalBucket: get("HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET"),
		JournalPrefix: get("HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX"),
		FileSystemID:  get("HAKO_WORKSPACE_EFS_FILE_SYSTEM_ID"),
		PollInterval:  500 * time.Millisecond,
		PollTimeout:   30 * time.Second,
	}
	if config.JournalBucket == "" || region == "" || config.FileSystemID == "" {
		return workspaceRuntimeDeployment{}, errors.New("Workspace runtime journal bucket, EFS filesystem ID, and AWS region are required")
	}
	profileJSON := get("HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON")
	if profileJSON == "" || len(profileJSON) > maxWorkspaceRuntimeProfileBytes {
		return workspaceRuntimeDeployment{}, errors.New("HAKO_WORKSPACE_RUNTIME_LAUNCH_SPEC_JSON must contain one bounded Resource Plane-owned launch profile")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(profileJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config.Spec); err != nil {
		return workspaceRuntimeDeployment{}, errors.New("Workspace runtime launch profile is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return workspaceRuntimeDeployment{}, errors.New("Workspace runtime launch profile has trailing data")
	}
	if err := workspaceruntime.ValidateLaunchSpec(config.Spec); err != nil || config.Spec.RunHookPayload != "" {
		return workspaceRuntimeDeployment{}, errors.New("Workspace runtime launch profile is unsupported")
	}
	if err := validateWorkspaceRuntimeRegion(config.Spec, region); err != nil {
		return workspaceRuntimeDeployment{}, err
	}
	var err error
	if config.PollInterval, err = parseBoundedDuration(get("HAKO_WORKSPACE_RUNTIME_POLL_INTERVAL"), config.PollInterval, 10*time.Millisecond, 10*time.Second); err != nil {
		return workspaceRuntimeDeployment{}, errors.New("HAKO_WORKSPACE_RUNTIME_POLL_INTERVAL must be between 10ms and 10s")
	}
	if config.PollTimeout, err = parseBoundedDuration(get("HAKO_WORKSPACE_RUNTIME_POLL_TIMEOUT"), config.PollTimeout, time.Second, 10*time.Minute); err != nil {
		return workspaceRuntimeDeployment{}, errors.New("HAKO_WORKSPACE_RUNTIME_POLL_TIMEOUT must be between 1s and 10m")
	}
	return config, nil
}

func validateWorkspaceRuntimeRegion(spec workspaceruntime.LaunchSpec, region string) error {
	imageParts := strings.SplitN(spec.ImageIdentifier, ":", 6)
	roleParts := strings.SplitN(spec.ExecutionRoleARN, ":", 6)
	if len(imageParts) != 6 || len(roleParts) != 6 || imageParts[3] != region || imageParts[4] != roleParts[4] {
		return errors.New("Workspace image region must match the Lambda region and its account must match the execution role")
	}
	for _, connector := range append(append([]string(nil), spec.EgressNetworkConnectors...), spec.IngressNetworkConnectors...) {
		parts := strings.SplitN(connector, ":", 6)
		if len(parts) != 6 || parts[3] != region || parts[4] != imageParts[4] {
			return errors.New("Workspace network connector region and account must match the image profile")
		}
	}
	return nil
}

func parseBoundedDuration(raw string, defaultValue, minimum, maximum time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultValue, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value < minimum || value > maximum {
		return 0, errors.New("duration outside allowed range")
	}
	return value, nil
}
