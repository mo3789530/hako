package githubwebhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const RepositoryEventSchemaVersion = 1

var commitSHAPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// RepositoryEvent is the versioned, GitHub-independent subset Hako stores for
// routing and future processing. Raw provider payloads are not copied here.
type RepositoryEvent struct {
	SchemaVersion       int             `json:"schema_version"`
	DeliveryID          string          `json:"delivery_id"`
	Type                string          `json:"type"`
	GitHubEvent         string          `json:"github_event"`
	Action              string          `json:"action,omitempty"`
	InstallationID      int64           `json:"installation_id,omitempty"`
	Repository          *RepositoryRef  `json:"repository,omitempty"`
	RepositoriesAdded   []RepositoryRef `json:"repositories_added,omitempty"`
	RepositoriesRemoved []RepositoryRef `json:"repositories_removed,omitempty"`
	Ref                 string          `json:"ref,omitempty"`
	CommitSHA           string          `json:"commit_sha,omitempty"`
	Actor               string          `json:"actor,omitempty"`
	PullRequest         *PullRequestRef `json:"pull_request,omitempty"`
	WorkflowJob         *WorkflowJobRef `json:"workflow_job,omitempty"`
}

type RepositoryRef struct {
	GitHubID      int64  `json:"github_id"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

type PullRequestRef struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
	BaseRef string `json:"base_ref"`
}

type WorkflowJobRef struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Name       string `json:"name"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

type githubPayload struct {
	Action       string `json:"action"`
	Ref          string `json:"ref"`
	After        string `json:"after"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	} `json:"installation"`
	Repository          *githubRepository  `json:"repository"`
	RepositoriesAdded   []githubRepository `json:"repositories_added"`
	RepositoriesRemoved []githubRepository `json:"repositories_removed"`
	Sender              struct {
		Login string `json:"login"`
	} `json:"sender"`
	PullRequest struct {
		Number int64  `json:"number"`
		State  string `json:"state"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	WorkflowJob struct {
		ID         int64  `json:"id"`
		RunID      int64  `json:"run_id"`
		Name       string `json:"name"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"workflow_job"`
}

type githubRepository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Normalize converts an authenticated GitHub delivery into the stable Hako
// event schema and validates identifiers needed by later tenant resolution.
func Normalize(delivery VerifiedDelivery) (RepositoryEvent, error) {
	if !validDeliveryID(delivery.DeliveryID) || len(delivery.Payload) == 0 || !supportedEventAction(delivery.Event, delivery.Action) {
		return RepositoryEvent{}, fmt.Errorf("%w: incomplete or unsupported verified delivery", ErrInvalidDelivery)
	}
	decoder := json.NewDecoder(bytes.NewReader(delivery.Payload))
	var payload githubPayload
	if err := decoder.Decode(&payload); err != nil || payload.Action != delivery.Action {
		return RepositoryEvent{}, fmt.Errorf("%w: decode GitHub event payload", ErrInvalidDelivery)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RepositoryEvent{}, fmt.Errorf("%w: payload must contain exactly one JSON object", ErrInvalidDelivery)
	}
	event := RepositoryEvent{
		SchemaVersion: RepositoryEventSchemaVersion,
		DeliveryID:    delivery.DeliveryID,
		GitHubEvent:   delivery.Event,
		Action:        delivery.Action,
		Actor:         boundedText(payload.Sender.Login, 100),
	}
	switch delivery.Event {
	case "push":
		repository, err := normalizeRepository(payload.Repository)
		if err != nil || payload.Installation.ID <= 0 || !validRef(payload.Ref) || !validSHA(payload.After) {
			return RepositoryEvent{}, fmt.Errorf("%w: push requires installation, repository, ref, and commit SHA", ErrInvalidDelivery)
		}
		event.Type = "repository.push"
		event.Repository, event.Ref, event.CommitSHA = repository, payload.Ref, strings.ToLower(payload.After)
		event.InstallationID = payload.Installation.ID
	case "pull_request":
		repository, err := normalizeRepository(payload.Repository)
		pr := payload.PullRequest
		if err != nil || payload.Installation.ID <= 0 || pr.Number <= 0 || !validRef(pr.Head.Ref) || !validSHA(pr.Head.SHA) || !validRef(pr.Base.Ref) || (pr.State != "open" && pr.State != "closed") {
			return RepositoryEvent{}, fmt.Errorf("%w: pull request requires installation, repository, and valid pull request metadata", ErrInvalidDelivery)
		}
		event.Type = "repository.pull_request." + delivery.Action
		event.Repository, event.InstallationID = repository, payload.Installation.ID
		event.Ref, event.CommitSHA = pr.Head.Ref, strings.ToLower(pr.Head.SHA)
		event.PullRequest = &PullRequestRef{Number: pr.Number, State: pr.State, HeadRef: pr.Head.Ref, HeadSHA: strings.ToLower(pr.Head.SHA), BaseRef: pr.Base.Ref}
	case "workflow_job":
		repository, err := normalizeRepository(payload.Repository)
		job := payload.WorkflowJob
		if err != nil || payload.Installation.ID <= 0 || job.ID <= 0 || job.RunID <= 0 || boundedText(job.Name, 200) == "" || !validRef(job.HeadBranch) || !validSHA(job.HeadSHA) || !oneOf(job.Status, "queued", "in_progress", "completed") {
			return RepositoryEvent{}, fmt.Errorf("%w: workflow job requires installation, repository, and valid job metadata", ErrInvalidDelivery)
		}
		event.Type = "repository.workflow_job." + delivery.Action
		event.Repository, event.InstallationID = repository, payload.Installation.ID
		event.Ref, event.CommitSHA = job.HeadBranch, strings.ToLower(job.HeadSHA)
		event.WorkflowJob = &WorkflowJobRef{ID: job.ID, RunID: job.RunID, Name: boundedText(job.Name, 200), HeadBranch: job.HeadBranch, HeadSHA: strings.ToLower(job.HeadSHA), Status: job.Status, Conclusion: boundedText(job.Conclusion, 100)}
	case "installation":
		if payload.Installation.ID <= 0 {
			return RepositoryEvent{}, fmt.Errorf("%w: installation event requires installation ID", ErrInvalidDelivery)
		}
		event.Type, event.InstallationID = "github.installation."+delivery.Action, payload.Installation.ID
		event.Actor = boundedText(payload.Installation.Account.Login, 100)
	case "installation_repositories":
		if payload.Installation.ID <= 0 {
			return RepositoryEvent{}, fmt.Errorf("%w: repository installation event requires installation ID", ErrInvalidDelivery)
		}
		added, err := normalizeRepositories(payload.RepositoriesAdded)
		if err != nil {
			return RepositoryEvent{}, err
		}
		removed, err := normalizeRepositories(payload.RepositoriesRemoved)
		if err != nil {
			return RepositoryEvent{}, err
		}
		if (delivery.Action == "added" && len(added) == 0) || (delivery.Action == "removed" && len(removed) == 0) {
			return RepositoryEvent{}, fmt.Errorf("%w: installation repository event is missing changed repositories", ErrInvalidDelivery)
		}
		event.Type = "github.installation_repositories." + delivery.Action
		event.InstallationID, event.RepositoriesAdded, event.RepositoriesRemoved = payload.Installation.ID, added, removed
	default:
		return RepositoryEvent{}, fmt.Errorf("%w: unsupported event %q", ErrInvalidDelivery, delivery.Event)
	}
	return event, nil
}

func normalizeRepository(repository *githubRepository) (*RepositoryRef, error) {
	if repository == nil || repository.ID <= 0 {
		return nil, fmt.Errorf("%w: repository ID is required", ErrInvalidDelivery)
	}
	owner, name := boundedText(repository.Owner.Login, 100), boundedText(repository.Name, 100)
	if owner == "" || name == "" {
		return nil, fmt.Errorf("%w: repository owner and name are required", ErrInvalidDelivery)
	}
	return &RepositoryRef{GitHubID: repository.ID, Owner: owner, Name: name, DefaultBranch: boundedText(repository.DefaultBranch, 255)}, nil
}

func normalizeRepositories(repositories []githubRepository) ([]RepositoryRef, error) {
	if len(repositories) > 1000 {
		return nil, fmt.Errorf("%w: too many repositories in installation event", ErrInvalidDelivery)
	}
	result := make([]RepositoryRef, 0, len(repositories))
	for index := range repositories {
		repository, err := normalizeRepository(&repositories[index])
		if err != nil {
			return nil, err
		}
		result = append(result, *repository)
	}
	return result, nil
}

func boundedText(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return ""
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return ""
		}
	}
	return value
}

func validRef(value string) bool {
	return boundedText(value, 1024) != "" && !strings.Contains(value, "..")
}

func validSHA(value string) bool { return commitSHAPattern.MatchString(value) }
