package dbaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
)

// RuleSchema is the admin-authored DB action rule body.
const RuleSchema = "elitea.runtime.db-action-rule.v1"

// Rule limits. Checked at rule write and again when a rule is read for
// admission, so a row written by an older binary cannot widen them.
const (
	// MaxRulesPerProject bounds one project's DB action rule table.
	MaxRulesPerProject   = 256
	maxRuleNameBytes     = 128
	maxDatabaseNameBytes = 128
	maxMarkerTableBytes  = 128
	maxPolicyWireBytes   = 16 * 1024
)

// Supported SQL dialects (T-5E §6).
const (
	DialectPostgres = "postgres"
	DialectMySQL    = "mysql"
)

// Supported DB action modes (T-5E §3.1, §3.2).
const (
	ModeRead  = "read"
	ModeWrite = "write"
)

// DefaultMarkerTable is the default target-database effect marker table used
// when an admin creates a write rule without overriding marker_table (T-5E §3.2).
const DefaultMarkerTable = "elitea_effects"

// Sentinel errors for callers branching on DB action rule failures.
var (
	ErrInvalid      = errors.New("invalid DB action rule")
	ErrPolicyDenied = errors.New("DB action denied by policy")
	ErrUnavailable  = errors.New("DB action service unavailable")
)

// DefaultPort returns the standard TCP port for a supported SQL dialect.
func DefaultPort(dialect string) (uint16, bool) {
	switch dialect {
	case DialectPostgres:
		return 5432, true
	case DialectMySQL:
		return 3306, true
	default:
		return 0, false
	}
}

// Rule is one project-scoped DB action rule. Connection host, port, username
// and password never live here — only the referenced `sql` configuration_id.
// Pipeline YAML names a rule solely by Name.
type Rule struct {
	ID              int64  `json:"id,omitempty"`
	ProjectID       int64  `json:"-"`
	Revision        int64  `json:"revision,omitempty"`
	SchemaVersion   string `json:"schema_version"`
	Name            string `json:"name"`
	ConfigurationID int32  `json:"configuration_id"`
	Dialect         string `json:"dialect"`
	DatabaseName    string `json:"database_name"`
	Mode            string `json:"mode"`
	MarkerTable     string `json:"marker_table,omitempty"`
	Enabled         bool   `json:"enabled"`
	// UpdatedBy records the last user who wrote this rule (DB column
	// updated_by). It is never serialized on the wire; credential resolution
	// uses it to ensure a private `sql` configuration belongs to both the
	// executing actor and the rule's last writer.
	UpdatedBy int64 `json:"-"`
}

// RuleError names the invalid field. It never echoes the submitted value.
type RuleError struct {
	Field  string
	Reason string
}

func (e *RuleError) Error() string { return "DB action rule " + e.Field + " " + e.Reason }
func (e *RuleError) Unwrap() error { return ErrInvalid }

// IsRuleError reports whether err is a field-level rule refusal.
func IsRuleError(err error) (*RuleError, bool) {
	var target *RuleError
	ok := errors.As(err, &target)
	return target, ok
}

var (
	ruleNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	databaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,127}$`)
	markerTablePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}(\.[A-Za-z_][A-Za-z0-9_]{0,62})?$`)
)

// ValidRuleName reports whether name matches the DB action rule identifier grammar.
func ValidRuleName(name string) bool {
	return len(name) > 0 && len(name) <= maxRuleNameBytes && ruleNamePattern.MatchString(name)
}

// Normalize fills defaults a client may omit (schema_version, read mode, and
// write marker_table default) without altering any explicit authored value.
func (r *Rule) Normalize() {
	if r.SchemaVersion == "" {
		r.SchemaVersion = RuleSchema
	}
	if r.Mode == "" {
		r.Mode = ModeRead
	}
	if r.Mode == ModeWrite && r.MarkerTable == "" {
		r.MarkerTable = DefaultMarkerTable
	}
}

