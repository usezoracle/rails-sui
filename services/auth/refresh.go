// Package auth provides server-side session helpers — refresh token
// issuance, rotation, replay detection, and revocation.
//
// The refresh-token state machine lives here so controllers stay thin
// and the security-critical logic (atomic rotate-and-revoke, family
// kill on replay) is in one auditable place.

package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/refreshtoken"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/token"
)

// ErrInvalidRefresh is returned when the submitted token isn't in the
// store, has expired, or was replayed after rotation. Callers should
// map this to 401 and force the client to re-login.
var ErrInvalidRefresh = errors.New("auth: refresh token is invalid")

// IssuedRefresh is the raw token to hand back to the client plus the
// row id (useful for tests and audit logging).
type IssuedRefresh struct {
	Raw       string
	RowID     uuid.UUID
	FamilyID  uuid.UUID
	ExpiresAt time.Time
}

// IssueNewFamily creates a fresh refresh token in a new family — used
// at /auth/login (and anywhere else a brand-new session is started).
func IssueNewFamily(ctx context.Context, userID uuid.UUID, ttl time.Duration, userAgent, ip string) (*IssuedRefresh, error) {
	familyID := uuid.New()
	return issue(ctx, userID, familyID, nil, ttl, userAgent, ip)
}

// Rotate validates the submitted raw refresh, revokes it, and issues a
// new refresh in the same family chain. Atomic — both writes happen in
// one transaction, so a crash partway can't leave both rows valid.
//
// Replay detection: if the submitted token is already revoked, the
// entire family is killed and ErrInvalidRefresh is returned. The
// legitimate user will have to sign in again — but so will the attacker.
func Rotate(ctx context.Context, rawSubmitted string, ttl time.Duration, userAgent, ip string) (*IssuedRefresh, *ent.User, error) {
	row, err := storage.Client.RefreshToken.
		Query().
		Where(refreshtoken.TokenHashEQ(token.HashToken(rawSubmitted))).
		WithOwner().
		Only(ctx)
	if err != nil {
		return nil, nil, ErrInvalidRefresh
	}

	// Replay — this token was already consumed. Kill the family.
	if row.RevokedAt != nil {
		_ = revokeFamily(ctx, row.FamilyID, "replay")
		return nil, nil, ErrInvalidRefresh
	}

	// Expired.
	if time.Now().After(row.ExpiresAt) {
		return nil, nil, ErrInvalidRefresh
	}

	if row.Edges.Owner == nil {
		return nil, nil, ErrInvalidRefresh
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Mark the old row revoked. The replaced_by_id is filled in below
	// once we have the new row's id; we set it in a follow-up update to
	// avoid a circular dependency at insert time.
	if _, err := tx.RefreshToken.
		UpdateOneID(row.ID).
		SetRevokedAt(time.Now()).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}

	issued, err := issueInTx(ctx, tx, row.Edges.Owner.ID, row.FamilyID, &row.ID, ttl, userAgent, ip)
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}

	if _, err := tx.RefreshToken.
		UpdateOneID(row.ID).
		SetReplacedByID(issued.RowID).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return issued, row.Edges.Owner, nil
}

// RevokeByRaw revokes the family the submitted token belongs to. Used
// at /auth/logout. Returns nil on success even when the token isn't
// found, so the endpoint stays idempotent and doesn't leak validity.
func RevokeByRaw(ctx context.Context, rawSubmitted string) error {
	row, err := storage.Client.RefreshToken.
		Query().
		Where(refreshtoken.TokenHashEQ(token.HashToken(rawSubmitted))).
		Only(ctx)
	if err != nil {
		return nil
	}
	return revokeFamily(ctx, row.FamilyID, "logout")
}

// RevokeAllForUser is the nuclear option — invalidate every session for
// a user. Useful after a password reset or admin-triggered force-logout.
func RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := storage.Client.RefreshToken.
		Update().
		Where(
			refreshtoken.HasOwnerWith(userEnt.IDEQ(userID)),
			refreshtoken.RevokedAtIsNil(),
		).
		SetRevokedAt(time.Now()).
		Save(ctx)
	return err
}

// ── internal helpers ────────────────────────────────────────────────

func issue(ctx context.Context, userID uuid.UUID, familyID uuid.UUID, parentID *uuid.UUID, ttl time.Duration, userAgent, ip string) (*IssuedRefresh, error) {
	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	issued, err := issueInTx(ctx, tx, userID, familyID, parentID, ttl, userAgent, ip)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return issued, nil
}

func issueInTx(ctx context.Context, tx *ent.Tx, userID uuid.UUID, familyID uuid.UUID, parentID *uuid.UUID, ttl time.Duration, userAgent, ip string) (*IssuedRefresh, error) {
	raw, err := token.GenerateOpaqueToken()
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(ttl)

	create := tx.RefreshToken.
		Create().
		SetTokenHash(token.HashToken(raw)).
		SetFamilyID(familyID).
		SetExpiresAt(expiresAt).
		SetOwnerID(userID)
	if parentID != nil {
		create = create.SetParentID(*parentID)
	}
	if userAgent != "" {
		// Truncate; schema caps user_agent at 255 to keep the row size
		// predictable even if browsers send something pathological.
		if len(userAgent) > 255 {
			userAgent = userAgent[:255]
		}
		create = create.SetUserAgent(userAgent)
	}
	if ip != "" {
		create = create.SetIPAddress(ip)
	}

	row, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return &IssuedRefresh{
		Raw:       raw,
		RowID:     row.ID,
		FamilyID:  familyID,
		ExpiresAt: expiresAt,
	}, nil
}

func revokeFamily(ctx context.Context, familyID uuid.UUID, _ string) error {
	_, err := storage.Client.RefreshToken.
		Update().
		Where(
			refreshtoken.FamilyIDEQ(familyID),
			refreshtoken.RevokedAtIsNil(),
		).
		SetRevokedAt(time.Now()).
		Save(ctx)
	return err
}

