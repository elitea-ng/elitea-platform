// Package egresslib is the ONE parser for the LLM gateway's egress allowlist.
//
// # Why this grammar has its own module
//
// The allowlist reaches the gateway from two places and is validated in a third
// service:
//
//	GATEWAY_EGRESS_ALLOWLIST   the operator's bootstrap FLOOR, set in the chart.
//	gateway.governance_config  `egress_allowlist` rows, authored at runtime in
//	                           the admin UI and polled by the gateway.
//	elitea-main                the admin API that WRITES those rows, and must
//	                           refuse an entry the gateway would later drop.
//
// Three copies of one grammar drift. A host the admin form accepts and the
// gateway silently ignores is the exact failure the whole feature exists to
// end: the operator sees a saved rule and the gateway dials nothing. So the
// grammar lives here, in a module both `services/elitea-llm-gateway` (outside
// go.work, Go 1.26.4) and `services/elitea-main` require by path.
//
// # The grammar
//
//	example.com            an exact host, any port
//	example.com:8000       an exact host, that port only
//	*.example.com          any host UNDER example.com (never example.com itself)
//	*.example.com:8000     the same, that port only
//	192.168.29.60:8000     an exact IP literal and port
//	192.168.29.0/24        a CIDR block, matched against IP-LITERAL hosts only
//
// An entry carries no scheme, no path and no spaces. A malformed entry is an
// ERROR, never a silent skip: a typo that dropped one rule would either open
// the gateway wider than intended or wedge a legitimate provider.
//
// # Why a NAME allowlist, and what the CIDR form does NOT do
//
// `api_base` comes from the tenant: any user who can author a credential row
// picks the address the gateway dials. An IP-range check on a NAME would be a
// check-then-dial race — the address we resolve is not necessarily the one the
// transport later connects to, which is the classic DNS-rebinding bypass. A
// name allowlist has no such race: the operator asserts that a hostname is a
// legitimate destination, and whatever it resolves to at dial time is by
// definition operator-sanctioned.
//
// A CIDR entry therefore matches ONLY an api_base whose host is an IP literal.
// A hostname that happens to resolve inside the block is NOT matched, because
// matching it would reintroduce the very race the name allowlist avoids. An
// operator who runs a private endpoint under a name writes both entries:
//
//	vllm.ml.svc.cluster.local:8000    permits the NAME
//	10.0.0.0/8                        declares that private egress is intended
//
// # AllowsPrivateNetwork
//
// bifrost's SSRF-safe dialer refuses RFC-1918, loopback and link-local
// destinations unless NetworkConfig.AllowPrivateNetwork is set, and that flag is
// per PROVIDER, not per host. AllowsPrivateNetwork reports whether the operator
// EXPLICITLY named a private destination — a private IP literal or a CIDR that
// overlaps a private range. It is deliberately not "is any allowlist set at
// all": a deployment whose allowlist names only public SaaS hosts gains nothing
// from a dialer that would also reach the cluster's own service network.
package egresslib

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Kind names the three entry shapes.
type Kind string

const (
	// KindExact is a single host, optionally pinned to one port.
	KindExact Kind = "exact"
	// KindWildcard is "*.domain": any host with at least one extra label in
	// front of the domain, optionally pinned to one port.
	KindWildcard Kind = "wildcard"
	// KindCIDR is an IP block. It matches IP-literal hosts only.
	KindCIDR Kind = "cidr"
)

// Entry is one parsed allowlist rule.
type Entry struct {
	kind Kind
	// text is the normalised spelling, used for deduplication and for the
	// operator-facing report.
	text string
	// host is the normalised host for KindExact, and the dotted suffix
	// (".example.com") for KindWildcard.
	host string
	// port is "" when the entry pinned no port.
	port string
	// ipNet is set for KindCIDR only.
	ipNet *net.IPNet
}

// Kind reports the entry shape.
func (e Entry) Kind() Kind { return e.kind }

// String returns the normalised spelling of the entry.
func (e Entry) String() string { return e.text }

// privateBlocks are the ranges that AllowPrivateNetwork actually UNLOCKS in
// bifrost's SSRF-safe dialer (core/network/utils.go IsPrivateIP). A CIDR entry
// that OVERLAPS any of them, or an exact IP literal inside one, is an explicit
// declaration that private egress is intended.
//
// They are listed rather than derived because net.IP's IsPrivate/IsLoopback
// helpers answer for a single address, and a CIDR entry has to be tested for
// overlap with a range.
//
// LINK-LOCAL IS DELIBERATELY ABSENT (169.254.0.0/16, fe80::/10). bifrost refuses
// those addresses whatever AllowPrivateNetwork says, because they carry the
// cloud instance-metadata endpoints. An entry naming one declares nothing this
// package can grant, so it must not flip the flag and imply that it did.
var privateBlocks = mustParseCIDRs(
	"10.0.0.0/8",     // RFC 1918
	"172.16.0.0/12",  // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"127.0.0.0/8",    // loopback
	"::1/128",        // IPv6 loopback
	"fc00::/7",       // IPv6 unique-local
)

