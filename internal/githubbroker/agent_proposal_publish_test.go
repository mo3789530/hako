package githubbroker

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/githubapp"
)

const (
	proposalBaseCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	proposalTreeSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	proposalBlobSHA    = "cccccccccccccccccccccccccccccccccccccccc"
	proposalCommitSHA  = "dddddddddddddddddddddddddddddddddddddddd"
)

type fakeAgentProposalToken struct {
	value     string
	expiresAt time.Time
}

func (t fakeAgentProposalToken) Value() string        { return t.value }
func (t fakeAgentProposalToken) ExpiresAt() time.Time { return t.expiresAt }

func TestAgentProposalGitHubWriterCreatesAndRecoversOneBranchAndPR(t *testing.T) {
	now := time.Now().UTC()
	archive := proposalTestArchive(t)
	proposalSHA := proposalArchiveSHA(archive)
	branchExists, pullRequestExists := false, false
	branchRefWrites, pullRequestWrites, blobWrites, treeWrites := 0, 0, 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-write-token" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("GitHub request missing expected broker auth/version: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/ref/heads/main":
			_, _ = fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q}}`, proposalBaseCommit)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/ref/heads/hako/agent/proposal-abc":
			if !branchExists {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"ref":"refs/heads/hako/agent/proposal-abc","object":{"sha":%q}}`, proposalCommitSHA)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/commits/"+proposalBaseCommit:
			_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}`, proposalBaseCommit)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/commits/"+proposalCommitSHA:
			_, _ = fmt.Fprintf(w, `{"sha":%q,"message":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`,
				proposalCommitSHA, proposalCommitMessage(testProposalRequest(proposalSHA, archive)), proposalTreeSHA, proposalBaseCommit)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/blobs":
			blobWrites++
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["encoding"] != "base64" ||
				string(mustDecodeBase64(t, body["content"])) != "package main\n" {
				t.Errorf("unexpected blob payload: %#v", body)
			}
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalBlobSHA)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/trees":
			treeWrites++
			var body struct {
				BaseTree string `json:"base_tree"`
				Tree     []struct {
					Path string `json:"path"`
					Mode string `json:"mode"`
					Type string `json:"type"`
					SHA  string `json:"sha"`
				} `json:"tree"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.BaseTree != "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" || len(body.Tree) != 1 ||
				body.Tree[0].Path != "main.go" || body.Tree[0].Mode != "100644" || body.Tree[0].Type != "blob" || body.Tree[0].SHA != proposalBlobSHA {
				t.Errorf("unexpected Git tree payload: %+v", body)
			}
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalTreeSHA)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/commits":
			var body struct {
				Message string   `json:"message"`
				Tree    string   `json:"tree"`
				Parents []string `json:"parents"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Tree != proposalTreeSHA || len(body.Parents) != 1 || body.Parents[0] != proposalBaseCommit ||
				!strings.Contains(body.Message, "Hako-Proposal-SHA256: "+proposalSHA) {
				t.Errorf("unexpected Git commit payload: %+v", body)
			}
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalCommitSHA)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/refs":
			branchRefWrites++
			branchExists = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ref":"refs/heads/hako/agent/proposal-abc","object":{"sha":"dddddddddddddddddddddddddddddddddddddddd"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls":
			if !pullRequestExists {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[ {"number":17,"state":"open","head":{"ref":"hako/agent/proposal-abc","repo":{"full_name":"acme/api"}},"base":{"ref":"main"}} ]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/pulls":
			pullRequestWrites++
			pullRequestExists = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"number":17,"state":"open","head":{"ref":"hako/agent/proposal-abc","repo":{"full_name":"acme/api"}},"base":{"ref":"main"}}`)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	writer, err := NewAgentProposalGitHubWriter(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	writer.now = func() time.Time { return now }
	request := testProposalRequest(proposalSHA, archive)
	token := fakeAgentProposalToken{value: "synthetic-write-token", expiresAt: now.Add(20 * time.Minute)}
	first, err := writer.Publish(context.Background(), token, request)
	if err != nil || first.BranchCommitSHA != proposalCommitSHA || first.PullRequestNumber != 17 || first.Recovered {
		t.Fatalf("first publish = %+v error=%v", first, err)
	}
	second, err := writer.Publish(context.Background(), token, request)
	if err != nil || second.BranchCommitSHA != proposalCommitSHA || second.PullRequestNumber != 17 || !second.Recovered {
		t.Fatalf("recovered publish = %+v error=%v", second, err)
	}
	if branchRefWrites != 1 || pullRequestWrites != 1 || blobWrites != 2 || treeWrites != 2 {
		t.Fatalf("publication retries created duplicate refs/PRs or skipped Git-object preparation: refs=%d prs=%d blobs=%d trees=%d",
			branchRefWrites, pullRequestWrites, blobWrites, treeWrites)
	}
}

