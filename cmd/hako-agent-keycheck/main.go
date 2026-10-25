// hako-agent-keycheck validates the public ticket keys copied into a Hako
// Lambda MicroVM Agent image. Private signing material belongs in a trusted
// Resource Plane service and must never be included in the image.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mo3789530/hako/internal/workloadagent"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("hako-agent-keycheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyFile := flags.String("file", "", "image-owned Hako Agent public key JSON file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *keyFile == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Hako Agent key validation requires -file and no positional arguments")
		return 2
	}
	file, err := os.Open(*keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "Hako Agent public keys are invalid")
		return 1
	}
	err = workloadagent.ValidateTrustedKeySet(file, "")
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		fmt.Fprintln(stderr, "Hako Agent public keys are invalid")
		return 1
	}
	return 0
}
