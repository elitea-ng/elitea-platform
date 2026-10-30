package platformconfig

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionCatalogue is the platform's connector catalogue mode (ADR-0044
// decision 1, #1501). The admin Configuration page writes it; every place a
// toolkit or MCP server is listed for selection, created, configured,
// discovered or admitted into a run reads it.
//
// `open` (the default) is today's behaviour: the toolkit type policy and the
// guardrails decide what a project may use. `approved_only` makes a toolkit
// type, a pre-built MCP catalogue entry or a custom MCP server URL usable
// only by the projects an approved connector request names. The governed
// credential profile forces `approved_only`, whatever this says
// (connectorapproval.EffectiveMode).
const SectionCatalogue = "catalogue"

// Field keys of SectionCatalogue.
const (
	KeyCatalogueMode = "mode"
	// KeyCatalogueRequireArchitectureAttestation makes a valid `architecture`
	// attestation of the request (ADR-0040 decision 6) a precondition of
	// its approval.
	KeyCatalogueRequireArchitectureAttestation = "require_architecture_attestation"
)

// CatalogueMode is the platform mode.
type CatalogueMode string

const (
	CatalogueOpen         CatalogueMode = "open"
	CatalogueApprovedOnly CatalogueMode = "approved_only"
)

// CatalogueSettings is the platform section.
type CatalogueSettings struct {
	Mode                           CatalogueMode `json:"mode"`
	RequireArchitectureAttestation bool          `json:"require_architecture_attestation"`
}

// DefaultCatalogueSettings is a deployment that never saved the section.
func DefaultCatalogueSettings() CatalogueSettings {
	return CatalogueSettings{Mode: CatalogueOpen}
}

// LoadCatalogueSettings reads the section over the defaults. A query error
// answers the defaults and the error; an enforcement point refuses rather
// than admit on that error. A database without the table never saved the
// section, so it answers the defaults without an error.
func LoadCatalogueSettings(ctx context.Context, pool *pgxpool.Pool) (CatalogueSettings, error) {
	values, err := Load(ctx, pool, SectionCatalogue)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return DefaultCatalogueSettings(), nil
	}
	if err != nil {
		return DefaultCatalogueSettings(), err
	}
	return CatalogueSettingsFromValues(values), nil
}

// CatalogueSettingsFromValues is the overlay. An unknown mode is
// `approved_only`: a hand-written row that misspelt the mode narrows, never
// widens.
func CatalogueSettingsFromValues(values Values) CatalogueSettings {
	settings := DefaultCatalogueSettings()
	if raw, present := values[KeyCatalogueMode]; present {
		mode, _ := raw.(string)
		switch CatalogueMode(strings.TrimSpace(mode)) {
		case CatalogueOpen:
			settings.Mode = CatalogueOpen
		default:
			settings.Mode = CatalogueApprovedOnly
		}
	}
	if raw, present := values[KeyCatalogueRequireArchitectureAttestation]; present {
		required, ok := raw.(bool)
		// A value that is not a boolean requires the attestation: narrower.
		settings.RequireArchitectureAttestation = required || !ok
	}
	return settings
}
