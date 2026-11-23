package workspaces

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrWorkspaceRepositoryCheckoutNotFound = errors.New("Workspace repository checkout was not found")
	ErrWorkspaceRepositoryCommitConflict   = errors.New("Workspace repository commit pin conflicts with the recorded commit")
	ErrWorkspaceRepositoryCheckoutConflict = errors.New("Workspace repository checkout result conflicts with its recorded state")
)

// PinWorkspaceRepositoryCommit records the immutable GitHub head selected by
// the trusted checkout Broker. Replays with the same SHA are harmless; a
// different SHA can never replace the original Workspace source pin.
func PinWorkspaceRepositoryCommit(ctx context.Context, pool transaction.Beginner, policy transaction.Policy,
	tenantID domain.TenantID, workspaceID domain.WorkspaceID, commitSHA string, now time.Time) error {
	if pool == nil || ctx == nil || tenantID == "" || workspaceID == "" || !validWorkspaceRepositoryCommit(commitSHA) || now.IsZero() {
		return errors.New("Workspace repository commit pin is invalid")
	}
	commitSHA = strings.ToLower(commitSHA)
	_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var pinned string
		err := tx.QueryRow(ctx, `UPDATE workspace_github_checkouts
			SET commit_sha=COALESCE(commit_sha,$3), updated_at=$4
			WHERE tenant_id=$1 AND workspace_id=$2 AND (commit_sha IS NULL OR commit_sha=$3)
			RETURNING commit_sha`, tenantID, workspaceID, commitSHA, now.UTC()).Scan(&pinned)
		if err == nil {
			if pinned != commitSHA {
				return struct{}{}, ErrWorkspaceRepositoryCommitConflict
			}
			return struct{}{}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, fmt.Errorf("persist immutable Workspace repository commit: %w", err)
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT commit_sha FROM workspace_github_checkouts WHERE tenant_id=$1 AND workspace_id=$2`, tenantID, workspaceID).Scan(&existing); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, ErrWorkspaceRepositoryCheckoutNotFound
			}
			return struct{}{}, fmt.Errorf("read immutable Workspace repository commit: %w", err)
		}
		if existing != commitSHA {
			return struct{}{}, ErrWorkspaceRepositoryCommitConflict
		}
		return struct{}{}, nil
	})
	return err
}

// RecordWorkspaceRepositoryCheckoutResult applies a Resource Plane result.
// The commit pin is immutable across retries; a failed checkout may later
// become ready only at the same commit. A ready checkout can never regress.
func RecordWorkspaceRepositoryCheckoutResult(ctx context.Context, pool transaction.Beginner, policy transaction.Policy,
	tenantID domain.TenantID, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID, installationID, repositoryID int64,
	commitSHA, state string, now time.Time) error {
	if pool == nil || ctx == nil || tenantID == "" || workspaceID == "" || resourcePlaneID == "" || installationID < 1 || repositoryID < 1 || now.IsZero() ||
		(state != "ready" && state != "failed") || (commitSHA != "" && !validWorkspaceRepositoryCommit(commitSHA)) || (state == "ready" && !validWorkspaceRepositoryCommit(commitSHA)) {
		return errors.New("Workspace repository checkout result is invalid")
	}
	commitSHA = strings.ToLower(commitSHA)
	_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var storedSHA string
		err := tx.QueryRow(ctx, `UPDATE workspace_github_checkouts
			SET commit_sha=COALESCE(commit_sha,NULLIF($5,'')), checkout_state=$6, updated_at=$7
			WHERE tenant_id=$1 AND workspace_id=$2 AND installation_id=$3 AND github_repository_id=$4
			AND EXISTS (SELECT 1 FROM placements p WHERE p.workspace_id=$2 AND p.resource_plane_id=$8)
			AND EXISTS (SELECT 1 FROM workspace_status s WHERE s.workspace_id=$2 AND s.desired_state <> 'deleted' AND s.observed_state <> 'deleted')
			AND (commit_sha IS NULL OR $5='' OR commit_sha=$5)
			AND (checkout_state <> 'ready' OR ($6='ready' AND commit_sha=$5))
			RETURNING COALESCE(commit_sha,'')`, tenantID, workspaceID, installationID, repositoryID, commitSHA, state, now.UTC(), resourcePlaneID).Scan(&storedSHA)
		if err == nil {
			if state == "ready" && storedSHA != commitSHA {
				return struct{}{}, ErrWorkspaceRepositoryCheckoutConflict
			}
			return struct{}{}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, fmt.Errorf("record Workspace repository checkout result: %w", err)
		}
		var existingSHA, existingState string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(commit_sha,''), checkout_state FROM workspace_github_checkouts
			WHERE tenant_id=$1 AND workspace_id=$2 AND installation_id=$3 AND github_repository_id=$4
			AND EXISTS (SELECT 1 FROM placements p WHERE p.workspace_id=$2 AND p.resource_plane_id=$5)
			AND EXISTS (SELECT 1 FROM workspace_status s WHERE s.workspace_id=$2 AND s.desired_state <> 'deleted' AND s.observed_state <> 'deleted')`,
			tenantID, workspaceID, installationID, repositoryID, resourcePlaneID).Scan(&existingSHA, &existingState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, ErrWorkspaceRepositoryCheckoutNotFound
			}
			return struct{}{}, fmt.Errorf("read Workspace repository checkout result: %w", err)
		}
		if existingState == state && (existingSHA == commitSHA || state == "failed" && commitSHA == "") {
			return struct{}{}, nil
		}
		return struct{}{}, ErrWorkspaceRepositoryCheckoutConflict
	})
	return err
}

func validWorkspaceRepositoryCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
