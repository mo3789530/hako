//go:build !linux

package main

import "io"

func enableWorkspaceTerminal(io.Reader, io.Writer, func(uint16, uint16) error) (func(), error) {
	return func() {}, nil
}
