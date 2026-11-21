package actionsrunner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

type lambdaRunnerAPIStub struct {
	input  *lambdamicrovms.RunMicrovmInput
	output *lambdamicrovms.RunMicrovmOutput
	err    error
}

func (s *lambdaRunnerAPIStub) RunMicrovm(_ context.Context, input *lambdamicrovms.RunMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error) {
	s.input = input
	return s.output, s.err
}

func TestLambdaRunnerLauncherUsesSafeHookAndFencedMicroVMProfile(t *testing.T) {
	config := LambdaRunnerLaunchConfig{ResourcePlaneID: "rp-tokyo-01", ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-actions-runner",
		ImageVersion: "3", EgressNetworkConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:github-runner-restricted", MaximumDuration: 3600}
	api := &lambdaRunnerAPIStub{output: &lambdamicrovms.RunMicrovmOutput{MicrovmId: aws.String("runner-microvm-1"),
		Endpoint: aws.String("https://runner-microvm-1.lambda-microvm.ap-northeast-1.on.aws"), ImageArn: aws.String(config.ImageIdentifier), ImageVersion: aws.String(config.ImageVersion),
		MaximumDurationInSeconds: aws.Int32(config.MaximumDuration), EgressNetworkConnectors: []string{config.EgressNetworkConnector}, State: types.MicrovmStatePending}}
	launcher, err := NewLambdaRunnerLauncher(config, api)
	if err != nil {
		t.Fatal(err)
	}
	job := lambdaRunnerJob()
	instance, err := launcher.Launch(context.Background(), job, RunnerRepository{Owner: "acme", Name: "api"})
	if err != nil || instance.ResourcePlaneID != config.ResourcePlaneID || instance.MicroVMID != "runner-microvm-1" {
		t.Fatalf("Launch() = %+v, %v", instance, err)
	}
	if aws.ToString(api.input.ImageIdentifier) != config.ImageIdentifier || aws.ToString(api.input.ImageVersion) != config.ImageVersion ||
		len(api.input.EgressNetworkConnectors) != 1 || api.input.EgressNetworkConnectors[0] != config.EgressNetworkConnector ||
		aws.ToInt32(api.input.MaximumDurationInSeconds) != config.MaximumDuration || aws.ToString(api.input.ExecutionRoleArn) != "" {
		t.Fatalf("MicroVM launch escaped the trusted runtime profile: %+v", api.input)
	}
	var hook RunnerLaunchPayload
	if err := json.Unmarshal([]byte(aws.ToString(api.input.RunHookPayload)), &hook); err != nil {
		t.Fatal(err)
	}
	if hook.SchemaVersion != 1 || hook.WorkflowJobID != job.JobID || hook.WorkflowRunID != job.WorkflowRunID || hook.RunAttempt != job.RunAttempt ||
		hook.RepositoryURL != "https://github.com/acme/api" || hook.RunnerName != "hako-9901-1" || strings.Join(hook.Labels, ",") != "self-hosted,hako,standard" {
		t.Fatalf("Run Hook lost the admitted job identity: %+v", hook)
	}
	if strings.Contains(aws.ToString(api.input.RunHookPayload), "registration-token") || strings.Contains(aws.ToString(api.input.RunHookPayload), "ghs_") {
		t.Fatal("Run Hook contains a GitHub registration credential")
	}
	firstToken := aws.ToString(api.input.ClientToken)
	if _, err := launcher.Launch(context.Background(), job, RunnerRepository{Owner: "acme", Name: "api"}); err != nil || aws.ToString(api.input.ClientToken) != firstToken {
		t.Fatalf("retry changed the idempotent provider client token: %q err=%v", aws.ToString(api.input.ClientToken), err)
	}
}

func TestLambdaRunnerLauncherRejectsUntrustedOrMismatchedLaunch(t *testing.T) {
	config := LambdaRunnerLaunchConfig{ResourcePlaneID: "rp-tokyo-01", ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-actions-runner",
		ImageVersion: "3", EgressNetworkConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:github-runner-restricted"}
	validOutput := func() *lambdamicrovms.RunMicrovmOutput {
		return &lambdamicrovms.RunMicrovmOutput{MicrovmId: aws.String("runner-microvm-1"), Endpoint: aws.String("https://runner-microvm-1.lambda-microvm.ap-northeast-1.on.aws"),
			ImageArn: aws.String(config.ImageIdentifier), ImageVersion: aws.String(config.ImageVersion), MaximumDurationInSeconds: aws.Int32(3600),
			EgressNetworkConnectors: []string{config.EgressNetworkConnector}, State: types.MicrovmStatePending}
	}
	for name, mutate := range map[string]func(*githubwebhook.ActionsRunnerJob, *lambdamicrovms.RunMicrovmOutput){
		"stale claim": func(job *githubwebhook.ActionsRunnerJob, _ *lambdamicrovms.RunMicrovmOutput) {
			job.LeaseExpires = time.Now().Add(-time.Second)
		},
		"unsafe label": func(job *githubwebhook.ActionsRunnerJob, _ *lambdamicrovms.RunMicrovmOutput) {
			job.Labels[0] = "hako,privileged"
		},
		"foreign image": func(_ *githubwebhook.ActionsRunnerJob, output *lambdamicrovms.RunMicrovmOutput) {
			output.ImageArn = aws.String("arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:other")
		},
		"foreign egress": func(_ *githubwebhook.ActionsRunnerJob, output *lambdamicrovms.RunMicrovmOutput) {
			output.EgressNetworkConnectors = []string{"arn:aws:lambda:ap-northeast-1:123456789012:network-connector:internet"}
		},
		"invalid endpoint": func(_ *githubwebhook.ActionsRunnerJob, output *lambdamicrovms.RunMicrovmOutput) {
			output.Endpoint = aws.String("http://attacker.invalid")
		},
		"provider error": func(_ *githubwebhook.ActionsRunnerJob, _ *lambdamicrovms.RunMicrovmOutput) {},
	} {
		t.Run(name, func(t *testing.T) {
			job, output := lambdaRunnerJob(), validOutput()
			mutate(&job, output)
			api := &lambdaRunnerAPIStub{output: output}
			if name == "provider error" {
				api.err = errors.New("provider unavailable")
			}
			launcher, err := NewLambdaRunnerLauncher(config, api)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := launcher.Launch(context.Background(), job, RunnerRepository{Owner: "acme", Name: "api"}); !errors.Is(err, ErrMicroVMUnavailable) && !errors.Is(err, ErrInvalidRunnerLaunch) {
				t.Fatalf("untrusted launch accepted: %v", err)
			}
		})
	}
	if _, err := NewLambdaRunnerLauncher(LambdaRunnerLaunchConfig{ResourcePlaneID: "rp-tokyo-01", ImageIdentifier: config.ImageIdentifier,
		ImageVersion: "3", EgressNetworkConnector: config.EgressNetworkConnector, MaximumDuration: 8*60*60 + 1}, &lambdaRunnerAPIStub{}); !errors.Is(err, ErrInvalidRunnerLaunch) {
		t.Fatalf("runtime duration over the Lambda 8-hour maximum accepted: %v", err)
	}
}

func lambdaRunnerJob() githubwebhook.ActionsRunnerJob {
	return githubwebhook.ActionsRunnerJob{IntegrationID: "integration_runner", TenantID: domain.TenantID("tenant_runner"), Installation: 7121, Repository: 7122,
		JobID: 9901, WorkflowRunID: 9902, RunAttempt: 1, WorkflowName: "CI", JobName: "test", HeadBranch: "feature/task",
		HeadSHA: strings.Repeat("a", 40), Labels: []string{"self-hosted", "hako", "standard"}, State: "starting", ClaimID: "runner-claim-1", LeaseExpires: time.Now().Add(time.Minute)}
}
