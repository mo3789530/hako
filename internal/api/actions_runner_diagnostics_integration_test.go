//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestActionsRunnerDiagnosticsCapabilityAttachesRedactedLogToExactCommitCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const integrationID = "integration_diag"
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenBytes, err := hex.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	tokenDigest := sha256.Sum256(tokenBytes)
	clear(tokenBytes)
	now := time.Now().UTC()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_diag','Diagnostics', $1)`, []any{now}},
		{`INSERT INTO github_actions_runner_jobs
			(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,
			head_branch,head_sha,labels,state,created_at,updated_at,claim_id,lease_expires_at,registration_token_issued_at,resource_plane_id,diagnostics_upload_token_sha256)
			VALUES ($1,'delivery_diag','tenant_diag',10,20,30,40,1,'CI','test','main',$2,'["self-hosted","hako"]'::jsonb,'starting',$3,$3,'claim-diagnostics',$4,$3,'rp-tokyo-01',$5)`,
			[]any{integrationID, strings.Repeat("a", 40), now, now.Add(time.Hour), hex.EncodeToString(tokenDigest[:])}},
		{`INSERT INTO github_actions_runner_checks
			(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,workflow_run_id,commit_sha,check_name,external_id,
			status,conclusion,started_at,completed_at,output_title,output_summary,revision,updated_at)
			VALUES ($1,30,1,'tenant_diag',10,20,40,$2,'Hako Actions Runner','hako-actions-runner/integration_diag/30/1',
			'completed','success',$3,$3,'Hako Actions Runner','GitHub reported this workflow job completed with conclusion: success.',2,$3)`,
			[]any{integrationID, strings.Repeat("a", 40), now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewHandler(nil, pool)
	path := "/v1/internal/actions-runner/integrations/" + integrationID + "/jobs/30/attempts/1/diagnostics"
	logs := []byte("job complete\nAuthorization: Bearer ghs_syntheticgithubsecret123456\napi_key=sk-ant-synthetic-secret\n")
	request := func(body []byte, credential string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Cache-Control", "no-store")
		digest := sha256.Sum256(body)
		req.Header.Set("X-Hako-SHA256", hex.EncodeToString(digest[:]))
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	if got := request(logs, token); got.Code != http.StatusNoContent || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("upload response status=%d headers=%v body=%s", got.Code, got.Header(), got.Body.String())
	}
	var summary string
	var revision int64
	var storedDiagnostics, storedDigest string
	if err := pool.QueryRow(ctx, `SELECT output_summary,revision,diagnostics_summary,diagnostics_sha256 FROM github_actions_runner_checks WHERE integration_id=$1 AND workflow_job_id=30 AND run_attempt=1`, integrationID).
		Scan(&summary, &revision, &storedDiagnostics, &storedDigest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "Redacted runner diagnostics") || !strings.Contains(summary, "job complete") ||
		strings.Contains(summary, "ghs_syntheticgithubsecret123456") || strings.Contains(summary, "sk-ant-synthetic-secret") || revision != 3 || len(storedDigest) != 64 ||
		!strings.Contains(storedDiagnostics, "job complete") || strings.Contains(storedDiagnostics, "ghs_syntheticgithubsecret123456") || strings.Contains(storedDiagnostics, "sk-ant-synthetic-secret") {
		t.Fatalf("diagnostics were not redacted and attached to the immutable Check (revision=%d): %q", revision, summary)
	}
	if got := request(logs, token); got.Code != http.StatusNoContent {
		t.Fatalf("identical capability replay status=%d body=%s", got.Code, got.Body.String())
	}
	changed := []byte("different logs\n")
	if got := request(changed, token); got.Code != http.StatusForbidden {
		t.Fatalf("changed capability replay status=%d, want forbidden", got.Code)
	}
	if got := request(logs, strings.Repeat("b", 64)); got.Code != http.StatusForbidden {
		t.Fatalf("foreign capability status=%d, want forbidden", got.Code)
	}
}
