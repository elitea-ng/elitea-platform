package httpaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// RuleSchema is the admin-authored rule body (libs/jsonschema/runtime/v1/http-action-rule.schema.json).
const RuleSchema = "elitea.runtime.http-action-rule.v1"

// Rule limits. Each one is checked at rule write and again when a rule is read
// for an action, so a row written by an older binary cannot widen them.
const (
	// MaxRulesPerProject bounds one project's rule table (rules repo, create).
	MaxRulesPerProject = 256
	// MaxQueryNames bounds the query parameter names one rule allows.
	MaxQueryNames = 32
	// MaxPathPlaceholders bounds `{name}` segments in one path template; it is
	// the node schema's bindingMap maxProperties.
	MaxPathPlaceholders = 32
	maxRuleNameBytes    = 128
	maxPathTemplate     = 2048
)

// Replay is the rule's attested retry behaviour for non-safe methods (D2).
const (
	// ReplayNever: an unknown outcome of POST/PUT/PATCH/DELETE is reconciled by
	// an operator (class C), never repeated.
	ReplayNever = "never"
	// ReplayIdempotencyKey: the admin attests the remote honours
	// Idempotency-Key; with a bound key a retry re-sends the same key (class I).
	ReplayIdempotencyKey = "idempotency_key"
)

// Rule is one project-scoped HTTP action rule. Origin, path, method and
// credential live only here; pipeline YAML names the rule by Name.
type Rule struct {
	ID              int64    `json:"id,omitempty"`
	ProjectID       int64    `json:"-"`
	Revision        int64    `json:"revision,omitempty"`
	SchemaVersion   string   `json:"schema_version"`
	Name            string   `json:"name"`
	Origin          string   `json:"origin"`
	PathTemplate    string   `json:"path_template"`
	Method          string   `json:"method"`
	QueryNames      []string `json:"query_names"`
	ConfigurationID int32    `json:"configuration_id"`
	Toolkit         string   `json:"toolkit"`
	Tool            string   `json:"tool"`
	OutputBucket    string   `json:"output_bucket,omitempty"`
	Replay          string   `json:"replay,omitempty"`
	Enabled         bool     `json:"enabled"`
}

// RuleError names the invalid field. It never echoes the submitted value.
type RuleError struct {
	Field  string
	Reason string
}

func (e *RuleError) Error() string { return "HTTP action rule " + e.Field + " " + e.Reason }
func (e *RuleError) Unwrap() error { return ErrInvalid }

// IsRuleError reports whether err is a field-level rule refusal.
func IsRuleError(err error) (*RuleError, bool) {
	var target *RuleError
	ok := errors.As(err, &target)
	return target, ok
}

var (
	ruleNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	ruleOriginPattern   = regexp.MustCompile(`^https://[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	literalSegment      = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	placeholderSegment  = regexp.MustCompile(`^\{([a-z][a-z0-9_]{0,63})\}$`)
	queryNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_.~-]{1,64}$`)
	ruleToolPattern     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	outputBucketPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// Normalize fills the defaults a client may omit (schema version, replay,
// empty query names) without changing any authored value.
func (r *Rule) Normalize() {
	if r.SchemaVersion == "" {
		r.SchemaVersion = RuleSchema
	}
	if r.Replay == "" {
		r.Replay = ReplayNever
	}
	if r.QueryNames == nil {
		r.QueryNames = []string{}
	}
}

// Validate checks the rule shape. Egress (where the origin resolves) is
// checked separately by the egress guard at write and at dial time.
func (r Rule) Validate() error {
	if r.SchemaVersion != RuleSchema {
		return &RuleError{"schema_version", "is not " + RuleSchema}
	}
	if len(r.Name) == 0 || len(r.Name) > maxRuleNameBytes || !ruleNamePattern.MatchString(r.Name) {
		return &RuleError{"name", "must be dot-separated lowercase identifiers, at most 128 characters"}
	}
	if len(r.Origin) > 262 || !ruleOriginPattern.MatchString(r.Origin) {
		return &RuleError{"origin", "must be https://host or https://host:port with no path"}
	}
	if _, port, found := strings.Cut(strings.TrimPrefix(r.Origin, "https://"), ":"); found {
		if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
			return &RuleError{"origin", "has a port outside 1-65535"}
		}
	}
	if _, err := r.Placeholders(); err != nil {
		return err
	}
	switch r.Method {
	case "GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE":
	default:
		return &RuleError{"method", "must be GET, HEAD, OPTIONS, POST, PUT, PATCH or DELETE"}
	}
	if len(r.QueryNames) > MaxQueryNames {
		return &RuleError{"query_names", "allows at most 32 names"}
	}
	seen := make(map[string]bool, len(r.QueryNames))
	for _, name := range r.QueryNames {
		if !queryNamePattern.MatchString(name) || seen[name] {
			return &RuleError{"query_names", "must be unique names of letters, digits and _.~-"}
		}
		if secretQueryName(name) {
			return &RuleError{"query_names", "must not carry credentials; use a configuration"}
		}
		seen[name] = true
	}
	if r.ConfigurationID < 0 {
		return &RuleError{"configuration_id", "must be 0 (none) or a configuration id"}
	}
	if !ruleToolPattern.MatchString(r.Toolkit) {
		return &RuleError{"toolkit", "must be 1-128 letters, digits and _.:-"}
	}
	if !ruleToolPattern.MatchString(r.Tool) {
		return &RuleError{"tool", "must be 1-128 letters, digits and _.:-"}
	}
	if r.OutputBucket != "" && !outputBucketPattern.MatchString(r.OutputBucket) {
		return &RuleError{"output_bucket", "must be 1-128 letters, digits and ._-"}
	}
	switch r.Replay {
	case ReplayNever, ReplayIdempotencyKey:
	default:
		return &RuleError{"replay", "must be never or idempotency_key"}
	}
	return nil
}

