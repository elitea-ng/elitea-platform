package configurations

import "strings"

// The `pgvector` configuration type and the `pgvector_configuration` toolkit
// setting are retired (owner decision 2026-10-21). Toolkit indexes are kept by
// elitea-vector (ADR-0031) and their metadata by the index registry (ADR-0030),
// so nothing in the platform reads a PgVector connection any more.
//
// The policy for what a deployment still holds is deliberately non-destructive:
//
//   - A create, or an update of a stored row, of type `pgvector` is refused with
//     400 and RetiredPgvectorConfigurationMessage.
//   - Stored `pgvector` rows are kept, never deleted by a migration. They are no
//     longer listed (configuration lists, the type list, the model catalogue)
//     and no runtime path reads them. DELETE by id still works, so an operator
//     can remove the stale connection string.
//   - A toolkit's stored `pgvector_configuration` is dropped from its settings
//     on read and on write; the key is accepted on write only to be discarded,
//     so an older client that still sends it is not refused.
const (
	RetiredPgvectorConfigurationType    = "pgvector"
	RetiredPgvectorToolkitSetting       = "pgvector_configuration"
	RetiredPgvectorConfigurationMessage = "The pgvector configuration type has been removed: " +
		"indexes are kept in the platform vector store and need no configuration."
)

// IsRetiredConfigurationType reports whether typeName is a configuration type
// the platform no longer creates, updates, lists or reads.
//
// The comparison ignores case and surrounding whitespace. The type column is
// free text and an unknown type is stored as sent, so an exact match let
// "PgVector" or "pgvector " through the gate and into the table, where the
// list filter (the same normalisation in SQL, retiredConfigurationTypeFilter
// in the v2 handler) would then have to decide what it is.
func IsRetiredConfigurationType(typeName string) bool {
	return strings.EqualFold(strings.TrimSpace(typeName), RetiredPgvectorConfigurationType)
}

// WithoutRetiredToolkitSettings returns settings without the retired
// `pgvector_configuration` key. The input map is never written to: a copy is
// returned only when the key is present, otherwise the same map comes back.
func WithoutRetiredToolkitSettings(settings map[string]any) map[string]any {
	if _, present := settings[RetiredPgvectorToolkitSetting]; !present {
		return settings
	}
	kept := make(map[string]any, len(settings)-1)
	for key, value := range settings {
		if key != RetiredPgvectorToolkitSetting {
			kept[key] = value
		}
	}
	return kept
}
