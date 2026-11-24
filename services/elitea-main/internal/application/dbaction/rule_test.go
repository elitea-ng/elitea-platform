package dbaction

import (
	"errors"
	"strings"
	"testing"
)

func validReadRule() Rule {
	r := Rule{
		ID:              10,
		ProjectID:       1,
		Revision:        1,
		Name:            "warehouse_pg.read",
		ConfigurationID: 7,
		Dialect:         DialectPostgres,
		DatabaseName:    "analytics",
		Mode:            ModeRead,
		Enabled:         true,
	}
	r.Normalize()
	return r
}

func validWriteRule() Rule {
	r := Rule{
		ID:              11,
		ProjectID:       1,
		Revision:        2,
		Name:            "warehouse_mysql.load",
		ConfigurationID: 9,
		Dialect:         DialectMySQL,
		DatabaseName:    "etl_prod",
		Mode:            ModeWrite,
		MarkerTable:     "public.elitea_effects",
		Enabled:         true,
	}
	r.Normalize()
	return r
}

func TestRuleNormalizeAndValidate(t *testing.T) {
	t.Parallel()

	// Omitted mode defaults to read, omitted schema_version defaults to RuleSchema.
	readRule := Rule{
		Name:            "sales.orders",
		ConfigurationID: 3,
		Dialect:         DialectPostgres,
		DatabaseName:    "sales_db",
	}
	readRule.Normalize()
	if readRule.SchemaVersion != RuleSchema || readRule.Mode != ModeRead || readRule.MarkerTable != "" {
		t.Fatalf("unexpected normalized read rule: %+v", readRule)
	}
	if err := readRule.Validate(); err != nil {
		t.Fatalf("valid read rule rejected: %v", err)
	}

	// Write rule without marker_table defaults to elitea_effects on Normalize.
	writeRule := Rule{
		Name:            "sales.load",
		ConfigurationID: 3,
		Dialect:         DialectMySQL,
		DatabaseName:    "sales_db",
		Mode:            ModeWrite,
	}
	writeRule.Normalize()
	if writeRule.MarkerTable != DefaultMarkerTable {
		t.Fatalf("write rule default marker_table = %q, want %q", writeRule.MarkerTable, DefaultMarkerTable)
	}
	if err := writeRule.Validate(); err != nil {
		t.Fatalf("valid write rule rejected: %v", err)
	}

	// Unnormalized write rule without marker_table is rejected by Validate.
	missingMarker := Rule{
		SchemaVersion:   RuleSchema,
		Name:            "sales.load",
		ConfigurationID: 3,
		Dialect:         DialectPostgres,
		DatabaseName:    "sales_db",
		Mode:            ModeWrite,
	}
	if err := missingMarker.Validate(); err == nil {
		t.Fatal("write rule without marker_table accepted")
	}
}

func TestRuleValidateRejectsInvalidFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*Rule)
		field  string
	}{
		{"wrong schema", func(r *Rule) { r.SchemaVersion = "v0" }, "schema_version"},
		{"empty name", func(r *Rule) { r.Name = "" }, "name"},
		{"uppercase name", func(r *Rule) { r.Name = "Warehouse.Read" }, "name"},
		{"name 129 bytes", func(r *Rule) { r.Name = strings.Repeat("a", 129) }, "name"},
		{"name trailing dot", func(r *Rule) { r.Name = "warehouse." }, "name"},
		{"zero configuration_id", func(r *Rule) { r.ConfigurationID = 0 }, "configuration_id"},
		{"negative configuration_id", func(r *Rule) { r.ConfigurationID = -1 }, "configuration_id"},
		{"unsupported dialect sqlite", func(r *Rule) { r.Dialect = "sqlite" }, "dialect"},
		{"empty database_name", func(r *Rule) { r.DatabaseName = "" }, "database_name"},
		{"database_name 129 bytes", func(r *Rule) { r.DatabaseName = strings.Repeat("d", 129) }, "database_name"},
		{"database_name semicolon", func(r *Rule) { r.DatabaseName = "db;DROP" }, "database_name"},
		{"database_name space", func(r *Rule) { r.DatabaseName = "my db" }, "database_name"},
		{"invalid mode", func(r *Rule) { r.Mode = "admin" }, "mode"},
		{"read rule with marker_table", func(r *Rule) { r.Mode = ModeRead; r.MarkerTable = "elitea_effects" }, "marker_table"},
		{"write rule with empty marker_table", func(r *Rule) { r.Mode = ModeWrite; r.MarkerTable = "" }, "marker_table"},
		{"write rule with quoted marker_table", func(r *Rule) { r.Mode = ModeWrite; r.MarkerTable = `"elitea_effects"` }, "marker_table"},
		{"write rule with SQL injection marker_table", func(r *Rule) { r.Mode = ModeWrite; r.MarkerTable = "t; DROP TABLE x" }, "marker_table"},
		{"write rule with three-part marker_table", func(r *Rule) { r.Mode = ModeWrite; r.MarkerTable = "a.b.c" }, "marker_table"},
		{"write rule with 64-byte identifier part", func(r *Rule) { r.Mode = ModeWrite; r.MarkerTable = strings.Repeat("a", 64) }, "marker_table"},
		{"write rule with 129-byte marker_table", func(r *Rule) {
			r.Mode = ModeWrite
			r.MarkerTable = strings.Repeat("a", 63) + "." + strings.Repeat("b", 65)
		}, "marker_table"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := validReadRule()
			tc.mutate(&rule)
			err := rule.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error does not wrap ErrInvalid: %v", err)
			}
			ruleErr, ok := IsRuleError(err)
			if !ok || ruleErr.Field != tc.field {
				t.Fatalf("RuleError = %+v (ok=%v), want field %q", ruleErr, ok, tc.field)
			}
			// Never echo submitted injection payloads.
			if strings.Contains(err.Error(), "DROP") {
				t.Fatalf("error echoed submitted input: %v", err)
			}
		})
	}

	// Exact boundary lengths (128-byte name, 128-byte database_name, 63.63 marker_table) pass.
	boundary := validWriteRule()
	boundary.Name = strings.Repeat("a", 128)
	boundary.DatabaseName = strings.Repeat("d", 128)
	boundary.MarkerTable = strings.Repeat("s", 63) + "." + strings.Repeat("t", 63)
	if err := boundary.Validate(); err != nil {
		t.Fatalf("exact boundary rule rejected: %v", err)
	}
}

func TestPolicyWireRoundTripAndTamperDetection(t *testing.T) {
	t.Parallel()

	for _, rule := range []Rule{validReadRule(), validWriteRule()} {
		wire := rule.PolicyWire()
		decoded, err := DecodePolicyWire(wire)
		if err != nil {
			t.Fatalf("DecodePolicyWire(%s): %v", rule.Name, err)
		}
		if decoded != rule {
			t.Fatalf("decoded = %+v, want %+v", decoded, rule)
		}
		if len(rule.PolicyDigest()) != 64 {
			t.Fatalf("digest length = %d, want 64", len(rule.PolicyDigest()))
		}
	}

	// Non-canonical whitespace or unknown fields fail closed.
	wire := validReadRule().PolicyWire()
	if _, err := DecodePolicyWire(append([]byte(" "), wire...)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-canonical wire err = %v, want ErrInvalid", err)
	}
	if _, err := DecodePolicyWire([]byte(`{"extra":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown field wire err = %v, want ErrInvalid", err)
	}
}
