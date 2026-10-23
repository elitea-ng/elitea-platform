package repos

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestConfigurationMetadataReadsAStoredNullAsAnEmptyObject covers the rows a
// shipped deployment already holds.
//
// The compatibility create route stored `meta = 'null'::jsonb` whenever the
// client omitted the key. Refusing such a row here aborted the WHOLE list, so
// ONE row answered 500 for every configuration in its section — the same
// whole-catalogue failure the model repository already refuses to accept from
// one bad row. `meta` is annotation; a null there names no lost value.
func TestConfigurationMetadataReadsAStoredNullAsAnEmptyObject(t *testing.T) {
	row := sqlcgen.GetCurrentConfigurationRow{
		ID:        1,
		ProjectID: 1,
		Data:      []byte(`{"base_url":"https://example.invalid"}`),
		Meta:      []byte("null"),
		CreatedAt: pgtype.Timestamp{Valid: true},
	}
	configuration, err := mapCurrentConfiguration(row)
	if err != nil {
		t.Fatalf("a stored null metadata must not fail the read: %v", err)
	}
	if configuration.Meta == nil || len(configuration.Meta) != 0 {
		t.Fatalf("meta=%#v", configuration.Meta)
	}
	if len(configuration.Data) != 1 {
		t.Fatalf("data=%#v", configuration.Data)
	}
}

// `data` stays strict. A null there would present a configuration as empty,
// which is a claim about its contents rather than an absent annotation.
func TestConfigurationDataStillRefusesAStoredNull(t *testing.T) {
	row := sqlcgen.GetCurrentConfigurationRow{
		ID:        1,
		ProjectID: 1,
		Data:      []byte("null"),
		Meta:      []byte("{}"),
		CreatedAt: pgtype.Timestamp{Valid: true},
	}
	if _, err := mapCurrentConfiguration(row); err == nil {
		t.Fatal("a stored null data must be refused")
	}
}

// Everything else malformed is still refused, so a row that lost its shape is
// not quietly published as `{}`.
func TestConfigurationMetadataRefusesOtherMalformedValues(t *testing.T) {
	for _, meta := range []string{`[]`, `"text"`, `{} {}`, ``, `{`} {
		row := sqlcgen.GetCurrentConfigurationRow{
			ID:        1,
			ProjectID: 1,
			Data:      []byte(`{}`),
			Meta:      []byte(meta),
			CreatedAt: pgtype.Timestamp{Valid: true},
		}
		if _, err := mapCurrentConfiguration(row); err == nil {
			t.Fatalf("meta=%q must be refused", meta)
		}
	}
}
