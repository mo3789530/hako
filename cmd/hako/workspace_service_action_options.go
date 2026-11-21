package main

import "errors"

type workspaceServiceActionOptions struct {
	yes bool
}

func parseWorkspaceServiceActionOptions(action string, args []string) (workspaceServiceActionOptions, error) {
	options := workspaceServiceActionOptions{}
	if len(args) == 0 {
		return options, nil
	}
	if len(args) != 1 || args[0] != "--yes" || action != "remove" {
		return workspaceServiceActionOptions{}, errors.New("--yes is supported only for service remove")
	}
	options.yes = true
	return options, nil
}
