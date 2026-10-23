package auth

// Short-lived, project-bound bearers for a provider service to call back with
// (ADR-0022 decision 6).
//
// WHY IT LIVES HERE AND NOT IN THE CALLER. Issuing a usable bearer is three
// things that must happen together: a token row, a project binding, and a
// signature made with the key THIS deployment's validator reads back. The
// route above already knows all three, including the signer-beats-raw-key rule
// that a deployment with an authentication configuration file depends on
// (signBaselineToken's header records what happened when that was wrong). A
// second implementation elsewhere would be a second place to get any of them
// wrong, and the failure mode is a token that is issued successfully and
// rejected on first use.
//
// WHAT MAKES IT DIFFERENT FROM A PERSONAL ACCESS TOKEN. Nothing structural —
// it is the same row and the same bearer format, which is exactly why the
// platform's own validator accepts it with no changes. What differs is how it
// is issued: never at a user's request, always with an expiry, always bound to
// one project, and named after the invocation it was minted for so an operator
// reading the token list can tell what asked for it.
//
// WHAT THIS REPLACES. The legacy flow shipped workers a no-expiry system-user
// PAT. Anything that read one worker's environment held a credential to the
// whole platform, forever. This one expires in minutes and can only bill and
// reach the one project whose wiki is being generated.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsvc"
)

// ErrCallbackTokenRefused reports a mint the platform would not perform: an
// owner who is not a member of the project, a missing signing key, an invalid
// lifetime.
var ErrCallbackTokenRefused = errors.New("callback token refused")

// MinimumCallbackTokenLifetime is the floor on a mint.
//
// It is not arbitrary. SignBaselinePAT formats the expiry as
// "2006-01-02T15:04" — MINUTE precision, with the seconds discarded. A
// 30-second token would therefore be signed as expiring at the start of the
// containing minute, which is in the PAST for most of that minute: the bearer
// would be dead on arrival, intermittently, depending on when it was minted.
// One minute is the shortest lifetime the wire format can express.
const MinimumCallbackTokenLifetime = time.Minute

// CallbackTokenMinter issues them.
type CallbackTokenMinter struct {
	tokens     tokenRepository
	signer     TokenSigner
	signingKey []byte
}

// NewCallbackTokenMinter builds a minter over the platform's own token store.
//
// Exactly one of signer or signingKey must be usable, and the signer wins when
// both are present — the same precedence the route uses, for the same reason:
// a deployment with an authentication configuration file validates with the
// bytes of credentials.pat_signing_key_file, and signing with
// APPLICATION_SECRET_KEY there produces tokens that fail on first use.
func NewCallbackTokenMinter(
	pool *pgxpool.Pool,
	signer TokenSigner,
	signingKey []byte,
) (*CallbackTokenMinter, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: a database pool is required", ErrCallbackTokenRefused)
	}
	if signer == nil && len(signingKey) == 0 {
		return nil, fmt.Errorf(
			"%w: no signer and no signing key, so every minted token would be rejected on use",
			ErrCallbackTokenRefused)
	}
	return &CallbackTokenMinter{
		tokens:     newPostgresTokenRepository(pool),
		signer:     signer,
		signingKey: signingKey,
	}, nil
}

// CallbackToken is one minted bearer.
type CallbackToken struct {
	// Bearer is the signed string. It is never persisted in this form — only
	// the row it names is — so it exists in this process and in the invoke
	// payload, and nowhere else.
	Bearer string
	// Expires is when it stops validating, at minute precision.
	Expires time.Time
	// UUID identifies the token row, for revocation and for logging which
	// token an invocation was given without logging the bearer.
	UUID string
}

// Mint issues a bearer for ownerID, bound to projectID, valid for lifetime.
//
// The project binding is not advisory: the create path verifies the owner's
// membership INSIDE the creating transaction, so an owner who cannot reach the
// project gets no token row at all rather than a key bound to a project they
// were removed from between the check and the insert.
func (m *CallbackTokenMinter) Mint(
	ctx context.Context,
	ownerID int64,
	projectID int64,
	name string,
	lifetime time.Duration,
) (CallbackToken, error) {
	if m == nil {
		return CallbackToken{}, ErrCallbackTokenRefused
	}
	if ownerID <= 0 || projectID <= 0 {
		return CallbackToken{}, fmt.Errorf(
			"%w: owner %d project %d", ErrCallbackTokenRefused, ownerID, projectID)
	}
	if lifetime < MinimumCallbackTokenLifetime {
		return CallbackToken{}, fmt.Errorf(
			"%w: lifetime %s is shorter than the %s the signed expiry can express",
			ErrCallbackTokenRefused, lifetime, MinimumCallbackTokenLifetime)
	}

	// Rounded UP to the minute the signature will carry. Truncating would sign
	// an expiry earlier than the lifetime asked for, and the difference is
	// invisible until a generation runs past it.
	expires := time.Now().UTC().Add(lifetime).Truncate(time.Minute).Add(time.Minute)

	tokenName := name
	record, err := m.tokens.Create(ctx, tokenCreateInput{
		OwnerID:   ownerID,
		Name:      &tokenName,
		Expires:   &expires,
		ProjectID: &projectID,
	})
	if err != nil {
		if errors.Is(err, errTokenProjectForbidden) || errors.Is(err, errTokenForbidden) {
			return CallbackToken{}, fmt.Errorf(
				"%w: user %d may not act in project %d", ErrCallbackTokenRefused, ownerID, projectID)
		}
		return CallbackToken{}, fmt.Errorf("mint callback token: %w", err)
	}
	if record.UUID == nil {
		// Without a UUID the bearer names no row, and the validator has
		// nothing to resolve. Better to fail here than to hand out a string
		// that authenticates as nobody.
		return CallbackToken{}, fmt.Errorf(
			"%w: the token row carries no uuid", ErrCallbackTokenRefused)
	}

	bearer, err := m.sign(record)
	if err != nil {
		return CallbackToken{}, fmt.Errorf("sign callback token: %w", err)
	}
	return CallbackToken{Bearer: bearer, Expires: expires, UUID: *record.UUID}, nil
}

// Revoke removes a minted token row.
//
// Called when the invocation the token was minted for never started — the
// provider refused it, the hop failed. Leaving the row would be a live
// credential nobody is using, which is the state this whole design exists to
// avoid. It is best-effort by nature: the token expires on its own, so a
// failure here is a short-lived leftover and not an open-ended one.
func (m *CallbackTokenMinter) Revoke(ctx context.Context, ownerID int64, tokenUUID string) error {
	if m == nil || tokenUUID == "" || ownerID <= 0 {
		return ErrCallbackTokenRefused
	}
	return m.tokens.DeleteOwned(ctx, ownerID, tokenUUID)
}

func (m *CallbackTokenMinter) sign(record tokenRecord) (string, error) {
	if m.signer != nil {
		return m.signer.SignPAT(record.UUID, record.Expires)
	}
	return authsvc.SignBaselinePAT(m.signingKey, record.UUID, record.Expires)
}
