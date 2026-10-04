package repos

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

// SetCurrentModelDefault preserves the current Configurations Vault contract:
// both default-model keys are replaced by one locked vault rewrite.
func (r *CurrentSecretVaultRepository) SetCurrentModelDefault(
	ctx context.Context,
	selection configurationapp.CurrentModelDefaultSelection,
) error {
	target := selection.TargetProjectID
	prefix := "default_" + selection.Section + "_model_"
	return r.MutateProject(ctx, int64(selection.ProjectID), []centrysecrets.Mutation{
		{
			Collection: centrysecrets.RegularSecrets,
			Name:       prefix + "name",
			Value:      selection.Name,
		},
		{
			Collection:   centrysecrets.RegularSecrets,
			Name:         prefix + "project_id",
			IntegerValue: &target,
		},
	})
}

// ClearCurrentModelDefault removes both default-model keys of one section
// from a project vault, or from the admin vault (#6826).
//
// The read and the delete run under the same row lock. With request.Match set,
// only a stored default that names exactly that model is removed, so a choice
// made after the model was deleted is never undone. It reports whether it
// removed anything. A vault that does not exist holds no default, so it is not
// an error.
func (r *CurrentSecretVaultRepository) ClearCurrentModelDefault(
	ctx context.Context,
	request configurationapp.CurrentModelDefaultClear,
) (bool, error) {
	if r == nil || r.store == nil {
		return false, ErrCurrentVaultUnavailable
	}
	if ctx == nil || request.Section == "" || (!request.Admin && request.ProjectID <= 0) {
		return false, ErrInvalidCurrentVaultMutation
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	vaultID := "admin"
	if !request.Admin {
		vaultID = "project-" + strconv.FormatInt(int64(request.ProjectID), 10)
	}
	prefix := "default_" + request.Section + "_model_"
	nameKey, projectIDKey := prefix+"name", prefix+"project_id"

	cleared := false
	err := r.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		vault, err := lockCurrentSecretVault(ctx, tx, vaultID)
		if errors.Is(err, errCurrentVaultAbsent) {
			return nil
		}
		if err != nil {
			return err
		}
		defer vault.destroy()

		var opened *centrysecrets.Vault
		if len(r.masterKey) == 0 {
			opened, err = centrysecrets.OpenUnwrapped(vault.encryptedProjectKey, vault.encryptedVault)
		} else {
			opened, err = centrysecrets.OpenWrapped(r.masterKey, vault.encryptedProjectKey, vault.encryptedVault)
		}
		if err != nil {
			return ErrCurrentVaultUnavailable
		}
		name, nameErr := opened.LookupRegular(nameKey)
		projectID, projectIDErr := opened.LookupRegularProjectID(projectIDKey)
		nameStored := !errors.Is(nameErr, centrysecrets.ErrSecretNotFound)
		projectIDStored := !errors.Is(projectIDErr, centrysecrets.ErrSecretNotFound)
		if !nameStored && !projectIDStored {
			return nil
		}
		if request.Match != nil &&
			(nameErr != nil || projectIDErr != nil ||
				name.Value != request.Match.Name || projectID.Value != request.Match.ProjectID) {
			return nil
		}
		if err := vault.mutate(ctx, tx, r.masterKey, []centrysecrets.Mutation{
			{Collection: centrysecrets.RegularSecrets, Name: nameKey, Delete: true},
			{Collection: centrysecrets.RegularSecrets, Name: projectIDKey, Delete: true},
		}); err != nil {
			return err
		}
		cleared = true
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, err
	}
	return cleared, nil
}

var _ configurationapp.PlatformModelDefaultWriter = (*CurrentSecretVaultRepository)(nil)
