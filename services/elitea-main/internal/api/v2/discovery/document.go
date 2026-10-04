// Package discovery serves the public discovery document,
// GET /.well-known/elitea-client (ADR-0025 decision 1): what a client given
// only a deployment's origin needs before it signs in — the server and client
// contract versions, who operates the deployment, its brand, its native
// sign-in endpoints (or null) and the public part of its client policy.
//
// It is anonymous, so it discloses nothing a login page does not: the server
// version is published as major.minor only (the full build string stays
// admin-only, coordinator decision 3), and the policy is the public subset.
package discovery

import (
	"strconv"
	"strings"
)

// Path is where the document is served. It is root-mounted, outside /api/v2,
// because RFC 8615 reserves /.well-known/ for exactly this kind of document.
const Path = "/.well-known/elitea-client"

// ClientContract is the client contract version this server speaks
// (ADR-0025 decision 6). WP5 adds the contract lock that pins it.
const ClientContract = "1.0"

// Deployment kinds (ADR-0025 decision 1). The value is process configuration
// (ELITEA_DEPLOYMENT_KIND), not an admin setting: it states who operates the
// deployment, which nobody inside the product can change.
const (
	DeploymentKindSaaS       = "saas"
	DeploymentKindSelfHosted = "self_hosted"
)

// Document is the discovery document. Field order is the JSON key order and
// the set is a contract (document_test.go pins it): adding a key is additive,
// renaming or removing one is a client_contract major bump.
type Document struct {
	ServerVersion  string `json:"server_version"`
	ClientContract string `json:"client_contract"`
	DeploymentKind string `json:"deployment_kind"`
	DisplayName    string `json:"display_name"`
	// BrandPackURL is absolute and content-addressed (…/pack.json?v=<etag>).
	BrandPackURL string `json:"brand_pack_url"`
	// NativeAuth is null while no native client is registered.
	NativeAuth   *NativeAuth  `json:"native_auth"`
	ClientPolicy PublicPolicy `json:"client_policy"`
	// MinClientVersion maps a registered, enabled client id to the lowest
	// client version this deployment serves it (the higher of the policy's and
	// the client's own minimum; absent when neither sets one). Always an
	// object, never null.
	MinClientVersion map[string]string `json:"min_client_version"`
}

// NativeAuth lists the native authorization server's endpoints (ADR-0025
// decision 3). All URLs are absolute; Issuer is the deployment origin a
// client compares an authorization response's `iss` against (RFC 9207).
type NativeAuth struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RevocationEndpoint            string   `json:"revocation_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// PublicPolicy is the public subset of the native client policy (ADR-0025
// decision 5). The full policy travels with every token response; this
// subset is what a client may act on before sign-in.
type PublicPolicy struct {
	// RequireDeviceLock: the client must refuse to run without a device
	// passcode or biometric lock.
	RequireDeviceLock bool `json:"require_device_lock"`
	// OfflineEnabled: offline storage is allowed (offline_retention_days > 0).
	OfflineEnabled bool `json:"offline_enabled"`
	// MinClientVersion is the deployment-wide minimum client version; empty
	// means none. A client's entry in Document.MinClientVersion, when present,
	// is its effective minimum and is never lower than this.
	MinClientVersion string `json:"min_client_version"`
}

// DefaultPublicPolicy is the subset served until the native_client_policy
// section exists (WP4): no device lock, offline allowed, no minimum.
func DefaultPublicPolicy() PublicPolicy {
	return PublicPolicy{RequireDeviceLock: false, OfflineEnabled: true, MinClientVersion: ""}
}

// MajorMinor reduces a build version ("v1.62.3", "1.62.0-rc.1") to
// "major.minor". A version that does not start with two numeric components
// ("dev", a local build) is published as "dev": the honest answer, and no
// more disclosing than the literal.
func MajorMinor(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "dev"
	}
	major, minor := parts[0], parts[1]
	if i := strings.IndexAny(minor, "-+"); i >= 0 {
		minor = minor[:i]
	}
	if !isNumeric(major) || !isNumeric(minor) {
		return "dev"
	}
	return major + "." + minor
}

func isNumeric(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil && !strings.HasPrefix(s, "+") && !strings.HasPrefix(s, "-")
}
