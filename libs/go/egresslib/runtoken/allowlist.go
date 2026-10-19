package runtoken

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/addrcheck"
)

// MaxEntries bounds one run's allowlist.
const MaxEntries = 128

// maxEntryLen bounds one entry: a 253-byte name, "*.", ":" and a port.
const maxEntryLen = 262

// Rule is one parsed allowlist entry.
type Rule struct {
	// host is the normalised host for an exact name, the dotted suffix
	// (".example.com") for a wildcard, and empty for an IP literal.
	host     string
	wildcard bool
	// ip is set for an IP-literal entry, in canonical form.
	ip net.IP
	// port is 0 when the entry pinned no port: then only 443 and 80 match.
	port int
}

// String returns the normalised spelling of the rule.
func (r Rule) String() string {
	var host string
	switch {
	case r.ip != nil:
		host = r.ip.String()
	case r.wildcard:
		host = "*" + r.host
	default:
		host = r.host
	}
	if r.port == 0 {
		if r.ip != nil && r.ip.To4() == nil {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(r.port))
}

func (r Rule) portMatches(port int) bool {
	if r.port == 0 {
		return port == 443 || port == 80
	}
	return r.port == port
}

// Allowlist is the parsed host allowlist of one run. It is immutable.
//
// # The grammar
//
//	example.com          the exact host, on 443 or 80
//	example.com:8443     the exact host, on that port only
//	*.example.com        any host UNDER example.com (never example.com itself), on 443 or 80
//	*.example.com:8443   the same, on that port only
//	203.0.113.7          that public IP literal, on 443 or 80
//	[2001:db8::7]:8443   an IPv6 literal, in brackets when it pins a port
//
// An entry carries no scheme, no path, no CIDR and no spaces. A wildcard needs
// a suffix of at least two labels, so `*.com` is refused. A name whose last
// label is numeric is refused, because some resolvers read it as an IPv4
// address. An IP literal must be a public address: a private, loopback,
// link-local, metadata or reserved address can never be allowed, whatever the
// entry says. A name is matched by name only; the proxy still refuses every
// resolved address that is not public.
type Allowlist struct {
	rules []Rule
}

// ParseAllowlist parses a run's allowlist. One malformed entry refuses the
// whole list: a dropped rule would wedge a legitimate run, and a widened one
// would open egress the project did not name. Blank entries are refused too.
func ParseAllowlist(entries []string) (*Allowlist, error) {
	if len(entries) > MaxEntries {
		return nil, fmt.Errorf("the allowlist has %d entries, more than %d", len(entries), MaxEntries)
	}
	a := &Allowlist{rules: make([]Rule, 0, len(entries))}
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		rule, err := ParseRule(raw)
		if err != nil {
			return nil, err
		}
		key := rule.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		a.rules = append(a.rules, rule)
	}
	return a, nil
}

// errEntry wraps every grammar refusal with the offending entry. The entry is
// project configuration, not a secret.
func errEntry(raw, why string) error {
	return fmt.Errorf("egress allowlist entry %q: %s", raw, why)
}

// ParseRule parses one entry of the grammar on Allowlist.
func ParseRule(raw string) (Rule, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case e == "":
		return Rule{}, errors.New("egress allowlist entry is empty")
	case len(e) > maxEntryLen:
		return Rule{}, errEntry(raw, "the entry is too long")
	case strings.ContainsAny(e, " \t/@?#\\") || strings.Contains(e, "://"):
		return Rule{}, errEntry(raw, "want host, host:port, *.domain or *.domain:port (no scheme, path, CIDR or spaces)")
	}

	host, port, err := splitEntry(e)
	if err != nil {
		return Rule{}, errEntry(raw, err.Error())
	}

	if ip := net.ParseIP(host); ip != nil {
		ip = addrcheck.Canonical(ip)
		if addrcheck.Classify(ip) != addrcheck.Public {
			return Rule{}, errEntry(raw, "a private, loopback, link-local, metadata or reserved address can never be allowed")
		}
		return Rule{ip: ip, port: port}, nil
	}

	if strings.HasPrefix(host, "*.") {
		suffix := strings.TrimSuffix(host[2:], ".")
		if err := checkName(suffix); err != nil {
			return Rule{}, errEntry(raw, err.Error())
		}
		if !strings.Contains(suffix, ".") {
			return Rule{}, errEntry(raw, "a wildcard needs a suffix of at least two labels, such as *.example.com")
		}
		return Rule{host: "." + suffix, wildcard: true, port: port}, nil
	}
	name := strings.TrimSuffix(host, ".")
	if err := checkName(name); err != nil {
		return Rule{}, errEntry(raw, err.Error())
	}
	return Rule{host: name, port: port}, nil
}

