// Package servicecatalog defines the versioned, built-in development service
// catalog. Templates describe private Workspace-local services; they do not
// publish ports or execute user-supplied commands.
package servicecatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const SchemaVersion = 1

var (
	ErrUnknownTemplate = errors.New("service catalog template is not available")
	ErrInvalidTemplate = errors.New("invalid service catalog template")
	namePattern        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	versionPattern     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	imagePattern       = regexp.MustCompile(`^docker\.io/library/(postgres|redis):([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:-alpine[0-9]+\.[0-9]+)?)$`)
	environmentPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

type Resources struct {
	MemoryBytes int64 `json:"memory_bytes"`
	MilliCPU    int   `json:"milli_cpu"`
}

type Port struct {
	Number   uint16 `json:"number"`
	Protocol string `json:"protocol"`
}

type Volume struct {
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"`
	Persistent bool   `json:"persistent"`
}

type HealthCheck struct {
	Command  []string `json:"command"`
	Interval string   `json:"interval"`
	Timeout  string   `json:"timeout"`
	Retries  int      `json:"retries"`
}

// Template is a versioned catalog contract. Ports are private Workspace
// listeners only; there is intentionally no hostPort/publicIngress field.
type Template struct {
	SchemaVersion    int               `json:"schema_version"`
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Image            string            `json:"image"`
	Resources        Resources         `json:"resources"`
	Ports            []Port            `json:"ports"`
	Volumes          []Volume          `json:"volumes"`
	HealthCheck      HealthCheck       `json:"healthcheck"`
	Environment      map[string]string `json:"environment,omitempty"`
	GeneratedSecrets []string          `json:"generated_secrets,omitempty"`
	Network          string            `json:"network"`
	Privileged       bool              `json:"privileged"`
}

var builtins = map[string]Template{
	"postgres": {
		SchemaVersion: SchemaVersion, Name: "postgres", Version: "1.0.0",
		Image:            "docker.io/library/postgres:18.6-alpine3.23",
		Resources:        Resources{MemoryBytes: 1 << 30, MilliCPU: 750},
		Ports:            []Port{{Number: 5432, Protocol: "tcp"}},
		Volumes:          []Volume{{Name: "postgres-data", MountPath: "/var/lib/postgresql/data", Persistent: true}},
		HealthCheck:      HealthCheck{Command: []string{"pg_isready", "-U", "postgres"}, Interval: "10s", Timeout: "3s", Retries: 5},
		Environment:      map[string]string{"POSTGRES_DB": "postgres", "POSTGRES_USER": "postgres"},
		GeneratedSecrets: []string{"POSTGRES_PASSWORD"},
		Network:          "workspace-private",
	},
	"redis": {
		SchemaVersion: SchemaVersion, Name: "redis", Version: "1.0.0",
		Image:       "docker.io/library/redis:8.10.2-alpine3.23",
		Resources:   Resources{MemoryBytes: 256 << 20, MilliCPU: 250},
		Ports:       []Port{{Number: 6379, Protocol: "tcp"}},
		HealthCheck: HealthCheck{Command: []string{"redis-cli", "ping"}, Interval: "10s", Timeout: "3s", Retries: 5},
		Network:     "workspace-private",
	},
}

// List returns validated templates sorted by their stable catalog name.
func List() []Template {
	result := make([]Template, 0, len(builtins))
	for _, template := range builtins {
		result = append(result, clone(template))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func Resolve(name string) (Template, error) {
	template, ok := builtins[strings.TrimSpace(name)]
	if !ok {
		return Template{}, ErrUnknownTemplate
	}
	template = clone(template)
	if err := Validate(template); err != nil {
		return Template{}, err
	}
	return template, nil
}

// ValidateInstanceName checks the stable, Workspace-local name used by the
// Service API and runtime agent.
func ValidateInstanceName(name string) bool {
	return namePattern.MatchString(strings.TrimSpace(name))
}

// Decode accepts one strict JSON Template document. Catalog input is
// administrative configuration, not a workload-provided override.
func Decode(reader io.Reader) (Template, error) {
	if reader == nil {
		return Template{}, ErrInvalidTemplate
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<10))
	decoder.DisallowUnknownFields()
	var template Template
	if err := decoder.Decode(&template); err != nil {
		return Template{}, fmt.Errorf("decode service catalog template: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Template{}, fmt.Errorf("decode service catalog template trailing data: %w", ErrInvalidTemplate)
	}
	if err := Validate(template); err != nil {
		return Template{}, err
	}
	return template, nil
}

func Validate(template Template) error {
	if template.SchemaVersion != SchemaVersion || !namePattern.MatchString(template.Name) || !versionPattern.MatchString(template.Version) ||
		!imagePattern.MatchString(template.Image) || template.Network != "workspace-private" || template.Privileged ||
		template.Resources.MemoryBytes < 64<<20 || template.Resources.MemoryBytes > 8<<30 ||
		template.Resources.MilliCPU < 50 || template.Resources.MilliCPU > 8000 ||
		len(template.Ports) == 0 || len(template.Ports) > 16 || len(template.HealthCheck.Command) == 0 ||
		len(template.HealthCheck.Command) > 16 || template.HealthCheck.Retries < 1 || template.HealthCheck.Retries > 20 ||
		!validDuration(template.HealthCheck.Interval) || !validDuration(template.HealthCheck.Timeout) {
		return ErrInvalidTemplate
	}
	seenPorts := make(map[uint16]struct{}, len(template.Ports))
	for _, port := range template.Ports {
		if port.Number == 0 || port.Protocol != "tcp" {
			return ErrInvalidTemplate
		}
		if _, exists := seenPorts[port.Number]; exists {
			return ErrInvalidTemplate
		}
		seenPorts[port.Number] = struct{}{}
	}
	seenVolumes := make(map[string]struct{}, len(template.Volumes))
	for _, volume := range template.Volumes {
		if !namePattern.MatchString(volume.Name) || !path.IsAbs(volume.MountPath) || path.Clean(volume.MountPath) != volume.MountPath ||
			volume.MountPath == "/" || strings.HasPrefix(volume.MountPath, "/proc") || strings.HasPrefix(volume.MountPath, "/sys") ||
			strings.HasPrefix(volume.MountPath, "/dev") {
			return ErrInvalidTemplate
		}
		if _, exists := seenVolumes[volume.Name]; exists {
			return ErrInvalidTemplate
		}
		seenVolumes[volume.Name] = struct{}{}
	}
	for _, argument := range template.HealthCheck.Command {
		if strings.TrimSpace(argument) == "" || len(argument) > 256 {
			return ErrInvalidTemplate
		}
	}
	if template.HealthCheck.Command[0] == "/bin/sh" || template.HealthCheck.Command[0] == "sh" || template.HealthCheck.Command[0] == "bash" {
		return ErrInvalidTemplate
	}
	for key, value := range template.Environment {
		if !environmentPattern.MatchString(key) || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
			return ErrInvalidTemplate
		}
	}
	for _, name := range template.GeneratedSecrets {
		if !environmentPattern.MatchString(name) {
			return ErrInvalidTemplate
		}
		if _, literal := template.Environment[name]; literal {
			return ErrInvalidTemplate
		}
	}
	return nil
}

func validDuration(value string) bool {
	if value == "" || len(value) > 16 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	duration, err := time.ParseDuration(value)
	return err == nil && duration > 0 && duration <= time.Hour
}

func clone(template Template) Template {
	template.Ports = append([]Port(nil), template.Ports...)
	template.Volumes = append([]Volume(nil), template.Volumes...)
	template.HealthCheck.Command = append([]string(nil), template.HealthCheck.Command...)
	template.GeneratedSecrets = append([]string(nil), template.GeneratedSecrets...)
	if template.Environment != nil {
		environment := make(map[string]string, len(template.Environment))
		for key, value := range template.Environment {
			environment[key] = value
		}
		template.Environment = environment
	}
	return template
}

// CanonicalJSON serializes a validated template for persistence as immutable
// configuration metadata. It contains generated-secret declarations only,
// never generated credential values.
func CanonicalJSON(template Template) ([]byte, error) {
	if err := Validate(template); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(template); err != nil {
		return nil, fmt.Errorf("encode service catalog template: %w", err)
	}
	return bytes.TrimSpace(buffer.Bytes()), nil
}
