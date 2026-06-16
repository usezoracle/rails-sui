package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/ent/user"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

type IntegratorsController struct {
	apiKeyService *services.APIKeyService
}

func NewIntegratorsController() *IntegratorsController {
	return &IntegratorsController{
		apiKeyService: services.NewAPIKeyService(),
	}
}

func apiKeySummary(k *ent.APIKey) gin.H {
	if k == nil {
		return nil
	}
	return gin.H{
		"id":      k.ID.String(),
		"secret":  "",
		"present": true,
	}
}

func senderIntegratorView(s *ent.SenderProfile) gin.H {
	out := gin.H{
		"id":               s.ID.String(),
		"type":             "sender",
		"email":            s.Edges.User.Email,
		"first_name":       s.Edges.User.FirstName,
		"last_name":        s.Edges.User.LastName,
		"is_active":        s.IsActive,
		"webhook_url":      s.WebhookURL,
		"provider_id":      s.ProviderID,
		"is_partner":       s.IsPartner,
		"updated_at":       s.UpdatedAt.Format(tsLayout),
		"api_key":          apiKeySummary(s.Edges.APIKey),
		"domain_whitelist": s.DomainWhitelist,
	}
	return out
}

func providerIntegratorView(p *ent.ProviderProfile) gin.H {
	out := gin.H{
		"id":                       p.ID,
		"type":                     "provider",
		"email":                    p.Edges.User.Email,
		"first_name":               p.Edges.User.FirstName,
		"last_name":                p.Edges.User.LastName,
		"is_active":                p.IsActive,
		"is_available":             p.IsAvailable,
		"trading_name":             p.TradingName,
		"currency":                 p.Edges.Currency.Code,
		"visibility_mode":          p.VisibilityMode,
		"is_kyb_verified":          p.IsKybVerified,
		"safehaven_account_number": p.SafehavenAccountNumber,
		"safehaven_account_id":     p.SafehavenAccountID,
		"updated_at":               p.UpdatedAt.Format(tsLayout),
		"api_key":                  apiKeySummary(p.Edges.APIKey),
	}
	return out
}

type createIntegratorReq struct {
	FirstName       string   `json:"first_name" binding:"required"`
	LastName        string   `json:"last_name" binding:"required"`
	Email           string   `json:"email" binding:"required,email"`
	Password        string   `json:"password" binding:"required,min=8,max=128"`
	WebhookURL      string   `json:"webhook_url"`
	DomainWhitelist []string `json:"domain_whitelist"`
}

// CreateIntegrator creates a sender integrator and returns the new API key secret.
//
//	POST /v1/admin/integrators
func (c *IntegratorsController) CreateIntegrator(ctx *gin.Context) {
	var body createIntegratorReq
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "failed to validate payload", u.GetErrorData(err))
		return
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to start transaction", nil)
		return
	}
	defer func() { _ = tx.Rollback() }()

	email := strings.ToLower(strings.TrimSpace(body.Email))
	existing, err := tx.User.Query().Where(user.EmailEQ(email)).Exist(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to validate email", nil)
		return
	}
	if existing {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "email already exists", nil)
		return
	}

	userRow, err := tx.User.Create().
		SetFirstName(strings.TrimSpace(body.FirstName)).
		SetLastName(strings.TrimSpace(body.LastName)).
		SetEmail(email).
		SetPassword(body.Password).
		SetScope("sender").
		Save(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "failed to create user", err.Error())
		return
	}

	profile, err := tx.SenderProfile.Create().
		SetUser(userRow).
		SetWebhookURL(strings.TrimSpace(body.WebhookURL)).
		SetDomainWhitelist(body.DomainWhitelist).
		SetIsActive(true).
		Save(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "failed to create sender profile", err.Error())
		return
	}

	key, secret, err := c.apiKeyService.GenerateAPIKey(ctx, tx, profile, nil)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to create api key", nil)
		return
	}

	if err := tx.Commit(); err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to commit integrator creation", nil)
		return
	}

	writeAudit(ctx, "integrator.create", profile.ID.String(), map[string]any{
		"api_key_id": key.ID.String(),
		"email":      email,
	})
	u.APIResponse(ctx, http.StatusCreated, "success", "integrator created", gin.H{
		"integrator": senderIntegratorView(&ent.SenderProfile{
			ID:             profile.ID,
			WebhookURL:     profile.WebhookURL,
			DomainWhitelist: profile.DomainWhitelist,
			IsActive:       profile.IsActive,
			UpdatedAt:      profile.UpdatedAt,
			Edges: ent.SenderProfileEdges{
				User: userRow,
				APIKey: key,
			},
		}),
		"api_key": gin.H{
			"id":      key.ID.String(),
			"secret":  secret,
			"present": true,
		},
	})
}

// GetIntegrators returns sender and provider integrators in one inventory view.
//
//	GET /v1/admin/integrators
func (c *IntegratorsController) GetIntegrators(ctx *gin.Context) {
	senders, err := storage.Client.SenderProfile.
		Query().
		WithUser().
		WithAPIKey().
		All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load sender integrators", nil)
		return
	}
	providers, err := storage.Client.ProviderProfile.
		Query().
		WithUser().
		WithCurrency().
		WithAPIKey().
		All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load provider integrators", nil)
		return
	}

	rows := make([]gin.H, 0, len(senders)+len(providers))
	for _, s := range senders {
		rows = append(rows, senderIntegratorView(s))
	}
	for _, p := range providers {
		rows = append(rows, providerIntegratorView(p))
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"count":       len(rows),
		"integrators": rows,
	})
}

