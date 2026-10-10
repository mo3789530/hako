package repositorycheckout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
)

type testAuthorizer struct {
	access githubapp.RepositoryAccess
	err    error
	calls  int
	wait   bool
}

func (a *testAuthorizer) AuthorizeRepository(ctx context.Context, _ domain.TenantID, _, _ int64) (githubapp.RepositoryAccess, error) {
	a.calls++
	if a.wait {
		<-ctx.Done()
		return githubapp.RepositoryAccess{}, ctx.Err()
	}
	return a.access, a.err
}

type testCredentials struct {
	token string
	err   error
	calls int
	wait  bool
}

func (c *testCredentials) CheckoutToken(ctx context.Context, _ domain.TenantID, _, _ int64) (string, error) {
	c.calls++
	if c.wait {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return c.token, c.err
}

func testRun(commit string) domain.WorkloadRun {
	return domain.WorkloadRun{ID: "run_checkout", TenantID: "tenant_checkout", Kind: domain.WorkloadJob,
		GitHubInstallation: 123, GitHubRepository: 456, CommitSHA: commit, Ref: "refs/heads/main",
		DesiredState: domain.WorkloadDesiredRunning, State: domain.WorkloadQueued}
}

func newCheckout(t *testing.T, remoteBase string, authorizer *testAuthorizer, credentials *testCredentials) *GitRepositoryCheckout {
	t.Helper()
	checkout, err := NewGitRepositoryCheckout(authorizer, credentials)
	if err != nil {
		t.Fatal("create GitHub checkout provider")
	}
	checkout.baseURL = remoteBase
	return checkout
}

func createTestRemote(t *testing.T) (string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "remotes", "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(remote), 0o700); err != nil {
		t.Fatal("create fake repository directory")
	}
	runTestCommand(t, root, "git", "init", "-b", "main", work)
	runTestCommand(t, work, "git", "config", "user.name", "Checkout Test")
	runTestCommand(t, work, "git", "config", "user.email", "checkout@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "value.txt"), []byte("first commit\n"), 0o600); err != nil {
		t.Fatal("write first fake repository file")
	}
	runTestCommand(t, work, "git", "add", "value.txt")
	runTestCommand(t, work, "git", "commit", "-m", "first")
	first := strings.TrimSpace(string(runTestCommand(t, work, "git", "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(work, "value.txt"), []byte("second commit\n"), 0o600); err != nil {
		t.Fatal("write second fake repository file")
	}
	runTestCommand(t, work, "git", "commit", "-am", "second")
	second := strings.TrimSpace(string(runTestCommand(t, work, "git", "rev-parse", "HEAD")))
	runTestCommand(t, root, "git", "clone", "--bare", work, remote)
	return "file://" + filepath.Join(root, "remotes"), first, second, root
}

func runTestCommand(t *testing.T, dir string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fake Git fixture command failed: %s", strings.TrimSpace(string(output)))
	}
	return output
}

func TestCheckoutUsesPersistedCommitAndCleansPrivateDirectory(t *testing.T) {
	remote, first, second, _ := createTestRemote(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	const tokenValue = "github-token-should-never-be-written"
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{token: tokenValue}
	checkout, err := NewGitRepositoryCheckout(authorizer, credentials)
	if err != nil {
		t.Fatal("create checkout")
	}
	checkout.baseURL = remote
	result, err := checkout.Checkout(context.Background(), testRun(first))
	if err != nil {
		t.Fatalf("checkout exact persisted SHA: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(result.Path, "value.txt")); err != nil || string(got) != "first commit\n" {
		t.Fatal("checkout followed mutable ref metadata instead of persisted commit SHA")
	}
	if strings.TrimSpace(string(runTestCommand(t, result.Path, "git", "rev-parse", "HEAD"))) != first || first == second {
		t.Fatal("checked out HEAD did not match the immutable run SHA")
	}
	config, err := os.ReadFile(filepath.Join(result.Path, ".git", "config"))
	if err != nil || strings.Contains(string(config), tokenValue) {
		t.Fatal("credential was written to Git configuration")
	}
	if info, err := os.Stat(filepath.Dir(result.Path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("checkout parent is not private")
	}
	path := result.Path
	if err := result.Cleanup(); err != nil {
		t.Fatal("cleanup successful checkout")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful checkout path remained after cleanup")
	}
	if err := result.Cleanup(); err != nil {
		t.Fatal("cleanup is not idempotent")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatal("checkout temporary files remained after cleanup")
	}
	if authorizer.calls != 1 || credentials.calls != 1 {
		t.Fatal("checkout did not authorize and request credentials exactly once")
	}
}

func TestCheckoutRejectsInvalidRunBeforeAuthorizationOrCredentialUse(t *testing.T) {
	valid := strings.Repeat("a", 40)
	tests := []struct {
		name string
		edit func(*domain.WorkloadRun)
	}{
		{"wrong-kind", func(run *domain.WorkloadRun) { run.Kind = domain.WorkloadAgent }},
		{"wrong-state", func(run *domain.WorkloadRun) { run.State = domain.WorkloadSucceeded }},
		{"cancelled-desired-state", func(run *domain.WorkloadRun) { run.DesiredState = domain.WorkloadDesiredCancelled }},
		{"uppercase-sha", func(run *domain.WorkloadRun) { run.CommitSHA = strings.Repeat("A", 40) }},
		{"mutable-ref-as-sha", func(run *domain.WorkloadRun) { run.CommitSHA = "main" }},
		{"missing-repository", func(run *domain.WorkloadRun) { run.GitHubRepository = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
			credentials := &testCredentials{token: "token"}
			checkout := newCheckout(t, "https://github.com", authorizer, credentials)
			run := testRun(valid)
			test.edit(&run)
			if _, err := checkout.Checkout(context.Background(), run); !errors.Is(err, ErrCheckoutInvalidRun) {
				t.Fatalf("invalid run was not rejected: %v", err)
			}
			if authorizer.calls != 0 || credentials.calls != 0 {
				t.Fatal("invalid run reached repository authorization or credential service")
			}
		})
	}
}

func TestCheckoutRejectsWrongRepositoryAndSuspendedInstallationBeforeTokenRequest(t *testing.T) {
	tests := []struct {
		name    string
		access  githubapp.RepositoryAccess
		authErr error
	}{
		{"suspended-installation", githubapp.RepositoryAccess{}, errors.New("inactive")},
		{"wrong-repository", githubapp.RepositoryAccess{}, errors.New("repository ID does not belong to Tenant")},
		{"path-traversal-owner", githubapp.RepositoryAccess{Owner: "../other", Name: "widget"}, nil},
		{"path-traversal-repository", githubapp.RepositoryAccess{Owner: "acme", Name: "../../etc"}, nil},
		{"wrong-empty-repository", githubapp.RepositoryAccess{Owner: "acme"}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorizer := &testAuthorizer{access: test.access, err: test.authErr}
			credentials := &testCredentials{token: "token"}
			checkout := newCheckout(t, "https://github.com", authorizer, credentials)
			if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("b", 40))); !errors.Is(err, ErrCheckoutNotAuthorized) {
				t.Fatalf("unauthorized repository was not rejected: %v", err)
			}
			if credentials.calls != 0 {
				t.Fatal("unauthorized Installation/Repository reached credential service")
			}
		})
	}
}

func TestCheckoutUnavailableCommitRemovesTemporaryFiles(t *testing.T) {
	remote, _, _, _ := createTestRemote(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{token: "fake-token"}
	checkout := newCheckout(t, remote, authorizer, credentials)
	_, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("f", 40)))
	if !errors.Is(err, ErrCheckoutFailed) {
		t.Fatalf("unavailable commit error = %v", err)
	}
	entries, readErr := os.ReadDir(tmp)
	if readErr != nil || len(entries) != 0 {
		t.Fatal("failed checkout left a temporary directory")
	}
}

