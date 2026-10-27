package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// ErrRequiredButAbsent is a revision that requires Vault for a credential
// class on a deployment with no Vault configured.
var ErrRequiredButAbsent = errors.New("secretstore vault: the policy revision requires Vault, which is not configured")

// Prerequisite is the governed-profile readiness check for Vault/OpenBao
// (profile specification item 1.5, gate D1).
//
// A revision that requires Vault for no class passes without any network
// call, whether or not Vault is configured. A revision that requires it for
// at least one class needs a configured store whose readiness scope logs in
// and whose KV mount enforces cas_required=true.
type Prerequisite struct {
	store *Store
}

var _ profile.Prerequisite = Prerequisite{}

// NewPrerequisite returns the check for store, which is nil when Vault is
// not configured.
func NewPrerequisite(store *Store) Prerequisite { return Prerequisite{store: store} }

// Name implements profile.Prerequisite.
func (Prerequisite) Name() string { return "secretstore.vault" }

// Check implements profile.Prerequisite.
func (p Prerequisite) Check(ctx context.Context, revision profile.PolicyRevision) error {
	required := len(revision.Document.VaultRequiredCredentialClasses)
	if required == 0 {
		return nil
	}
	if p.store == nil {
		return fmt.Errorf("%w (%d credential classes)", ErrRequiredButAbsent, required)
	}
	if err := p.store.CheckReady(ctx); err != nil {
		return fmt.Errorf("secretstore vault is not ready for %d required credential classes: %w", required, err)
	}
	return nil
}
