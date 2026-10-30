package platformconfig

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionRemoteAgents governs calls between the platform and agents outside
// a run (ADR-0044 decision 4, #1501).
//
//   - outbound_assertions: which of a run's calls to remote MCP servers
//     carry a signed delegation assertion (header Elitea-Agent-Assertion).
//     The assertion names the user, the project, the run, the mandate and
//     the run's toolkit types, so it is sent to no third party unless an
//     administrator says so: "off" (the default) sends none, "allowlist"
//     sends one only to the endpoints whose origin is in
//     assertion_audiences, and "on" sends one to every remote MCP server. A
//     governed run always carries one (governance means every call is
//     attributable), whatever this says.
//   - assertion_audiences: the origins ("https://host[:port]") that
//     "allowlist" sends assertions to.
//   - trusted_issuers: the issuers whose delegation assertions the
//     platform's MCP endpoint accepts. Empty (the default) accepts none. The
//     composition root adds the entries of ELITEA_REMOTE_AGENT_TRUSTED_ISSUERS
//     (Helm bootstrap) to the stored ones.
const SectionRemoteAgents = "remote_agents"

// Field keys of SectionRemoteAgents.
const (
	KeyRemoteAgentsOutboundAssertions = "outbound_assertions"
	KeyRemoteAgentsAssertionAudiences = "assertion_audiences"
	KeyRemoteAgentsTrustedIssuers     = "trusted_issuers"
)

// OutboundAssertionsMode is the outbound switch.
type OutboundAssertionsMode string

const (
	OutboundAssertionsOff       OutboundAssertionsMode = "off"
	OutboundAssertionsAllowlist OutboundAssertionsMode = "allowlist"
	OutboundAssertionsOn        OutboundAssertionsMode = "on"
)

// ValidOutboundAssertionsMode reports a mode the section accepts.
func ValidOutboundAssertionsMode(mode OutboundAssertionsMode) bool {
	switch mode {
	case OutboundAssertionsOff, OutboundAssertionsAllowlist, OutboundAssertionsOn:
		return true
	}
	return false
}

// Bounds of the trusted issuer list.
const (
	MaxRemoteAgentTrustedIssuers   = 32
	MaxRemoteAgentTrustedIssuerLen = 1024
	MaxRemoteAgentAudiences        = 256
)

// EliteaAgentJWKSPath is where an Elitea deployment publishes its agent
// assertion keys (eliteanative.JWKSPath); repeated here so the platform
// config has no import of the issuer package.
const EliteaAgentJWKSPath = "/.well-known/elitea-agent-jwks.json"

// RemoteAgentSettings is the section.
type RemoteAgentSettings struct {
	OutboundAssertions OutboundAssertionsMode `json:"outbound_assertions"`
	// AssertionAudiences are the stored origins of "allowlist", as written
	// (the mint normalizes each with eliteanative.NormalizeAudience).
	AssertionAudiences []string `json:"assertion_audiences"`
	// TrustedIssuers is the stored list, each "<issuer> [<jwks_url>]".
	TrustedIssuers []string `json:"trusted_issuers"`
}

// DefaultRemoteAgentSettings is a deployment that never saved the section.
func DefaultRemoteAgentSettings() RemoteAgentSettings {
	return RemoteAgentSettings{OutboundAssertions: OutboundAssertionsOff, AssertionAudiences: []string{}, TrustedIssuers: []string{}}
}

// LoadRemoteAgentSettings reads the section over the defaults. A query
// error answers the defaults and the error. A database without the table
// never saved the section.
func LoadRemoteAgentSettings(ctx context.Context, pool *pgxpool.Pool) (RemoteAgentSettings, error) {
	values, err := Load(ctx, pool, SectionRemoteAgents)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return DefaultRemoteAgentSettings(), nil
	}
	if err != nil {
		return DefaultRemoteAgentSettings(), err
	}
	return RemoteAgentSettingsFromValues(values), nil
}

// RemoteAgentSettingsFromValues is the overlay. A stored mode that is not
// one of the three reads as "off" (nothing is sent to a third party by
// mistake). A stored issuer list is
// kept as written; ParseTrustedIssuer refuses a malformed entry where it is
// read, so a bad entry trusts nothing.
func RemoteAgentSettingsFromValues(values Values) RemoteAgentSettings {
	settings := DefaultRemoteAgentSettings()
	if raw, present := values[KeyRemoteAgentsOutboundAssertions]; present {
		mode, _ := raw.(string)
		if parsed := OutboundAssertionsMode(strings.TrimSpace(mode)); ValidOutboundAssertionsMode(parsed) {
			settings.OutboundAssertions = parsed
		}
	}
	settings.AssertionAudiences = stringList(values[KeyRemoteAgentsAssertionAudiences], settings.AssertionAudiences)
	settings.TrustedIssuers = stringList(values[KeyRemoteAgentsTrustedIssuers], settings.TrustedIssuers)
	return settings
}

// stringList answers the strings of a stored array, or fallback when the
// value is not an array.
func stringList(raw any, fallback []string) []string {
	items, ok := raw.([]any)
	if !ok {
		return fallback
	}
	entries := make([]string, 0, len(items))
	for _, item := range items {
		if entry, ok := item.(string); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// TrustedIssuer is one parsed entry: an issuer and the JWKS URL its keys
// are fetched from.
type TrustedIssuer struct {
	Issuer  string
	JWKSURL string
}

// ParseTrustedIssuer reads one entry: "<issuer>" or "<issuer> <jwks_url>".
// Both are https URLs without credentials, query or fragment. With no JWKS
// URL the issuer is an Elitea deployment: its keys are at the issuer's
// origin + /.well-known/elitea-agent-jwks.json.
func ParseTrustedIssuer(entry string) (TrustedIssuer, error) {
	fields := strings.Fields(entry)
	if len(entry) > MaxRemoteAgentTrustedIssuerLen || len(fields) == 0 || len(fields) > 2 {
		return TrustedIssuer{}, fmt.Errorf("entry %q must be an issuer URL, optionally followed by its JWKS URL", entry)
	}
	issuer, err := httpsURL(fields[0])
	if err != nil {
		return TrustedIssuer{}, fmt.Errorf("issuer %q: %w", fields[0], err)
	}
	out := TrustedIssuer{Issuer: fields[0]}
	if len(fields) == 2 {
		if _, err := httpsURL(fields[1]); err != nil {
			return TrustedIssuer{}, fmt.Errorf("JWKS URL %q: %w", fields[1], err)
		}
		out.JWKSURL = fields[1]
	} else {
		out.JWKSURL = "https://" + issuer.Host + EliteaAgentJWKSPath
	}
	return out, nil
}

// ParseTrustedIssuers parses a list, refusing duplicates and more than the
// bound. The error names the entry.
func ParseTrustedIssuers(entries []string) ([]TrustedIssuer, error) {
	if len(entries) > MaxRemoteAgentTrustedIssuers {
		return nil, fmt.Errorf("at most %d trusted issuers", MaxRemoteAgentTrustedIssuers)
	}
	seen := map[string]bool{}
	out := make([]TrustedIssuer, 0, len(entries))
	for _, entry := range entries {
		parsed, err := ParseTrustedIssuer(entry)
		if err != nil {
			return nil, err
		}
		if seen[parsed.Issuer] {
			return nil, fmt.Errorf("issuer %q is listed twice", parsed.Issuer)
		}
		seen[parsed.Issuer] = true
		out = append(out, parsed)
	}
	return out, nil
}

func httpsURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must be an https URL without credentials, query or fragment")
	}
	return u, nil
}
