package material

// The adapter from the platform's token minter to this package's Minter.
//
// It exists so a facade's own tests do not need a database to exercise the
// invoke path: Minter is a two-method interface over values this package
// defines, and the real implementation lives where tokens are issued. The
// alternative — importing the auth package's concrete type into every
// facade's signatures — would make every test of a body rewrite need a
// pgxpool, and a test that needs a database to check JSON is a test that does
// not get written.

import (
	"context"
	"time"

	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
)

type authCallbackMinter struct {
	minter *v2auth.CallbackTokenMinter
}

// NewCallbackMinter wraps the platform's minter.
//
// Returns nil for a nil minter rather than a non-nil interface holding a nil
// pointer: composition roots test `!= nil` to decide whether a facade can be
// mounted, and a typed nil would pass that test and panic on the first
// invocation — the trap this service has already been bitten by on /healthz.
func NewCallbackMinter(minter *v2auth.CallbackTokenMinter) Minter {
	if minter == nil {
		return nil
	}
	return authCallbackMinter{minter: minter}
}

func (a authCallbackMinter) Mint(
	ctx context.Context,
	ownerID, projectID int64,
	name string,
	lifetime time.Duration,
) (Grant, error) {
	token, err := a.minter.Mint(ctx, ownerID, projectID, name, lifetime)
	if err != nil {
		return Grant{}, err
	}
	return Grant{Bearer: token.Bearer, Expires: token.Expires, UUID: token.UUID}, nil
}

func (a authCallbackMinter) Revoke(ctx context.Context, ownerID int64, tokenUUID string) error {
	return a.minter.Revoke(ctx, ownerID, tokenUUID)
}