func TestCheckoutGitTimeoutAndHeadMismatchCleanTemporaryDirectory(t *testing.T) {
	remote, first, _, _ := createTestRemote(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{token: "timeout-test-token"}
	checkout := newCheckout(t, remote, authorizer, credentials)
	realGit := checkout.gitPath
	checkout.gitPath = writeFakeGit(t, t.TempDir(), realGit, `case "$*" in *fetch*) while :; do :; done ;; *) exec REAL_GIT "$@" ;; esac`)
	checkout.timeout = 20 * time.Millisecond
	if _, err := checkout.Checkout(context.Background(), testRun(first)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetch timeout returned %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatal("timed-out fetch left a temporary checkout")
	}

	checkout.gitPath = writeFakeGit(t, t.TempDir(), realGit, `case "$*" in *rev-parse*) printf '%040d\\n' 0 ;; *) exec REAL_GIT "$@" ;; esac`)
	checkout.timeout = defaultRepositoryCheckoutLimit
	if _, err := checkout.Checkout(context.Background(), testRun(first)); !errors.Is(err, ErrCheckoutCommitMismatch) {
		t.Fatalf("mismatched HEAD returned %v", err)
	}
	entries, err = os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatal("HEAD mismatch left a temporary checkout")
	}
}

func writeFakeGit(t *testing.T, directory, realGit, script string) string {
	t.Helper()
	path := filepath.Join(directory, "fake-git")
	script = strings.ReplaceAll(script, "REAL_GIT", realGit)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal("write fake Git executable")
	}
	return path
}

