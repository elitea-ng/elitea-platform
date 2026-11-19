package configurations

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftlogin"
)

// MicrosoftLoginCredentials is the sign-in's read of a saved Microsoft credential
// (ADR-0057 decision 6): the stored row of the project's own schema, with its
// references expanded and its secrets redeemed by the same resolver the stored
// connection check uses. A Custom app's client secret is only ever read here, at
// the moment the start or the callback needs it, and held for that request.
func (h *Handler) MicrosoftLoginCredentials() microsoftlogin.Credentials {
	return microsoftLoginCredentials{handler: h}
}

type microsoftLoginCredentials struct{ handler *Handler }

// Load reads the credential by its uuid in the project's schema. A credential of
// another project names no row in this schema, so it is not found.
func (c microsoftLoginCredentials) Load(ctx context.Context, projectID int64, credentialID string) (microsoftlogin.Credential, error) {
	h := c.handler
	project := strconv.FormatInt(projectID, 10)
	schema, err := tenantschema.Quote(project)
	if err != nil {
		return microsoftlogin.Credential{}, microsoftlogin.ErrCredentialNotFound
	}
	// Only the uuid addresses a credential here; a numeric id would also match the
	// serial column, which no client of this flow holds.
	if _, numeric := strconv.Atoi(credentialID); numeric == nil {
		return microsoftlogin.Credential{}, microsoftlogin.ErrCredentialNotFound
	}
	row, found, err := h.loadStoredConfigurationRow(ctx, schema, credentialID)
	if err != nil {
		return microsoftlogin.Credential{}, err
	}
	if !found || !strings.EqualFold(row.uuid, credentialID) {
		return microsoftlogin.Credential{}, microsoftlogin.ErrCredentialNotFound
	}
	if _, known := microsoftauth.For(row.configType); !known {
		return microsoftlogin.Credential{}, microsoftlogin.ErrCredentialNotFound
	}
	if h.storedResolver == nil {
		return microsoftlogin.Credential{}, errors.New("the stored credential resolver is not composed")
	}
	resolution, ok := storedResolutionFor(project, row)
	if !ok {
		return microsoftlogin.Credential{}, microsoftlogin.ErrCredentialNotFound
	}
	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	if err != nil || resolved == nil {
		// Never log err: a resolver error can name a secret reference.
		slog.WarnContext(ctx, "microsoft login: the credential did not resolve", "type", row.configType, "project_id", projectID)
		return microsoftlogin.Credential{}, errors.New("the credential did not resolve")
	}
	return microsoftlogin.Credential{ID: row.uuid, Type: row.configType, Data: resolved}, nil
}
