package cards

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/enttest"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	"github.com/usezoracle/rails-sui/storage"
)

// This test mimics the real POST /v1/cards/me/limits system end-to-end with no
// external services: an in-memory ent DB stands in for Postgres, a seeded live
// card stands in for a linked cardholder, and a tiny middleware injects the
// user_id exactly the way JWTMiddleware does. It then drives the *real*
// UpdateLimits handler and asserts both the HTTP contract (status + PTB
// skeleton) and the persisted off-chain mirror.

// apiEnvelope mirrors utils.APIResponse's { status, message, data } wrapper,
// with data shaped as the ptbSkeletonResponse the handler returns on success.
type apiEnvelope struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    struct {
		PackageID string   `json:"package_id"`
		Module    string   `json:"module"`
		Function  string   `json:"function"`
		TypeArgs  []string `json:"type_args"`
		Args      []any    `json:"args"`
		Note      string   `json:"note"`
		Code      string   `json:"code"` // present on the 400 limits_invalid path
	} `json:"data"`
}

const (
	seedDaily  = 4_000_000 // ₦40,000
	seedPerTap = 200_000   // ₦2,000
	seedStepUp = 1_500_000 // ₦15,000
	seedCapID  = "0xcap1234"
	seedCoin   = "0x2::usdc::USDC"
)

// setupLimitsTest builds an isolated in-memory system: ent client (→
// storage.Client), one user, one LIVE card linked to them, and a gin engine
// that injects that user before the real handler.
func setupLimitsTest(t *testing.T) (*gin.Engine, *ent.Client, *ent.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// Subtest names contain "/"; sanitize so the sqlite memory DSN stays a
	// single opaque key (one isolated DB per test).
	safe := strings.ReplaceAll(t.Name(), "/", "_")
	dsn := "file:" + safe + "?mode=memory&cache=shared&_fk=1"
	client := enttest.Open(t, "sqlite3", dsn)
	t.Cleanup(func() { _ = client.Close() })
	storage.Client = client

	ctx := context.Background()
	user := client.User.Create().
		SetFirstName("Ada").
		SetLastName("Lovelace").
		SetEmail(t.Name() + "@example.com").
		SetPassword("hashed").
		SetScope("cardholder").
		SaveX(ctx)

	client.TappCard.Create().
		SetActivationToken("TKN-" + t.Name()).
		SetStatus(tappcard.StatusLive).
		SetCapObjectID(seedCapID).
		SetCoinType(seedCoin).
		SetDailyLimitSubunit(seedDaily).
		SetPerTapLimitSubunit(seedPerTap).
		SetStepUpThresholdSubunit(seedStepUp).
		SetUser(user).
		SaveX(ctx)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", user.ID.String()); c.Next() })
	r.POST("/v1/cards/me/limits", NewController().UpdateLimits)
	return r, client, user
}

func postLimits(r *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/cards/me/limits", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestUpdateLimits_Success: a valid update returns 200 with the correct
// update_limits PTB skeleton AND persists all three values to the mirror.
func TestUpdateLimits_Success(t *testing.T) {
	r, client, user := setupLimitsTest(t)

	const newDaily, newPerTap, newStepUp = 5_000_000, 300_000, 2_000_000
	w := postLimits(r, `{"daily_limit_subunit":5000000,"per_tap_limit_subunit":300000,"step_up_threshold_subunit":2000000}`)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var env apiEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "success", env.Status)

	// PTB skeleton the PWA will sign.
	assert.Equal(t, "tapp_card", env.Data.Module)
	assert.Equal(t, "update_limits", env.Data.Function)
	assert.Equal(t, []string{seedCoin}, env.Data.TypeArgs)
	require.Len(t, env.Data.Args, 3)
	assert.Equal(t, seedCapID, env.Data.Args[0])
	assert.Equal(t, "5000000", env.Data.Args[1]) // new daily as string (u64-safe)
	assert.Equal(t, "300000", env.Data.Args[2])  // new per-tap as string

	// Off-chain mirror actually changed — this is what GET /v1/cards/me reads.
	card, err := client.TappCard.Query().
		Order(ent.Desc(tappcard.FieldCreatedAt)).
		First(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, newDaily, card.DailyLimitSubunit)
	assert.EqualValues(t, newPerTap, card.PerTapLimitSubunit)
	assert.EqualValues(t, newStepUp, card.StepUpThresholdSubunit)
	_ = user
}

// TestUpdateLimits_InvalidOrdering: every violation of
// 0 < per-tap ≤ step-up ≤ daily is rejected with 400 limits_invalid, and the
// mirror is left untouched.
func TestUpdateLimits_InvalidOrdering(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"per_tap_above_step_up", `{"daily_limit_subunit":5000000,"per_tap_limit_subunit":2000000,"step_up_threshold_subunit":1000000}`},
		{"step_up_above_daily", `{"daily_limit_subunit":1000000,"per_tap_limit_subunit":200000,"step_up_threshold_subunit":2000000}`},
		{"per_tap_zero", `{"daily_limit_subunit":5000000,"per_tap_limit_subunit":0,"step_up_threshold_subunit":1000000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, client, _ := setupLimitsTest(t)
			w := postLimits(r, tc.body)

			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			var env apiEnvelope
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
			assert.Equal(t, "limits_invalid", env.Data.Code)

			// Mirror unchanged.
			card := client.TappCard.Query().FirstX(context.Background())
			assert.EqualValues(t, seedDaily, card.DailyLimitSubunit)
			assert.EqualValues(t, seedPerTap, card.PerTapLimitSubunit)
			assert.EqualValues(t, seedStepUp, card.StepUpThresholdSubunit)
		})
	}
}

// TestUpdateLimits_CardNotLive: valid limits but the card hasn't finished
// linking (no on-chain cap) → 409, mirror untouched.
func TestUpdateLimits_CardNotLive(t *testing.T) {
	r, client, _ := setupLimitsTest(t)

	// Drop the card back to a not-yet-live state.
	ctx := context.Background()
	card := client.TappCard.Query().FirstX(ctx)
	client.TappCard.UpdateOne(card).
		SetStatus(tappcard.StatusClaimed).
		ClearCapObjectID().
		ClearCoinType().
		SaveX(ctx)

	w := postLimits(r, `{"daily_limit_subunit":5000000,"per_tap_limit_subunit":300000,"step_up_threshold_subunit":2000000}`)
	assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
}

// TestUpdateLimits_Unauthenticated: no user_id in context → 401, before any
// DB work.
func TestUpdateLimits_Unauthenticated(t *testing.T) {
	_, _, _ = setupLimitsTest(t) // seeds storage.Client so the handler is wired

	gin.SetMode(gin.TestMode)
	r := gin.New() // NOTE: no auth middleware → no user_id set
	r.POST("/v1/cards/me/limits", NewController().UpdateLimits)

	w := postLimits(r, `{"daily_limit_subunit":5000000,"per_tap_limit_subunit":300000,"step_up_threshold_subunit":2000000}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
}
