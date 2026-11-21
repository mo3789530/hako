package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/resourceplanehealth"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const (
	healthReportTimestampHeader = "X-Hako-Timestamp"
	healthReportIDHeader        = "X-Hako-Report-ID"
	healthReportSignatureHeader = "X-Hako-Signature"
	maxHealthReportBodyBytes    = 4 << 10
)

// ResourcePlaneHealthReportKeyResolver returns the active HMAC key for a
// Resource Plane. Implementations should resolve it from a protected secret
// store; callers must not persist the key in the control-plane database.
type ResourcePlaneHealthReportKeyResolver func(context.Context, domain.ResourcePlaneID) ([]byte, error)

type automatedResourcePlaneHealthReport struct {
	Status     resourceplanehealth.Status `json:"status"`
	Reason     string                     `json:"reason"`
	ObservedAt time.Time                  `json:"observed_at"`
}

type automatedResourcePlaneHealthResponse struct {
	Health   resourceplanehealth.Health `json:"health"`
	Accepted bool                       `json:"accepted"`
}

func putAutomatedResourcePlaneHealth(pool transaction.Beginner, resolveKey ResourcePlaneHealthReportKeyResolver) echo.HandlerFunc {
	return func(c *echo.Context) error {
		resourcePlaneID := domain.ResourcePlaneID(c.Param("resource_plane_id"))
		if resourcePlaneID == "" || pool == nil || resolveKey == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid health report credentials")
		}
		contentType := strings.ToLower(strings.TrimSpace(c.Request().Header.Get("Content-Type")))
		if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "health report must be application/json")
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, maxHealthReportBodyBytes))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid health report body")
		}
		reportID := c.Request().Header.Get(healthReportIDHeader)
		timestampText := c.Request().Header.Get(healthReportTimestampHeader)
		timestamp, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || reportID == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid health report credentials")
		}
		now := time.Now().UTC()
		requestTime := time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid health report credentials")
		}
		resolverCtx, cancelResolver := context.WithTimeout(c.Request().Context(), 2*time.Second)
		key, err := resolveKey(resolverCtx, resourcePlaneID)
		cancelResolver()
		if err != nil || len(key) < 32 {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid health report credentials")
		}
		now = time.Now().UTC()
		requestTime = time.Unix(timestamp, 0).UTC()
		if requestTime.Before(now.Add(-resourceplanehealth.FreshnessTTL)) || requestTime.After(now.Add(30*time.Second)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid health report credentials")
		}
		if !validResourcePlaneHealthSignature(key, c.Request().Method, c.Request().URL.EscapedPath(), timestampText, reportID, body,
			c.Request().Header.Get(healthReportSignatureHeader)) {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid health report credentials")
		}
		var report automatedResourcePlaneHealthReport
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid health report")
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid health report")
		}
		if report.ObservedAt.IsZero() {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid health report")
		}
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (automatedResourcePlaneHealthResponse, error) {
			health, accepted, err := resourceplanehealth.PutAutomatedReport(ctx, tx, resourcePlaneID, reportID,
				report.Status, report.Reason, report.ObservedAt, now)
			return automatedResourcePlaneHealthResponse{Health: health, Accepted: accepted}, err
		})
		if errors.Is(err, resourceplanehealth.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "Resource Plane not found")
		}
		if errors.Is(err, resourceplanehealth.ErrReplayConflict) || errors.Is(err, resourceplanehealth.ErrStaleReport) || errors.Is(err, resourceplanehealth.ErrInvalid) {
			return echo.NewHTTPError(http.StatusConflict, "health report rejected")
		}
		if err != nil {
			return fmt.Errorf("store automated Resource Plane health report: %w", err)
		}
		return c.JSON(http.StatusOK, result)
	}
}

func validResourcePlaneHealthSignature(key []byte, method, path, timestamp, reportID string, body []byte, signature string) bool {
	if len(key) < 32 || len(signature) != sha256.Size*2 {
		return false
	}
	bodyHash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, path, timestamp, reportID, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	expected := make([]byte, hex.DecodedLen(len(signature)))
	if _, err := hex.Decode(expected, []byte(signature)); err != nil {
		return false
	}
	return hmac.Equal(mac.Sum(nil), expected)
}
