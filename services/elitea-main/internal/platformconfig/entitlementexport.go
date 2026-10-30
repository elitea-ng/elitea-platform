package platformconfig

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionEntitlementExport governs who may read the entitlement export over
// SCIM (ADR-0044 decision 5, #1501). The read-only SCIM tree at
// /api/v2/scim/v2/entitlements authenticates with the same SCIM client
// credential as provisioning, and SCIM clients carry no scope; this section
// is the opt-in. A client not listed here is refused (403) on every path of
// the tree, so an upgrade does not widen what an existing provisioning
// credential reveals. The admin CSV export is gated on the
// `entitlements.export` permission instead and is not affected.
const SectionEntitlementExport = "entitlement_export"

// KeyEntitlementExportSCIMClientIDs is the list of SCIM client ids
// (centry.scim_clients ids, Admin › Configuration › Authentication) allowed
// to read the tree.
const KeyEntitlementExportSCIMClientIDs = "scim_client_ids"

// MaxEntitlementExportSCIMClients bounds the list.
const MaxEntitlementExportSCIMClients = 64

// EntitlementExportSettings is the section.
type EntitlementExportSettings struct {
	SCIMClientIDs []int64 `json:"scim_client_ids"`
}

// DefaultEntitlementExportSettings is a deployment that never saved the
// section: no client may read the tree.
func DefaultEntitlementExportSettings() EntitlementExportSettings {
	return EntitlementExportSettings{SCIMClientIDs: []int64{}}
}

// AllowsSCIMClient reports whether the client id is listed.
func (s EntitlementExportSettings) AllowsSCIMClient(id int64) bool {
	for _, listed := range s.SCIMClientIDs {
		if listed == id {
			return true
		}
	}
	return false
}

// LoadEntitlementExportSettings reads the section over the defaults. A query
// error answers the defaults (nobody listed) and the error; the SCIM tree
// answers 503 on it. A database without the table never saved the section.
func LoadEntitlementExportSettings(ctx context.Context, pool *pgxpool.Pool) (EntitlementExportSettings, error) {
	values, err := Load(ctx, pool, SectionEntitlementExport)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return DefaultEntitlementExportSettings(), nil
	}
	if err != nil {
		return DefaultEntitlementExportSettings(), err
	}
	return EntitlementExportSettingsFromValues(values), nil
}

// EntitlementExportSettingsFromValues is the overlay. An entry that is not a
// positive integer is dropped: a hand-written row that misspelt an id lists
// fewer clients, never more.
func EntitlementExportSettingsFromValues(values Values) EntitlementExportSettings {
	settings := DefaultEntitlementExportSettings()
	raw, _ := values[KeyEntitlementExportSCIMClientIDs].([]any)
	for _, item := range raw {
		if id, ok := EntitlementExportClientID(item); ok {
			settings.SCIMClientIDs = append(settings.SCIMClientIDs, id)
		}
	}
	return settings
}

// EntitlementExportClientID reads one stored or submitted list entry: a
// positive integral JSON number.
func EntitlementExportClientID(item any) (int64, bool) {
	var number float64
	switch value := item.(type) {
	case float64:
		number = value
	case int64:
		return value, value > 0
	case int:
		return int64(value), value > 0
	default:
		return 0, false
	}
	if number <= 0 || number > 1<<53 || number != math.Trunc(number) {
		return 0, false
	}
	return int64(number), true
}

// ValidateEntitlementExportValues checks a save: the list, when present, is
// an array of at most MaxEntitlementExportSCIMClients positive integers.
func ValidateEntitlementExportValues(values Values) error {
	raw, present := values[KeyEntitlementExportSCIMClientIDs]
	if !present {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%q must be an array", KeyEntitlementExportSCIMClientIDs)
	}
	if len(items) > MaxEntitlementExportSCIMClients {
		return fmt.Errorf("%q takes at most %d clients", KeyEntitlementExportSCIMClientIDs, MaxEntitlementExportSCIMClients)
	}
	for index, item := range items {
		if _, ok := EntitlementExportClientID(item); !ok {
			return fmt.Errorf("%q[%d] must be a positive integer SCIM client id", KeyEntitlementExportSCIMClientIDs, index)
		}
	}
	return nil
}