func TestCheckoutEnforcesTreeSizeAndRejectsLFS(t *testing.T) {
	remote, first, _, _ := createTestRemote(t)
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{token: "size-check-token"}
	checkout := newCheckout(t, remote, authorizer, credentials)
	checkout.maxTreeSize = 1
	if _, err := checkout.Checkout(context.Background(), testRun(first)); !errors.Is(err, ErrCheckoutSourceTooLarge) {
		t.Fatalf("oversized source returned %v", err)
	}

	remoteRoot := strings.TrimPrefix(remote, "file://")
	fixtureRoot := filepath.Dir(remoteRoot)
	work := filepath.Join(fixtureRoot, "work")
	remoteRepo := filepath.Join(remoteRoot, "acme", "widget.git")
	if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs\n"), 0o600); err != nil {
		t.Fatal("write LFS attribute")
	}
	runTestCommand(t, work, "git", "add", ".gitattributes")
	runTestCommand(t, work, "git", "commit", "-m", "enable LFS")
	latest := strings.TrimSpace(string(runTestCommand(t, work, "git", "rev-parse", "HEAD")))
	runTestCommand(t, fixtureRoot, "git", "--git-dir", remoteRepo, "fetch", work, "main")
	runTestCommand(t, fixtureRoot, "git", "--git-dir", remoteRepo, "update-ref", "refs/heads/main", "FETCH_HEAD")
	checkout.maxTreeSize = maxCheckoutTreeSize
	if _, err := checkout.Checkout(context.Background(), testRun(latest)); !errors.Is(err, ErrCheckoutUnsupportedFeatures) {
		t.Fatalf("LFS source returned %v", err)
	}
}

func TestCheckoutTemporaryDirectoryFailureAndCleanupError(t *testing.T) {
	root := t.TempDir()
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{token: "private-token"}
	checkout := newCheckout(t, "https://github.com", authorizer, credentials)
	checkout.tempRoot = filepath.Join(root, "does-not-exist")
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("a", 40))); !errors.Is(err, ErrCheckoutFailed) {
		t.Fatalf("temporary directory failure returned %v", err)
	}
	wantErr := errors.New("cleanup failed")
	called := false
	if err := (CheckedOutRepository{cleanup: func() error { called = true; return wantErr }}).Cleanup(); !errors.Is(err, wantErr) || !called {
		t.Fatal("cleanup error was not returned")
	}
	if err := (CheckedOutRepository{}).Cleanup(); err != nil {
		t.Fatal("empty checkout cleanup should be harmless")
	}
}

