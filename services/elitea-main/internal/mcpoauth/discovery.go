package mcpoauth

// The one walk of an MCP server's PUBLIC authorization metadata: RFC 9728
// protected-resource metadata, then RFC 8414 (or OpenID) authorization
// server metadata. The tool sync and the OAuth proxy of
// internal/api/v2/eliteacore and the governed MCP OAuth of
// internal/governedmcp (#1123) all read through it, so they agree on which
// documents are tried, what must match and what is projected.
//
// A Fetcher sends every request. Which client it uses is the caller's
// choice, and the one decision that matters for SSRF: a caller whose answer
// decides where a code, a secret or a token is sent sets Validate to its
// egress guard's Validate and Do to a client that dials through the same
// guard. Validate runs before every fetch, so a document naming a private
// or loopback host is refused before any connection is made to it.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

var (
	// ErrDiscoveryMetadata is metadata that is missing, malformed, refused by
	// the egress guard or does not name itself.
	ErrDiscoveryMetadata = errors.New("MCP authorization metadata is unavailable")
	// ErrDiscoveryTimeout is a fetch that ran out of time.
	ErrDiscoveryTimeout = errors.New("MCP authorization metadata timed out")
)

const maxDiscoveryBytes = 64 << 10

// Fetcher reads metadata documents.
type Fetcher struct {
	// Do sends one GET. Required.
	Do func(*http.Request) (*http.Response, error)
	// Validate, when set, is checked before every fetch: the egress guard.
	Validate func(ctx context.Context, rawURL string) error
	// PerFetch bounds one fetch; zero leaves the caller's context alone.
	PerFetch time.Duration
	// Flags are boolean authorization server fields projected as well, such
	// as client_id_metadata_document_supported.
	Flags []string
}

// Discovered is a fail-closed walk of protected-resource then
// authorization-server metadata.
type Discovered struct {
	Resource    map[string]any
	MetadataURL string
	Servers     []string
	// Issuers holds the validated metadata of the servers that answered, in
	// order. With all=false it holds only the first server's.
	Issuers []map[string]any
	// TimedOut reports that at least one fetch ran out of time. A server that
	// did not answer is skipped, but a refusal that followed a timeout is not
	// the same thing as one that followed a mismatch.
	TimedOut bool
}

// Discover walks the metadata of endpoint. advertised is the
// resource_metadata URL of a WWW-Authenticate challenge, or empty. Every
// document must validate: a server list that is empty, over maxServers or
// holds an issuer with a query is refused outright, and so is a first
// issuer that does not answer. With all=true an issuer that does not answer
// is skipped, and at least one must. stop, when set, ends the walk once an
// issuer satisfies it.
func (f Fetcher) Discover(
	ctx context.Context, endpoint *url.URL, advertised string, maxServers int, all bool,
	stop func(issuer map[string]any) bool,
) (Discovered, error) {
	resource, metadataURL, err := f.ProtectedResource(ctx, endpoint, advertised)
	if err != nil {
		return Discovered{TimedOut: errors.Is(err, ErrDiscoveryTimeout)}, err
	}
	servers, ok := BoundedMetadataList(resource["authorization_servers"])
	if !ok || len(servers) == 0 || len(servers) > maxServers {
		return Discovered{}, ErrDiscoveryMetadata
	}
	for _, server := range servers {
		issuer, err := ValidateMetadataURL(server)
		if err != nil || issuer.RawQuery != "" {
			return Discovered{}, ErrDiscoveryMetadata
		}
	}
	found := Discovered{Resource: resource, MetadataURL: metadataURL, Servers: servers}
	for _, server := range servers {
		metadata, err := f.AuthorizationServer(ctx, server)
		if err == nil {
			found.Issuers = append(found.Issuers, metadata)
			// The caller has what it came for: later servers, which may be
			// slow or dead, must not spend the budget the exchange needs.
			if stop != nil && stop(metadata) {
				break
			}
		} else if !all {
			return Discovered{TimedOut: errors.Is(err, ErrDiscoveryTimeout)}, err
		} else if errors.Is(err, ErrDiscoveryTimeout) {
			found.TimedOut = true
			if ctx.Err() != nil {
				break
			}
		}
		if !all {
			break
		}
	}
	if len(found.Issuers) == 0 {
		err := ErrDiscoveryMetadata
		if found.TimedOut {
			err = ErrDiscoveryTimeout
		}
		return found, err
	}
	return found, nil
}

