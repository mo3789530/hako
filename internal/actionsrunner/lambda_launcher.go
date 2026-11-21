package actionsrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
)

var (
	ErrInvalidRunnerLaunch = errors.New("invalid GitHub Actions runner Lambda MicroVM launch")
	arnPattern             = regexp.MustCompile(`^arn:aws:lambda:([a-z0-9-]+):([0-9]{12}):([a-z0-9-]+):([A-Za-z0-9._:/-]+)$`)
	roleARNPattern         = regexp.MustCompile(`^arn:aws:iam::([0-9]{12}):role/([A-Za-z0-9+=,.@_/-]{1,512})$`)
)

type LambdaRunnerAPI interface {
	RunMicrovm(context.Context, *lambdamicrovms.RunMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error)
}

type LambdaRunnerLaunchConfig struct {
	ResourcePlaneID        string
	ImageIdentifier        string
	ImageVersion           string
	ExecutionRoleARN       string
	EgressNetworkConnector string
	MaximumDuration        int32
}

// RunnerLaunchPayload is deliberately non-secret. The short-lived GitHub
// registration token is delivered only after launch over the authenticated
// MicroVM endpoint, never through RunHookPayload (which is part of the AWS API
// request and can be retained in request diagnostics).
type RunnerLaunchPayload struct {
	SchemaVersion int      `json:"schema_version"`
	WorkflowJobID int64    `json:"workflow_job_id"`
	WorkflowRunID int64    `json:"workflow_run_id"`
	RunAttempt    int64    `json:"run_attempt"`
	RepositoryURL string   `json:"repository_url"`
	RunnerName    string   `json:"runner_name"`
	Labels        []string `json:"labels"`
}

type RunnerRepository struct {
	Owner string
	Name  string
}

type LambdaRunnerInstance struct {
	ResourcePlaneID string
	MicroVMID       string
	Endpoint        string
	ImageIdentifier string
	ImageVersion    string
}

type LambdaRunnerLauncher struct {
	config LambdaRunnerLaunchConfig
	api    LambdaRunnerAPI
}

func NewLambdaRunnerLauncher(config LambdaRunnerLaunchConfig, api LambdaRunnerAPI) (*LambdaRunnerLauncher, error) {
	image := arnPattern.FindStringSubmatch(config.ImageIdentifier)
	egress := arnPattern.FindStringSubmatch(config.EgressNetworkConnector)
	if !validResourcePlaneID(config.ResourcePlaneID) || api == nil || len(image) != 5 || image[3] != "microvm-image" ||
		len(egress) != 5 || egress[3] != "network-connector" || image[1] != egress[1] || image[2] != egress[2] ||
		!regexp.MustCompile(`^[1-9][0-9]{0,8}$`).MatchString(config.ImageVersion) {
		return nil, ErrInvalidRunnerLaunch
	}
	if config.ExecutionRoleARN != "" {
		role := roleARNPattern.FindStringSubmatch(config.ExecutionRoleARN)
		if len(role) != 3 || role[1] != image[2] {
			return nil, ErrInvalidRunnerLaunch
		}
	}
	if config.MaximumDuration == 0 {
		config.MaximumDuration = 3600
	}
	if config.MaximumDuration < 1 || config.MaximumDuration > 8*60*60 {
		return nil, ErrInvalidRunnerLaunch
	}
	return &LambdaRunnerLauncher{config: config, api: api}, nil
}

// Launch starts exactly one MicroVM image selected by trusted platform config.
// Repository labels can select metadata but cannot select image, IAM role, or
// network connectors. ClientToken is stable across retries of the same claim.
func (l *LambdaRunnerLauncher) Launch(ctx context.Context, job githubwebhook.ActionsRunnerJob, repository RunnerRepository) (LambdaRunnerInstance, error) {
	if l == nil || ctx == nil || validateRunnerLaunchJob(job) != nil || !validRepositoryPart(repository.Owner) || !validRepositoryPart(repository.Name) {
		return LambdaRunnerInstance{}, ErrInvalidRunnerLaunch
	}
	runnerName := "hako-" + strconv.FormatInt(job.JobID, 10) + "-" + strconv.FormatInt(job.RunAttempt, 10)
	payload, err := json.Marshal(RunnerLaunchPayload{SchemaVersion: 1, WorkflowJobID: job.JobID, WorkflowRunID: job.WorkflowRunID,
		RunAttempt: job.RunAttempt, RepositoryURL: "https://github.com/" + repository.Owner + "/" + repository.Name,
		RunnerName: runnerName, Labels: append([]string(nil), job.Labels...)})
	if err != nil || len(payload) > 16<<10 {
		return LambdaRunnerInstance{}, ErrInvalidRunnerLaunch
	}
	clientToken := runnerClientToken(job)
	input := &lambdamicrovms.RunMicrovmInput{ImageIdentifier: aws.String(l.config.ImageIdentifier), ImageVersion: aws.String(l.config.ImageVersion),
		ClientToken: aws.String(clientToken), EgressNetworkConnectors: []string{l.config.EgressNetworkConnector},
		MaximumDurationInSeconds: aws.Int32(l.config.MaximumDuration), RunHookPayload: aws.String(string(payload))}
	if l.config.ExecutionRoleARN != "" {
		input.ExecutionRoleArn = aws.String(l.config.ExecutionRoleARN)
	}
	output, err := l.api.RunMicrovm(ctx, input)
	if err != nil || output == nil || !validLambdaRunnerOutput(l.config, output) {
		return LambdaRunnerInstance{}, ErrMicroVMUnavailable
	}
	return LambdaRunnerInstance{ResourcePlaneID: l.config.ResourcePlaneID, MicroVMID: aws.ToString(output.MicrovmId),
		Endpoint: aws.ToString(output.Endpoint), ImageIdentifier: l.config.ImageIdentifier, ImageVersion: l.config.ImageVersion}, nil
}

