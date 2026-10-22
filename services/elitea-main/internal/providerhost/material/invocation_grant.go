package material

// Every callback bearer a facade mints carries a recorded grant
// (elitea_identity.callback_token_grant, shared/0159), not only Inventory
// `investigate`'s.
//
// WHY. elitea-vector admits an engine's callback bearer only when elitea-main's
// TokenIntrospectionService answers it active (ADR-0031), and that answer
// needs a grant naming the provider: the provider decides the one vector
// source the token may use (vectorintrospection.ProviderSource), and a PAT
// with the same name, binding and expiry has no grant and is refused. A
// DeepWiki build and an Inventory ingestion write vectors with the bearer of
// their own invocation, so every mint records one.
//
// HOW. GrantingMinter wraps the facade's Minter: a mint is followed by the
// invocation grant (provider, invoked tool, bound project), and a grant that
// cannot be recorded revokes the token it was for and fails the mint — the
// rewrite then refuses the invocation, as for any other mint failure. The
// source-tool grant `investigate` records afterwards (SourceRewriter.
// recordedGrant) upgrades this row in place (repos.CallbackTokenGrants.Record).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// InvocationGrantRecorder writes the grant of a freshly minted bearer
// (*repos.CallbackTokenGrants).
type InvocationGrantRecorder interface {
	RecordInvocation(ctx context.Context, grant repos.CallbackInvocationGrant) error
}

var _ InvocationGrantRecorder = (*repos.CallbackTokenGrants)(nil)

// UnnamedInvocationTool is the tool an invocation grant records when the mint
// is not made under an invoke route that names one.
const UnnamedInvocationTool = "invoke"

type invocationToolKey struct{}

// WithInvocationTool names the tool a mint under ctx is for. Invocation.Serve
// sets it from the route before the rewrite runs.
func WithInvocationTool(ctx context.Context, tool string) context.Context {
	return context.WithValue(ctx, invocationToolKey{}, tool)
}

// InvocationTool is the tool WithInvocationTool named, or
// UnnamedInvocationTool.
func InvocationTool(ctx context.Context) string {
	if tool, ok := ctx.Value(invocationToolKey{}).(string); ok && tool != "" {
		return tool
	}
	return UnnamedInvocationTool
}

// GrantingMinter records an invocation grant for provider with every bearer
// minter mints. It returns nil when minter is nil (a typed nil would pass a
// composition root's `!= nil`), and refuses a nil recorder or an empty
// provider: a minter that silently records nothing is the half-wired state
// every facade constructor here refuses.
func GrantingMinter(minter Minter, grants InvocationGrantRecorder, provider string) (Minter, error) {
	if minter == nil {
		return nil, nil
	}
	if grants == nil || provider == "" {
		return nil, errors.New("a granting callback minter needs a grant recorder and a provider")
	}
	return grantingMinter{minter: minter, grants: grants, provider: provider}, nil
}

type grantingMinter struct {
	minter   Minter
	grants   InvocationGrantRecorder
	provider string
}

func (m grantingMinter) Mint(ctx context.Context, ownerID, projectID int64, name string, lifetime time.Duration) (Grant, error) {
	grant, err := m.minter.Mint(ctx, ownerID, projectID, name, lifetime)
	if err != nil {
		return Grant{}, err
	}
	if err := m.grants.RecordInvocation(ctx, repos.CallbackInvocationGrant{
		TokenUUID: grant.UUID, OwnerID: ownerID, ProjectID: projectID,
		Provider: m.provider, Tool: InvocationTool(ctx),
	}); err != nil {
		// The caller never sees this bearer (the mint fails), so nothing else
		// would revoke it. WithoutCancel: a cancelled request must not leave
		// a live, ungranted token behind.
		if revokeErr := m.minter.Revoke(context.WithoutCancel(ctx), ownerID, grant.UUID); revokeErr != nil {
			err = errors.Join(err, fmt.Errorf("revoke ungranted callback token: %w", revokeErr))
		}
		return Grant{}, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	return grant, nil
}

func (m grantingMinter) Revoke(ctx context.Context, ownerID int64, tokenUUID string) error {
	return m.minter.Revoke(ctx, ownerID, tokenUUID)
}
