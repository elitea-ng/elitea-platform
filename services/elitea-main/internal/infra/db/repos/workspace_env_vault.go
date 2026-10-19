package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// WorkspaceEnvVault keeps a handed-off thread's harvested environment as one
// HIDDEN secret of the project vault (ADR-0034 decision 5, amended
// 2026-10-15). The secret's name is random (`workspace_env_<32 hex>`) and
// lives only on the handoff row, so no configuration can reference it with
// {{secret.NAME}}. The value is a JSON object of name to value; it is never
// logged.
type WorkspaceEnvVault struct {
	writer  *CurrentSecretVaultRepository
	reader  storage.SecretVaultLoader
	creator ProjectVaultCreator
}

var _ workspacehandoff.EnvVault = (*WorkspaceEnvVault)(nil)

var workspaceEnvSecretName = regexp.MustCompile(`^workspace_env_[0-9a-f]{32}$`)

// NewWorkspaceEnvVault builds the adapter. writer and reader must be built
// from the SAME master key source; creator is the ONE project vault creator
// (internal/api/v2/secrets.Handler, #399), which makes the vault of a project
// that has none (the default project of a fresh install) so a send with env
// never fails on a missing vault.
func NewWorkspaceEnvVault(writer *CurrentSecretVaultRepository, reader storage.SecretVaultLoader, creator ProjectVaultCreator) (*WorkspaceEnvVault, error) {
	if writer == nil || reader == nil || creator == nil {
		return nil, errors.New("workspace env vault dependencies are required")
	}
	return &WorkspaceEnvVault{writer: writer, reader: reader, creator: creator}, nil
}

func (v *WorkspaceEnvVault) PutEnv(ctx context.Context, projectID int64, name string, env map[string]string) error {
	if !workspaceEnvSecretName.MatchString(name) {
		return errors.New("workspace env vault: invalid secret name")
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return err
	}
	mutation := []centrysecrets.Mutation{{Collection: centrysecrets.HiddenSecrets, Name: name, Value: string(encoded)}}
	err = v.writer.MutateProject(ctx, projectID, mutation)
	if errors.Is(err, errCurrentVaultAbsent) {
		// Create the vault (idempotent: create-if-absent), then write.
		if createErr := v.creator.EnsureProjectVault(ctx, strconv.FormatInt(projectID, 10)); createErr != nil {
			return fmt.Errorf("workspace env vault: create the project vault: %w", createErr)
		}
		err = v.writer.MutateProject(ctx, projectID, mutation)
	}
	return err
}

func (v *WorkspaceEnvVault) DeleteEnv(ctx context.Context, projectID int64, name string) error {
	if !workspaceEnvSecretName.MatchString(name) {
		return errors.New("workspace env vault: invalid secret name")
	}
	err := v.writer.MutateProject(ctx, projectID, []centrysecrets.Mutation{{
		Collection: centrysecrets.HiddenSecrets, Name: name, Delete: true,
	}})
	if errors.Is(err, errCurrentVaultAbsent) {
		return nil // no vault: nothing to delete
	}
	return err
}

func (v *WorkspaceEnvVault) ReadEnv(ctx context.Context, projectID int64, name string) (map[string]string, error) {
	if !workspaceEnvSecretName.MatchString(name) {
		return nil, errors.New("workspace env vault: invalid secret name")
	}
	vault, err := v.reader.LoadProjectVault(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("workspace env vault: open: %w", err)
	}
	secret, err := vault.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("workspace env vault: lookup: %w", err)
	}
	if !secret.Hidden {
		return nil, errors.New("workspace env vault: the secret is not hidden")
	}
	env := map[string]string{}
	if err := json.Unmarshal([]byte(secret.Value), &env); err != nil {
		return nil, errors.New("workspace env vault: the secret does not decode")
	}
	return env, nil
}