func TestCheckoutRejectsMalformedCredentialWithoutPersistence(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{token: "bad\ntoken"}
	checkout := newCheckout(t, "https://github.com", authorizer, credentials)
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("a", 40))); !errors.Is(err, ErrCheckoutCredentialFailure) {
		t.Fatalf("malformed credential returned %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid credential caused temporary files to be created")
	}
}

func TestCheckoutCredentialFailureAndTimeoutAreBounded(t *testing.T) {
	authorizer := &testAuthorizer{access: githubapp.RepositoryAccess{Owner: "acme", Name: "widget"}}
	credentials := &testCredentials{err: errors.New("secret-bearing error")}
	checkout := newCheckout(t, "https://github.com", authorizer, credentials)
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("c", 40))); !errors.Is(err, ErrCheckoutCredentialFailure) {
		t.Fatalf("credential failure was not safely mapped: %v", err)
	}
	if strings.Contains(ErrCheckoutCredentialFailure.Error(), "secret-bearing") {
		t.Fatal("credential provider error leaked into public checkout error")
	}
	credentials.err, credentials.wait, credentials.calls = nil, true, 0
	checkout.timeout = 10 * time.Millisecond
	started := time.Now()
	_, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("c", 40)))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("credential wait was not bounded by checkout deadline: %v", err)
	}
}

func TestCheckoutAuthorizationTimeoutAndInvalidProvider(t *testing.T) {
	authorizer := &testAuthorizer{wait: true}
	credentials := &testCredentials{token: "token"}
	checkout := newCheckout(t, "https://github.com", authorizer, credentials)
	checkout.timeout = 10 * time.Millisecond
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("c", 40))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("authorization wait was not bounded by checkout deadline: %v", err)
	}
	if credentials.calls != 0 {
		t.Fatal("timed out authorization reached credential service")
	}
	var missing *GitRepositoryCheckout
	if _, err := missing.Checkout(context.Background(), testRun(strings.Repeat("c", 40))); !errors.Is(err, ErrCheckoutInvalidRun) {
		t.Fatal("nil checkout provider was accepted")
	}
	checkout.authorizer = nil
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("c", 40))); !errors.Is(err, ErrCheckoutInvalidRun) {
		t.Fatal("missing authorizer was accepted")
	}
	checkout.authorizer = authorizer
	checkout.credentials = nil
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("c", 40))); !errors.Is(err, ErrCheckoutInvalidRun) {
		t.Fatal("missing credential provider was accepted")
	}
	checkout.credentials = credentials
	checkout.gitPath = ""
	if _, err := checkout.Checkout(context.Background(), testRun(strings.Repeat("c", 40))); !errors.Is(err, ErrCheckoutInvalidRun) {
		t.Fatal("missing Git executable was accepted")
	}
}

func TestCheckoutHelpersRejectUnsupportedFeaturesAndEnforceSize(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.bin"), make([]byte, 11), 0o600); err != nil {
		t.Fatal("write size-limit fixture")
	}
	if tooLarge, err := checkoutTreeTooLarge(root, 10); err != nil || !tooLarge {
		t.Fatal("checkout size limit was not enforced")
	}
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule]\n"), 0o600); err != nil {
		t.Fatal("write submodule fixture")
	}
	if !unsupportedCheckoutFeatures(root) {
		t.Fatal("submodule repository was not rejected")
	}
	if err := os.Remove(filepath.Join(root, ".gitmodules")); err != nil {
		t.Fatal("remove submodule fixture")
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs\n"), 0o600); err != nil {
		t.Fatal("write LFS attribute fixture")
	}
	if !unsupportedCheckoutFeatures(root) {
		t.Fatal("LFS repository was not rejected")
	}
	if _, err := checkoutTreeTooLarge(filepath.Join(root, "missing"), 10); err == nil {
		t.Fatal("filesystem traversal errors were ignored")
	}
	if unsupportedCheckoutFeatures(filepath.Join(root, "missing")) {
		t.Fatal("filesystem access error was mistaken for an unsupported repository feature")
	}
	symlinkRoot := t.TempDir()
	if err := os.Symlink("../outside", filepath.Join(symlinkRoot, "source-link")); err != nil {
		t.Fatal("create symlink fixture")
	}
	if unsupportedCheckoutFeatures(symlinkRoot) {
		t.Fatal("symlink target was followed while checking repository features")
	}
}

