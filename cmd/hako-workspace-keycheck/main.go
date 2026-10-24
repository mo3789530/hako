// hako-workspace-keycheck validates the public ticket keys copied into a
// Lambda MicroVM Workspace image. It is used by the image build target.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mo3789530/hako/internal/workloadagent"
)

func main() {
	keyFile := flag.String("file", "", "image-owned Workspace Agent public key JSON file")
	requiredKeyID := flag.String("require-id", "", "required Resource Plane Guest-ticket signing key ID")
	flag.Parse()
	if *keyFile == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Workspace Agent key validation requires -file and no positional arguments")
		os.Exit(2)
	}
	file, err := os.Open(*keyFile)
	if err == nil {
		err = workloadagent.ValidateTrustedKeySet(file, *requiredKeyID)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Workspace Agent public keys are invalid or do not contain the required signer ID")
		os.Exit(1)
	}
}
