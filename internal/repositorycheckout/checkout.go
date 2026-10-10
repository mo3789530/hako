package repositorycheckout

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
)

var (
	ErrCheckoutInvalidRun          = errors.New("Workload Run is not eligible for source checkout")
	ErrCheckoutNotAuthorized       = errors.New("GitHub Repository is not authorized for this Workload Run")
	ErrCheckoutCredentialFailure   = errors.New("GitHub checkout credential is unavailable")
	ErrCheckoutFailed              = errors.New("immutable GitHub source checkout failed")
	ErrCheckoutCommitMismatch      = errors.New("checked out commit does not match Workload Run")
	ErrCheckoutSourceTooLarge      = errors.New("checked out source exceeds configured size limit")
	ErrCheckoutUnsupportedFeatures = errors.New("repository uses unsupported submodule or Git LFS features")
	defaultRepositoryCheckoutLimit = 2 * time.Minute
)

const (
	maxGitPackBytes     = 512 << 20
	maxCheckoutTreeSize = 1 << 30
)

// RepositoryCheckout returns a private temporary source tree. The caller must
// call Cleanup as soon as the isolated executor has finished consuming it.
type RepositoryCheckout interface {
	Checkout(context.Context, domain.WorkloadRun) (CheckedOutRepository, error)
}

type CheckedOutRepository struct {
	Path    string
	cleanup func() error
}

func (r CheckedOutRepository) Cleanup() error {
	if r.cleanup == nil {
		return nil
	}
	return r.cleanup()
}

type CredentialProvider interface {
	CheckoutToken(context.Context, domain.TenantID, int64, int64) (string, error)
}

type CredentialProviderFunc func(context.Context, domain.TenantID, int64, int64) (string, error)

func (f CredentialProviderFunc) CheckoutToken(ctx context.Context, tenant domain.TenantID, installationID, repositoryID int64) (string, error) {
	return f(ctx, tenant, installationID, repositoryID)
}

type GitRepositoryCheckout struct {
	authorizer  githubapp.RepositoryAuthorizer
	credentials CredentialProvider
	gitPath     string
	timeout     time.Duration
	baseURL     string
	tempRoot    string
	maxTreeSize int64
}

func NewGitRepositoryCheckout(authorizer githubapp.RepositoryAuthorizer, credentials CredentialProvider) (*GitRepositoryCheckout, error) {
	if authorizer == nil || credentials == nil {
		return nil, errors.New("repository authorizer and checkout credentials are required")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("git executable is unavailable")
	}
	return &GitRepositoryCheckout{authorizer: authorizer, credentials: credentials, gitPath: gitPath,
		timeout: defaultRepositoryCheckoutLimit, baseURL: "https://github.com", maxTreeSize: maxCheckoutTreeSize}, nil
}

