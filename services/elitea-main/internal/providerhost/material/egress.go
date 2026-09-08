package material

// The facade's half of the git-host egress control (ADR-0022 decision 6).
//
// TWO CHECKS, TWO DIFFERENT JOBS. The provider service enforces the same
// allowlist before its engine builds a clone URL — that one governs the
// socket. This one governs the VAULT: it runs before the decrypt, so a
// repository host nobody allowed never causes a credential to be read out of
// storage at all. Neither makes the other redundant: the provider's check
// happens on a request that already carries the plaintext, and by then the
// decrypt has happened.
//
// The matching rules are copied deliberately, not shared. The two live in
// different languages and different processes, and an operator sets ONE
// allowlist per app for both — so a host the facade admits and the provider
// refuses would be an invocation that starts and then fails, which is worse
// than either answer alone.
//
// ONE VARIABLE PER APP, and the policy carries its own name so the refusal
// tells an operator which one to set. DeepWiki reads
// ELITEA_DEEPWIKI_GIT_ALLOWLIST, Inventory ELITEA_INVENTORY_GIT_ALLOWLIST;
// sharing one variable across apps would make it impossible to let one clone
// from a host the other may not.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrEgressRefused reports a repository host outside the allowlist. It is
// returned BEFORE the vault is touched.
var ErrEgressRefused = errors.New("provider repository host is not allowed")

// GitEgressPolicy is an immutable allowlist decision.
//
// The zero value refuses everything, which is the fail-closed default and the
// same choice the provider makes: an unset allowlist that means "allow
// anything" is a control that silently does nothing while looking present.
type GitEgressPolicy struct {
	entries []string
	env     string
}

// ParseGitEgress reads a comma- or whitespace-separated host list.
//
// Entries are hostnames, compared case-insensitively, with an optional leading
// `*.` for DIRECT subdomains only:
//
//	github.com     matches github.com and nothing else
//	*.github.com   matches api.github.com; NOT github.com, NOT a.b.github.com
//
// A bare `*` disables the control. It exists so a deployment that genuinely
// wants no restriction has to say so out loud, in configuration that shows up
// in review, rather than getting it by leaving a variable unset.
//
// env is the variable the value came from, quoted back in a refusal.
func ParseGitEgress(raw string, env string) GitEgressPolicy {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	entries := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.ToLower(strings.TrimSpace(field)); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return GitEgressPolicy{entries: entries, env: env}
}

// IsEmpty reports an unconfigured policy, which refuses everything.
func (p GitEgressPolicy) IsEmpty() bool { return len(p.entries) == 0 }

// Allow refuses unless host is on the allowlist.
func (p GitEgressPolicy) Allow(host string) error {
	if p.permits(host) {
		return nil
	}
	if p.IsEmpty() {
		return fmt.Errorf(
			"%w: no git-host allowlist is configured, so %q is refused. Set %s to the "+
				"hosts this deployment may clone from (or '*' to disable the control explicitly)",
			ErrEgressRefused, host, p.env)
	}
	return fmt.Errorf("%w: %q is not on %s", ErrEgressRefused, host, p.env)
}

func (p GitEgressPolicy) permits(host string) bool {
	for _, entry := range p.entries {
		if entry == "*" {
			return true
		}
	}
	if host == "" || p.IsEmpty() {
		return false
	}

	candidate := stripPort(strings.ToLower(strings.TrimSpace(host)))
	for _, entry := range p.entries {
		if strings.HasPrefix(entry, "*.") {
			suffix := entry[1:] // ".github.com"
			if !strings.HasSuffix(candidate, suffix) {
				continue
			}
			// DIRECT subdomains only: what remains in front of the suffix must
			// be one label. `*.github.com` covering `a.b.github.com` would let
			// an allowlist for one vendor's API reach anything that vendor
			// hosts, which is not what an operator writing it means.
			if !strings.Contains(strings.TrimSuffix(candidate, suffix), ".") {
				return true
			}
			continue
		}
		if candidate == entry {
			return true
		}
	}
	return false
}

// stripPort removes a trailing :port. The control is about WHERE a request
// goes, and :443 versus :8443 on one host is one host.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") { // bracketed IPv6 literal
		if end := strings.Index(host, "]"); end > 0 {
			return host[1:end]
		}
		return host
	}
	if strings.Count(host, ":") == 1 {
		return host[:strings.Index(host, ":")]
	}
	return host
}

// HostFrom reads the host a clone would reach out of an UNEXPANDED
// configuration value, or refuses.
//
// It is what makes "the allowlist is checked before any decrypt" true: the
// value is read with its secrets still sealed, and a URL assembled FROM a
// secret is refused outright rather than checked against a string with
// braces in it — such a configuration has no safe order at all. `field`
// names the key in the refusal so an operator can find it.
//
// It was DeepWiki's `providerHost` and Inventory's `hostOf`; the two
// differed only in whether an unusable host was an error or an empty string
// that every allowlist then refused. This is the erroring one, and
// Inventory's caller keeps its own behaviour by ignoring the error.
func HostFrom(raw, fallback, field string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: no %s to check against the allowlist", ErrEgressRefused, field)
	}
	if strings.Contains(raw, "{{secret.") {
		return "", fmt.Errorf(
			"%w: %s is built from a secret, so it cannot be checked before decrypting it",
			ErrEgressRefused, field)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("%w: %s %q is not a URL", ErrEgressRefused, field, raw)
	}
	return strings.ToLower(parsed.Hostname()), nil
}
