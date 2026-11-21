package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const agentProposalPublishRoute = "/v1/agent-proposals/publish"

// WithAgentProposalPublication adds the Control Plane-only mTLS publication
// endpoint. The request contains only trusted database lookup coordinates.
func WithAgentProposalPublication(next http.Handler, service *AgentProposalPublicationService) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentProposalPublishRoute {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed)
			return
		}
		caller, err := authenticatedCaller(r)
		if err != nil || caller != controlPlaneURI {
			writeError(w, http.StatusUnauthorized)
			return
		}
		if service == nil || r.Body == nil || r.ContentLength > 2048 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 2049))
		if err != nil || len(data) > 2048 {
			writeError(w, http.StatusBadRequest)
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var input AgentProposalPublicationRequest
		if decoder.Decode(&input) != nil {
			writeError(w, http.StatusBadRequest)
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			writeError(w, http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
		defer cancel()
		response, err := service.Publish(ctx, caller, input)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeResponse(w, response)
	})
}
