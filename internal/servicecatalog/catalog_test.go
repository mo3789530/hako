package servicecatalog

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltinCatalogIsVersionedPrivateAndBounded(t *testing.T) {
	templates := List()
	if len(templates) != 2 || templates[0].Name != "postgres" || templates[1].Name != "redis" {
		t.Fatalf("catalog order/content = %#v", templates)
	}
	for _, template := range templates {
		if err := Validate(template); err != nil {
			t.Errorf("%s: %v", template.Name, err)
		}
		if template.SchemaVersion != SchemaVersion || template.Version == "" || template.Network != "workspace-private" || template.Privileged {
			t.Errorf("unsafe/incomplete template metadata: %+v", template)
		}
		if !imagePattern.MatchString(template.Image) {
			t.Errorf("image is not an immutable ARM64 digest reference: %q", template.Image)
		}
		for _, port := range template.Ports {
			if port.Protocol != "tcp" || port.Number == 0 {
				t.Errorf("invalid private service port: %+v", port)
			}
		}
		for _, volume := range template.Volumes {
			if strings.HasPrefix(volume.MountPath, "/proc") || strings.HasPrefix(volume.MountPath, "/sys") || strings.HasPrefix(volume.MountPath, "/dev") {
				t.Errorf("unsafe mount path: %+v", volume)
			}
		}
	}
}

func TestResolveReturnsDefensiveTemplateCopies(t *testing.T) {
	first, err := Resolve("postgres")
	if err != nil {
		t.Fatal(err)
	}
	first.Ports[0].Number = 1
	first.Environment["POSTGRES_USER"] = "attacker"
	second, err := Resolve("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if second.Ports[0].Number != 5432 || second.Environment["POSTGRES_USER"] != "postgres" {
		t.Fatalf("catalog backing template mutated: %+v", second)
	}
	if _, err := Resolve("mysql"); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown template error = %v", err)
	}
}

func TestPostgresTemplateDeclaresGeneratedPasswordWithoutStoringValue(t *testing.T) {
	postgres, err := Resolve("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(postgres.GeneratedSecrets) != 1 || postgres.GeneratedSecrets[0] != "POSTGRES_PASSWORD" {
		t.Fatalf("postgres secret contract = %#v", postgres.GeneratedSecrets)
	}
	if _, literal := postgres.Environment["POSTGRES_PASSWORD"]; literal {
		t.Fatal("catalog must not store a plaintext database password")
	}
	encoded, err := CanonicalJSON(postgres)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password value") || !strings.Contains(string(encoded), `"generated_secrets":["POSTGRES_PASSWORD"]`) {
		t.Fatalf("canonical config violates generated-secret contract: %s", encoded)
	}
}

func TestDecodeRejectsUnknownFieldsAndUnsafeTemplates(t *testing.T) {
	base, err := Resolve("redis")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := CanonicalJSON(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(strings.NewReader(strings.TrimSuffix(string(encoded), "}") + `,"host_port":6379}`)); err == nil {
		t.Fatal("unknown host port field accepted")
	}
	if _, err := Decode(strings.NewReader(string(encoded) + ` {}`)); err == nil {
		t.Fatal("multiple JSON values accepted")
	}
	for _, image := range []string{
		"docker.io/library/redis:8.10.2-alpine3.23",
		"docker.io/library/redis:latest",
		"docker.io/library/redis@sha256:ABCDEF",
		"docker.io/library/postgres@sha256:" + strings.Repeat("a", 63),
	} {
		base.Image = image
		if err := Validate(base); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("mutable or malformed image reference accepted (%q): %v", image, err)
		}
	}
	base, _ = Resolve("redis")
	base.HealthCheck.Command = []string{"/bin/sh", "-c", "echo unsafe"}
	if err := Validate(base); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("shell health check accepted: %v", err)
	}
	base, _ = Resolve("redis")
	base.Volumes = []Volume{{Name: "host", MountPath: "/proc/1/root", Persistent: true}}
	if err := Validate(base); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("unsafe host filesystem mount accepted: %v", err)
	}
}
