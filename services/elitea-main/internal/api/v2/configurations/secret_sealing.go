package configurations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/jackc/pgx/v5"
)

// SecretSealer stores plaintext configuration secrets in the project vault.
//
// It runs inside the transaction the caller owns, so the sanitized row and the
// sealed values commit together. A partial failure stores neither.
type SecretSealer interface {
	SealProjectHiddenSecrets(
		ctx context.Context,
		tx pgx.Tx,
		projectID int64,
		mutations []configurationapp.HiddenSecretMutation,
	) error
}

// WithSecretSealer supplies the project vault this router seals credentials
// into.
//
// Without it a write that carries a schema-declared password field FAILS. It
// does not fall back to a plaintext row. The compatibility Create and Update
// used to marshal the caller's `data` object into p_{project}.configuration
// verbatim, so every provider api_key was stored in clear text and every
// project VIEWER read it back through the list and detail routes.
func WithSecretSealer(sealer SecretSealer) Option {
	return func(handler *Handler) {
		handler.secretSealer = sealer
	}
}

// errConfigurationSecretStoreUnavailable reports that a write carries a secret
// and this router has no vault to put it in.
var errConfigurationSecretStoreUnavailable = errors.New("configuration secret store is unavailable")

// configurationWriteFailure is one refused write. The handler writes it and
// stops. The pointer is nil when the write may continue.
type configurationWriteFailure struct {
	status  int
	message string
}

func (f *configurationWriteFailure) write(w http.ResponseWriter) {
	apierr.WriteStatus(w, f.status, f.message)
}

// newConfigurationSecretID returns one hidden-secret name.
//
// The name is 32 lowercase hexadecimal characters. That is the current
// UUID-without-hyphens shape, and ExtractCurrentConfigurationSecrets rejects
// every other shape.
func newConfigurationSecretID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// sealConfigurationSecrets replaces every schema-declared password value in
// `data` with a {{secret.NAME}} reference. It returns the sanitized object and
// the plaintext values the caller must seal in the vault.
//
// A type the pinned catalogue does not describe is REFUSED. It used to keep
// its data verbatim, and that default is the leak this route exists to close:
// the catalogue is the only authority on which field is a secret, so "no
// entry" meant "no field is a secret" and the api_key of every uncatalogued
// provider type was written into p_{project}.configuration in clear text. The
// same absence also makes `sectionFor` return "", so the row is invisible to
// the gateway's `WHERE section = 'ai_credentials'` read — a stored credential
// that leaks and does not work. `anthropic`, `open_ai_azure` and `vllm` were
// exactly that, until they were added to the catalogue.
//
// Absence now fails closed, which is what the target mutation service already
// does (application/configurations/mutation.go refuses an unknown type with
// CurrentConfigurationMutationUnknownType). A deployment that grows a dynamic
// MCP or provider-hub type must reconcile it into the catalogue first; that is
// the documented boundary, and it is a smaller cost than storing a credential
// this platform cannot describe.
//
// The reference IS the redaction. The read paths return the stored column, so
// they now emit the reference rather than the credential, which is what the
// reference implementation returns as well.
func (h *Handler) sealConfigurationSecrets(
	ctx context.Context,
	configType string,
	data map[string]any,
) (map[string]any, []configurationapp.HiddenSecretMutation, *configurationWriteFailure) {
	if len(data) == 0 {
		return data, nil, nil
	}
	properties, ok := h.configurationDataProperties(configType)
	if !ok {
		// The message does not echo the submitted type. It names the rule, so
		// an operator reads "this deployment's catalogue does not describe it"
		// rather than "unsupported", which sends them to the wrong place.
		return nil, nil, &configurationWriteFailure{
			status: http.StatusBadRequest,
			message: "this platform does not describe this configuration type, " +
				"so its data cannot be stored safely",
		}
	}
	sanitized, mutations, err := configurationapp.SealCurrentConfigurationSecrets(
		ctx, data, properties, configType, newConfigurationSecretID)
	if err != nil {
		return nil, nil, sealFailureFor(err)
	}
	if len(mutations) > 0 && h.secretSealer == nil {
		// Fail closed. A plaintext fallback reproduces the defect in the
		// profile the chart ships by default.
		return nil, nil, &configurationWriteFailure{
			status:  http.StatusServiceUnavailable,
			message: "the configuration secret store is not available",
		}
	}
	return sanitized, mutations, nil
}

