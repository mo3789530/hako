package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/mo3789530/hako/internal/factoryconfig"
)

type factoryValidateCLIInput struct {
	path string
}

func parseFactoryValidateCLI(args []string) (factoryValidateCLIInput, error) {
	input := factoryValidateCLIInput{path: ".hako/factory.yaml"}
	flags := flag.NewFlagSet("factory validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.path, "file", input.path, "factory manifest path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.path) == "" {
		return factoryValidateCLIInput{}, errors.New("usage: hako factory validate [--file .hako/factory.yaml]")
	}
	return input, nil
}

func validateFactoryManifestCLI(args []string, stdout io.Writer) error {
	if stdout == nil {
		return errors.New("output writer is required")
	}
	input, err := parseFactoryValidateCLI(args)
	if err != nil {
		return err
	}
	file, err := os.Open(input.path)
	if err != nil {
		return errors.New("cannot open factory manifest")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, factoryconfig.MaxManifestBytes+1))
	if err != nil {
		return errors.New("cannot read factory manifest")
	}
	config, err := factoryconfig.Parse(data)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(config.Pipelines))
	for name := range config.Pipelines {
		names = append(names, name)
	}
	sort.Strings(names)
	if _, err := fmt.Fprintf(stdout, "Valid Hako Factory manifest v%d (%d pipelines): %s\n", config.Version, len(names), strings.Join(names, ", ")); err != nil {
		return errors.New("write factory manifest result")
	}
	return nil
}