// ProtectedResource reads the RFC 9728 document of endpoint: the advertised
// URL, else the path-inserted well-known URI, then the host root one. The
// document must name endpoint.
func (f Fetcher) ProtectedResource(ctx context.Context, endpoint *url.URL, advertised string) (map[string]any, string, error) {
	candidates := []string{advertised}
	if advertised == "" {
		base := endpoint.Scheme + "://" + endpoint.Host
		candidates = []string{base + "/.well-known/oauth-protected-resource" + endpoint.EscapedPath(), base + "/.well-known/oauth-protected-resource"}
	}
	timedOut := false
	for _, candidate := range candidates {
		metadata, err := f.Read(ctx, candidate)
		if errors.Is(err, ErrDiscoveryTimeout) {
			// The path-specific document being slow does not excuse skipping
			// the root one; both are tried while the budget lasts.
			timedOut = true
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if err != nil {
			continue
		}
		resource, ok := metadata["resource"].(string)
		if !ok || !EquivalentMetadataURL(resource, endpoint.String()) {
			return nil, "", ErrDiscoveryMetadata
		}
		return metadata, candidate, nil
	}
	if timedOut {
		return nil, "", ErrDiscoveryTimeout
	}
	return nil, "", ErrDiscoveryMetadata
}

// AuthorizationServer reads the RFC 8414 document of rawIssuer, then its
// OpenID configuration, and projects the endpoints (each validated), the
// bounded lists and the Fetcher's flags. A document naming another issuer
// moves on to the next candidate.
func (f Fetcher) AuthorizationServer(ctx context.Context, rawIssuer string) (map[string]any, error) {
	issuer, err := ValidateMetadataURL(rawIssuer)
	if err != nil {
		return nil, err
	}
	// RFC 8414 inserts the well-known segment between host and path; a trailing
	// slash on the issuer must not leak into the built URL ("…/server/" or "//").
	base := issuer.Scheme + "://" + issuer.Host
	path := strings.TrimRight(issuer.EscapedPath(), "/")
	candidates := []string{base + "/.well-known/oauth-authorization-server" + path, base + path + "/.well-known/openid-configuration"}
	timedOut := false
	for _, candidate := range candidates {
		metadata, err := f.Read(ctx, candidate)
		if errors.Is(err, ErrDiscoveryTimeout) {
			timedOut = true
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if err != nil {
			continue
		}
		// RFC 8414 §3.3 asks for an identical issuer, but providers differ by
		// a trailing slash (Box advertises "https://api.box.com/" and publishes
		// "https://api.box.com"). That is the same URL, so it is accepted. A
		// real mismatch moves on to the next candidate document.
		published, ok := metadata["issuer"].(string)
		if !ok || !EquivalentMetadataURL(published, rawIssuer) {
			continue
		}
		return f.project(rawIssuer, metadata)
	}
	if timedOut {
		return nil, ErrDiscoveryTimeout
	}
	return nil, ErrDiscoveryMetadata
}

func (f Fetcher) project(rawIssuer string, metadata map[string]any) (map[string]any, error) {
	projected := map[string]any{"issuer": rawIssuer}
	for _, key := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint", "revocation_endpoint"} {
		value, exists := metadata[key]
		if !exists && key != "authorization_endpoint" && key != "token_endpoint" {
			continue
		}
		address, ok := value.(string)
		if !ok {
			return nil, ErrDiscoveryMetadata
		}
		if _, err := ValidateMetadataURL(address); err != nil {
			return nil, err
		}
		projected[key] = address
	}
	for _, key := range []string{"scopes_supported", "grant_types_supported", "response_types_supported", "code_challenge_methods_supported", "token_endpoint_auth_methods_supported"} {
		if values, ok := BoundedMetadataList(metadata[key]); ok {
			projected[key] = values
		}
	}
	for _, key := range f.Flags {
		if value, ok := metadata[key].(bool); ok {
			projected[key] = value
		}
	}
	return projected, nil
}

// Read fetches one JSON object of at most 64 KiB with a 200 answer.
func (f Fetcher) Read(ctx context.Context, address string) (map[string]any, error) {
	endpoint, err := ValidateMetadataURL(address)
	if err != nil {
		return nil, err
	}
	if f.PerFetch > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.PerFetch)
		defer cancel()
	}
	if f.Validate != nil {
		if err := f.Validate(ctx, endpoint.String()); err != nil {
			return nil, ErrDiscoveryMetadata
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrDiscoveryMetadata
	}
	request.Header.Set("Accept", "application/json")
	response, err := f.Do(request)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil, ErrDiscoveryTimeout
		}
		return nil, ErrDiscoveryMetadata
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, ErrDiscoveryMetadata
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBytes+1))
	if err != nil || len(body) > maxDiscoveryBytes {
		return nil, ErrDiscoveryMetadata
	}
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil || metadata == nil {
		return nil, ErrDiscoveryMetadata
	}
	return metadata, nil
}

// EquivalentMetadataURL compares two metadata URLs (an issuer or a
// protected resource) as the same resource identifier: scheme, host and port
// must match exactly (case-insensitive, default ports made explicit), the
// query must match, and the paths must match once trailing slashes are
// removed, so an empty path equals "/".
func EquivalentMetadataURL(first, second string) bool {
	if first == second {
		return true
	}
	a, err := url.Parse(first)
	if err != nil {
		return false
	}
	b, err := url.Parse(second)
	if err != nil {
		return false
	}
	if a.Host == "" || b.Host == "" || a.User != nil || b.User != nil || a.Opaque != "" || b.Opaque != "" {
		return false
	}
	return SameOrigin(a, b) &&
		strings.TrimRight(a.EscapedPath(), "/") == strings.TrimRight(b.EscapedPath(), "/") &&
		a.RawQuery == b.RawQuery
}

// SameOrigin compares scheme, host (case and trailing dot ignored) and the
// effective port.
func SameOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(strings.TrimSuffix(first.Hostname(), "."), strings.TrimSuffix(second.Hostname(), ".")) &&
		effectivePort(first) == effectivePort(second)
}

func effectivePort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return "443"
	}
	return "80"
}

// ValidateMetadataURL accepts an MCP endpoint-shaped URL (https, or http to
// loopback) of at most 4 KiB with no fragment.
func ValidateMetadataURL(address string) (*url.URL, error) {
	if len(address) > 4096 || strings.Contains(address, "#") {
		return nil, ErrDiscoveryMetadata
	}
	endpoint, err := mcpregistry.ValidateEndpointURL(address)
	if err != nil {
		return nil, ErrDiscoveryMetadata
	}
	return endpoint, nil
}

// BoundedMetadataList reads a JSON string list of at most 64 non-empty
// entries and 4 KiB in all.
func BoundedMetadataList(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || len(items) > 64 {
		return nil, false
	}
	values := make([]string, 0, len(items))
	total := 0
	for _, item := range items {
		text, ok := item.(string)
		total += len(text)
		if !ok || text == "" || total > 4096 || strings.ContainsAny(text, "\r\n\x00") {
			return nil, false
		}
		values = append(values, text)
	}
	return values, true
}
