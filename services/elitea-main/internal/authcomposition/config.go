// Package authcomposition owns the production authentication graph.
package authcomposition

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	FormSchemaVersion = "elitea.auth.form.v1"
	MaxConfigBytes    = int64(1 << 20)

	maxConfigDepth              = 24
	maxConfigNodes              = 4096
	maxScalarBytes              = 64 << 10
	maxCollectionEntries        = 512
	maxPathBytes                = 4096
	maxPublicOriginBytes        = 2048
	maxRedisURLBytes            = 2048
	maxRedisPrefixBytes         = 128
	maxTrustedProxyCIDRs        = 64
	maxInitialAdmins            = 256
	maxProjectRoles             = 64
	maxAllowedDomainsBytes      = 4096
	maxAllowedDomains           = 64
	maxRoleBytes                = 128
	minCookieLifetimeSeconds    = int64(time.Minute / time.Second)
	maxCookieLifetimeSeconds    = int64((30 * 24 * time.Hour) / time.Second)
	formProviderKind            = "form"
	singleEndpointRedisTopology = "single_primary_endpoint"
)

var ErrInvalidConfiguration = errors.New("invalid authentication composition configuration")

// Config is one complete startup snapshot. It contains references to private
// files, never secret values. This first schema intentionally supports only
// the Form provider; OIDC and SAML receive their own versioned schemas when
// their complete production contracts are ready.
type Config struct {
	SchemaVersion     string              `yaml:"schema_version"`
	PublicOrigin      string              `yaml:"public_origin"`
	TrustedProxyCIDRs []string            `yaml:"trusted_proxy_cidrs"`
	Redirects         RedirectConfig      `yaml:"redirects"`
	Cookie            CookieConfig        `yaml:"cookie"`
	Redis             RedisConfig         `yaml:"redis"`
	Credentials       CredentialConfig    `yaml:"credentials"`
	Mappers           MapperConfig        `yaml:"mappers"`
	Authorization     AuthorizationConfig `yaml:"authorization"`
	Identity          IdentityConfig      `yaml:"identity"`
	Provider          ProviderConfig      `yaml:"provider"`
}

type RedirectConfig struct {
	// DirectAccessDenied and MainAccessDenied stay separate because current
	// Auth and Main redirect to different same-origin UI routes.
	DirectAccessDenied string `yaml:"direct_access_denied"`
	MainAccessDenied   string `yaml:"main_access_denied"`
	DefaultLogin       string `yaml:"default_login"`
	DefaultLogout      string `yaml:"default_logout"`
}

// CookieConfig describes only the browser-auth cookie. The current Main
// selected-project/UI session has a separate cookie and store and must not be
// collapsed into this contract during migration. Secure, HttpOnly, Path=/ and
// host-only Domain are derived by composition from PublicOrigin.
type CookieConfig struct {
	Name            string `yaml:"name"`
	SameSite        string `yaml:"same_site"`
	LifetimeSeconds int64  `yaml:"lifetime_seconds"`
}

// RedisConfig v1 intentionally models one TLS primary endpoint, including a
// managed highly-available endpoint. Sentinel and Cluster/Ring clients require
// separate discriminated schemas and remain production-mount gates.
type RedisConfig struct {
	Topology       string `yaml:"topology"`
	URL            string `yaml:"url"`
	PasswordFile   string `yaml:"password_file"`
	CAFile         string `yaml:"ca_file"`
	KeyPrefix      string `yaml:"key_prefix"`
	AttemptKeyFile string `yaml:"attempt_key_file"`
}

type CredentialConfig struct {
	PATSigningKeyFile string `yaml:"pat_signing_key_file"`
	// Headers are only current other_auth_headers aliases. Authorization
	// Bearer and Basic are mandatory built-ins assembled independently.
	Headers []CredentialHeaderConfig `yaml:"credential_headers"`
}

type CredentialHeaderConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

// MapperConfig selects one immutable, source-evidenced projection contract.
// It is intentionally not a second JSONPath/configuration language: fleet
// overrides must be inventoried and receive a typed design before another
// contract version can be accepted.
type MapperConfig struct {
	Contract string `yaml:"contract"`
}

