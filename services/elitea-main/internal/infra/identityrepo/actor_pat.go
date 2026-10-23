package identityrepo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// ActorPATName is the `auth_core__token.name` this service writes when it
// issues a personal access token on a person's behalf.
//
// It is deliberately a SENTENCE and not a plausible user-chosen label. The
// token is listed on /settings/tokens next to the keys the person created
// themselves (ListOwnedPATs filters nothing), and the name is the only thing
// that tells the two apart. "api" is already taken by the per-project system
// user, so it must not be reused here either.
const ActorPATName = "Auto-issued at first sign-in (revocable)"

// EnsureActorPAT gives a freshly provisioned account the credential the agent
// runtime signs its calls with, and returns whether it had to create one.
//
// # Why this exists
//
// internal/infra/authsvc.LocalIssuer never creates, rotates or stores a PAT —
// that contract is load-bearing and is not the place for this. It RE-SIGNS an
// existing active row (GetActivePATForUser). With no row, the lookup misses,
// the miss becomes ErrTokenRejected, and the chat turn fails as
// runtimeContextUnavailable(runtimeContextStageActorPATIssuance): an error that
// names a database stage for what is really missing setup. Before this, the
// only writer of that row was the user-driven POST /api/v2/auth/token/, so a
// person who had just signed in through single sign-on could not complete a
// chat turn until they went and created a key by hand — and a fresh install
// needed deploy/scripts/standalone-stack.sh to seed one with raw SQL.
//
// # The three decisions
//
//   - NAME: ActorPATName, above. Visibly system-issued.
//   - EXPIRY: NONE. An expiring auto-issued key would break every chat turn on
//     its expiry day, with the same misleading database-stage error and nothing
//     on screen to explain it. The lifetime that actually bounds it is the
//     account: GetActivePATPrincipalByUUID joins auth_core__user and refuses a
//     suspended owner, so suspending or deleting the account revokes the key.
//   - VISIBILITY: LISTED, on /settings/tokens, and deletable there like any
//     other key. A long-lived per-user credential its owner cannot see or
//     revoke is strictly worse than one they can.
//
// # Idempotency
//
// The condition is "this account has no active PAT at all", read through the
// SAME query the issuer reads (GetActivePATForUser), not "no token named
// ActorPATName". So a person who has created their own key gets nothing extra,
// a second sign-in creates nothing, and a person who deleted every key —
// including this one — gets a working runtime back on their next sign-in
// instead of a permanently broken one.
func EnsureActorPAT(
	ctx context.Context,
	queries *sqlcgen.Queries,
	userID int32,
) (bool, error) {
	if queries == nil {
		return false, errors.New("identityrepo: queries are required to issue an actor PAT")
	}
	if userID <= 0 {
		return false, errors.New("identityrepo: actor PAT owner is invalid")
	}

	_, err := queries.GetActivePATForUser(ctx, userID)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("identityrepo: read active actor PAT: %w", err)
	}

	tokenUUID, err := newActorPATUUID()
	if err != nil {
		return false, fmt.Errorf("identityrepo: generate actor PAT identifier: %w", err)
	}
	name := ActorPATName
	_, err = queries.CreatePATForActiveUser(ctx, sqlcgen.CreatePATForActiveUserParams{
		Uuid:   tokenUUID,
		Name:   &name,
		UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The owner is suspended. Provisioning refuses a suspended account
		// before it ever gets here, so this is a race, not a state to paper
		// over: a token must not be minted for an account that just lost the
		// right to sign in.
		return false, errors.New("identityrepo: actor PAT owner is not active")
	}
	if err != nil {
		return false, fmt.Errorf("identityrepo: create actor PAT: %w", err)
	}
	return true, nil
}

// newActorPATUUID mirrors the version-4 formatting the user-driven token route
// writes, so both writers put the same shape in auth_core__token.uuid.
func newActorPATUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:]), nil
}
