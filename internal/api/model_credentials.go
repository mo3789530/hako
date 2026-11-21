package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/transaction"
)

type ModelCredentialRequest struct {
	Provider        string `json:"provider"`
	Purpose         string `json:"purpose"`
	BindingID       string `json:"binding_id"`
	SecretVersionID string `json:"secret_version_id"`
}

type ModelCredentialRotateRequest struct {
	Purpose            string `json:"purpose"`
	SecretVersionID    string `json:"secret_version_id"`
	ExpectedGeneration int64  `json:"expected_generation"`
}

type ModelCredentialDisableRequest struct {
	Purpose            string `json:"purpose"`
	ExpectedGeneration int64  `json:"expected_generation"`
}

type ModelCredentialResult struct {
	Credential *modelcredentials.Credential  `json:"credential,omitempty"`
	Items      []modelcredentials.Credential `json:"items,omitempty"`
}

func listModelCredentials(pool transaction.Beginner) echo.HandlerFunc {
	return modelCredentialHandler(pool, "list")
}

func createModelCredential(pool transaction.Beginner) echo.HandlerFunc {
	return modelCredentialHandler(pool, "create")
}

func rotateModelCredential(pool transaction.Beginner) echo.HandlerFunc {
	return modelCredentialHandler(pool, "rotate")
}

func disableModelCredential(pool transaction.Beginner) echo.HandlerFunc {
	return modelCredentialHandler(pool, "disable")
}

func modelCredentialHandler(pool transaction.Beginner, action string) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		if tenantID == "" {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_tenant", Message: "A valid Tenant ID is required"}})
		}
		var request ModelCredentialRequest
		var rotation ModelCredentialRotateRequest
		var disable ModelCredentialDisableRequest
		switch action {
		case "create":
			if err := decodeJSONRequest(c, &request); err != nil || request.Provider != modelcredentials.ProviderAnthropic || strings.TrimSpace(request.BindingID) == "" ||
				!modelcredentials.ValidSecretVersionID(strings.TrimSpace(request.SecretVersionID)) {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_model_credential", Message: "A supported provider, approved binding ID, and Secret version ID are required"}})
			}
			request.Purpose = normalizeModelCredentialPurpose(request.Purpose)
			if !modelcredentials.ValidPurpose(request.Purpose) {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_model_credential_purpose", Message: "A supported credential purpose is required"}})
			}
		case "rotate":
			if err := decodeJSONRequest(c, &rotation); err != nil || rotation.ExpectedGeneration <= 0 ||
				!modelcredentials.ValidSecretVersionID(strings.TrimSpace(rotation.SecretVersionID)) {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_rotation", Message: "A Secret version ID and positive expected generation are required"}})
			}
			rotation.Purpose = normalizeModelCredentialPurpose(rotation.Purpose)
			if !modelcredentials.ValidPurpose(rotation.Purpose) {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_model_credential_purpose", Message: "A supported credential purpose is required"}})
			}
		case "disable":
			if err := decodeJSONRequest(c, &disable); err != nil || disable.ExpectedGeneration <= 0 {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_disable", Message: "A positive expected generation is required"}})
			}
			disable.Purpose = normalizeModelCredentialPurpose(disable.Purpose)
			if !modelcredentials.ValidPurpose(disable.Purpose) {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_model_credential_purpose", Message: "A supported credential purpose is required"}})
			}
		}
		provider := request.Provider
		if action == "rotate" || action == "disable" {
			provider = strings.TrimSpace(c.Param("provider"))
		}
		if action != "list" && provider != modelcredentials.ProviderAnthropic {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "unsupported_provider", Message: "Unsupported model credential provider"}})
		}
		var credentialID string
		if action == "create" {
			var err error
			credentialID, err = idgen.New("mcred_")
			if err != nil {
				return databaseUnavailable(c)
			}
		}
		now := time.Now().UTC()
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (ModelCredentialResult, error) {
			if _, err := authz.RequireTenantRole(ctx, tx, user.ID, tenantID, domain.TenantRoleOwner, domain.TenantRoleAdmin); err != nil {
				return ModelCredentialResult{}, err
			}
			switch action {
			case "list":
				items, err := modelcredentials.List(ctx, tx, tenantID)
				return ModelCredentialResult{Items: items}, err
			case "create":
				created, err := modelcredentials.Create(ctx, tx, modelcredentials.Credential{ID: credentialID, TenantID: tenantID, Provider: request.Provider, Purpose: request.Purpose,
					BindingID: strings.TrimSpace(request.BindingID), SecretVersionID: strings.TrimSpace(request.SecretVersionID)}, user.ID, now)
				if err != nil {
					return ModelCredentialResult{}, err
				}
				if err := appendModelCredentialAudit(ctx, tx, tenantID, user.ID, "tenant.model_credential.create", created, now); err != nil {
					return ModelCredentialResult{}, err
				}
				return ModelCredentialResult{Credential: &created}, nil
			case "rotate":
				updated, err := modelcredentials.Rotate(ctx, tx, tenantID, provider, rotation.Purpose, strings.TrimSpace(rotation.SecretVersionID), rotation.ExpectedGeneration, user.ID, now)
				if err != nil {
					return ModelCredentialResult{}, err
				}
				if err := appendModelCredentialAudit(ctx, tx, tenantID, user.ID, "tenant.model_credential.rotate", updated, now); err != nil {
					return ModelCredentialResult{}, err
				}
				return ModelCredentialResult{Credential: &updated}, nil
			case "disable":
				updated, err := modelcredentials.Disable(ctx, tx, tenantID, provider, disable.Purpose, disable.ExpectedGeneration, user.ID, now)
				if err != nil {
					return ModelCredentialResult{}, err
				}
				if err := appendModelCredentialAudit(ctx, tx, tenantID, user.ID, "tenant.model_credential.disable", updated, now); err != nil {
					return ModelCredentialResult{}, err
				}
				return ModelCredentialResult{Credential: &updated}, nil
			default:
				return ModelCredentialResult{}, errors.New("unsupported model credential action")
			}
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, modelcredentials.ErrCredentialNotFound) {
			return tenantNotFound(c)
		}
		if errors.Is(err, modelcredentials.ErrCredentialExists) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "credential_exists", Message: "Tenant model credential already exists"}})
		}
		if errors.Is(err, modelcredentials.ErrCredentialConflict) {
			return c.JSON(http.StatusConflict, errorResponse{Error: errorBody{Code: "credential_changed", Message: "Credential is disabled, stale, or no longer approved"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, result)
	}
}

func normalizeModelCredentialPurpose(purpose string) string {
	if strings.TrimSpace(purpose) == "" {
		return modelcredentials.PurposeClaudeAction
	}
	return strings.TrimSpace(purpose)
}

func appendModelCredentialAudit(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, actor domain.UserID, action string,
	credential modelcredentials.Credential, now time.Time) error {
	details, err := json.Marshal(map[string]any{"provider": credential.Provider, "purpose": credential.Purpose, "binding_id": credential.BindingID,
		"credential_generation": credential.CredentialGeneration, "status": credential.Status})
	if err != nil {
		return errors.New("encode model credential audit details")
	}
	return audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: actor, Action: action, TargetType: "model_credential",
		TargetID: credential.ID, Details: details, OccurredAt: now})
}