// AuthorizationConfig contains only the static rules currently configured on
// Main. Route-owning modules contribute separate typed declarations when the
// production graph is assembled. Direct Auth has an intentionally empty public
// policy; Main traversal-after-rejection is compiled current-baseline behavior,
// not an operator-controlled switch.
type AuthorizationConfig struct {
	MainConfiguredPublicRules []PublicRuleConfig `yaml:"main_configured_public_rules"`
}

type PublicRuleConfig struct {
	Name       string                      `yaml:"name"`
	Conditions []PublicRuleConditionConfig `yaml:"conditions"`
}

type PublicRuleConditionConfig struct {
	Field   string `yaml:"field"`
	Pattern string `yaml:"pattern"`
}

type IdentityConfig struct {
	// The production graph injects Main's existing PostgreSQL pool. This
	// contract deliberately cannot introduce a second Auth DSN or schema.
	InitialGlobalAdmins []string                 `yaml:"initial_global_admins"`
	ProjectEnrollment   *ProjectEnrollmentConfig `yaml:"project_enrollment"`
}

type ProjectEnrollmentConfig struct {
	ProjectID                             int32    `yaml:"project_id"`
	AllowedDomains                        string   `yaml:"allowed_domains"`
	AdditionalProjectRolesForGlobalAdmins []string `yaml:"additional_project_roles_for_global_admins"`
}

type ProviderConfig struct {
	Kind string              `yaml:"kind"`
	Form *FormProviderConfig `yaml:"form"`
}

type FormProviderConfig struct {
	// UsersJSONFile is canonical strict JSON exported from the current merged
	// Form YAML after its existing environment/vault resolution step.
	UsersJSONFile string `yaml:"users_json_file"`
}

// Load reads a bounded, canonical, non-symlink configuration file once. It
// does not read the referenced secrets; materialization is a separate startup
// step so parsing never turns YAML into an implicit secret transport.
func Load(path string) (Config, error) {
	raw, err := securefile.Read(path, MaxConfigBytes, securefile.PublicMaterial)
	if err != nil {
		return Config{}, fmt.Errorf("%w: read configuration", ErrInvalidConfiguration)
	}
	return Parse(raw)
}

// Parse decodes exactly one strict YAML document. JSON is accepted only as the
// YAML-compatible subset of the same typed contract.
func Parse(raw []byte) (Config, error) {
	if len(raw) == 0 || int64(len(raw)) > MaxConfigBytes || !utf8.Valid(raw) {
		return Config{}, invalid("configuration bytes")
	}

	var document yaml.Node
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := nodeDecoder.Decode(&document); err != nil {
		return Config{}, invalid("YAML document")
	}
	var extra yaml.Node
	if err := nodeDecoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, invalid("multiple YAML documents")
	}
	if err := validateDocument(&document); err != nil {
		return Config{}, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, invalid("typed YAML fields")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, invalid("trailing YAML document")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate also protects direct construction in package tests and future
// composition code. It never resolves files, environment variables, vault
// expressions, includes, or layered configuration.
func (config Config) Validate() error {
	if config.SchemaVersion != FormSchemaVersion {
		return invalid("schema version")
	}
	if !validPublicOrigin(config.PublicOrigin) {
		return invalid("public origin")
	}
	if !validTrustedProxyCIDRs(config.TrustedProxyCIDRs) {
		return invalid("trusted proxy CIDRs")
	}
	if !validRedirect(config.Redirects.DirectAccessDenied) ||
		!validRedirect(config.Redirects.MainAccessDenied) ||
		!validRedirect(config.Redirects.DefaultLogin) ||
		!validRedirect(config.Redirects.DefaultLogout) {
		return invalid("redirect target")
	}
	if !validCookie(config.Cookie) {
		return invalid("cookie policy")
	}
	if !validRedis(config.Redis) {
		return invalid("Redis policy")
	}
	if !validCredentials(config.Credentials) {
		return invalid("credential policy")
	}
	if _, err := browserapi.NewSuccessMapper(config.Mappers.Contract); err != nil {
		return invalid("mapper policy")
	}
	if !validAuthorization(config.Authorization) {
		return invalid("authorization policy")
	}
	if !validIdentity(config.Identity) {
		return invalid("identity policy")
	}
	if config.Provider.Kind != formProviderKind || config.Provider.Form == nil ||
		!validFilePath(config.Provider.Form.UsersJSONFile) {
		return invalid("Form provider")
	}
	materialPaths := []string{
		config.Redis.PasswordFile,
		config.Redis.CAFile,
		config.Redis.AttemptKeyFile,
		config.Credentials.PATSigningKeyFile,
		config.Provider.Form.UsersJSONFile,
	}
	seen := make(map[string]struct{}, len(materialPaths))
	for _, path := range materialPaths {
		if _, duplicate := seen[path]; duplicate {
			return invalid("material reference path separation")
		}
		seen[path] = struct{}{}
	}
	return nil
}

// MainConfiguredPublicRules returns a detached, typed copy of the configured
// Main rules. Production composition must append audited route-owned rules
// before compiling the effective Main policy.
func (config Config) MainConfiguredPublicRules() ([]forwardapp.PublicRule, error) {
	rules, ok := configuredPublicRules(config.Authorization.MainConfiguredPublicRules)
	if !ok {
		return nil, invalid("authorization policy")
	}
	if _, err := forwardapp.NewPublicPolicy(rules); err != nil {
		return nil, invalid("authorization policy")
	}
	return rules, nil
}

func validateDocument(document *yaml.Node) error {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return invalid("YAML root")
	}
	nodes := 0
	return validateNode(document.Content[0], 1, &nodes)
}

