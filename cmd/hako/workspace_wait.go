package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
)

const (
	defaultWorkspaceWaitTimeout  = 10 * time.Minute
	defaultWorkspaceWaitInterval = 2 * time.Second
	minWorkspaceWaitInterval     = 250 * time.Millisecond
	maxWorkspaceWaitInterval     = 30 * time.Second
	maxWorkspaceWaitTimeout      = 30 * time.Minute
)

type workspaceGetter func(context.Context, string, string) (api.WorkspaceView, error)

type workspaceWaitOptions struct {
	tenantID    string
	workspaceID string
	target      string
	timeout     time.Duration
	interval    time.Duration
}

func parseWorkspaceWaitArgs(args []string) (workspaceWaitOptions, error) {
	if len(args) < 2 {
		return workspaceWaitOptions{}, errors.New("usage: hako wait <tenant-id> <workspace-id> [--state running|suspended|deleted] [--timeout 10m] [--interval 2s]")
	}
	options := workspaceWaitOptions{tenantID: args[0], workspaceID: args[1], target: "running",
		timeout: defaultWorkspaceWaitTimeout, interval: defaultWorkspaceWaitInterval}
	flags := flag.NewFlagSet("hako wait", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.target, "state", options.target, "observed state to wait for")
	flags.DurationVar(&options.timeout, "timeout", options.timeout, "maximum wait duration")
	flags.DurationVar(&options.interval, "interval", options.interval, "poll interval")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return workspaceWaitOptions{}, errors.New("usage: hako wait <tenant-id> <workspace-id> [--state running|suspended|deleted] [--timeout 10m] [--interval 2s]")
	}
	options.target = strings.TrimSpace(options.target)
	if options.target != "running" && options.target != "suspended" && options.target != "deleted" {
		return workspaceWaitOptions{}, errors.New("state must be running, suspended, or deleted")
	}
	if options.timeout < time.Second || options.timeout > maxWorkspaceWaitTimeout {
		return workspaceWaitOptions{}, errors.New("timeout must be between 1s and 30m")
	}
	if options.interval < minWorkspaceWaitInterval || options.interval > maxWorkspaceWaitInterval {
		return workspaceWaitOptions{}, errors.New("interval must be between 250ms and 30s")
	}
	return options, nil
}

func workspaceWaitCLI(ctx context.Context, args []string, stdout io.Writer) error {
	if ctx == nil || stdout == nil {
		return errors.New("context and status output are required")
	}
	options, err := parseWorkspaceWaitArgs(args)
	if err != nil {
		return err
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL := strings.TrimSpace(os.Getenv("HAKO_API_URL"))
	if apiURL == "" {
		return errors.New("HAKO_API_URL is required")
	}
	getter := func(ctx context.Context, tenantID, workspaceID string) (api.WorkspaceView, error) {
		return api.GetWorkspace(ctx, apiURL, accessToken, tenantID, workspaceID, nil)
	}
	return waitForWorkspaceState(ctx, options, getter, stdout, nil)
}

func waitForWorkspaceState(ctx context.Context, options workspaceWaitOptions, get workspaceGetter, output io.Writer, sleep func(context.Context, time.Duration) error) error {
	if ctx == nil || get == nil || output == nil || options.tenantID == "" || options.workspaceID == "" || options.target == "" ||
		options.timeout <= 0 || options.interval <= 0 {
		return errors.New("Workspace wait configuration is incomplete")
	}
	if sleep == nil {
		sleep = sleepWithContext
	}
	waitCtx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()
	lastObserved := ""
	for {
		view, err := get(waitCtx, options.tenantID, options.workspaceID)
		if err != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out waiting for Workspace %s to reach %s (last observed=%s)", options.workspaceID, options.target, lastObservedState(lastObserved))
			}
			return fmt.Errorf("get Workspace state: %w", err)
		}
		observed := strings.TrimSpace(view.Status.ObservedState)
		if observed == "" {
			return errors.New("Hako API returned an empty Workspace observed state")
		}
		if observed != lastObserved {
			fmt.Fprintf(output, "Workspace %s: desired=%s observed=%s\n", view.Workspace.ID, view.Status.DesiredState, observed)
			lastObserved = observed
		}
		if observed == options.target {
			return nil
		}
		if observed == "failed" {
			return fmt.Errorf("Workspace %s entered failed state while waiting for %s", options.workspaceID, options.target)
		}
		if observed == "deleted" && options.target != "deleted" {
			return fmt.Errorf("Workspace %s was deleted while waiting for %s", options.workspaceID, options.target)
		}
		if err := sleep(waitCtx, options.interval); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out waiting for Workspace %s to reach %s (last observed=%s)", options.workspaceID, options.target, observed)
			}
			return err
		}
	}
}

func sleepWithContext(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func lastObservedState(state string) string {
	if state == "" {
		return "unknown"
	}
	return state
}
