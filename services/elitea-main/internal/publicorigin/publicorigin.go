// Package publicorigin answers "what is this deployment's public origin" for
// the anonymous documents that must carry absolute URLs (ADR-0025): the
// discovery document at /.well-known/elitea-client, the brand pack as JSON,
// and — from WP2 on — the native authorization server's `iss`.
//
// There is ONE source of truth: DEPLOYMENT_URL, which the chart already
// declares as "this deployment's public base URL" (the configurations
// self-reference guard and the mailer read it too). Normalize is the single
// function every consumer calls, so the discovery document's origin and the
// `iss` a native client verifies can never disagree (ADR-0025 decision 9).
//
// When DEPLOYMENT_URL is unset the anonymous documents fall back to the
// request's own Host (never X-Forwarded-Host: an anonymous, cacheable body
// built from a caller-chosen header is a cache-poisoning vector). Responses
// built that way carry `Vary` (see VaryRequestOrigin) and are never marked
// immutable.
package publicorigin

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// Env is the environment variable that names the public origin.
const Env = "DEPLOYMENT_URL"

// VaryRequestOrigin is the Vary value a response must carry when its body
// was built from FromRequest: the body depends on the Host and on the
// forwarded scheme.
const VaryRequestOrigin = "Host, X-Forwarded-Proto"

// ErrInvalid reports a DEPLOYMENT_URL that is set but cannot be an origin.
var ErrInvalid = errors.New("invalid public origin")

// Normalize turns a DEPLOYMENT_URL value into an origin: a lower-cased
// "scheme://host[:port]" with no path, query or fragment. An empty (or
// all-blank) value returns "" and no error — "not configured".
//
// Refused: a scheme other than http/https, a missing host, userinfo, a query
// or a fragment. A path is NOT refused — existing deployments may have
// written "https://host/" or a base path into DEPLOYMENT_URL for the mailer —
// but it is not part of the origin: every route this service serves is
// root-mounted. The returned dropped flag reports that a path was ignored, so
// the caller can log it.
func Normalize(raw string) (origin string, droppedPath bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, nil
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", false, fmt.Errorf("%w: %v", ErrInvalid, parseErr)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", false, fmt.Errorf("%w: scheme must be http or https, got %q", ErrInvalid, u.Scheme)
	}
	if u.Opaque != "" || u.Host == "" || !httpguts.ValidHostHeader(u.Host) {
		return "", false, fmt.Errorf("%w: %q has no valid host", ErrInvalid, raw)
	}
	if u.User != nil {
		return "", false, fmt.Errorf("%w: %q must not carry userinfo", ErrInvalid, raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", false, fmt.Errorf("%w: %q must not carry a query or fragment", ErrInvalid, raw)
	}
	path := u.EscapedPath()
	return scheme + "://" + strings.ToLower(u.Host), path != "" && path != "/", nil
}

// FromRequest derives an origin from the request itself — the fallback when
// no public origin is configured. The host is r.Host (the request's own
// authority, which is also part of every cache key); the scheme is https when
// the connection is TLS or a single X-Forwarded-Proto says "https", and http
// otherwise. X-Forwarded-Host is never read.
func FromRequest(r *http.Request) string {
	host := strings.ToLower(r.Host)
	if host == "" || !httpguts.ValidHostHeader(host) {
		host = "localhost"
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if values := r.Header.Values("X-Forwarded-Proto"); len(values) == 1 &&
		strings.EqualFold(strings.TrimSpace(values[0]), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

// Resolve returns the configured origin, or the request-derived one when
// none is configured. fromRequest reports which: a caller that got true must
// add `Vary: VaryRequestOrigin` and must not answer with an immutable cache
// policy.
func Resolve(configured string, r *http.Request) (origin string, fromRequest bool) {
	if configured != "" {
		return configured, false
	}
	return FromRequest(r), true
}
