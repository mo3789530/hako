package main

import (
	"errors"
	"flag"
	"io"
)

type workspaceActionOptions struct {
	idempotencyKey string
	wait           bool
}

func parseWorkspaceActionOptions(action string, args []string) (workspaceActionOptions, error) {
	if action != "suspend" && action != "resume" && action != "delete" {
		return workspaceActionOptions{}, errors.New("unsupported Workspace action")
	}
	options := workspaceActionOptions{}
	flags := flag.NewFlagSet("hako "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.idempotencyKey, "idempotency-key", "", "idempotency key for retry")
	flags.BoolVar(&options.wait, "wait", false, "wait until the Workspace reaches the requested observed state")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return workspaceActionOptions{}, errors.New("invalid Workspace action options")
	}
	if options.wait && action != "resume" {
		return workspaceActionOptions{}, errors.New("--wait is supported only for resume")
	}
	return options, nil
}