// Placeholders returns the `{name}` segments of the path template in order.
// A placeholder fills a whole segment; literal segments use A-Za-z0-9._~-;
// `.` and `..` are refused.
func (r Rule) Placeholders() ([]string, error) {
	invalid := &RuleError{"path_template", "must be /-separated literal or whole-segment {name} parts, without . or .."}
	if len(r.PathTemplate) == 0 || len(r.PathTemplate) > maxPathTemplate || r.PathTemplate[0] != '/' {
		return nil, invalid
	}
	if r.PathTemplate == "/" {
		return []string{}, nil
	}
	names := []string{}
	seen := map[string]bool{}
	for _, segment := range strings.Split(r.PathTemplate[1:], "/") {
		if segment == "." || segment == ".." {
			return nil, invalid
		}
		if match := placeholderSegment.FindStringSubmatch(segment); match != nil {
			if seen[match[1]] {
				return nil, &RuleError{"path_template", "repeats a placeholder"}
			}
			seen[match[1]] = true
			names = append(names, match[1])
			continue
		}
		if !literalSegment.MatchString(segment) {
			return nil, invalid
		}
	}
	if len(names) > MaxPathPlaceholders {
		return nil, &RuleError{"path_template", "has more than 32 placeholders"}
	}
	return names, nil
}

// SafeMethod reports the methods whose unknown outcome may be retried (class I).
func SafeMethod(method string) bool {
	return method == "GET" || method == "HEAD" || method == "OPTIONS"
}

// Retryable reports whether a new attempt of an action under this rule may
// follow an earlier attempt whose outcome was not completed (D2): safe
// methods always; other methods only when the rule attests idempotency-key
// replay and the request carries a key. Everything else is class C.
func (r Rule) Retryable(request Request) bool {
	return SafeMethod(r.Method) || r.Replay == ReplayIdempotencyKey && request.IdempotencyKey != ""
}

// PolicyWire is the canonical rule exactly as read for one admission
// (identity, revision and every field). The effect row stores it so recovery
// re-renders the request with the rule that produced it.
func (r Rule) PolicyWire() []byte {
	return canonicalValue(map[string]any{
		"schema_version": RuleSchema, "id": ruleNumber(r.ID), "project_id": ruleNumber(r.ProjectID), "revision": ruleNumber(r.Revision),
		"name": r.Name, "origin": r.Origin, "path_template": r.PathTemplate, "method": r.Method,
		"query_names": stringsAny(r.QueryNames), "configuration_id": ruleNumber(int64(r.ConfigurationID)),
		"toolkit": r.Toolkit, "tool": r.Tool, "output_bucket": r.OutputBucket, "replay": r.Replay, "enabled": r.Enabled,
	})
}

// PolicyDigest is the SHA-256 of PolicyWire.
func (r Rule) PolicyDigest() string { return Digest(r.PolicyWire()) }

// DecodePolicyWire reads a stored PolicyWire back and re-validates it.
func DecodePolicyWire(wire []byte) (Rule, error) {
	if len(wire) == 0 || len(wire) > 16*1024 {
		return Rule{}, ErrInvalid
	}
	canonical, err := CanonicalJSON(wire)
	if err != nil || !bytes.Equal(canonical, wire) {
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

func secretQueryName(name string) bool {
	switch strings.ToLower(name) {
	case "access_token", "token", "api_key", "apikey", "password", "client_secret", "authorization":
		return true
	}
	return false
}

func stringsAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
