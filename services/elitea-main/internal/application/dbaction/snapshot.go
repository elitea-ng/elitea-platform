package dbaction

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// SnapshotSchema is the start-time freeze of project DB action rules and their
// sealed `sql` configuration references, ready for claim-time materialization
// by CurrentConfigurationsMaterializer in 5e-3.
const SnapshotSchema = "elitea.runtime.db-action-snapshot.v1"

// Materialized SQL credential limits (aligned with Rust SQL/config.rs).
const (
	MaxSQLHostBytes     = 253
	MaxSQLUsernameBytes = 256
	MaxSQLPasswordBytes = 16 * 1024
)

// FrozenSnapshot holds the start-time DB action rule bindings for one pipeline
// execution. Each FrozenRule carries its sealed `sql` configuration map stamped
// with CurrentFrozenConfigurationMarker so the existing claim-time materializer
// unsecrets vault references only for the claimed Worker.
type FrozenSnapshot struct {
	SchemaVersion string       `json:"schema_version"`
	ProjectID     int64        `json:"project_id"`
	ApplicationID string       `json:"application_id"`
	VersionID     string       `json:"version_id"`
	Rules         []FrozenRule `json:"rules"`
}

// FrozenRule is one start-time frozen DB rule and its sealed SQL configuration.
type FrozenRule struct {
	Name             string         `json:"name"`
	RuleID           int64          `json:"rule_id"`
	Revision         int64          `json:"revision"`
	Dialect          string         `json:"dialect"`
	DatabaseName     string         `json:"database_name"`
	Mode             string         `json:"mode"`
	MarkerTable      string         `json:"marker_table,omitempty"`
	ConfigurationID  int32          `json:"configuration_id"`
	SQLConfiguration map[string]any `json:"sql_configuration"`
	PolicyDigest     string         `json:"policy_digest"`
	BindingDigest    string         `json:"binding_digest"`
}

// RuleLoader loads one enabled project DB rule and its sealed `sql`
// configuration after checking the executing actor's live RBAC on that
// configuration (implemented by repos.DBActionCredentials).
type RuleLoader interface {
	LoadFrozenRule(ctx context.Context, projectID, actorID int64, ruleName string) (FrozenRule, error)
}

