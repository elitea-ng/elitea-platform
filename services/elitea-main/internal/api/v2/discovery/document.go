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
const ClientContract = "1.3"

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
	// Attachments states the chat attachment limits (client contract 1.1),
	// so a client can refuse a file before it uploads it. The server still
	// enforces every one of them.
	Attachments AttachmentPolicy `json:"attachments"`
}

// AttachmentPolicy is the deployment's chat attachment limits (client
// contract 1.1). Every byte value is in bytes. A project's storage policy may
// set a different per-file cap; the upload then answers 400 naming it.
type AttachmentPolicy struct {
	// MaxFiles is the number of files one message carries in the composer.
	MaxFiles int `json:"max_files"`
	// MaxTotalBytes bounds one upload (all chunks of one file).
	MaxTotalBytes int64 `json:"max_total_bytes"`
	// MaxFileBytes bounds one non-image file; MaxImageBytes one image.
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxImageBytes int64 `json:"max_image_bytes"`
	// ChunkBytes is the largest single upload request; a larger file is sent
	// in chunks of at most this size.
	ChunkBytes int64 `json:"chunk_bytes"`
	// AcceptedExtensions are lower-case, leading-dot extensions the model
	// can read.
	AcceptedExtensions []string `json:"accepted_extensions"`
	// MaxExtractBytes is the largest document whose text is extracted for
	// the model; a larger one is stored but not read.
	MaxExtractBytes int64 `json:"max_extract_bytes"`
	// InlineImageMaxBytes is the largest image handed to the model as an
	// image, in InlineImageFormats (lower-case extensions).
	InlineImageMaxBytes int64    `json:"inline_image_max_bytes"`
	InlineImageFormats  []string `json:"inline_image_formats"`
	// InlineImageDownscale: a larger image in one of InlineImageFormats is
	// downscaled by the server to fit, so a client need not.
	InlineImageDownscale bool `json:"inline_image_downscale"`
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

	// The data controls (client contract 1.3), the same values as the token
	// response's client_policy. They are public so that a part of the app
	// that runs without a token (a share extension, a widget) obeys them.
	AllowShareOut            bool   `json:"allow_share_out"`
	AllowShareIn             bool   `json:"allow_share_in"`
	AllowCloudSTT            bool   `json:"allow_cloud_stt"`
	NotificationPreview      string `json:"notification_preview"`
	AllowNotificationActions bool   `json:"allow_notification_actions"`
	AllowSystemSurfaces      bool   `json:"allow_system_surfaces"`
}

// DefaultPublicPolicy is the subset served without a policy source: no device
// lock, offline allowed, no minimum, and the data-control defaults of
// platformconfig.DefaultNativeClientPolicy (TestDefaultPublicPolicyMatchesThePlatformDefault
// in internal/api ties the two together).
func DefaultPublicPolicy() PublicPolicy {
	return PublicPolicy{
		RequireDeviceLock: false, OfflineEnabled: true, MinClientVersion: "",
		AllowShareOut: true, AllowShareIn: true, AllowCloudSTT: false,
		NotificationPreview: "none", AllowNotificationActions: true, AllowSystemSurfaces: false,
	}
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
