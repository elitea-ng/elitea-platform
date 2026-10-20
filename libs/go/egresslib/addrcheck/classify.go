// Package addrcheck is the address classification and the checked dial that
// every Go egress boundary in this repository shares: elitea-main's egress
// guard (services/elitea-main/internal/infra/egress) and elitea-egress-proxy.
//
// # What it decides
//
//   - Classify puts one resolved address in one of three classes. Public
//     addresses are dialled. Private addresses (loopback, RFC 1918, ULA, CGNAT,
//     benchmarking, site-local) are dialled only when a caller permits them.
//     Forbidden addresses (unspecified, link-local, multicast, reserved,
//     IETF-protocol, discard, Teredo and cloud metadata) are never dialled.
//   - IPv4 reached through an IPv6 form (IPv4-mapped, NAT64 64:ff9b::/96,
//     6to4 2002::/16) is classified as the IPv4 address it reaches.
//   - PrivatePermits reads the entries of an egresslib allowlist that name
//     private space: a CIDR entry permits its own block, an IP literal that
//     address, `localhost` the loopback blocks, each only on the entry's port
//     when it pins one. A host name or `*.` wildcard permits no private
//     address.
//   - Resolve resolves a host to a bounded list of addresses, and DialFirst
//     dials the checked IP literals, so DNS rebinding cannot swap the address
//     between the check and the connect.
//
// The package moved here from elitea-main so the egress proxy cannot drift
// from the guard. elitea-main's egress tests pin the behaviour; they did not
// change in the move. This package's own tests pin it too.
//
// The package uses the standard library and egresslib only, because
// services/elitea-llm-gateway (outside go.work) requires this module by path.
package addrcheck

import (
	"net"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

// Class is what an egress boundary does with one resolved address.
type Class int

const (
	// Public is dialled.
	Public Class = iota
	// Private is dialled only when the caller permits the address.
	Private
	// Forbidden is never dialled, whatever an allowlist says.
	Forbidden
)

// String names the class for logs and test output.
func (c Class) String() string {
	switch c {
	case Public:
		return "public"
	case Private:
		return "private"
	case Forbidden:
		return "forbidden"
	default:
		return "unknown"
	}
}

// forbiddenBlocks are never a legitimate tenant destination. The cloud
// metadata endpoints are listed even where they sit inside a liftable range
// (CGNAT, ULA): an operator allowing private egress has not allowed metadata.
var forbiddenBlocks = MustParseCIDRs(
	"0.0.0.0/8",          // "this network"; 0.x reaches the local host on Linux
	"192.0.0.0/24",       // IETF protocol assignments
	"240.0.0.0/4",        // reserved, including 255.255.255.255
	"100.100.100.200/32", // Alibaba Cloud metadata, inside CGNAT
	"168.63.129.16/32",   // Azure platform endpoint
	"fd00:ec2::254/128",  // AWS instance metadata over IPv6, inside ULA
	"64:ff9b:1::/48",     // local-use NAT64 (RFC 8215); embedding is not fixed
	"100::/64",           // discard-only
	"2001::/32",          // Teredo; the embedded IPv4 is obfuscated
	"::/96",              // IPv4-compatible (deprecated); :: and ::1 are matched first
)

// privateBlocks extend net.IP.IsPrivate (RFC 1918, ULA) with the other ranges
// that are internal by convention. They are liftable for the same reason
// RFC 1918 is: an operator may run a receiver on Tailscale (CGNAT) or a lab
// network (benchmarking).
var privateBlocks = MustParseCIDRs(
	"100.64.0.0/10", // CGNAT
	"198.18.0.0/15", // benchmarking
	"fec0::/10",     // deprecated IPv6 site-local
)

var (
	nat64Block     = MustParseCIDRs("64:ff9b::/96")[0]
	sixToFourBlock = MustParseCIDRs("2002::/16")[0]
)

// MustParseCIDRs parses built-in blocks and panics on a typo. It is for
// package-level tables, never for operator input.
func MustParseCIDRs(blocks ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(blocks))
	for _, b := range blocks {
		_, n, err := net.ParseCIDR(b)
		if err != nil {
			panic("egress: bad built-in block " + b)
		}
		out = append(out, n)
	}
	return out
}

