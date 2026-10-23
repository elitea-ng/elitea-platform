package egress

import (
	"net"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/addrcheck"
)

// The address classification and the private-permits grammar live in
// libs/go/egresslib/addrcheck, which elitea-egress-proxy shares. These two
// names keep this package unchanged for its call sites and tests.

// classify decides one address (addrcheck.Classify).
func classify(ip net.IP) addrcheck.Class { return addrcheck.Classify(ip) }

// privatePermits reads the entries that permit private egress, each for its
// own range only (addrcheck.PrivatePermits).
func privatePermits(allowlist *egresslib.Allowlist) addrcheck.PermitSet {
	return addrcheck.PrivatePermits(allowlist)
}
