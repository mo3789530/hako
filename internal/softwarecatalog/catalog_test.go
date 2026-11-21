package softwarecatalog

import (
	"errors"
	"testing"
)

func TestValidateSoftwareCatalogService(t *testing.T) {
	base := Service{TenantID: "tenant_a", ServiceID: "payments-api", OwnerTeam: "payments", InstallationID: 12,
		GitHubRepositoryID: 34, Runtime: "lambda-microvm", Language: "go", Dependencies: []string{"redis", "user-api"},
		ProductionURL: "https://payments.example.com/api"}
	if err := Validate(base); err != nil {
		t.Fatalf("valid catalog service rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Service){
		"invalid service id":       func(s *Service) { s.ServiceID = "../payments" },
		"invalid tenant id":        func(s *Service) { s.TenantID = "tenant/a" },
		"unsupported runtime":      func(s *Service) { s.Runtime = "ec2" },
		"unsupported language":     func(s *Service) { s.Language = "shell" },
		"self dependency":          func(s *Service) { s.Dependencies = []string{s.ServiceID} },
		"unsorted dependencies":    func(s *Service) { s.Dependencies = []string{"user-api", "redis"} },
		"duplicate dependencies":   func(s *Service) { s.Dependencies = []string{"redis", "redis"} },
		"secret-bearing URL":       func(s *Service) { s.ProductionURL = "https://user:password@example.com" },
		"non HTTPS production URL": func(s *Service) { s.ProductionURL = "http://payments.example.com" },
		"URL fragment":             func(s *Service) { s.ProductionURL = "https://payments.example.com/#token" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Dependencies = append([]string(nil), base.Dependencies...)
			mutate(&candidate)
			if err := Validate(candidate); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNormalizeSortsCopyWithoutChangingInput(t *testing.T) {
	input := Service{Dependencies: []string{"zeta", "alpha"}}
	got := Normalize(input)
	if got.Dependencies[0] != "alpha" || got.Dependencies[1] != "zeta" || input.Dependencies[0] != "zeta" {
		t.Fatalf("Normalize() = %#v, input = %#v", got.Dependencies, input.Dependencies)
	}
}
