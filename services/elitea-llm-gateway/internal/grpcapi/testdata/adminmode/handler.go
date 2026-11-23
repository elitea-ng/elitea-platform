// Fixture for the administration-mode scan (ADR-0059 R1.8): an internal
// handler that builds a synthetic principal and resolves the administration
// mode for it: by auth's constant, by rolepolicy's, by a literal held in a
// constant and by a literal argument.
package adminmode

import (
	"context"

	"example.invalid/fixture/auth"
	"example.invalid/fixture/rolepolicy"
)

const heldMode = "administration"

func resolve(ctx context.Context, resolver auth.PermissionResolver, actor int64) error {
	principal := auth.User{ID: actor, UserID: actor}
	for _, mode := range []string{auth.PermissionModeAdministration, rolepolicy.ModeAdministration, heldMode} {
		if _, err := resolver.ResolvePermissions(ctx, principal, mode, ""); err != nil {
			return err
		}
	}
	_, err := resolver.ResolvePermissions(ctx, principal, "administration", "")
	return err
}
