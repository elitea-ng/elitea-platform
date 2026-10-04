package discovery

import "context"

// The seams later work packages fill in. WP1 compiles and serves a complete
// document without them: a nil source means `native_auth: null`, the default
// public policy and an empty min_client_version map.

// NativeAuthSource reports the native authorization endpoints, or nil while
// no native client is registered (ADR-0025 WP2: nativeauth.Registry). The
// endpoints must be absolute and built from the same public origin
// (internal/publicorigin) the authorization server stamps into `iss`.
type NativeAuthSource interface {
	NativeAuth(ctx context.Context, origin string) (*NativeAuth, error)
}

// ClientPolicySource reports the public policy subset and the per-client
// minimum versions (ADR-0025 WP4: internal/api/native_policy.go adapts the
// one nativepolicy.Service). The map may be nil.
type ClientPolicySource interface {
	PublicPolicy(ctx context.Context) (PublicPolicy, map[string]string, error)
}