// configurationDataProperties returns the `data.properties` object of one
// configuration type, and reports whether the catalogue describes it.
//
// A false second result is a refusal, not a fallback: see
// sealConfigurationSecrets.
func (h *Handler) configurationDataProperties(configType string) (map[string]any, bool) {
	if configType == "" {
		return nil, false
	}
	dataSchema, ok := h.catalog.DataSchemaByType(configType)
	if !ok {
		return nil, false
	}
	properties, ok := dataSchema["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		return nil, false
	}
	return properties, true
}

// sealFailureFor maps one sanitizer error to a status.
//
// A caller mistake — a password field that holds a number, or a duplicated
// reference name — is a 400. A generator failure is this server's fault.
func sealFailureFor(err error) *configurationWriteFailure {
	if errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationSecrets) {
		return &configurationWriteFailure{
			status:  http.StatusBadRequest,
			message: "invalid configuration data",
		}
	}
	return &configurationWriteFailure{
		status:  http.StatusInternalServerError,
		message: "internal server error",
	}
}

// withConfigurationSecretTx runs one row write and its vault write in ONE
// transaction.
//
// Order matters only for locking. Both writes commit together, so a failed
// vault write leaves no {{secret.NAME}} reference that names nothing.
func (h *Handler) withConfigurationSecretTx(
	ctx context.Context,
	projectID int,
	mutations []configurationapp.HiddenSecretMutation,
	write func(pgx.Tx) error,
) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := write(tx); err != nil {
		return err
	}
	if err := h.sealTransactionSecrets(ctx, tx, projectID, mutations); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// sealTransactionSecrets stores the plaintext values in the project vault. It
// runs inside the transaction that wrote the row.
func (h *Handler) sealTransactionSecrets(
	ctx context.Context,
	tx pgx.Tx,
	projectID int,
	mutations []configurationapp.HiddenSecretMutation,
) error {
	if len(mutations) == 0 {
		return nil
	}
	if h.secretSealer == nil {
		return errConfigurationSecretStoreUnavailable
	}
	if err := h.secretSealer.SealProjectHiddenSecrets(ctx, tx, int64(projectID), mutations); err != nil {
		return fmt.Errorf("%w: %w", errConfigurationSecretStoreUnavailable, err)
	}
	return nil
}

// updatedConfigurationType names the type the row holds after this partial
// update.
//
// The body may omit `type`. The stored row then keeps its own type, and that
// type names the schema which says which field holds a password. The read
// locks the row, so the answer cannot go stale before the UPDATE.
func (h *Handler) updatedConfigurationType(
	ctx context.Context,
	tx pgx.Tx,
	body map[string]any,
	schema string,
	configID string,
) (string, error) {
	if submitted := strVal(body, "type"); submitted != "" {
		return submitted, nil
	}
	if _, present := body["data"]; !present {
		// The statement writes no data column, so it can store no secret.
		return "", nil
	}
	query := fmt.Sprintf(
		`SELECT type FROM %s.configuration WHERE %s = $1 FOR UPDATE`,
		schema, configurationIDColumn(configID))
	var storedType string
	if err := tx.QueryRow(ctx, query, configID).Scan(&storedType); err != nil {
		return "", err
	}
	return storedType, nil
}

// sealConfigurationBodyData replaces the password values inside body["data"]
// with hidden-secret references. The caller builds its statement from the
// sanitized body.
func (h *Handler) sealConfigurationBodyData(
	ctx context.Context,
	body map[string]any,
	configType string,
) ([]configurationapp.HiddenSecretMutation, *configurationWriteFailure) {
	raw, present := body["data"]
	if !present {
		return nil, nil
	}
	data, isObject := raw.(map[string]any)
	if !isObject || len(data) == 0 {
		return nil, nil
	}
	sealed, mutations, failure := h.sealConfigurationSecrets(ctx, configType, data)
	if failure != nil {
		return nil, failure
	}
	body["data"] = sealed
	return mutations, nil
}

// invalidStoredConfiguration reports a row this handler read and could not
// decode.
func invalidStoredConfiguration() *configurationWriteFailure {
	return &configurationWriteFailure{
		status:  http.StatusInternalServerError,
		message: "invalid stored configuration",
	}
}
