package configurations

import "testing"

// The gate folds case and trims whitespace: the type column is free text and
// an unknown type is stored as sent, so "PgVector" must not pass as one.
func TestIsRetiredConfigurationTypeIgnoresCaseAndPadding(t *testing.T) {
	for _, typeName := range []string{"pgvector", "PgVector", "pgvector ", " PGVECTOR\t", "\npgvector\n"} {
		if !IsRetiredConfigurationType(typeName) {
			t.Errorf("IsRetiredConfigurationType(%q) = false, want true", typeName)
		}
	}
	for _, typeName := range []string{"", "openai", "pgvector_x", "vector", "pg vector"} {
		if IsRetiredConfigurationType(typeName) {
			t.Errorf("IsRetiredConfigurationType(%q) = true, want false", typeName)
		}
	}
}