// splitEntry splits host and optional port. A bare IPv6 literal carries no
// port; an IPv6 literal with a port is written in brackets.
func splitEntry(e string) (string, int, error) {
	if ip := net.ParseIP(e); ip != nil {
		return e, 0, nil
	}
	if strings.HasPrefix(e, "[") && strings.HasSuffix(e, "]") {
		inner := e[1 : len(e)-1]
		if net.ParseIP(inner) == nil || !strings.Contains(inner, ":") {
			return "", 0, errors.New("brackets must hold an IPv6 literal")
		}
		return inner, 0, nil
	}
	if !strings.Contains(e, ":") {
		return e, 0, nil
	}
	host, portText, err := net.SplitHostPort(e)
	if err != nil {
		return "", 0, errors.New("want host or host:port")
	}
	port, err := ParsePort(portText)
	if err != nil {
		return "", 0, err
	}
	if (strings.HasPrefix(e, "[") || strings.Contains(host, ":")) && (net.ParseIP(host) == nil || !strings.Contains(host, ":")) {
		return "", 0, errors.New("brackets must hold an IPv6 literal")
	}
	return host, port, nil
}

// ParsePort reads a decimal port from 1 to 65535. It refuses a sign, a
// leading zero and any other spelling that two parsers could read apart.
func ParsePort(text string) (int, error) {
	if text == "" || len(text) > 5 || text[0] == '0' {
		return 0, errors.New("the port must be a number from 1 to 65535")
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, errors.New("the port must be a number from 1 to 65535")
		}
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("the port must be a number from 1 to 65535")
	}
	return n, nil
}

// checkName accepts a DNS name of letters, digits, '-' and '_', with labels of
// 1 to 63 bytes and at most 253 bytes in all. The last label must not be
// numeric (see Allowlist).
func checkName(name string) error {
	if name == "" || len(name) > 253 {
		return errors.New("the host name is empty or longer than 253 bytes")
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return errors.New("each host label must be 1 to 63 bytes")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("a host label must not start or end with '-'")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return errors.New("a host name holds only letters, digits, '-' and '_' (use the punycode form of an IDN)")
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
		return errors.New("the last label of a host name must not be numeric")
	}
	return nil
}

// Entries returns the normalised rules, in the order they were given.
func (a *Allowlist) Entries() []string {
	if a == nil {
		return []string{}
	}
	out := make([]string, 0, len(a.rules))
	for _, r := range a.rules {
		out = append(out, r.String())
	}
	return out
}

// Len is the number of distinct rules.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.rules)
}

// Target is a destination a request names, normalised for matching.
type Target struct {
	// Host is the lower-case name without a trailing dot, or the canonical
	// text of an IP literal.
	Host string
	// IP is set when the request named an IP literal.
	IP   net.IP
	Port int
}

// ParseTarget reads the host and port a request names. It refuses a name
// outside the grammar's character set, so a host that matches no rule fails
// here or in Allows, never by a parser differential.
func ParseTarget(host string, port int) (Target, error) {
	if port < 1 || port > 65535 {
		return Target{}, errors.New("the port must be a number from 1 to 65535")
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if ip := net.ParseIP(h); ip != nil {
		ip = addrcheck.Canonical(ip)
		return Target{Host: ip.String(), IP: ip, Port: port}, nil
	}
	if strings.Contains(h, "%") {
		return Target{}, errors.New("a zoned address is not a destination")
	}
	h = strings.TrimSuffix(h, ".")
	if err := checkName(h); err != nil {
		return Target{}, err
	}
	return Target{Host: h, Port: port}, nil
}

// Allows reports whether a rule admits the target. An IP-literal target
// matches only an IP-literal rule naming the same address; a wildcard never
// matches an IP literal.
func (a *Allowlist) Allows(t Target) bool { return a.allows(t, true) }

// AllowsHost reports whether a rule admits the target's host on any port. A
// proxy uses it only to tell "port not allowed" from "host not allowed" in a
// refusal; it never grants anything.
func (a *Allowlist) AllowsHost(t Target) bool { return a.allows(t, false) }

func (a *Allowlist) allows(t Target, checkPort bool) bool {
	if a == nil {
		return false
	}
	for _, r := range a.rules {
		if checkPort && !r.portMatches(t.Port) {
			continue
		}
		switch {
		case t.IP != nil:
			if r.ip != nil && r.ip.Equal(t.IP) {
				return true
			}
		case r.ip != nil:
		case r.wildcard:
			if len(t.Host) > len(r.host) && strings.HasSuffix(t.Host, r.host) {
				return true
			}
		default:
			if t.Host == r.host {
				return true
			}
		}
	}
	return false
}
