package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
)

func downloadWorkloadArtifactCLI(args []string) error {
	outputPath, positional, err := parseArtifactDownloadArgs("hako workload artifact download", args, 3)
	if err != nil {
		return errors.New("usage: hako workload artifact download <tenant-id> <run-id> <artifact-id> --out <path>")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := strings.TrimSpace(os.Getenv("HAKO_API_URL"))
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	gatewayURL := strings.TrimSpace(os.Getenv("HAKO_GATEWAY_URL"))
	if gatewayURL == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	if err := downloadWorkloadArtifact(context.Background(), apiURL, gatewayURL, accessToken,
		positional[0], positional[1], positional[2], outputPath, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "Workload artifact %s saved to %s\n", positional[2], outputPath)
	return err
}

func downloadAgentChangesCLI(args []string) error {
	outputPath, positional, err := parseArtifactDownloadArgs("hako workload changes download", args, 2)
	if err != nil {
		return errors.New("usage: hako workload changes download <tenant-id> <run-id> --out <path>")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := strings.TrimSpace(os.Getenv("HAKO_API_URL"))
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	run, err := api.GetWorkloadRun(context.Background(), apiURL, accessToken, positional[0], positional[1], nil)
	if err != nil {
		return err
	}
	if string(run.TenantID) != positional[0] || string(run.ID) != positional[1] {
		return errors.New("Hako API returned a Workload Run with a mismatched identity")
	}
	artifact, err := agentChangesArtifact(run)
	if err != nil {
		return err
	}
	gatewayURL := strings.TrimSpace(os.Getenv("HAKO_GATEWAY_URL"))
	if gatewayURL == "" {
		return errors.New("HAKO_GATEWAY_URL is required")
	}
	if err := downloadWorkloadArtifactExpected(context.Background(), apiURL, gatewayURL, accessToken,
		positional[0], positional[1], artifact.ID, outputPath, nil, &artifact); err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "Agent change proposal %s saved to %s; review it as untrusted source before use\n", artifact.ID, outputPath)
	return err
}

func inspectAgentChangesCLI(args []string, output io.Writer) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" || output == nil {
		return errors.New("usage: hako workload changes inspect <archive.zip>")
	}
	info, err := os.Lstat(args[0])
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 8<<20 {
		return errors.New("Agent change archive is unavailable or exceeds its size limit")
	}
	file, err := os.Open(args[0])
	if err != nil {
		return errors.New("Agent change archive is unavailable")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return errors.New("Agent change archive changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil || int64(len(data)) != info.Size() || len(data) > 8<<20 {
		return errors.New("Agent change archive could not be read within its size limit")
	}
	summary, err := workloadagent.InspectAgentChangeArchive(data)
	clear(data)
	if err != nil {
		return errors.New("Agent change archive failed safety validation")
	}
	if err := json.NewEncoder(output).Encode(summary); err != nil {
		return errors.New("write Agent change archive inspection result")
	}
	return nil
}

func showAgentPublicationCLI(args []string, output io.Writer) error {
	if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" || output == nil {
		return errors.New("usage: hako workload publication show <tenant-id> <run-id>")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := strings.TrimSpace(os.Getenv("HAKO_API_URL"))
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	status, err := api.GetWorkloadAgentPublication(context.Background(), apiURL, accessToken, args[0], args[1], nil)
	if err != nil {
		return err
	}
	if status.TenantID != args[0] || status.WorkloadRunID != args[1] {
		return errors.New("Hako API returned an Agent publication with a mismatched identity")
	}
	if err := json.NewEncoder(output).Encode(status); err != nil {
		return errors.New("write Agent publication status")
	}
	return nil
}

func submitAgentPublicationCLI(args []string, output io.Writer) error {
	if len(args) != 2 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" || output == nil {
		return errors.New("usage: hako workload publication submit <tenant-id> <agent-run-id>")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := strings.TrimSpace(os.Getenv("HAKO_API_URL"))
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	result, err := api.SubmitWorkloadAgentPublication(context.Background(), apiURL, accessToken, args[0], args[1], nil)
	if err != nil {
		return err
	}
	if result.TenantID != args[0] || result.WorkloadRunID != args[1] {
		return errors.New("Hako API returned an Agent publication with a mismatched identity")
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return errors.New("write Agent publication result")
	}
	return nil
}