func TestAgentProposalGitHubWriterRequiresCurrentBaseAndRejectsUnrelatedBranch(t *testing.T) {
	archive := proposalTestArchive(t)
	proposalSHA := proposalArchiveSHA(archive)
	for _, test := range []struct {
		name          string
		baseSHA       string
		branchExists  bool
		commitMessage string
		commitTree    string
	}{
		{name: "stale base", baseSHA: strings.Repeat("f", 40)},
		{name: "unrelated existing branch", baseSHA: proposalBaseCommit, branchExists: true,
			commitMessage: "unrelated commit", commitTree: proposalTreeSHA},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutationCount := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
					sha := proposalBaseCommit
					if strings.HasSuffix(r.URL.Path, "proposal-abc") {
						if !test.branchExists {
							http.NotFound(w, r)
							return
						}
						sha = proposalCommitSHA
					}
					_, _ = fmt.Fprintf(w, `{"ref":%q,"object":{"sha":%q}}`, "refs/heads/"+strings.TrimPrefix(r.URL.Path, "/repos/acme/api/git/ref/heads/"), sha)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/commits/"+proposalCommitSHA:
					_, _ = fmt.Fprintf(w, `{"sha":%q,"message":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`,
						proposalCommitSHA, test.commitMessage, test.commitTree, proposalBaseCommit)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/commits/"+proposalBaseCommit:
					_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}`, proposalBaseCommit)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/blobs":
					_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalBlobSHA)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/trees":
					_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalTreeSHA)
				case r.Method == http.MethodPost && (r.URL.Path == "/repos/acme/api/git/refs" || r.URL.Path == "/repos/acme/api/pulls"):
					mutationCount++
					t.Errorf("writer mutated a branch or PR for rejected proposal: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				default:
					t.Errorf("unexpected request while rejecting proposal: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			writer, err := NewAgentProposalGitHubWriter(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			request := testProposalRequest(proposalSHA, archive)
			request.BaseCommitSHA = test.baseSHA
			result, err := writer.Publish(context.Background(), fakeAgentProposalToken{value: "synthetic-write-token", expiresAt: time.Now().Add(time.Hour)}, request)
			if err == nil || !errors.Is(err, ErrAgentProposalGitHubDenied) || result.BranchCommitSHA != "" || mutationCount != 0 {
				t.Fatalf("unsafe proposal accepted: result=%+v mutations=%d err=%v", result, mutationCount, err)
			}
		})
	}
}

func TestAgentProposalGitHubWriterRecoversLostBranchAndPullRequestResponses(t *testing.T) {
	now := time.Now().UTC()
	archive := proposalTestArchive(t)
	proposalSHA := proposalArchiveSHA(archive)
	branchExists, pullRequestExists := false, false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/ref/heads/main":
			_, _ = fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":%q}}`, proposalBaseCommit)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/hako/agent/proposal-abc"):
			if !branchExists {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"ref":"refs/heads/hako/agent/proposal-abc","object":{"sha":%q}}`, proposalCommitSHA)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/commits/"+proposalBaseCommit:
			_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}`, proposalBaseCommit)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/git/commits/"+proposalCommitSHA:
			_, _ = fmt.Fprintf(w, `{"sha":%q,"message":%q,"tree":{"sha":%q},"parents":[{"sha":%q}]}`,
				proposalCommitSHA, proposalCommitMessage(testProposalRequest(proposalSHA, archive)), proposalTreeSHA, proposalBaseCommit)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/blobs":
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalBlobSHA)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/trees":
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalTreeSHA)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/commits":
			_, _ = fmt.Fprintf(w, `{"sha":%q}`, proposalCommitSHA)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/git/refs":
			branchExists = true
			dropGitHubResponse(t, w)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls":
			if !pullRequestExists {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[{"number":23,"state":"open","head":{"ref":"hako/agent/proposal-abc","repo":{"full_name":"acme/api"}},"base":{"ref":"main"}}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/pulls":
			pullRequestExists = true
			dropGitHubResponse(t, w)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	writer, err := NewAgentProposalGitHubWriter(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	writer.now = func() time.Time { return now }
	result, err := writer.Publish(context.Background(), fakeAgentProposalToken{value: "synthetic-write-token", expiresAt: now.Add(20 * time.Minute)},
		testProposalRequest(proposalSHA, archive))
	if err != nil || !result.Recovered || result.BranchCommitSHA != proposalCommitSHA || result.PullRequestNumber != 23 {
		t.Fatalf("lost GitHub responses were not recovered: result=%+v err=%v", result, err)
	}
}

func TestAgentProposalGitHubWriterRejectsInvalidConfigurationAndArchive(t *testing.T) {
	if _, err := NewAgentProposalGitHubWriter("http://github.example", nil); err == nil {
		t.Fatal("accepted a non-TLS GitHub API URL")
	}
	writer, err := NewAgentProposalGitHubWriter("https://api.github.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testProposalRequest(strings.Repeat("a", 64), []byte("not a ZIP"))
	if _, err := writer.Publish(context.Background(), fakeAgentProposalToken{value: "synthetic-write-token", expiresAt: time.Now().Add(time.Hour)}, request); !errors.Is(err, ErrAgentProposalGitHubDenied) {
		t.Fatalf("invalid proposal archive error=%v", err)
	}
	request = testProposalRequest(strings.Repeat("a", 64), []byte("archive"))
	request.BaseBranch = "release"
	if _, err := writer.Publish(context.Background(), fakeAgentProposalToken{value: "synthetic-write-token", expiresAt: time.Now().Add(time.Hour)}, request); !errors.Is(err, ErrAgentProposalGitHubDenied) {
		t.Fatalf("non-default PR base error=%v", err)
	}
}

func testProposalRequest(proposalSHA string, archive []byte) AgentProposalGitHubPublishRequest {
	return AgentProposalGitHubPublishRequest{Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"},
		DefaultBranch: "main", BaseBranch: "main", BaseCommitSHA: proposalBaseCommit, BranchName: "hako/agent/proposal-abc", ProposalSHA256: proposalSHA,
		RunID: "run_agent_123", AttemptID: "attempt_agent_123", Archive: archive,
		Policy: agenttask.WritePolicy{Enabled: true, BranchPrefixes: []string{"hako/agent/"}, AllowPullRequests: true,
			RequiredChecks: []string{"hako/test"}, RequireChecksToPass: true}, PassedChecks: map[string]bool{"hako/test": true}, OpenPullRequest: true}
}

func proposalTestArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "package main\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func proposalArchiveSHA(archive []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(archive))
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func dropGitHubResponse(t *testing.T, writer http.ResponseWriter) {
	t.Helper()
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		t.Fatal("test GitHub server does not support dropping the response")
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
}
