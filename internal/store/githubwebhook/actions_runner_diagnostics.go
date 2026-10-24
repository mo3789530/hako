package githubwebhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const maxActionsRunnerDiagnosticsBytes = 64 << 10

var ErrActionsRunnerDiagnosticsDenied = errors.New("Actions Runner diagnostics upload denied")

// PutActionsRunnerDiagnostics accepts one immutable, short-lived launch
// capability and stores its redacted log against the exact admitted Check.
// Replaying the same content is idempotent; a different payload is rejected.
func PutActionsRunnerDiagnostics(ctx context.Context, tx pgx.Tx, integrationID string, workflowJobID, runAttempt int64,
	token string, logs []byte, digest string, now time.Time) error {
	if ctx == nil || tx == nil || !validStoredIntegrationID(integrationID) || workflowJobID <= 0 || runAttempt <= 0 ||
		len(token) != 64 || now.IsZero() || len(logs) == 0 || len(logs) > maxActionsRunnerDiagnosticsBytes {
		return ErrActionsRunnerDiagnosticsDenied
	}
	if strings.TrimSpace(token) != token || strings.TrimSpace(digest) != digest {
		return ErrActionsRunnerDiagnosticsDenied
	}
	tokenBytes, err := hex.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return ErrActionsRunnerDiagnosticsDenied
	}
	defer clear(tokenBytes)
	actualDigest := sha256.Sum256(logs)
	if digest != hex.EncodeToString(actualDigest[:]) {
		return ErrActionsRunnerDiagnosticsDenied
	}
	tokenDigest := sha256.Sum256(tokenBytes)
	var expectedTokenDigest string
	var jobCreatedAt time.Time
	var registrationTokenIssuedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT diagnostics_upload_token_sha256,created_at,registration_token_issued_at
		FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 FOR UPDATE`,
		integrationID, workflowJobID, runAttempt).Scan(&expectedTokenDigest, &jobCreatedAt, &registrationTokenIssuedAt)
	if err != nil || expectedTokenDigest != hex.EncodeToString(tokenDigest[:]) || registrationTokenIssuedAt == nil ||
		jobCreatedAt.Before(now.UTC().Add(-24*time.Hour)) {
		return ErrActionsRunnerDiagnosticsDenied
	}
	check, err := GetActionsRunnerCheck(ctx, tx, integrationID, workflowJobID, runAttempt)
	if err != nil || check.TenantID == "" {
		return ErrActionsRunnerDiagnosticsDenied
	}
	var storedDigest *string
	err = tx.QueryRow(ctx, `SELECT diagnostics_sha256 FROM github_actions_runner_checks
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 FOR UPDATE`, integrationID, workflowJobID, runAttempt).
		Scan(&storedDigest)
	if err == nil {
		if storedDigest != nil && *storedDigest == digest {
			return nil
		}
		if storedDigest != nil {
			return ErrActionsRunnerDiagnosticsDenied
		}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return fmt.Errorf("read Actions Runner Check diagnostics state: %w", err)
		}
	}
	excerpt := actionsRunnerDiagnosticsExcerpt(logs)
	check.OutputSummary = actionsRunnerDiagnosticsSummary(check.OutputSummary, excerpt)
	tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_checks SET output_summary=$1,diagnostics_sha256=$2,diagnostics_summary=$3,
		diagnostics_uploaded_at=$4,revision=revision+1,updated_at=$4
		WHERE integration_id=$5 AND workflow_job_id=$6 AND run_attempt=$7 AND revision=$8 AND diagnostics_sha256 IS NULL`,
		check.OutputSummary, digest, excerpt, now.UTC(), integrationID, workflowJobID, runAttempt, check.Revision)
	if err != nil || tag.RowsAffected() != 1 {
		if err != nil {
			return fmt.Errorf("update Actions Runner Check with diagnostics: %w", err)
		}
		return errors.New("Actions Runner Check revision changed while attaching diagnostics")
	}
	check.Revision++
	return enqueueActionsRunnerCheck(ctx, tx, check, now.UTC())
}

func actionsRunnerDiagnosticsExcerpt(logs []byte) string {
	const maxExcerptBytes = 3072
	text := strings.ToValidUTF8(string(logs), "�")
	text = strings.ReplaceAll(text, "`", "\\`")
	text = html.EscapeString(text)
	if len(text) > maxExcerptBytes {
		text = text[:maxExcerptBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

func actionsRunnerDiagnosticsSummary(summary, excerpt string) string {
	const maxSummaryBytes = 4096
	const wrapper = "\n\n<details><summary>Redacted runner diagnostics</summary>\n\n```text\n"
	const suffix = "\n```\n</details>"
	available := maxSummaryBytes - len(summary) - len(wrapper) - len(suffix)
	if available <= 0 {
		return summary
	}
	text := excerpt
	if len(text) > available {
		text = text[:available]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	result := summary + wrapper + text + suffix
	return result
}

func preserveActionsRunnerDiagnosticsSummary(ctx context.Context, tx pgx.Tx, check ActionsRunnerCheck, summary string) (string, error) {
	var excerpt string
	err := tx.QueryRow(ctx, `SELECT diagnostics_summary FROM github_actions_runner_checks
		WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3`, check.IntegrationID, check.WorkflowJobID, check.RunAttempt).Scan(&excerpt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return summary, nil
		}
		return "", fmt.Errorf("load Actions Runner diagnostics for Check: %w", err)
	}
	if excerpt == "" {
		return summary, nil
	}
	return actionsRunnerDiagnosticsSummary(summary, excerpt), nil
}
