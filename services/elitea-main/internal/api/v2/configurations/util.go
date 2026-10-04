package configurations

import "strconv"

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// firstStrVal returns the first non-empty string value among keys. The create
// and update bodies the UI sends carry the display name under `elitea_title`
// (features/credentials/api/configurations.ts CreateConfigurationBody); `name`
// is accepted as a legacy alias. Reading `name` alone wrote every row's
// elitea_title as "" — and that column is UNIQUE, so the second configuration
// in any project failed permanently (#131).
func firstStrVal(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strVal(m, key); value != "" {
			return value
		}
	}
	return ""
}

// configurationIDColumn picks the column a `{configID}` path segment matches.
// POST returns both `id` and `uuid`; matching on `id` alone made every
// detail/update/delete call carrying the uuid 404 (#131). The result is one of
// two literals, never caller data, so it is safe to interpolate into SQL.
//
// The id column is a 32-bit integer. A numeric segment outside that range
// matches the uuid text instead, which no row has, so the caller answers 404.
// Binding it to the integer column made PostgreSQL fail the statement, and the
// route answered 500.
func configurationIDColumn(configID string) string {
	if _, err := strconv.ParseInt(configID, 10, 32); err == nil {
		return "id"
	}
	return "uuid::text"
}

// configurationSectionFilter builds the `AND section = ANY($n)` clause for a
// list request's `?section=` values, or an empty clause (and no arguments)
// when none were supplied — matching the legacy "no filter means everything"
// semantics. Repeated values follow Flask request.args.getlist, so
// `?section=llm&section=tts` selects both. The placeholder index is passed in
// because the surrounding queries already bind limit/offset.
func configurationSectionFilter(sections []string, placeholder int) (string, []any) {
	if len(sections) == 0 {
		return "", nil
	}
	return " AND section = ANY($" + strconv.Itoa(placeholder) + ")", []any{sections}
}

// nullableStrVal returns nil rather than "" so a COALESCE($n, column) update
// preserves the stored value for a field the request body omits, which is what
// the COALESCE was already written to do.
func nullableStrVal(value string) any {
	if value == "" {
		return nil
	}
	return value
}
