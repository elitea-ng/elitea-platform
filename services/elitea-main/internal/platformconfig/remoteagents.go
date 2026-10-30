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
//   - outbound_assertions: whether a run's calls to remote MCP servers carry
//     a signed delegation assertion (header Elitea-Agent-Assertion). "on" is
//     the default: an extra header is harmless to a server that ignores it.
//     A governed run always carries one, whatever this says.
//   - trusted_issuers: the issuers whose delegation assertions the
//     platform's MCP endpoint accepts. Empty (the default) accepts none. The
//     composition root adds the entries of ELITEA_REMOTE_AGENT_TRUSTED_ISSUERS
//     (Helm bootstrap) to the stored ones.
const SectionRemoteAgents = "remote_agents"

// Field keys of SectionRemoteAgents.
const (
	KeyRemoteAgentsOutboundAssertions = "outbound_assertions"
	KeyRemoteAgentsTrustedIssuers     = "trusted_issuers"
)

// OutboundAssertionsMode is the outbound switch.
type OutboundAssertionsMode string

const (
	OutboundAssertionsOn  OutboundAssertionsMode = "on"
	OutboundAssertionsOff OutboundAssertionsMode = "off"
)

// Bounds of the trusted issuer list.
const (
	MaxRemoteAgentTrustedIssuers   = 32
	MaxRemoteAgentTrustedIssuerLen = 1024
)

// EliteaAgentJWKSPath is where an Elitea deployment publishes its agent
// assertion keys (eliteanative.JWKSPath); repeated here so the platform
// config has no import of the issuer package.
const EliteaAgentJWKSPath = "/.well-known/elitea-agent-jwks.json"

// RemoteAgentSettings is the section.
type RemoteAgentSettings struct {
	OutboundAssertions OutboundAssertionsMode `json:"outbound_assertions"`
	// TrustedIssuers is the stored list, each "<issuer> [<jwks_url>]".
	TrustedIssuers []string `json:"trusted_issuers"`
}

// DefaultRemoteAgentSettings is a deployment that never saved the section.
func DefaultRemoteAgentSettings() RemoteAgentSettings {
	return RemoteAgentSettings{OutboundAssertions: OutboundAssertionsOn, TrustedIssuers: []string{}}
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

// RemoteAgentSettingsFromValues is the overlay. Only "off" turns outbound
// assertions off; any other stored value is "on". A stored issuer list is
// kept as written; ParseTrustedIssuer refuses a malformed entry where it is
// read, so a bad entry trusts nothing.
func RemoteAgentSettingsFromValues(values Values) RemoteAgentSettings {
	settings := DefaultRemoteAgentSettings()
	if raw, present := values[KeyRemoteAgentsOutboundAssertions]; present {
		mode, _ := raw.(string)
		if OutboundAssertionsMode(strings.TrimSpace(mode)) == OutboundAssertionsOff {
			settings.OutboundAssertions = OutboundAssertionsOff
		}
	}
	if raw, present := values[KeyRemoteAgentsTrustedIssuers].([]any); present {
		entries := make([]string, 0, len(raw))
		for _, item := range raw {
			if entry, ok := item.(string); ok {
				entries = append(entries, entry)
			}
		}
		settings.TrustedIssuers = entries
	}
	return settings
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
