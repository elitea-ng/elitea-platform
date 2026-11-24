package egress

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DBActionAllowlistEnv is the operator's outer bound for DB action rules
// (pipeline `type: db` nodes) and the model-facing `sql` toolkit. Unset or
// empty permits public destinations only; an entry names the private address
// or CIDR block (optionally port-pinned) a rule may reach. The Worker enforces
// the same env var at every TCP dial (5e-1); Main validates rule writes against
// it here so private-target misconfigurations fail fast with a clear 400.
const DBActionAllowlistEnv = "ELITEA_DB_EGRESS_ALLOWLIST"

// NewDBActionGuard parses ELITEA_DB_EGRESS_ALLOWLIST and returns the egress
// guard used to validate DB action rule destinations at write time. A malformed
// entry fails closed so Main refuses to start rather than widen or narrow the
// operator's allowlist silently.
func NewDBActionGuard(raw string, opts ...Option) (*Guard, error) {
	allowlist, err := ParseAllowlist(splitAllowlist(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", DBActionAllowlistEnv, err)
	}
	return New(allowlist, opts...), nil
}

// ValidateHostPort checks a TCP host and port (such as a PostgreSQL or MySQL
// database target) against the guard's resolved-address and private-permit
// policy. Like Validate, it is the write-time check; the Worker's Rust egress
// guard re-resolves and pins the dialled IP on every connection.
func (g *Guard) ValidateHostPort(ctx context.Context, host string, port uint16) error {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" || port == 0 {
		return fmt.Errorf("%w: the destination host or port is empty", ErrDestinationRefused)
	}
	// Strip optional IPv6 brackets so literal IPv6 hosts resolve cleanly.
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		trimmed = trimmed[1 : len(trimmed)-1]
	}
	ips, err := g.resolveHost(ctx, trimmed)
	if err != nil {
		return fmt.Errorf("%w: could not resolve %q: %v", ErrDestinationRefused, trimmed, err)
	}
	if len(g.permittedIPs(ips, strconv.Itoa(int(port)))) == 0 {
		return notPermitted(trimmed, ips)
	}
	return nil
}
