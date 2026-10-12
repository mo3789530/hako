package workloadagent

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

// FilesystemExecutionResolver builds an execution plan only from a verified,
// execution-bound Agent ticket and one image-configured source directory. No
// request, queue, repository, or ticket field can select a filesystem path.
type FilesystemExecutionResolver struct {
	repositoryPath string
	now            func() time.Time
}

func NewFilesystemExecutionResolver(repositoryPath string, now func() time.Time) (*FilesystemExecutionResolver, error) {
	repositoryPath = strings.TrimSpace(repositoryPath)
	if repositoryPath == "" || !filepath.IsAbs(repositoryPath) || strings.ContainsAny(repositoryPath, "\r\n,\\") {
		return nil, ErrExecutionPlan
	}
	absolute, err := filepath.Abs(repositoryPath)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == string(filepath.Separator) {
		return nil, ErrExecutionPlan
	}
	if now == nil {
		now = time.Now
	}
	return &FilesystemExecutionResolver{repositoryPath: absolute, now: now}, nil
}

func (r *FilesystemExecutionResolver) ResolveExecution(ctx context.Context, claims TicketClaims) (ExecutionPlan, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || claims.Operation != OperationStart || claims.Execution == nil ||
		!validIdentity(claims.Identity.Command()) || !validExecutionBinding(*claims.Execution) {
		return ExecutionPlan{}, ErrExecutionPlan
	}
	now := r.now().UTC()
	deadline := claims.Execution.Deadline.UTC()
	if !deadline.After(now) || deadline.Sub(now) > 8*time.Hour {
		return ExecutionPlan{}, ErrExecutionPlan
	}
	return ExecutionPlan{Identity: claims.Identity.Command(), RepositoryPath: r.repositoryPath,
		SourceDigest: claims.Execution.SourceDigest, CommitSHA: claims.Execution.CommitSHA, Deadline: deadline}, nil
}

var _ ExecutionResolver = (*FilesystemExecutionResolver)(nil)