// Validate checks every field and cross-field invariant of a DB action rule.
// Configuration resolution and egress allowlist checks run separately.
func (r Rule) Validate() error {
	if r.SchemaVersion != RuleSchema {
		return &RuleError{"schema_version", "is not " + RuleSchema}
	}
	if !ValidRuleName(r.Name) {
		return &RuleError{"name", "must be dot-separated lowercase identifiers, at most 128 characters"}
	}
	if r.ConfigurationID <= 0 {
		return &RuleError{"configuration_id", "must reference a project SQL configuration"}
	}
	switch r.Dialect {
	case DialectPostgres, DialectMySQL:
	default:
		return &RuleError{"dialect", "must be postgres or mysql"}
	}
	if len(r.DatabaseName) == 0 || len(r.DatabaseName) > maxDatabaseNameBytes || !databaseNamePattern.MatchString(r.DatabaseName) {
		return &RuleError{"database_name", "must be 1-128 letters, digits, _, - or $ starting with a letter, digit or _"}
	}
	switch r.Mode {
	case ModeRead:
		if r.MarkerTable != "" {
			return &RuleError{"marker_table", "must be empty when mode is read"}
		}
	case ModeWrite:
		if r.MarkerTable == "" {
			return &RuleError{"marker_table", "is required when mode is write"}
		}
		if len(r.MarkerTable) > maxMarkerTableBytes || !markerTablePattern.MatchString(r.MarkerTable) {
			return &RuleError{"marker_table", "must be a valid SQL table identifier (table or schema.table)"}
		}
	default:
		return &RuleError{"mode", "must be read or write"}
	}
	return nil
}

// PolicyWire is the canonical JSON representation of one rule as stored
// (identity, revision and every field).
func (r Rule) PolicyWire() []byte {
	fields := map[string]any{
		"configuration_id": json.Number(strconv.FormatInt(int64(r.ConfigurationID), 10)),
		"database_name":    r.DatabaseName,
		"dialect":          r.Dialect,
		"enabled":          r.Enabled,
		"id":               json.Number(strconv.FormatInt(r.ID, 10)),
		"marker_table":     r.MarkerTable,
		"mode":             r.Mode,
		"name":             r.Name,
		"project_id":       json.Number(strconv.FormatInt(r.ProjectID, 10)),
		"revision":         json.Number(strconv.FormatInt(r.Revision, 10)),
		"schema_version":   RuleSchema,
	}
	return canonicalObject(fields)
}

// PolicyDigest is the domain-separated SHA-256 of PolicyWire.
func (r Rule) PolicyDigest() string {
	h := sha256.New()
	h.Write([]byte("elitea.db.rule.v1\x00"))
	h.Write(r.PolicyWire())
	return hex.EncodeToString(h.Sum(nil))
}

// DecodePolicyWire parses and re-validates a canonical PolicyWire byte slice.
func DecodePolicyWire(wire []byte) (Rule, error) {
	if len(wire) == 0 || len(wire) > maxPolicyWireBytes {
		return Rule{}, ErrInvalid
	}
	var stored struct {
		Rule
		ID        int64 `json:"id"`
		ProjectID int64 `json:"project_id"`
		Revision  int64 `json:"revision"`
	}
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&stored) != nil {
		return Rule{}, ErrInvalid
	}
	rule := stored.Rule
	rule.ID, rule.ProjectID, rule.Revision = stored.ID, stored.ProjectID, stored.Revision
	if rule.ID <= 0 || rule.ProjectID <= 0 || rule.Revision <= 0 || rule.Validate() != nil || !bytes.Equal(rule.PolicyWire(), wire) {
		return Rule{}, ErrInvalid
	}
	return rule, nil
}

func canonicalObject(fields map[string]any) []byte {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		out.Write(encodedKey)
		out.WriteByte(':')
		switch value := fields[key].(type) {
		case json.Number:
			out.WriteString(string(value))
		default:
			encodedVal, _ := json.Marshal(value)
			out.Write(encodedVal)
		}
	}
	out.WriteByte('}')
	return out.Bytes()
}