func mustParseCIDRs(blocks ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(blocks))
	for _, b := range blocks {
		_, n, err := net.ParseCIDR(b)
		if err != nil {
			panic("egresslib: bad built-in private block " + b)
		}
		out = append(out, n)
	}
	return out
}

// ParseEntry parses and normalises ONE allowlist entry.
//
// An empty entry is an error here. Parse skips empties, because a CSV
// environment variable routinely carries a trailing comma; an authored row is a
// different thing, and an admin surface that accepted a blank host would store
// a rule that governs nothing.
func ParseEntry(raw string) (Entry, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" {
		return Entry{}, errors.New("egress allowlist entry is empty: want a host, host:port, *.domain or CIDR block")
	}
	if strings.ContainsAny(e, " \t") || strings.Contains(e, "://") {
		return Entry{}, fmt.Errorf("egress allowlist entry %q: want a host, host:port, *.domain or CIDR block "+
			"(no scheme, no path, no spaces)", raw)
	}

	if strings.Contains(e, "/") {
		_, ipNet, err := net.ParseCIDR(e)
		if err != nil {
			return Entry{}, fmt.Errorf("egress allowlist entry %q: a value with \"/\" must be a CIDR block "+
				"such as 192.168.0.0/16; a path is not part of the grammar", raw)
		}
		return Entry{kind: KindCIDR, text: ipNet.String(), ipNet: ipNet}, nil
	}

	host, port := e, ""
	if h, p, err := net.SplitHostPort(e); err == nil {
		host, port = h, p
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Entry{}, fmt.Errorf("egress allowlist entry %q: port must be a number from 1 to 65535", raw)
		}
	}

	if strings.HasPrefix(host, "*.") {
		dotted := host[1:] // "*.example.com" -> ".example.com"
		if dotted == "." || strings.Contains(dotted[1:], "*") {
			return Entry{}, fmt.Errorf("egress allowlist entry %q: the wildcard must be a single leading \"*.\"", raw)
		}
		return Entry{kind: KindWildcard, text: joinPort("*"+dotted, port), host: dotted, port: port}, nil
	}
	if host == "" || strings.Contains(host, "*") {
		return Entry{}, fmt.Errorf("egress allowlist entry %q: a wildcard is only allowed as a leading \"*.\"", raw)
	}
	host = normaliseHost(host)
	if host == "" {
		return Entry{}, fmt.Errorf("egress allowlist entry %q: the host is empty", raw)
	}
	return Entry{kind: KindExact, text: joinPort(host, port), host: host, port: port}, nil
}

// NamesPrivateNetwork reports whether this entry explicitly names a destination
// inside a private range. See the package doc for why "any entry at all" is not
// the test.
func (e Entry) NamesPrivateNetwork() bool {
	switch e.kind {
	case KindCIDR:
		for _, block := range privateBlocks {
			if netsOverlap(e.ipNet, block) {
				return true
			}
		}
		return false
	case KindExact:
		// "localhost" is a name, but it is the one name whose meaning is fixed
		// by every resolver on the planet. Refusing to read it as private would
		// make the single most common local-development entry silently fail.
		if e.host == "localhost" {
			return true
		}
		ip := net.ParseIP(e.host)
		return ip != nil && ipIsPrivate(ip)
	default:
		// A wildcard names a DNS subtree. Nothing about it says where those
		// names resolve, so it cannot be read as a private declaration.
		return false
	}
}

// matches reports whether one entry admits the candidate host/port. ip is
// non-nil only when host is an IP literal.
func (e Entry) matches(host string, ip net.IP, port string) bool {
	switch e.kind {
	case KindCIDR:
		// IP literals only — a name is never resolved here. See the package doc.
		return ip != nil && e.ipNet.Contains(ip)
	case KindExact:
		if e.host != host {
			return false
		}
		return e.port == "" || e.port == port
	case KindWildcard:
		// "at least one extra label in front": ".example.com" must not match
		// the bare "example.com", only "a.example.com" and deeper.
		if !strings.HasSuffix(host, e.host) || len(host) <= len(e.host) {
			return false
		}
		return e.port == "" || e.port == port
	default:
		return false
	}
}

// Allowlist is an immutable set of parsed entries.
type Allowlist struct {
	entries []Entry
}