func (s *GitRepositoryCheckout) Checkout(ctx context.Context, run domain.WorkloadRun) (CheckedOutRepository, error) {
	if s == nil || ctx == nil || s.authorizer == nil || s.credentials == nil || s.gitPath == "" || !validCheckoutRun(run) {
		return CheckedOutRepository{}, ErrCheckoutInvalidRun
	}
	timeout := s.timeout
	if timeout <= 0 || timeout > defaultRepositoryCheckoutLimit {
		timeout = defaultRepositoryCheckoutLimit
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Resolve the repository from the persisted Tenant/Installation/Repository
	// binding before asking the credential service to mint or return a token.
	access, err := s.authorizer.AuthorizeRepository(ctx, run.TenantID, run.GitHubInstallation, run.GitHubRepository)
	if err != nil || !validGitHubPathPart(access.Owner, 39) || !validGitHubPathPart(access.Name, 100) {
		if ctx.Err() != nil {
			return CheckedOutRepository{}, ctx.Err()
		}
		return CheckedOutRepository{}, ErrCheckoutNotAuthorized
	}
	token, err := s.credentials.CheckoutToken(ctx, run.TenantID, run.GitHubInstallation, run.GitHubRepository)
	if err != nil || !validCheckoutToken(token) {
		if ctx.Err() != nil {
			return CheckedOutRepository{}, ctx.Err()
		}
		return CheckedOutRepository{}, ErrCheckoutCredentialFailure
	}

	root, err := os.MkdirTemp(s.tempRoot, "hako-workload-checkout-")
	if err != nil {
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	cleanup := func() error { return os.RemoveAll(root) }
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		_ = cleanup()
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	askpass := filepath.Join(root, "askpass")
	if err := writeAskpass(askpass); err != nil {
		_ = cleanup()
		return CheckedOutRepository{}, ErrCheckoutFailed
	}

	remote := strings.TrimRight(s.baseURL, "/") + "/" + access.Owner + "/" + access.Name + ".git"
	if err := s.run(ctx, source, "init", "--quiet"); err != nil {
		_ = cleanup()
		if ctx.Err() != nil {
			return CheckedOutRepository{}, ctx.Err()
		}
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	if err := s.fetch(ctx, source, askpass, remote, run.CommitSHA, token); err != nil {
		_ = cleanup()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return CheckedOutRepository{}, err
		}
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	if err := s.run(ctx, source, "-c", "core.hooksPath=/dev/null", "-c", "submodule.recurse=false", "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		_ = cleanup()
		if ctx.Err() != nil {
			return CheckedOutRepository{}, ctx.Err()
		}
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	output, err := s.output(ctx, source, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(string(output)) != run.CommitSHA {
		_ = cleanup()
		if ctx.Err() != nil {
			return CheckedOutRepository{}, ctx.Err()
		}
		return CheckedOutRepository{}, ErrCheckoutCommitMismatch
	}
	maxTreeSize := s.maxTreeSize
	if maxTreeSize <= 0 {
		maxTreeSize = maxCheckoutTreeSize
	}
	tooLarge, err := checkoutTreeTooLarge(source, maxTreeSize)
	if err != nil {
		_ = cleanup()
		return CheckedOutRepository{}, ErrCheckoutFailed
	}
	if tooLarge {
		_ = cleanup()
		return CheckedOutRepository{}, ErrCheckoutSourceTooLarge
	}
	if unsupportedCheckoutFeatures(source) {
		_ = cleanup()
		return CheckedOutRepository{}, ErrCheckoutUnsupportedFeatures
	}
	return CheckedOutRepository{Path: source, cleanup: cleanup}, nil
}

func writeAskpass(path string) error {
	return os.WriteFile(path, []byte("#!/bin/sh\ncase \"$1\" in\n  *sername*) printf '%s\\n' 'x-access-token' ;;\n  *assword*) IFS= read -r token <&3 || exit 1; printf '%s\\n' \"$token\"; unset token ;;\n  *) exit 1 ;;\nesac\n"), 0o700)
}

func validCheckoutRun(run domain.WorkloadRun) bool {
	if run.Kind != domain.WorkloadJob || run.TenantID == "" || run.GitHubInstallation <= 0 || run.GitHubRepository <= 0 ||
		run.DesiredState != domain.WorkloadDesiredRunning || !validCommitSHA(run.CommitSHA) {
		return false
	}
	switch run.State {
	case domain.WorkloadPending, domain.WorkloadQueued, domain.WorkloadProvisioning, domain.WorkloadRunning:
		return true
	default:
		return false
	}
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validGitHubPathPart(value string, max int) bool {
	if value == "" || len(value) > max || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func validCheckoutToken(value string) bool {
	return value != "" && len(value) <= 16384 && !strings.ContainsAny(value, "\r\n\x00 \t")
}

func (s *GitRepositoryCheckout) fetch(ctx context.Context, dir, askpass, remote, sha, token string) error {
	readToken, writeToken, err := os.Pipe()
	if err != nil {
		return ErrCheckoutCredentialFailure
	}
	if _, err := io.WriteString(writeToken, token+"\n"); err != nil {
		_ = readToken.Close()
		_ = writeToken.Close()
		return ErrCheckoutCredentialFailure
	}
	if err := writeToken.Close(); err != nil {
		_ = readToken.Close()
		return ErrCheckoutCredentialFailure
	}
	defer readToken.Close()
	args := checkoutFetchArgs(dir, remote, sha, strings.HasPrefix(remote, "file://"))
	return runGit(ctx, s.gitPath, dir, askpass, readToken, args...)
}

func (s *GitRepositoryCheckout) run(ctx context.Context, dir string, args ...string) error {
	return runGit(ctx, s.gitPath, dir, "", nil, args...)
}

func (s *GitRepositoryCheckout) output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.gitPath, append([]string{"-C", dir}, args...)...)
	cmd.Env = checkoutGitEnv(dir, "")
	return cmd.Output()
}

func runGit(ctx context.Context, gitPath, dir, askpass string, tokenFD *os.File, args ...string) error {
	// RLIMIT_FSIZE bounds the packfile even if a repository advertises an
	// unexpectedly large object graph. The worktree has a separate total-size
	// check after checkout.
	fileSizeBlocks := int64(maxGitPackBytes / 512)
	wrappedArgs := append([]string{"-c", "ulimit -f " + strconv.FormatInt(fileSizeBlocks, 10) + "; exec \"$@\"", "--", gitPath}, args...)
	cmd := exec.CommandContext(ctx, "/bin/sh", wrappedArgs...)
	cmd.Dir = dir
	cmd.Env = checkoutGitEnv(dir, askpass)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if tokenFD != nil {
		cmd.ExtraFiles = []*os.File{tokenFD}
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrCheckoutFailed
	}
	return nil
}

func checkoutFetchArgs(dir, remote, sha string, allowFile bool) []string {
	args := []string{"-c", "core.hooksPath=/dev/null", "-c", "http.followRedirects=false", "-c", "credential.interactive=never", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always"}
	if allowFile {
		// Local-file transport is only useful for package tests. Production
		// construction fixes the remote base to https://github.com.
		args = append(args, "-c", "protocol.file.allow=always")
	}
	return append(args, "-C", dir, "fetch", "--no-tags", "--depth=1", "--no-recurse-submodules", remote, sha)
}

func checkoutGitEnv(home, askpass string) []string {
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "GIT_TRACE=0", "GIT_CURL_VERBOSE=0"}
	if askpass != "" {
		env = append(env, "GIT_ASKPASS="+askpass, "SSH_ASKPASS="+askpass, "GIT_ASKPASS_REQUIRE=force")
	}
	return env
}

func checkoutTreeTooLarge(root string, maxBytes int64) (bool, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filepath.Base(path) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > maxBytes {
			return errCheckoutTreeLimit
		}
		return nil
	})
	if errors.Is(err, errCheckoutTreeLimit) {
		return true, nil
	}
	return false, err
}

var errCheckoutTreeLimit = errors.New("checkout tree size limit")

func unsupportedCheckoutFeatures(root string) bool {
	if _, err := os.Lstat(filepath.Join(root, ".gitmodules")); err == nil {
		return true
	}
	unsupported := false
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || unsupported {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if filepath.Base(path) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if filepath.Base(path) == ".gitattributes" {
			contents, readErr := os.ReadFile(path)
			if readErr == nil && strings.Contains(string(contents), "filter=lfs") {
				unsupported = true
				return filepath.SkipAll
			}
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		var header [64]byte
		n, _ := file.Read(header[:])
		_ = file.Close()
		if strings.HasPrefix(string(header[:n]), "version https://git-lfs.github.com/spec/v1") {
			unsupported = true
			return filepath.SkipAll
		}
		return nil
	})
	return unsupported
}
