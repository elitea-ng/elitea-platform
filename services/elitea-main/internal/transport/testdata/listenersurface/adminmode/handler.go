// Fixture for the administration-mode scan (ADR-0059 R1.8): an internal
// handler that builds a synthetic principal and resolves the administration
// mode for it, once by the constant and once by the literal.
package adminmode

import (
	"context"

	"example.invalid/fixture/auth"
)

func resolve(ctx context.Context, resolver auth.PermissionResolver, actor int64) error {
	principal := auth.User{ID: actor, UserID: actor}
	if _, err := resolver.ResolvePermissions(ctx, principal, auth.PermissionModeAdministration, ""); err != nil {
		return err
	}
	_, err := resolver.ResolvePermissions(ctx, principal, "administration", "")
	return err
}
