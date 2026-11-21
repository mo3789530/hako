package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/actionsrunner"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const maxActionsRunnerDiagnosticsBody = 64 << 10

func putActionsRunnerDiagnostics(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if pool == nil || c.Request().Method != http.MethodPost || c.Request().URL.RawQuery != "" || c.Request().URL.Fragment != "" ||
			c.Request().Header.Get("Content-Type") != "application/octet-stream" || c.Request().Header.Get("Cache-Control") != "no-store" {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid runner diagnostics request")
		}
		integrationID := c.Param("integration_id")
		workflowJobID, jobErr := strconv.ParseInt(c.Param("workflow_job_id"), 10, 64)
		runAttempt, attemptErr := strconv.ParseInt(c.Param("run_attempt"), 10, 64)
		if jobErr != nil || attemptErr != nil || workflowJobID <= 0 || runAttempt <= 0 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid runner diagnostics capability")
		}
		authorization := c.Request().Header.Get("Authorization")
		parts := strings.Split(authorization, " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) != 64 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid runner diagnostics capability")
		}
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxActionsRunnerDiagnosticsBody+1))
		if err != nil || len(body) == 0 || len(body) > maxActionsRunnerDiagnosticsBody {
			clear(body)
			return echo.NewHTTPError(http.StatusBadRequest, "invalid runner diagnostics body")
		}
		defer clear(body)
		digest := sha256.Sum256(body)
		if c.Request().Header.Get("X-Hako-SHA256") != hex.EncodeToString(digest[:]) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid runner diagnostics digest")
		}
		redacted := actionsrunner.RedactRunnerDiagnostics(body)
		defer clear(redacted)
		redactedDigest := sha256.Sum256(redacted)
		now := time.Now().UTC()
		_, err = transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			return struct{}{}, githubwebhook.PutActionsRunnerDiagnostics(ctx, tx, integrationID, workflowJobID, runAttempt, parts[1], redacted,
				hex.EncodeToString(redactedDigest[:]), now)
		})
		if err != nil {
			if errors.Is(err, githubwebhook.ErrActionsRunnerDiagnosticsDenied) {
				return echo.NewHTTPError(http.StatusForbidden, "runner diagnostics upload rejected")
			}
			return databaseUnavailable(c)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.NoContent(http.StatusNoContent)
	}
}