func TestCheckoutEnvironmentAndAskpassKeepTokenOutOfArgumentsAndConfig(t *testing.T) {
	const token = "must-not-be-in-process-arguments-or-environment"
	env := strings.Join(checkoutGitEnv("/tmp/private", "/tmp/private/askpass"), "\n")
	args := strings.Join(checkoutFetchArgs("/tmp/private/source", "https://github.com/acme/widget.git", strings.Repeat("d", 40), false), " ")
	if strings.Contains(env, token) || strings.Contains(args, token) {
		t.Fatal("credential appeared in Git process arguments or environment")
	}
	if strings.Contains(args, "main") || !strings.Contains(args, strings.Repeat("d", 40)) {
		t.Fatal("fetch revision was not the immutable SHA")
	}
	root := t.TempDir()
	askpass := filepath.Join(root, "askpass")
	if err := writeAskpass(askpass); err != nil {
		t.Fatal("write credential prompt helper")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal("create credential pipe")
	}
	if _, err := writer.WriteString(token + "\n"); err != nil {
		t.Fatal("send credential through pipe")
	}
	_ = writer.Close()
	cmd := exec.Command(askpass, "Password for https://github.com/acme/widget.git")
	cmd.ExtraFiles = []*os.File{reader}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	output, err := cmd.Output()
	_ = reader.Close()
	if err != nil || strings.TrimSpace(string(output)) != token {
		t.Fatal("askpass did not read the token from its private file descriptor")
	}
	if strings.Contains(strings.Join(cmd.Args, " "), token) || strings.Contains(strings.Join(cmd.Env, "\n"), token) {
		t.Fatal("askpass exposed token in process arguments or environment")
	}
	if strings.Contains(string(mustReadFile(t, askpass)), token) {
		t.Fatal("askpass helper script contains the token")
	}
	if !validCheckoutToken(token) || validCheckoutToken("bad\ntoken") || validCheckoutToken("") {
		t.Fatal("credential token validation accepted unsafe input")
	}
	if !validCommitSHA(strings.Repeat("e", 64)) || validCommitSHA(fmt.Sprintf("%040s", "A")) {
		t.Fatal("commit SHA validation accepted invalid input")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read test helper")
	}
	return contents
}

func TestCheckoutRunValidationRequiresRealIdentity(t *testing.T) {
	run := testRun(strings.Repeat("a", 40))
	if !validCheckoutRun(run) {
		t.Fatal("valid run was rejected")
	}
	run.TenantID = ""
	if validCheckoutRun(run) {
		t.Fatal("missing Tenant identity was accepted")
	}
	if _, err := NewGitRepositoryCheckout(nil, &testCredentials{token: "token"}); err == nil {
		t.Fatal("nil repository authorizer was accepted")
	}
	if _, err := NewGitRepositoryCheckout(&testAuthorizer{}, nil); err == nil {
		t.Fatal("nil credential provider was accepted")
	}
	checkout, _ := NewGitRepositoryCheckout(&testAuthorizer{}, &testCredentials{token: "token"})
	if _, err := checkout.Checkout(nil, run); !errors.Is(err, ErrCheckoutInvalidRun) {
		t.Fatal("nil checkout context was accepted")
	}
	provider := CredentialProviderFunc(func(context.Context, domain.TenantID, int64, int64) (string, error) { return "wrapped-token", nil })
	if value, err := provider.CheckoutToken(context.Background(), "tenant", 1, 2); err != nil || value != "wrapped-token" {
		t.Fatal("credential function adapter did not pass through the trusted provider")
	}
}
