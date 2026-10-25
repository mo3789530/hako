package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestInvalidRequestHelpersReturnStableJSONErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*echo.Context) error
		code    string
	}{
		{name: "workspace request", handler: invalidRequest, code: "invalid_request"},
		{name: "workload run request", handler: invalidWorkloadRunRequest, code: "invalid_request"},
		{name: "github installation request", handler: invalidGitHubInstallationRequest, code: "invalid_request"},
		{name: "artifact access request", handler: invalidWorkloadArtifactAccess, code: "invalid_request"},
		{name: "workspace service action", handler: invalidServiceAction, code: "invalid_service_action"},
		{name: "tenant quota request", handler: invalidTenantQuotaRequest, code: "invalid_quota"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), recorder)
			if err := test.handler(context); err != nil {
				t.Fatalf("error response helper returned error: %v", err)
			}
			if recorder.Code != http.StatusBadRequest || recorder.Header().Get(echo.HeaderContentType) != echo.MIMEApplicationJSON {
				t.Fatalf("response status/content-type = %d/%q", recorder.Code, recorder.Header().Get(echo.HeaderContentType))
			}
			var response errorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error.Code != test.code || response.Error.Message == "" {
				t.Fatalf("error response = %+v, want code %q and a safe message", response.Error, test.code)
			}
		})
	}
}