// Parse builds an Allowlist from raw entries, rejecting the whole set when any
// entry is malformed. Blank entries are skipped: a CSV environment variable
// routinely carries a trailing comma.
func Parse(raw []string) (*Allowlist, error) {
	a := &Allowlist{}
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		entry, err := ParseEntry(r)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[entry.text]; dup {
			continue
		}
		seen[entry.text] = struct{}{}
		a.entries = append(a.entries, entry)
	}
	sortEntries(a.entries)
	return a, nil
}

// ParsePartial builds an Allowlist from every entry it CAN parse and returns
// the raw text of the ones it could not.
//
// It exists for the runtime merge of the environment floor with the authored
// rows. elitea-main validates a row on write and the gateway's policy compiler
// rejects a bad one by name, so a malformed entry should never reach here — but
// "should never" is not a guarantee across a database restore or an older
// writer, and one bad authored entry must not drop the operator's whole
// environment floor. The caller logs what was skipped.
func ParsePartial(raw []string) (*Allowlist, []string) {
	a := &Allowlist{}
	var bad []string
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		if strings.TrimSpace(r) == "" {
			continue
		}
		entry, err := ParseEntry(r)
		if err != nil {
			bad = append(bad, r)
			continue
		}
		if _, dup := seen[entry.text]; dup {
			continue
		}
		seen[entry.text] = struct{}{}
		a.entries = append(a.entries, entry)
	}
	sortEntries(a.entries)
	return a, bad
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].text < entries[j].text })
}

// Configured reports whether the allowlist carries any entry. When false it
// imposes NO host restriction — see Allows.
func (a *Allowlist) Configured() bool { return a != nil && len(a.entries) > 0 }

// Entries returns the normalised entry text, sorted. It backs the
// operator-facing report of what the gateway is actually enforcing.
func (a *Allowlist) Entries() []string {
	if a == nil {
		return []string{}
	}
	out := make([]string, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.text)
	}
	return out
}

// AllowsPrivateNetwork reports whether ANY entry explicitly names a private
// destination.
func (a *Allowlist) AllowsPrivateNetwork() bool {
	if a == nil {
		return false
	}
	for _, e := range a.entries {
		if e.NamesPrivateNetwork() {
			return true
		}
	}
	return false
}

// Allows reports whether apiBase may be dialled.
//
// An EMPTY apiBase is allowed: the credential then carries no endpoint at all
// and the provider's own default is used, which is not tenant-controlled and so
// is not what this allowlist governs.
//
// An UNCONFIGURED allowlist allows everything. That is the historical default
// and it is safe only in combination with the dialer: with no entry,
// AllowsPrivateNetwork is false, so the SSRF-safe dialer stays on and no tenant
// can steer the gateway into a private network.
//
// A non-empty apiBase that cannot be parsed into a host is REFUSED. Failing
// closed here matters: an unparsable value is exactly what an attacker probing
// for a parser differential between this check and the transport would submit.
func (a *Allowlist) Allows(apiBase string) bool {
	if strings.TrimSpace(apiBase) == "" {
		return true
	}
	if !a.Configured() {
		return true
	}

	u, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil || u.Host == "" {
		return false
	}
	host := normaliseHost(strings.ToLower(u.Hostname()))
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	port := u.Port()
	if port == "" {
		// Compare against the scheme's default so a "host:443" rule still
		// matches "https://host" and vice versa.
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}

	for _, e := range a.entries {
		if e.matches(host, ip, port) {
			return true
		}
	}
	return false
}

// Merge returns a new Allowlist holding the union of a and the extra raw
// entries, skipping the ones that do not parse and reporting them.
//
// Union, never intersection: the environment variable is the operator's FLOOR.
// An authored row can add a destination; nothing an admin authors at runtime
// can withdraw a host the chart named, so a compromised or mistaken admin
// session cannot cut the platform off from its own provider.
func (a *Allowlist) Merge(extra []string) (*Allowlist, []string) {
	base := a.Entries()
	merged, bad := ParsePartial(append(base, extra...))
	return merged, bad
}

// normaliseHost strips a trailing root dot and canonicalises an IP literal, so
// "EXAMPLE.com." and "example.com", and "::1" and "0:0:0:0:0:0:0:1", are one
// value on both sides of the comparison.
func normaliseHost(host string) string {
	host = strings.TrimSuffix(host, ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func joinPort(host, port string) string {
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// ipIsPrivate answers for ONE address, using the same block set as a CIDR
// entry, so "192.168.29.60" and "192.168.29.0/24" cannot disagree.
func ipIsPrivate(ip net.IP) bool {
	for _, block := range privateBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func netsOverlap(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Contains(b.IP) || b.Contains(a.IP)
}
