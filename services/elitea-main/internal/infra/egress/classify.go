package egress

import (
	"net"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

// addressClass is what the guard does with one resolved address.
type addressClass int

const (
	// classPublic is dialled.
	classPublic addressClass = iota
	// classPrivate is dialled only when the allowlist declares private egress.
	classPrivate
	// classForbidden is never dialled, whatever the allowlist says.
	classForbidden
)

// forbiddenBlocks are never a legitimate tenant destination. The cloud
// metadata endpoints are listed even where they sit inside a liftable range
// (CGNAT, ULA): an operator allowing private egress has not allowed metadata.
var forbiddenBlocks = mustParseCIDRs(
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
var privateBlocks = mustParseCIDRs(
	"100.64.0.0/10", // CGNAT
	"198.18.0.0/15", // benchmarking
	"fec0::/10",     // deprecated IPv6 site-local
)

var (
	nat64Block     = mustParseCIDRs("64:ff9b::/96")[0]
	sixToFourBlock = mustParseCIDRs("2002::/16")[0]
)

func mustParseCIDRs(blocks ...string) []*net.IPNet {
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

func inAny(blocks []*net.IPNet, ip net.IP) bool {
	for _, block := range blocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// classify decides one address. IPv4-mapped addresses are matched as IPv4 by
// net.IPNet and net.IP; NAT64 and 6to4 addresses are classified as the IPv4
// address they reach.
func classify(ip net.IP) addressClass {
	if v4 := embeddedIPv4(ip); v4 != nil {
		return classify(v4)
	}
	switch {
	case ip.IsUnspecified():
		return classForbidden
	case ip.IsLoopback():
		return classPrivate
	case inAny(forbiddenBlocks, ip):
		return classForbidden
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return classForbidden
	case ip.IsPrivate(), inAny(privateBlocks, ip):
		return classPrivate
	default:
		return classPublic
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

// declaresPrivateEgress reports whether the allowlist declares that private
// egress is intended. egresslib answers for RFC 1918, loopback and ULA; this
// adds an entry naming one of privateBlocks, so an operator who names a CGNAT
// receiver keeps reaching it. An entry naming only forbidden space (the
// metadata address) declares nothing.
func declaresPrivateEgress(allowlist *egresslib.Allowlist) bool {
	if allowlist.AllowsPrivateNetwork() {
		return true
	}
	for _, text := range allowlist.Entries() {
		entry, err := egresslib.ParseEntry(text)
		if err != nil {
			continue
		}
		switch entry.Kind() {
		case egresslib.KindCIDR:
			if _, block, err := net.ParseCIDR(text); err == nil && namesPrivateBlock(block) {
				return true
			}
		case egresslib.KindExact:
			host := text
			if h, _, err := net.SplitHostPort(text); err == nil {
				host = h
			}
			if ip := net.ParseIP(host); ip != nil && classify(ip) == classPrivate {
				return true
			}
		}
	}
	return false
}

// namesPrivateBlock reports whether block overlaps a privateBlocks range and
// is not wholly inside a forbidden one.
func namesPrivateBlock(block *net.IPNet) bool {
	ones, _ := block.Mask.Size()
	for _, forbidden := range forbiddenBlocks {
		fOnes, _ := forbidden.Mask.Size()
		if forbidden.Contains(block.IP) && fOnes <= ones && len(forbidden.IP) == len(block.IP) {
			return false
		}
	}
	for _, private := range privateBlocks {
		if private.Contains(block.IP) || block.Contains(private.IP) {
			return true
		}
	}
	return false
}
