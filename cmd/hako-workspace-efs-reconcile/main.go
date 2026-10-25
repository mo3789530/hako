package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

const maxJournalInputBytes = 16 << 20

type efsClientFactory func(context.Context, string) (workspacestorage.EFSAPI, error)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, defaultEFSClient); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "workspace EFS reconciliation failed")
		os.Exit(1)
	}
}

func defaultEFSClient(ctx context.Context, region string) (workspacestorage.EFSAPI, error) {
	config, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return efs.NewFromConfig(config), nil
}

func run(ctx context.Context, args []string, output io.Writer, newClient efsClientFactory) error {
	if ctx == nil || output == nil || newClient == nil {
		return errors.New("invalid reconciliation dependencies")
	}
	flags := flag.NewFlagSet("hako-workspace-efs-reconcile", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	region := flags.String("region", "", "AWS Region containing the Workspace EFS filesystem")
	fileSystemID := flags.String("filesystem", "", "Workspace EFS filesystem ID")
	bindingsPath := flags.String("bindings", "", "JSON export of durable Workspace journal bindings")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*region) == "" ||
		strings.TrimSpace(*fileSystemID) == "" || strings.TrimSpace(*bindingsPath) == "" {
		return errors.New("region, filesystem, and bindings are required")
	}
	bindings, err := readBindings(*bindingsPath)
	if err != nil {
		return err
	}
	clientCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	api, err := newClient(clientCtx, strings.TrimSpace(*region))
	if err != nil {
		return errors.New("AWS EFS client initialization failed")
	}
	storage, err := workspacestorage.NewEFS(api, strings.TrimSpace(*fileSystemID), strings.TrimSpace(*region))
	if err != nil {
		return errors.New("Workspace EFS configuration is invalid")
	}
	entries, err := storage.Reconcile(clientCtx, bindings)
	if err != nil {
		return errors.New("Workspace EFS reconciliation could not complete")
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(struct {
		FileSystemID string                                 `json:"file_system_id"`
		Region       string                                 `json:"region"`
		ReadOnly     bool                                   `json:"read_only"`
		Findings     []workspacestorage.ReconciliationEntry `json:"findings"`
	}{FileSystemID: strings.TrimSpace(*fileSystemID), Region: strings.TrimSpace(*region), ReadOnly: true, Findings: entries}); err != nil {
		return errors.New("Workspace EFS reconciliation report could not be encoded")
	}
	return nil
}

func readBindings(path string) ([]workspacestorage.WorkspaceBinding, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open Workspace journal binding export")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJournalInputBytes+1))
	if err != nil || len(data) > maxJournalInputBytes {
		return nil, errors.New("Workspace journal binding export is unreadable or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var bindings []workspacestorage.WorkspaceBinding
	if err := decoder.Decode(&bindings); err != nil || bindings == nil {
		return nil, errors.New("Workspace journal binding export is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("Workspace journal binding export has trailing data")
	}
	if len(bindings) > 100000 {
		return nil, errors.New("Workspace journal binding export has too many entries")
	}
	return bindings, nil
}
