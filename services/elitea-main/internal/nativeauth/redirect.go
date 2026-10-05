package nativeauth

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Redirect URI rules (RFC 8252 §7), applied by both registration layers, the
// admin PUT and boot.
//
//  1. Private-use scheme: `scheme:/path`, the scheme in reverse-domain form
//     with at least two dot-separated labels (`ca.example.app:/oauth/callback`),
//     lower case, no authority (`//`), no query, no fragment. A scheme with no
//     dot (`elitea:`) is refused, as the ADR requires.
//  2. Loopback: `http://127.0.0.1/path` or `http://[::1]/path`, registered
//     WITHOUT a port; the port is free at authorize time (RFC 8252 §7.3).
//     `localhost`, https and any other host are refused (§8.3).
//
// The path is restricted to unreserved characters and `/`: no percent-encoding,
// no IDN, nothing whose byte form and decoded form can differ, so "matches
// exactly" means one thing.

// MaxRedirectURIBytes bounds one redirect URI.
const MaxRedirectURIBytes = 512

// MaxRedirectURIs bounds the list on one client.
const MaxRedirectURIs = 5

var (
	privateUseSchemePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9-]+)+$`)
	redirectPathPattern     = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)
	deniedSchemes           = map[string]bool{
		"http": true, "https": true, "javascript": true, "data": true, "file": true, "blob": true,
		"about": true, "ftp": true, "ws": true, "wss": true, "mailto": true, "intent": true,
		"content": true, "vbscript": true,
	}
)

// RedirectKind classifies a registered redirect URI.
type RedirectKind int

const (
	RedirectPrivateUse RedirectKind = iota + 1
	RedirectLoopback
)

// ErrRedirectURI is wrapped by every refusal of ValidateRedirectURI. The
// message after the colon is the reason the admin API shows per URI.
var ErrRedirectURI = errors.New("invalid redirect URI")

func redirectError(reason string) error {
	return &redirectURIError{reason: reason}
}

type redirectURIError struct{ reason string }

func (e *redirectURIError) Error() string { return ErrRedirectURI.Error() + ": " + e.reason }
func (e *redirectURIError) Unwrap() error { return ErrRedirectURI }

// RedirectReason extracts the human reason from a ValidateRedirectURI error.
func RedirectReason(err error) string {
	var typed *redirectURIError
	if errors.As(err, &typed) {
		return typed.reason
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// ValidateRedirectURI checks a URI an administrator registers and reports its
// kind.
func ValidateRedirectURI(raw string) (RedirectKind, error) {
	if raw == "" {
		return 0, redirectError("the redirect URI is empty")
	}
	if len(raw) > MaxRedirectURIBytes {
		return 0, redirectError("the redirect URI is longer than 512 bytes")
	}
	for _, r := range raw {
		if r <= 0x20 || r >= 0x7f {
			return 0, redirectError("the redirect URI may contain only printable ASCII without spaces")
		}
	}
	if strings.ContainsAny(raw, "?#%\\") {
		return 0, redirectError("the redirect URI may not carry a query, a fragment, a backslash or percent-encoding")
	}
	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok || scheme == "" {
		return 0, redirectError("the redirect URI has no scheme")
	}
	if scheme == "http" {
		return validateLoopback(raw)
	}
	if strings.ToLower(scheme) != scheme {
		return 0, redirectError("the scheme must be lower case")
	}
	if deniedSchemes[scheme] {
		return 0, redirectError("the scheme " + scheme + " is not allowed for a native client")
	}
	if !privateUseSchemePattern.MatchString(scheme) {
		return 0, redirectError("a private-use scheme must be in reverse-domain form with at least one dot, " +
			"for example com.example.app")
	}
	if strings.HasPrefix(rest, "//") {
		return 0, redirectError("a private-use redirect URI has no authority: use scheme:/path")
	}
	if !redirectPathPattern.MatchString(rest) {
		return 0, redirectError("the path must start with / and use only letters, digits and . _ ~ - /")
	}
	return RedirectPrivateUse, nil
}

func validateLoopback(raw string) (RedirectKind, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Opaque != "" || parsed.User != nil {
		return 0, redirectError("a loopback redirect URI is http://127.0.0.1/path or http://[::1]/path")
	}
	if parsed.Port() != "" || strings.HasSuffix(parsed.Host, ":") {
		return 0, redirectError("register a loopback redirect URI without a port; any port is accepted at sign-in")
	}
	if parsed.Host != "127.0.0.1" && parsed.Host != "[::1]" {
		return 0, redirectError("only the loopback addresses 127.0.0.1 and [::1] are allowed for http; " +
			"localhost and other hosts are refused")
	}
	if !redirectPathPattern.MatchString(parsed.EscapedPath()) || parsed.EscapedPath() != parsed.Path {
		return 0, redirectError("the path must start with / and use only letters, digits and . _ ~ - /")
	}
	return RedirectLoopback, nil
}

// RedirectMatches reports whether a redirect_uri presented at /authorize or
// /token matches one registered URI: byte for byte for a private-use URI; on
// scheme, host and path with any port (or none) for a loopback URI.
func RedirectMatches(registered, presented string) bool {
	kind, err := ValidateRedirectURI(registered)
	if err != nil {
		return false
	}
	if kind == RedirectPrivateUse {
		return registered == presented
	}
	if len(presented) > MaxRedirectURIBytes || strings.ContainsAny(presented, "?#%\\ ") {
		return false
	}
	parsed, err := url.Parse(presented)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	if port := parsed.Port(); port != "" {
		number, convErr := strconv.Atoi(port)
		if convErr != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
			return false
		}
	} else if strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://"+host+parsed.EscapedPath() == registered
}

// MatchingRedirect returns the registered URI a presented one matches.
func MatchingRedirect(registered []string, presented string) (string, bool) {
	for _, candidate := range registered {
		if RedirectMatches(candidate, presented) {
			return candidate, true
		}
	}
	return "", false
}

// FormActionSource is the CSP `form-action` source that admits a redirect to
// this URI: `scheme:` for a private-use URI, both loopback hosts on any port
// for a loopback URI. The consent POST answers with a 302 to the app, and
// browsers enforce form-action on the redirect.
func FormActionSource(redirectURI string) string {
	kind, err := ValidateRedirectURI(redirectURIWithoutPort(redirectURI))
	if err != nil {
		return ""
	}
	if kind == RedirectLoopback {
		return "http://127.0.0.1:* http://[::1]:*"
	}
	scheme, _, _ := strings.Cut(redirectURI, ":")
	return scheme + ":"
}

func redirectURIWithoutPort(uri string) string {
	if !strings.HasPrefix(uri, "http://") {
		return uri
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + parsed.EscapedPath()
}