// InAny reports whether any block contains ip.
func InAny(blocks []*net.IPNet, ip net.IP) bool {
	for _, block := range blocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// Classify decides one address. IPv4-mapped addresses are matched as IPv4 by
// net.IPNet and net.IP; NAT64 and 6to4 addresses are classified as the IPv4
// address they reach. It does not allocate for an ordinary address.
func Classify(ip net.IP) Class {
	if v4 := embeddedIPv4(ip); v4 != nil {
		return Classify(v4)
	}
	switch {
	case ip.IsUnspecified():
		return Forbidden
	case ip.IsLoopback():
		return Private
	case InAny(forbiddenBlocks, ip):
		return Forbidden
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return Forbidden
	case ip.IsPrivate(), InAny(privateBlocks, ip):
		return Private
	default:
		return Public
	}
}

// embeddedIPv4 returns the IPv4 address a NAT64 (64:ff9b::/96) or 6to4
// (2002::/16) address reaches, or nil.
func embeddedIPv4(ip net.IP) net.IP {
	if ip.To4() != nil || len(ip) != net.IPv6len {
		return nil
	}
	switch {
	case nat64Block.Contains(ip):
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	case sixToFourBlock.Contains(ip):
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	default:
		return nil
	}
}

// Canonical is the address Classify judges: the IPv4 address an IPv4-mapped,
// NAT64 or 6to4 form reaches, as 4 bytes; any other address as itself.
func Canonical(ip net.IP) net.IP {
	if v4 := embeddedIPv4(ip); v4 != nil {
		ip = v4
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// CanonicalBlock returns an IPv4-mapped CIDR (::ffff:10.0.0.0/104) as the
// IPv4 block it names, so it matches the canonical IPv4 address.
func CanonicalBlock(block *net.IPNet) *net.IPNet {
	ones, bits := block.Mask.Size()
	if v4 := block.IP.To4(); v4 != nil && bits == 8*net.IPv6len && ones >= 96 {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(ones-96, 32)}
	}
	return block
}

// Permit is one allowlist entry that names private space: the block it names
// and, when the entry pinned one, the only port it permits.
type Permit struct {
	block *net.IPNet
	port  string
}

// Permits reports whether one permit admits ip (already canonical) on port.
func (p Permit) Permits(ip net.IP, port string) bool {
	return (p.port == "" || p.port == port) && p.block.Contains(ip)
}

// PermitSet is the private permits of one allowlist.
type PermitSet []Permit

// Admits reports whether a permit in the set names this private address on
// this port. ip need not be canonical.
func (s PermitSet) Admits(ip net.IP, port string) bool {
	if len(s) == 0 {
		return false
	}
	ip = Canonical(ip)
	for _, permit := range s {
		if permit.Permits(ip, port) {
			return true
		}
	}
	return false
}

// loopbackBlocks are what a `localhost` entry names. Resolve pins the name to
// 127.0.0.1, and egresslib reads the entry as private.
var loopbackBlocks = MustParseCIDRs("127.0.0.0/8", "::1/128")

// PrivatePermits reads the entries that permit private egress, each for its
// own range only: a CIDR entry permits its block, an IP literal entry that one
// address, and `localhost` the loopback blocks, each limited to the entry's
// port when it pins one. A host name or `*.` wildcard permits no private
// address, because nothing about it says where it resolves (egresslib's
// package doc). Naming one private range never opens the others: an operator
// who names a CGNAT receiver has not opened loopback or RFC 1918. Forbidden
// addresses stay refused whatever an entry names (Filter).
func PrivatePermits(allowlist *egresslib.Allowlist) PermitSet {
	var permits PermitSet
	for _, text := range allowlist.Entries() {
		entry, err := egresslib.ParseEntry(text)
		if err != nil {
			continue
		}
		switch entry.Kind() {
		case egresslib.KindCIDR:
			if _, block, err := net.ParseCIDR(text); err == nil {
				permits = append(permits, Permit{block: CanonicalBlock(block)})
			}
		case egresslib.KindExact:
			host, port := text, ""
			if h, p, err := net.SplitHostPort(text); err == nil {
				host, port = h, p
			}
			if host == "localhost" {
				for _, block := range loopbackBlocks {
					permits = append(permits, Permit{block: block, port: port})
				}
				continue
			}
			if ip := net.ParseIP(host); ip != nil {
				ip = Canonical(ip)
				bits := 8 * len(ip)
				permits = append(permits, Permit{
					block: &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)},
					port:  port,
				})
			}
		}
	}
	return permits
}

// Filter is the one address filter: it keeps public addresses, and a private
// one only when a permit names it (on this port, when the permit pins one).
// Forbidden addresses are never kept. A nil set keeps public addresses only.
// It allocates once, for the result.
func Filter(ips []net.IP, port string, private PermitSet) []net.IP {
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		switch Classify(ip) {
		case Public:
			allowed = append(allowed, ip)
		case Private:
			if private.Admits(ip, port) {
				allowed = append(allowed, ip)
			}
		}
	}
	return allowed
}