// GetIntegrator returns one sender/provider integrator by kind and id.
//
//	GET /v1/admin/integrators/:kind/:id
func (c *IntegratorsController) GetIntegrator(ctx *gin.Context) {
	kind := strings.ToLower(strings.TrimSpace(ctx.Param("kind")))
	id := strings.TrimSpace(ctx.Param("id"))

	switch kind {
	case "sender":
		uuidID, err := uuid.Parse(id)
		if err != nil {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
			return
		}
		row, err := storage.Client.SenderProfile.Query().
			Where(senderprofile.IDEQ(uuidID)).
			WithUser().
			WithAPIKey().
			Only(ctx)
		if err != nil {
			u.APIResponse(ctx, http.StatusNotFound, "error", "sender integrator not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusOK, "success", "ok", senderIntegratorView(row))
	case "provider":
		row, err := storage.Client.ProviderProfile.Query().
			Where(providerprofile.IDEQ(id)).
			WithUser().
			WithCurrency().
			WithAPIKey().
			Only(ctx)
		if err != nil {
			u.APIResponse(ctx, http.StatusNotFound, "error", "provider integrator not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusOK, "success", "ok", providerIntegratorView(row))
	default:
		u.APIResponse(ctx, http.StatusBadRequest, "error", "kind must be sender|provider", nil)
	}
}

type rotateAPIKeyReq struct {
	Reason string `json:"reason"`
}

// RotateAPIKey deletes the current key and issues a new one for an integrator.
//
//	POST /v1/admin/integrators/:kind/:id/api-key/rotate
func (c *IntegratorsController) RotateAPIKey(ctx *gin.Context) {
	kind := strings.ToLower(strings.TrimSpace(ctx.Param("kind")))
	id := strings.TrimSpace(ctx.Param("id"))

	var body rotateAPIKeyReq
	if err := ctx.ShouldBindJSON(&body); err != nil {
		body.Reason = ""
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to start transaction", nil)
		return
	}
	defer func() { _ = tx.Rollback() }()

	switch kind {
	case "sender":
		uuidID, err := uuid.Parse(id)
		if err != nil {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
			return
		}
		row, err := tx.SenderProfile.Query().Where(senderprofile.IDEQ(uuidID)).WithUser().WithAPIKey().Only(ctx)
		if err != nil {
			u.APIResponse(ctx, http.StatusNotFound, "error", "sender integrator not found", nil)
			return
		}
		if row.Edges.APIKey != nil {
			if err := tx.APIKey.DeleteOne(row.Edges.APIKey).Exec(ctx); err != nil {
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to delete current API key", nil)
				return
			}
		}
		newKey, secret, err := c.apiKeyService.GenerateAPIKey(ctx, tx, row, nil)
		if err != nil {
			logger.Errorf("admin rotate sender api key: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to issue API key", nil)
			return
		}
		if err := tx.Commit(); err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to commit API key rotation", nil)
			return
		}
		writeAudit(ctx, "integrator.api_key.rotate", row.ID.String(), map[string]any{
			"kind": kind, "reason": body.Reason, "api_key_id": newKey.ID.String(),
		})
		u.APIResponse(ctx, http.StatusOK, "success", "API key rotated", gin.H{
			"integrator": senderIntegratorView(row),
			"api_key": gin.H{
				"id":      newKey.ID.String(),
				"secret":  secret,
				"present": true,
			},
		})
	case "provider":
		row, err := tx.ProviderProfile.Query().Where(providerprofile.IDEQ(id)).WithUser().WithCurrency().WithAPIKey().Only(ctx)
		if err != nil {
			u.APIResponse(ctx, http.StatusNotFound, "error", "provider integrator not found", nil)
			return
		}
		if row.Edges.APIKey != nil {
			if err := tx.APIKey.DeleteOne(row.Edges.APIKey).Exec(ctx); err != nil {
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to delete current API key", nil)
				return
			}
		}
		newKey, secret, err := c.apiKeyService.GenerateAPIKey(ctx, tx, nil, row)
		if err != nil {
			logger.Errorf("admin rotate provider api key: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to issue API key", nil)
			return
		}
		if err := tx.Commit(); err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to commit API key rotation", nil)
			return
		}
		writeAudit(ctx, "integrator.api_key.rotate", row.ID, map[string]any{
			"kind": kind, "reason": body.Reason, "api_key_id": newKey.ID.String(),
		})
		u.APIResponse(ctx, http.StatusOK, "success", "API key rotated", gin.H{
			"integrator": providerIntegratorView(row),
			"api_key": gin.H{
				"id":      newKey.ID.String(),
				"secret":  secret,
				"present": true,
			},
		})
	default:
		u.APIResponse(ctx, http.StatusBadRequest, "error", "kind must be sender|provider", nil)
		return
	}
}