func validateRunnerLaunchJob(job githubwebhook.ActionsRunnerJob) error {
	if len(job.IntegrationID) < 3 || len(job.IntegrationID) > 128 || job.TenantID == "" || job.Installation <= 0 || job.Repository <= 0 ||
		job.JobID <= 0 || job.WorkflowRunID <= 0 || job.RunAttempt <= 0 || job.State != "starting" || len(job.ClaimID) < 8 || len(job.ClaimID) > 128 ||
		!job.LeaseExpires.After(time.Now().UTC()) || !validBranch(job.HeadBranch) || !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(job.HeadSHA) || len(job.Labels) == 0 || len(job.Labels) > 100 {
		return ErrInvalidRunnerLaunch
	}
	for _, value := range []string{job.IntegrationID, job.ClaimID} {
		for _, char := range value {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
				return ErrInvalidRunnerLaunch
			}
		}
	}
	if !validLabelText(job.WorkflowName) || !validLabelText(job.JobName) {
		return ErrInvalidRunnerLaunch
	}
	seen := make(map[string]struct{}, len(job.Labels))
	for _, label := range job.Labels {
		if len(label) == 0 || len(label) > 128 || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\x00\r\n,") {
			return ErrInvalidRunnerLaunch
		}
		if _, ok := seen[label]; ok {
			return ErrInvalidRunnerLaunch
		}
		seen[label] = struct{}{}
	}
	return nil
}

func validLambdaRunnerOutput(config LambdaRunnerLaunchConfig, output *lambdamicrovms.RunMicrovmOutput) bool {
	if output.MicrovmId == nil || strings.TrimSpace(*output.MicrovmId) != *output.MicrovmId || *output.MicrovmId == "" || len(*output.MicrovmId) > 256 ||
		aws.ToString(output.ImageArn) != config.ImageIdentifier || aws.ToString(output.ImageVersion) != config.ImageVersion ||
		aws.ToInt32(output.MaximumDurationInSeconds) != config.MaximumDuration || len(output.EgressNetworkConnectors) != 1 ||
		output.EgressNetworkConnectors[0] != config.EgressNetworkConnector ||
		aws.ToString(output.ExecutionRoleArn) != config.ExecutionRoleARN {
		return false
	}
	for _, char := range *output.MicrovmId {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	endpoint, err := url.Parse(aws.ToString(output.Endpoint))
	image := arnPattern.FindStringSubmatch(config.ImageIdentifier)
	return err == nil && endpoint.Scheme == "https" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" &&
		endpoint.Port() == "" && (endpoint.Path == "" || endpoint.Path == "/") && endpoint.Hostname() != "" &&
		endpoint.Hostname() == *output.MicrovmId+".lambda-microvm."+image[1]+".on.aws" &&
		(output.State == types.MicrovmStatePending || output.State == types.MicrovmStateRunning)
}

func runnerClientToken(job githubwebhook.ActionsRunnerJob) string {
	data := strings.Join([]string{job.IntegrationID, strconv.FormatInt(job.JobID, 10), strconv.FormatInt(job.RunAttempt, 10), job.ClaimID}, "\x00")
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

func validRepositoryPart(value string) bool {
	if len(value) < 1 || len(value) > 100 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char)) {
			return false
		}
	}
	return true
}

func validBranch(value string) bool {
	if len(value) < 1 || len(value) > 255 || strings.TrimSpace(value) != value || strings.Contains(value, "..") || strings.Contains(value, "//") ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "@{") {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}

func validLabelText(value string) bool {
	if len(value) < 1 || len(value) > 255 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}
