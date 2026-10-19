package egress

import (
	"fmt"
	"strings"
)

// HTTPActionAllowlistEnv is the operator's outer bound for HTTP action rules
// (pipeline HTTP Request nodes). Unset or empty permits public destinations
// only; an entry names the private address or block (optionally port-pinned)
// a rule may reach. The same value bounds rule writes (admin API) and every
// dial (runtime), so both read it through NewHTTPActionGuard.
const HTTPActionAllowlistEnv = "ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST"

// NewHTTPActionGuard parses the allowlist value and returns the HTTPS-only
// guard every HTTP action rule write and dial goes through. A malformed entry
// is an error: callers refuse to start rather than run with a wider or
// narrower bound than the operator wrote.
func NewHTTPActionGuard(raw string, opts ...Option) (*Guard, error) {
	allowlist, err := ParseAllowlist(splitAllowlist(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", HTTPActionAllowlistEnv, err)
	}
	return New(allowlist, append([]Option{RequireHTTPS()}, opts...)...), nil
}

// splitAllowlist reads the comma-separated form the other egress allowlists
// use (cmd/elitea-main splitEnvList).
func splitAllowlist(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	entries := []string{}
	for _, field := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}
