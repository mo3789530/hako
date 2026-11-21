// Package softwarecatalog defines Tenant-owned service metadata for the Hako
// Software Factory. It contains references to repositories, never credentials.
package softwarecatalog

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	ErrInvalid       = errors.New("invalid Software Catalog service")
	serviceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	tenantIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	ownerTeamPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	languagePattern  = regexp.MustCompile(`^[a-z][a-z0-9+#.-]{0,31}$`)
	allowedLanguages = map[string]struct{}{"go": {}, "java": {}, "node": {}, "python": {}, "rust": {}, "dotnet": {}, "other": {}}
)

const MaxDependencies = 16

// Service is a non-secret catalog entry bound to one Tenant-registered GitHub
// Repository. Runtime remains Lambda MicroVM; EC2 is intentionally excluded.
type Service struct {
	TenantID           string   `json:"tenant_id"`
	ServiceID          string   `json:"service_id"`
	OwnerTeam          string   `json:"owner_team"`
	InstallationID     int64    `json:"installation_id"`
	GitHubRepositoryID int64    `json:"github_repository_id"`
	Repository         string   `json:"repository"`
	Runtime            string   `json:"runtime"`
	Language           string   `json:"language"`
	Dependencies       []string `json:"dependencies"`
	ProductionURL      string   `json:"production_url,omitempty"`
}

func Validate(service Service) error {
	if !tenantIDPattern.MatchString(service.TenantID) || !serviceIDPattern.MatchString(service.ServiceID) ||
		!ownerTeamPattern.MatchString(service.OwnerTeam) || service.InstallationID <= 0 || service.GitHubRepositoryID <= 0 ||
		service.Runtime != "lambda-microvm" || !languagePattern.MatchString(service.Language) {
		return ErrInvalid
	}
	if _, ok := allowedLanguages[service.Language]; !ok || len(service.Dependencies) > MaxDependencies {
		return ErrInvalid
	}
	for i, dependency := range service.Dependencies {
		if !serviceIDPattern.MatchString(dependency) || dependency == service.ServiceID || (i > 0 && service.Dependencies[i-1] >= dependency) {
			return ErrInvalid
		}
	}
	if service.ProductionURL != "" {
		parsed, err := url.Parse(service.ProductionURL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
			strings.ContainsAny(service.ProductionURL, "\x00\r\n") {
			return ErrInvalid
		}
	}
	return nil
}

// Normalize sorts a copy of dependencies for CLI/API callers that want a
// canonical payload. Validation itself still rejects duplicates.
func Normalize(service Service) Service {
	service.Dependencies = append([]string(nil), service.Dependencies...)
	sort.Strings(service.Dependencies)
	return service
}
