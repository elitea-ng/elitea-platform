package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

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
	writer *CurrentSecretVaultRepository
	reader storage.SecretVaultLoader
}

var _ workspacehandoff.EnvVault = (*WorkspaceEnvVault)(nil)

var workspaceEnvSecretName = regexp.MustCompile(`^workspace_env_[0-9a-f]{32}$`)

// NewWorkspaceEnvVault builds the adapter. writer and reader must be built
// from the SAME master key source.
func NewWorkspaceEnvVault(writer *CurrentSecretVaultRepository, reader storage.SecretVaultLoader) (*WorkspaceEnvVault, error) {
	if writer == nil || reader == nil {
		return nil, errors.New("workspace env vault dependencies are required")
	}
	return &WorkspaceEnvVault{writer: writer, reader: reader}, nil
}

func (v *WorkspaceEnvVault) PutEnv(ctx context.Context, projectID int64, name string, env map[string]string) error {
	if !workspaceEnvSecretName.MatchString(name) {
		return errors.New("workspace env vault: invalid secret name")
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return v.writer.MutateProject(ctx, projectID, []centrysecrets.Mutation{{
		Collection: centrysecrets.HiddenSecrets, Name: name, Value: string(encoded),
	}})
}

func (v *WorkspaceEnvVault) DeleteEnv(ctx context.Context, projectID int64, name string) error {
	if !workspaceEnvSecretName.MatchString(name) {
		return errors.New("workspace env vault: invalid secret name")
	}
	return v.writer.MutateProject(ctx, projectID, []centrysecrets.Mutation{{
		Collection: centrysecrets.HiddenSecrets, Name: name, Delete: true,
	}})
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