func validateNode(node *yaml.Node, depth int, nodes *int) error {
	if node == nil || depth > maxConfigDepth {
		return invalid("YAML nesting")
	}
	(*nodes)++
	if *nodes > maxConfigNodes || node.Anchor != "" || node.Kind == yaml.AliasNode {
		return invalid("YAML node or alias")
	}

	switch node.Kind {
	case yaml.MappingNode:
		if node.Tag != "!!map" || len(node.Content)%2 != 0 || len(node.Content)/2 > maxCollectionEntries {
			return invalid("YAML mapping")
		}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" ||
				key.Value == "<<" || len(key.Value) > maxScalarBytes || key.Anchor != "" {
				return invalid("YAML mapping key")
			}
			(*nodes)++
			if *nodes > maxConfigNodes {
				return invalid("YAML node count")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return invalid("duplicate YAML field")
			}
			seen[key.Value] = struct{}{}
			if err := validateNode(node.Content[index+1], depth+1, nodes); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" || len(node.Content) > maxCollectionEntries {
			return invalid("YAML sequence")
		}
		for _, child := range node.Content {
			if err := validateNode(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if len(node.Value) > maxScalarBytes || strings.Contains(node.Value, "${") {
			return invalid("YAML scalar")
		}
		switch node.Tag {
		case "!!str":
		case "!!int":
			if !canonicalUnsignedInteger(node.Value) {
				return invalid("canonical YAML integer")
			}
		default:
			return invalid("YAML scalar tag")
		}
	default:
		return invalid("YAML node kind")
	}
	return nil
}

func canonicalUnsignedInteger(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func validPublicOrigin(raw string) bool {
	if raw == "" || len(raw) > maxPublicOriginBytes || !canonicalASCII(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.RawPath != "" || parsed.ForceQuery || parsed.Path != "" && parsed.Path != "/" ||
		!httpguts.ValidHostHeader(parsed.Host) || !canonicalHostPort(parsed) {
		return false
	}
	return true
}

func canonicalHostPort(parsed *url.URL) bool {
	hostname := parsed.Hostname()
	if hostname == "" || strings.HasSuffix(parsed.Host, ":") || !validEndpointHostname(hostname) {
		return false
	}
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil || port <= 0 || port > 65535 || strconv.Itoa(port) != portText {
			return false
		}
	}
	return true
}

func validEndpointHostname(hostname string) bool {
	if hostname == "" || len(hostname) > 253 || !canonicalASCII(hostname) {
		return false
	}
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Zone() == ""
	}
	if strings.HasPrefix(hostname, ".") || strings.HasSuffix(hostname, ".") {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || !asciiAlphaNumeric(label[0]) ||
			!asciiAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for index := 1; index < len(label)-1; index++ {
			if !asciiAlphaNumeric(label[index]) && label[index] != '-' {
				return false
			}
		}
	}
	return true
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func validTrustedProxyCIDRs(values []string) bool {
	if len(values) == 0 || len(values) > maxTrustedProxyCIDRs {
		return false
	}
	seen := make(map[netip.Prefix]struct{}, len(values))
	for _, raw := range values {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Zone() != "" || prefix.Masked() != prefix || prefix.String() != raw {
			return false
		}
		if _, duplicate := seen[prefix]; duplicate {
			return false
		}
		seen[prefix] = struct{}{}
	}
	return true
}

func validRedirect(value string) bool {
	return value != "" && strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") &&
		browserflow.ValidateReturnTarget(value) == nil
}

func validCookie(config CookieConfig) bool {
	if config.LifetimeSeconds < minCookieLifetimeSeconds ||
		config.LifetimeSeconds > maxCookieLifetimeSeconds ||
		config.SameSite != "lax" && config.SameSite != "strict" {
		return false
	}
	probe := &http.Cookie{Name: config.Name, Value: "v1.probe", Path: "/", Secure: true, HttpOnly: true}
	return probe.Valid() == nil
}

func validRedis(config RedisConfig) bool {
	if config.Topology != singleEndpointRedisTopology || !validRedisURL(config.URL) ||
		!validFilePath(config.PasswordFile) || !validFilePath(config.CAFile) ||
		!validFilePath(config.AttemptKeyFile) || !validRedisKeyPrefix(config.KeyPrefix) {
		return false
	}
	return true
}

func validRedisURL(raw string) bool {
	if raw == "" || len(raw) > maxRedisURLBytes || !canonicalASCII(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "rediss" || parsed.Host == "" || parsed.User == nil ||
		parsed.User.Username() == "" || parsed.Path != "/0" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery || parsed.Opaque != "" ||
		!httpguts.ValidHostHeader(parsed.Host) || !canonicalHostPort(parsed) {
		return false
	}
	if _, passwordPresent := parsed.User.Password(); passwordPresent {
		return false
	}
	username := parsed.User.Username()
	return validACLUsername(username) && parsed.User.String() == username
}

func validACLUsername(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validRedisKeyPrefix(value string) bool {
	if value == "" || len(value) > maxRedisPrefixBytes || !strings.HasSuffix(value, ":") {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == ':' || character == '.' ||
			character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validCredentials(config CredentialConfig) bool {
	if !validFilePath(config.PATSigningKeyFile) || config.Headers == nil ||
		len(config.Headers) >= forwardapp.MaxCredentials {
		return false
	}
	seen := make(map[string]struct{}, len(config.Headers))
	for _, header := range config.Headers {
		name := http.CanonicalHeaderKey(header.Name)
		if !httpguts.ValidHeaderFieldName(header.Name) || strings.EqualFold(name, "Authorization") ||
			header.Type != "bearer" && header.Type != "basic" {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func validAuthorization(config AuthorizationConfig) bool {
	if config.MainConfiguredPublicRules == nil {
		return false
	}
	rules, ok := configuredPublicRules(config.MainConfiguredPublicRules)
	if !ok {
		return false
	}
	_, err := forwardapp.NewPublicPolicy(rules)
	return err == nil
}

func configuredPublicRules(configs []PublicRuleConfig) ([]forwardapp.PublicRule, bool) {
	if len(configs) > forwardapp.MaxPublicRules {
		return nil, false
	}
	rules := make([]forwardapp.PublicRule, 0, len(configs))
	names := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		if !validTrimmedText(config.Name, forwardapp.MaxRuleNameBytes) || config.Conditions == nil ||
			len(config.Conditions) == 0 || len(config.Conditions) > forwardapp.MaxConditionsPerRule {
			return nil, false
		}
		if _, duplicate := names[config.Name]; duplicate {
			return nil, false
		}
		names[config.Name] = struct{}{}

		conditions := make([]forwardapp.RuleCondition, 0, len(config.Conditions))
		for _, configCondition := range config.Conditions {
			field, ok := publicSourceField(configCondition.Field)
			if !ok || len(configCondition.Pattern) > forwardapp.MaxPatternBytes ||
				!utf8.ValidString(configCondition.Pattern) ||
				strings.ContainsFunc(configCondition.Pattern, unicode.IsControl) {
				return nil, false
			}
			conditions = append(conditions, forwardapp.RuleCondition{
				Field:   field,
				Pattern: configCondition.Pattern,
			})
		}
		rules = append(rules, forwardapp.PublicRule{Name: config.Name, Conditions: conditions})
	}
	return rules, true
}

func publicSourceField(value string) (forwardapp.SourceField, bool) {
	switch value {
	case "method":
		return forwardapp.SourceMethod, true
	case "proto":
		return forwardapp.SourceProto, true
	case "host":
		return forwardapp.SourceHost, true
	case "uri":
		return forwardapp.SourceURI, true
	case "ip":
		return forwardapp.SourceIP, true
	case "target":
		return forwardapp.SourceTarget, true
	case "scope":
		return forwardapp.SourceScope, true
	default:
		return 0, false
	}
}

func validIdentity(config IdentityConfig) bool {
	if config.InitialGlobalAdmins == nil || len(config.InitialGlobalAdmins) > maxInitialAdmins {
		return false
	}
	admins := make(map[string]struct{}, len(config.InitialGlobalAdmins))
	for _, admin := range config.InitialGlobalAdmins {
		if !validProviderReference(admin) {
			return false
		}
		if _, duplicate := admins[admin]; duplicate {
			return false
		}
		admins[admin] = struct{}{}
	}
	if config.ProjectEnrollment == nil {
		return true
	}
	enrollment := config.ProjectEnrollment
	if enrollment.ProjectID <= 0 || enrollment.AdditionalProjectRolesForGlobalAdmins == nil ||
		len(enrollment.AdditionalProjectRolesForGlobalAdmins) > maxProjectRoles ||
		!validAllowedDomains(enrollment.AllowedDomains) {
		return false
	}
	roles := make(map[string]struct{}, len(enrollment.AdditionalProjectRolesForGlobalAdmins))
	for _, role := range enrollment.AdditionalProjectRolesForGlobalAdmins {
		if !validTrimmedText(role, maxRoleBytes) {
			return false
		}
		if _, duplicate := roles[role]; duplicate {
			return false
		}
		roles[role] = struct{}{}
	}
	return true
}

func validAllowedDomains(value string) bool {
	if value == "" || len(value) > maxAllowedDomainsBytes || !utf8.ValidString(value) ||
		strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > maxAllowedDomains {
		return false
	}
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		domain := strings.Trim(strings.TrimSpace(part), "@")
		if domain == "" || len(domain) > 253 || domain != "*" && !validDomainText(domain) {
			return false
		}
		if _, duplicate := seen[domain]; duplicate {
			return false
		}
		seen[domain] = struct{}{}
	}
	return true
}

func validDomainText(value string) bool {
	if !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || character == ',' || character == '@' {
			return false
		}
	}
	return true
}

func validProviderReference(value string) bool {
	return len(value) <= browserflow.MaxProviderReferenceBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) != "" && !strings.ContainsFunc(value, unicode.IsControl)
}

func validTrimmedText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && value == strings.TrimSpace(value) &&
		utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func validFilePath(path string) bool {
	return path != "" && len(path) <= maxPathBytes && utf8.ValidString(path) &&
		!strings.Contains(path, "${") && !strings.ContainsFunc(path, unicode.IsControl) &&
		filepath.IsAbs(path) && filepath.Clean(path) == path
}

func canonicalASCII(value string) bool {
	for index := range len(value) {
		if value[index] < '!' || value[index] > '~' || value[index] == '%' ||
			value[index] == '?' || value[index] == '#' {
			return false
		}
	}
	return true
}

func invalid(part string) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfiguration, part)
}