func parseArtifactDownloadArgs(name string, args []string, positionalCount int) (string, []string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	outputPath := flags.String("out", "", "destination file path")
	flagArgs := make([]string, 0, 2)
	positionals := make([]string, 0, positionalCount)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out":
			if i+1 >= len(args) {
				return "", nil, errors.New("missing artifact output path")
			}
			flagArgs = append(flagArgs, arg, args[i+1])
			i++
		case strings.HasPrefix(arg, "--out="):
			flagArgs = append(flagArgs, arg)
		case strings.HasPrefix(arg, "-"):
			flagArgs = append(flagArgs, arg)
		default:
			positionals = append(positionals, arg)
		}
	}
	if err := flags.Parse(flagArgs); err != nil || flags.NArg() != 0 || len(positionals) != positionalCount || strings.TrimSpace(*outputPath) == "" {
		return "", nil, errors.New("invalid artifact download arguments")
	}
	return *outputPath, positionals, nil
}

func agentChangesArtifact(run domain.WorkloadRun) (domain.WorkloadArtifactSummary, error) {
	if run.Kind != domain.WorkloadAgent || run.State != domain.WorkloadSucceeded {
		return domain.WorkloadArtifactSummary{}, errors.New("Agent changes are available only for successful Agent Runs")
	}
	var found *domain.WorkloadArtifactSummary
	for i := range run.Artifacts {
		artifact := &run.Artifacts[i]
		if artifact.Kind != "agent_changes" {
			continue
		}
		if found != nil {
			return domain.WorkloadArtifactSummary{}, errors.New("Agent Run has ambiguous change proposals")
		}
		if artifact.Redacted || artifact.SizeBytes < 1 || artifact.SizeBytes > 8<<20 || len(artifact.SHA256) != 64 {
			return domain.WorkloadArtifactSummary{}, errors.New("Agent change proposal metadata is invalid")
		}
		if digest, err := hex.DecodeString(artifact.SHA256); err != nil || len(digest) != sha256.Size || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
			return domain.WorkloadArtifactSummary{}, errors.New("Agent change proposal digest is invalid")
		}
		found = artifact
	}
	if found == nil {
		return domain.WorkloadArtifactSummary{}, errors.New("successful Agent Run has no change proposal")
	}
	return *found, nil
}

func downloadWorkloadArtifact(ctx context.Context, apiURL, gatewayURL, accessToken, tenantID, runID, artifactID, outputPath string,
	client *http.Client) error {
	return downloadWorkloadArtifactExpected(ctx, apiURL, gatewayURL, accessToken, tenantID, runID, artifactID, outputPath, client, nil)
}

func downloadWorkloadArtifactExpected(ctx context.Context, apiURL, gatewayURL, accessToken, tenantID, runID, artifactID, outputPath string,
	client *http.Client, expected *domain.WorkloadArtifactSummary) error {
	if ctx == nil || strings.TrimSpace(outputPath) == "" || !validArtifactGatewayURL(gatewayURL) {
		return errors.New("artifact download configuration is invalid")
	}
	ticket, err := api.CreateWorkloadArtifactDownloadTicket(ctx, apiURL, accessToken, tenantID, runID, artifactID, client)
	if err != nil {
		return err
	}
	if expected != nil && (ticket.Kind != expected.Kind || ticket.SizeBytes != expected.SizeBytes || ticket.SHA256 != expected.SHA256) {
		return errors.New("artifact download ticket does not match the Workload Run metadata")
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	origin, _ := url.Parse(strings.TrimRight(strings.TrimSpace(gatewayURL), "/"))
	endpoint, err := url.JoinPath(origin.String(), "/v1/workload-artifacts/", url.PathEscape(artifactID))
	if err != nil {
		return errors.New("artifact Gateway URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("create artifact download request")
	}
	request.Header.Set("Authorization", "Bearer "+ticket.Ticket)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("download Workload artifact from Resource Plane Gateway")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Resource Plane Gateway rejected Workload artifact download")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" || response.Header.Get("X-Hako-Artifact-SHA256") != ticket.SHA256 ||
		response.ContentLength != ticket.SizeBytes {
		return errors.New("Resource Plane Gateway returned invalid Workload artifact metadata")
	}
	absolutePath, err := filepath.Abs(outputPath)
	if err != nil {
		return errors.New("artifact output path is invalid")
	}
	directory := filepath.Dir(absolutePath)
	temporary, err := os.CreateTemp(directory, ".hako-artifact-*")
	if err != nil {
		return errors.New("create temporary artifact file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return errors.New("set artifact file permissions")
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(response.Body, workloadartifactaccess.MaxArtifactSize+1))
	if copyErr != nil || written != ticket.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != ticket.SHA256 {
		_ = temporary.Close()
		return errors.New("Workload artifact size or SHA-256 verification failed")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return errors.New("flush artifact file")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close artifact file")
	}
	if err := os.Rename(temporaryPath, absolutePath); err != nil {
		return errors.New("install verified artifact file")
	}
	return nil
}

func validArtifactGatewayURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
