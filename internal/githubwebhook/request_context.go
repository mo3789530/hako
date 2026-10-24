package githubwebhook

import (
	"context"
	"regexp"
)

type correlationIDContextKey struct{}

var webhookCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// WithCorrelationID attaches a server-generated Hako request ID to a webhook
// request before it crosses into the standalone webhook HTTP handler.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	if ctx == nil || !webhookCorrelationIDPattern.MatchString(correlationID) {
		return ctx
	}
	return context.WithValue(ctx, correlationIDContextKey{}, correlationID)
}

// CorrelationIDFromContext returns only an ID set through WithCorrelationID.
func CorrelationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	correlationID, _ := ctx.Value(correlationIDContextKey{}).(string)
	if !webhookCorrelationIDPattern.MatchString(correlationID) {
		return ""
	}
	return correlationID
}
