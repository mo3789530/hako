package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
)

func downloadWorkloadArtifactCLI(args []string) error {
	flags := flag.NewFlagSet("hako workload artifact download", flag.ContinueOnError)
	outputPath := flags.String("out", "", "destination file path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 3 || strings.TrimSpace(*outputPath) == "" {
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
		flags.Arg(0), flags.Arg(1), flags.Arg(2), *outputPath, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "Workload artifact %s saved to %s\n", flags.Arg(2), *outputPath)
	return err
}

func downloadWorkloadArtifact(ctx context.Context, apiURL, gatewayURL, accessToken, tenantID, runID, artifactID, outputPath string,
	client *http.Client) error {
	if ctx == nil || strings.TrimSpace(outputPath) == "" || !validArtifactGatewayURL(gatewayURL) {
		return errors.New("artifact download configuration is invalid")
	}
	ticket, err := api.CreateWorkloadArtifactDownloadTicket(ctx, apiURL, accessToken, tenantID, runID, artifactID, client)
	if err != nil {
		return err
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
