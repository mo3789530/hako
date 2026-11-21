package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/idgen"
)

const requestIDHeader = "X-Hako-Request-ID"

type requestIDContextKey struct{}

// RequestIDFromContext returns the Hako-generated identifier for one API
// request. Client-provided identifiers are deliberately not trusted.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

func requestIDMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		requestID, err := idgen.New("req_")
		if err != nil {
			return echo.NewHTTPError(500, "request identifier unavailable")
		}
		started := time.Now()
		c.Response().Header().Set(requestIDHeader, requestID)
		request := c.Request().WithContext(context.WithValue(c.Request().Context(), requestIDContextKey{}, requestID))
		c.SetRequest(request)
		err = next(c)
		status := http.StatusOK
		if response, ok := c.Response().(*echo.Response); ok && response.Status != 0 {
			status = response.Status
		}
		if err != nil {
			status = echoStatus(err)
		}
		slog.Default().Info("Hako API request",
			"request_id", requestID,
			"method", c.Request().Method,
			"route", c.Path(),
			"status", status,
			"duration_ms", time.Since(started).Milliseconds())
		return err
	}
}

func echoStatus(err error) int {
	var httpError *echo.HTTPError
	if errors.As(err, &httpError) && httpError.Code > 0 {
		return httpError.Code
	}
	return 500
}
