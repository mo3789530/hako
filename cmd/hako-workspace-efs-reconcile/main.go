package main

import (
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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

const maxJournalBindings = 100000

type journalBindingLister interface {
	ListStorageBindings(context.Context, int) ([]workspacestorage.WorkspaceBinding, error)
}

type clientsFactory func(context.Context, string, string, string) (workspacestorage.EFSAPI, journalBindingLister, error)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, defaultClients); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "workspace EFS reconciliation failed")
		os.Exit(1)
	}
}

func defaultClients(ctx context.Context, region, bucket, prefix string) (workspacestorage.EFSAPI, journalBindingLister, error) {
	config, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, nil, err
	}
	journalStore, err := workspacejournal.NewS3Store(s3.NewFromConfig(config), bucket, prefix)
	if err != nil {
		return nil, nil, err
	}
	return efs.NewFromConfig(config), journalStore, nil
}

func run(ctx context.Context, args []string, output io.Writer, newClients clientsFactory) error {
	if ctx == nil || output == nil || newClients == nil {
		return errors.New("invalid reconciliation dependencies")
	}
	flags := flag.NewFlagSet("hako-workspace-efs-reconcile", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	region := flags.String("region", "", "AWS Region containing the Workspace EFS filesystem")
	fileSystemID := flags.String("filesystem", "", "Workspace EFS filesystem ID")
	journalBucket := flags.String("journal-bucket", "", "S3 bucket containing Resource Plane Workspace runtime journals")
	journalPrefix := flags.String("journal-prefix", "", "dedicated S3 prefix containing Workspace runtime journals")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*region) == "" ||
		strings.TrimSpace(*fileSystemID) == "" || strings.TrimSpace(*journalBucket) == "" || strings.Trim(strings.TrimSpace(*journalPrefix), "/") == "" {
		return errors.New("region, filesystem, journal-bucket, and journal-prefix are required")
	}
	clientCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	api, journals, err := newClients(clientCtx, strings.TrimSpace(*region), strings.TrimSpace(*journalBucket), strings.Trim(strings.TrimSpace(*journalPrefix), "/"))
	if err != nil {
		return errors.New("AWS read-only client initialization failed")
	}
	if journals == nil {
		return errors.New("Workspace journal listing is unavailable")
	}
	storage, err := workspacestorage.NewEFS(api, strings.TrimSpace(*fileSystemID), strings.TrimSpace(*region))
	if err != nil {
		return errors.New("Workspace EFS configuration is invalid")
	}
	bindings, err := journals.ListStorageBindings(clientCtx, maxJournalBindings)
	if err != nil {
		return errors.New("Workspace journal listing could not complete")
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
