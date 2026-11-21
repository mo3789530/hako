package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/domain"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type platformFailedDeliveriesResponse struct {
	Deliveries []webhookstore.PlatformFailedDelivery `json:"deliveries"`
	Total      int64                                 `json:"total"`
	Limit      int                                   `json:"limit"`
	Offset     int                                   `json:"offset"`
}

type platformWebhookReplayResponse struct {
	TenantID   domain.TenantID `json:"tenant_id"`
	DeliveryID string          `json:"delivery_id"`
	Status     string          `json:"status"`
}

func listPlatformGitHubFailedDeliveries(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if pool == nil {
			return databaseUnavailable(c)
		}
		limit, offset, err := parseWorkspacePage(c)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_pagination", Message: "Invalid pagination query"}})
		}
		items, total, err := webhookstore.ListPlatformFailedDeliveries(c.Request().Context(), pool, transaction.DefaultPolicy(), limit, offset, time.Now().UTC())
		if err != nil {
			return databaseUnavailable(c)
		}
		return c.JSON(http.StatusOK, platformFailedDeliveriesResponse{Deliveries: items, Total: total, Limit: limit, Offset: offset})
	}
}

func replayPlatformGitHubDelivery(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		integrationID := strings.TrimSpace(c.Param("integration_id"))
		deliveryID := strings.TrimSpace(c.Param("delivery_id"))
		var input webhookReplayRequest
		if err := decodeJSONRequest(c, &input); err != nil {
			if isRequestTooLarge(err) {
				return requestTooLarge(c)
			}
			return githubWebhookReplayError(c, http.StatusBadRequest, "invalid_request", "A replay reason is required")
		}
		if len(strings.TrimSpace(input.Reason)) < 8 || len(strings.TrimSpace(input.Reason)) > webhookstore.MaxWebhookReplayReasonBytes {
			return githubWebhookReplayError(c, http.StatusBadRequest, "invalid_request", "A replay reason between 8 and 500 bytes is required")
		}
		tenantID, err := webhookstore.RequestPlatformDeliveryReplay(c.Request().Context(), pool, transaction.DefaultPolicy(),
			integrationID, deliveryID, user.ID, input.Reason, time.Now().UTC())
		switch {
		case err == nil:
			return c.JSON(http.StatusAccepted, platformWebhookReplayResponse{TenantID: tenantID, DeliveryID: deliveryID, Status: "received"})
		case errors.Is(err, webhookstore.ErrInvalidDeliveryReplay):
			return githubWebhookReplayError(c, http.StatusBadRequest, "invalid_request", "Replay request is invalid")
		case errors.Is(err, webhookstore.ErrDeliveryReplayNotFound):
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Eligible Tenant-owned delivery not found"}})
		case errors.Is(err, webhookstore.ErrDeliveryReplayRateLimited):
			c.Response().Header().Set("Retry-After", "3600")
			return githubWebhookReplayError(c, http.StatusTooManyRequests, "rate_limited", "Replay rate limit exceeded")
		case errors.Is(err, webhookstore.ErrDeliveryReplayNotEligible), errors.Is(err, webhookstore.ErrStaleTenantWebhook):
			return githubWebhookReplayError(c, http.StatusConflict, "replay_not_eligible", "Delivery cannot be replayed under current Tenant policy")
		default:
			return databaseUnavailable(c)
		}
	}
}
