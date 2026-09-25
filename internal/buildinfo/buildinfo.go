// Package buildinfo contains values injected into Hako binaries at build time.
package buildinfo

// Version is overridden by release builds using -ldflags. Local builds use
// the development value.
var Version = "dev"