// FreezeSnapshot resolves and freezes the distinct DB action rule names
// referenced by a pipeline at start time. Returning (nil, nil) when ruleNames
// is empty lets non-DB pipelines skip the snapshot cleanly.
func FreezeSnapshot(
	ctx context.Context,
	projectID, actorID, applicationID, versionID int64,
	ruleNames []string,
	loader RuleLoader,
) (*FrozenSnapshot, error) {
	if ctx == nil || projectID <= 0 || actorID <= 0 || applicationID <= 0 || versionID <= 0 {
		return nil, ErrInvalid
	}
	if len(ruleNames) == 0 {
		return nil, nil
	}
	if loader == nil {
		return nil, ErrUnavailable
	}
	unique := make([]string, 0, len(ruleNames))
	seen := make(map[string]bool, len(ruleNames))
	for _, name := range ruleNames {
		if !ValidRuleName(name) {
			return nil, ErrInvalid
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
		if len(unique) > MaxRulesPerProject {
			return nil, ErrInvalid
		}
	}
	sort.Strings(unique)

	snapshot := &FrozenSnapshot{
		SchemaVersion: SnapshotSchema,
		ProjectID:     projectID,
		ApplicationID: strconv.FormatInt(applicationID, 10),
		VersionID:     strconv.FormatInt(versionID, 10),
		Rules:         make([]FrozenRule, 0, len(unique)),
	}
	for _, name := range unique {
		loaded, err := loader.LoadFrozenRule(ctx, projectID, actorID, name)
		if err != nil {
			return nil, err
		}
		if loaded.Name != name || !validFrozenRulePolicy(projectID, loaded) {
			return nil, ErrPolicyDenied
		}
		loaded.BindingDigest = FrozenRuleBindingDigest(*snapshot, loaded)
		snapshot.Rules = append(snapshot.Rules, loaded)
	}
	return snapshot, nil
}

// FrozenRuleBindingDigest binds one frozen DB rule to the exact application
// version, project, rule revision, policy digest, and configuration id.
func FrozenRuleBindingDigest(snapshot FrozenSnapshot, rule FrozenRule) string {
	h := sha256.New()
	h.Write([]byte("elitea.runtime.db-frozen-rule.v1\x00"))
	parts := []string{
		snapshot.ApplicationID,
		snapshot.VersionID,
		strconv.FormatInt(snapshot.ProjectID, 10),
		rule.Name,
		strconv.FormatInt(rule.RuleID, 10),
		strconv.FormatInt(rule.Revision, 10),
		rule.PolicyDigest,
		strconv.FormatInt(int64(rule.ConfigurationID), 10),
	}
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks the snapshot's version binding, reconstructs and validates
// every rule's policy fields against PolicyDigest, and verifies BindingDigest.
func (s FrozenSnapshot) Verify(projectID, applicationID, versionID int64) error {
	if s.SchemaVersion != SnapshotSchema ||
		s.ProjectID != projectID ||
		s.ApplicationID != strconv.FormatInt(applicationID, 10) ||
		s.VersionID != strconv.FormatInt(versionID, 10) ||
		len(s.Rules) == 0 || len(s.Rules) > MaxRulesPerProject {
		return ErrPolicyDenied
	}
	seen := make(map[string]bool, len(s.Rules))
	for _, rule := range s.Rules {
		if seen[rule.Name] || !validFrozenRulePolicy(projectID, rule) ||
			!validHexDigest(rule.BindingDigest) ||
			rule.BindingDigest != FrozenRuleBindingDigest(s, rule) {
			return ErrPolicyDenied
		}
		seen[rule.Name] = true
	}
	return nil
}

// validFrozenRulePolicy reconstructs the authoritative Rule from every
// policy-bearing field of a FrozenRule, re-validates it, and proves
// rule.PolicyDigest matches the canonical PolicyDigest of those exact fields.
func validFrozenRulePolicy(projectID int64, rule FrozenRule) bool {
	if rule.RuleID <= 0 || projectID <= 0 || rule.Revision <= 0 || rule.ConfigurationID <= 0 ||
		rule.SQLConfiguration == nil || !validHexDigest(rule.PolicyDigest) {
		return false
	}
	candidate := Rule{
		ID:              rule.RuleID,
		ProjectID:       projectID,
		Revision:        rule.Revision,
		SchemaVersion:   RuleSchema,
		Name:            rule.Name,
		ConfigurationID: rule.ConfigurationID,
		Dialect:         rule.Dialect,
		DatabaseName:    rule.DatabaseName,
		Mode:            rule.Mode,
		MarkerTable:     rule.MarkerTable,
		Enabled:         true,
	}
	if candidate.Validate() != nil {
		return false
	}
	return candidate.PolicyDigest() == rule.PolicyDigest
}

// MaterializedSQLCredential is the claim-time unsecreted connection credential
// for one DB action rule. Password is redacted in formatting and JSON output.
type MaterializedSQLCredential struct {
	Dialect  string
	Host     string
	Port     uint16
	Username string
	password []byte
}

// Password returns a copy of the plaintext password bytes for the claimed Worker.
func (c MaterializedSQLCredential) Password() string {
	return string(c.password)
}

// Zero clears the in-memory password buffer.
func (c *MaterializedSQLCredential) Zero() {
	for i := range c.password {
		c.password[i] = 0
	}
	c.password = nil
}

// String redacts credentials so logs/errors never print passwords or hosts.
func (c MaterializedSQLCredential) String() string {
	return fmt.Sprintf("MaterializedSQLCredential{dialect=%s port=%d password=[REDACTED]}", c.Dialect, c.Port)
}

// GoString redacts credentials for %#v formatting.
func (c MaterializedSQLCredential) GoString() string { return c.String() }

// MarshalJSON refuses accidental JSON serialization of plaintext credentials.
func (c MaterializedSQLCredential) MarshalJSON() ([]byte, error) {
	return nil, ErrPolicyDenied
}

// ParseMaterializedSQLCredential validates a claim-time unsecreted `sql`
// configuration map using the same field and length bounds as Rust SQL/config.rs.
func ParseMaterializedSQLCredential(dialect string, data map[string]any) (MaterializedSQLCredential, error) {
	defaultPort, ok := DefaultPort(dialect)
	if !ok || data == nil {
		return MaterializedSQLCredential{}, ErrPolicyDenied
	}
	if rawDialect, exists := data["dialect"]; exists && rawDialect != nil {
		dialectText, ok := rawDialect.(string)
		if !ok || strings.TrimSpace(dialectText) != dialect {
			return MaterializedSQLCredential{}, ErrPolicyDenied
		}
	}
	host, err := parseSQLHost(data["host"])
	if err != nil {
		return MaterializedSQLCredential{}, ErrPolicyDenied
	}
	port, err := ParseSQLPort(data["port"], defaultPort)
	if err != nil {
		return MaterializedSQLCredential{}, ErrPolicyDenied
	}
	username, err := parseSQLRequiredText(data["username"], MaxSQLUsernameBytes)
	if err != nil {
		return MaterializedSQLCredential{}, ErrPolicyDenied
	}
	password, err := parseSQLPassword(data["password"])
	if err != nil {
		return MaterializedSQLCredential{}, ErrPolicyDenied
	}
	return MaterializedSQLCredential{
		Dialect:  dialect,
		Host:     host,
		Port:     port,
		Username: username,
		password: []byte(password),
	}, nil
}

// ParseSQLTarget extracts and validates the host and port from a sealed `sql`
// configuration map without touching or requiring plaintext password secrets.
func ParseSQLTarget(dialect string, data map[string]any) (string, uint16, error) {
	defaultPort, ok := DefaultPort(dialect)
	if !ok || data == nil {
		return "", 0, ErrInvalid
	}
	if rawDialect, exists := data["dialect"]; exists && rawDialect != nil {
		dialectText, ok := rawDialect.(string)
		if !ok || strings.TrimSpace(dialectText) != dialect {
			return "", 0, ErrInvalid
		}
	}
	host, err := parseSQLHost(data["host"])
	if err != nil {
		return "", 0, ErrInvalid
	}
	port, err := ParseSQLPort(data["port"], defaultPort)
	if err != nil {
		return "", 0, ErrInvalid
	}
	if _, err := parseSQLRequiredText(data["username"], MaxSQLUsernameBytes); err != nil {
		return "", 0, ErrInvalid
	}
	return host, port, nil
}

// ParseSQLPort parses an optional SQL configuration port, falling back to
// defaultPort when nil/omitted. Rejects 0, negatives, floats, and > 65535.
func ParseSQLPort(raw any, defaultPort uint16) (uint16, error) {
	if raw == nil {
		return defaultPort, nil
	}
	var parsed int64
	switch v := raw.(type) {
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, ErrInvalid
		}
		parsed = n
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < 1 || v > 65535 {
			return 0, ErrInvalid
		}
		parsed = int64(v)
	case int:
		parsed = int64(v)
	case int32:
		parsed = int64(v)
	case int64:
		parsed = v
	default:
		return 0, ErrInvalid
	}
	if parsed < 1 || parsed > 65535 {
		return 0, ErrInvalid
	}
	return uint16(parsed), nil
}

func parseSQLHost(raw any) (string, error) {
	host, err := parseSQLRequiredText(raw, MaxSQLHostBytes)
	if err != nil {
		return "", err
	}
	if host != strings.TrimSpace(host) {
		return "", ErrInvalid
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return "", ErrInvalid
		}
		return host, nil
	}
	if strings.HasSuffix(host, ".") {
		return "", ErrInvalid
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalid
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
				return "", ErrInvalid
			}
		}
	}
	return host, nil
}

func parseSQLRequiredText(raw any, maxBytes int) (string, error) {
	value, ok := raw.(string)
	if !ok || len(value) > maxBytes || strings.TrimSpace(value) == "" || hasControlChar(value) || strings.Contains(value, "{{secret.") {
		return "", ErrInvalid
	}
	return value, nil
}

func parseSQLPassword(raw any) (string, error) {
	value, ok := raw.(string)
	if !ok || len(value) == 0 || len(value) > MaxSQLPasswordBytes || strings.ContainsRune(value, '\x00') || strings.Contains(value, "{{secret.") {
		return "", ErrInvalid
	}
	return value, nil
}

func hasControlChar(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
