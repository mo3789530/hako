package workspacegateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mo3789530/hako/internal/workloadartifactaccess"
)

var ErrArtifactUnavailable = errors.New("Workload artifact is unavailable")

type ArtifactGrantResolver interface {
	Resolve(context.Context, workloadartifactaccess.Claims) (workloadartifactaccess.Grant, error)
}

type ArtifactObjectReader interface {
	Open(context.Context, workloadartifactaccess.Grant) (io.ReadCloser, error)
}

type ArtifactAccessConfig struct {
	TicketKey []byte
	Resolver  ArtifactGrantResolver
	Reader    ArtifactObjectReader
}

type artifactAccess struct {
	key      []byte
	resolver ArtifactGrantResolver
	reader   ArtifactObjectReader
}

func newArtifactAccess(config *ArtifactAccessConfig) (*artifactAccess, error) {
	if config == nil {
		return nil, nil
	}
	if len(config.TicketKey) < 32 || len(config.TicketKey) > 4096 || config.Resolver == nil || config.Reader == nil {
		return nil, ErrConfig
	}
	return &artifactAccess{key: append([]byte(nil), config.TicketKey...), resolver: config.Resolver, reader: config.Reader}, nil
}

func (g *Gateway) serveArtifact(w http.ResponseWriter, r *http.Request) {
	if g.artifacts == nil {
		http.NotFound(w, r)
		return
	}
	if r.TLS == nil {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Method != http.MethodGet {
		http.Error(w, "invalid artifact route", http.StatusBadRequest)
		return
	}
	artifactID := r.PathValue("artifact_id")
	token := bearer(r.Header.Get("Authorization"))
	claims, err := workloadartifactaccess.Verify(token, g.artifacts.key, time.Now().UTC())
	if err != nil || claims.ResourcePlane != string(g.config.ResourcePlaneID) || claims.ArtifactID != artifactID {
		http.Error(w, "artifact access is not authorized", http.StatusUnauthorized)
		return
	}
	grant, err := g.artifacts.resolver.Resolve(r.Context(), claims)
	if err != nil || !validArtifactGrant(grant) {
		http.Error(w, "artifact is unavailable", http.StatusNotFound)
		return
	}
	object, err := g.artifacts.reader.Open(r.Context(), grant)
	if err != nil || object == nil {
		http.Error(w, "artifact is unavailable", http.StatusNotFound)
		return
	}
	defer object.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(grant.SizeBytes, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+artifactID+`"`)
	w.Header().Set("X-Hako-Artifact-SHA256", grant.SHA256)
	w.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(w, object, grant.SizeBytes); err != nil {
		g.logger.Warn("Workload artifact stream ended early", "failure_class", "artifact_stream", "resource_plane_id", g.config.ResourcePlaneID)
	}
}

func validArtifactGrant(grant workloadartifactaccess.Grant) bool {
	return workloadartifactaccess.ValidateGrant(grant)
}
