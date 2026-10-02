package toolkits

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5"
)

var errToolkitSecretStoreUnavailable = errors.New("toolkit secret store is unavailable")

// ToolkitSecretSealer stores project secrets inside the toolkit row transaction.
type ToolkitSecretSealer interface {
	SealProjectHiddenSecrets(
		context.Context,
		pgx.Tx,
		int64,
		[]configurationapp.HiddenSecretMutation,
	) error
}

// WithSecretSealer enables atomic sealing for dynamic toolkit fields.
func WithSecretSealer(sealer ToolkitSecretSealer) Option {
	return func(handler *Handler) { handler.secretSealer = sealer }
}

type transactionalToolkitRepository interface {
	CreateToolkitTx(context.Context, pgx.Tx, string, map[string]any) (map[string]any, error)
	UpdateToolkitTx(context.Context, pgx.Tx, string, string, map[string]any) (map[string]any, error)
}

func newToolkitSecretID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (h *Handler) sealDynamicToolkitSettings(
	ctx context.Context,
	toolkitType string,
	settings map[string]any,
) (map[string]any, []configurationapp.HiddenSecretMutation, int, string) {
	if len(settings) == 0 || h.dynamicTypeSchemas == nil {
		return settings, nil, 0, ""
	}
	catalogue, err := h.dynamicTypeSchemas.ListToolkitTypeSchemas(ctx)
	if err != nil {
		return nil, nil, http.StatusServiceUnavailable, "dynamic toolkit schemas are unavailable"
	}
	typeSchema, found := catalogue[toolkitType]
	if !found {
		return settings, nil, 0, ""
	}
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		return nil, nil, http.StatusInternalServerError, "dynamic toolkit schema is invalid"
	}
	sealed, mutations, err := configurationapp.SealCurrentConfigurationSecrets(
		ctx,
		settings,
		properties,
		toolkitType,
		newToolkitSecretID,
	)
	if err != nil {
		return nil, nil, http.StatusBadRequest, "invalid toolkit settings"
	}
	if len(mutations) > 0 && (h.pool == nil || h.secretSealer == nil) {
		return nil, nil, http.StatusServiceUnavailable, "the toolkit secret store is unavailable"
	}
	return sealed, mutations, 0, ""
}

func (h *Handler) createToolkitWithSecrets(
	ctx context.Context,
	projectID string,
	body map[string]any,
	mutations []configurationapp.HiddenSecretMutation,
) (map[string]any, error) {
	return h.writeToolkitWithSecrets(ctx, projectID, mutations, func(repo transactionalToolkitRepository, tx pgx.Tx) (map[string]any, error) {
		return repo.CreateToolkitTx(ctx, tx, projectID, body)
	})
}

func (h *Handler) updateToolkitWithSecrets(
	ctx context.Context,
	projectID string,
	toolkitID string,
	body map[string]any,
	mutations []configurationapp.HiddenSecretMutation,
) (map[string]any, error) {
	return h.writeToolkitWithSecrets(ctx, projectID, mutations, func(repo transactionalToolkitRepository, tx pgx.Tx) (map[string]any, error) {
		return repo.UpdateToolkitTx(ctx, tx, projectID, toolkitID, body)
	})
}

func (h *Handler) writeToolkitWithSecrets(
	ctx context.Context,
	projectID string,
	mutations []configurationapp.HiddenSecretMutation,
	write func(transactionalToolkitRepository, pgx.Tx) (map[string]any, error),
) (map[string]any, error) {
	repo, ok := h.repo.(transactionalToolkitRepository)
	if !ok || h.pool == nil || h.secretSealer == nil {
		return nil, errToolkitSecretStoreUnavailable
	}
	projectNumber, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || projectNumber <= 0 {
		return nil, fmt.Errorf("invalid project")
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	item, err := write(repo, tx)
	if err != nil {
		return nil, err
	}
	if err := h.secretSealer.SealProjectHiddenSecrets(ctx, tx, projectNumber, mutations); err != nil {
		return nil, fmt.Errorf("%w: %w", errToolkitSecretStoreUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return item, nil
}
