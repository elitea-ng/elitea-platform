// Package mcp is the governed adapter of remote MCP servers (ADR-0039,
// issue #1123). A remote MCP toolkit's credential is oauth_delegated: the
// user consents to the MCP server's own authorization server (the MCP
// authorization specification), and the credential broker releases the
// access token for one bound tool call. The target is the server's
// canonical resource URI (RFC 8707), the same string mcpoauth binds its
// tokens to, so a token issued for one server never serves another.
//
// The descriptor is registered at the composition root beside the
// built-in ones (cmd/elitea-main/governed_mcp.go). It is not in the shared
// Go/Rust catalogue: the worker has no governed MCP adapter yet, so a
// governed run reports a remote MCP toolkit as not governable.
package mcp

import (
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
)

// ProviderType is the adapter id stored on credential references.
const ProviderType = "mcp"

// OperationToolCall is the one canonical operation: a tools/call whose
// resource ref is the tool name. A tool may change anything, so it is a
// write.
const OperationToolCall credentialgrant.OperationID = "mcp.tool.call"

// maxTargetBytes is the target binding column's bound.
const maxTargetBytes = 512

// CanonicalResource is the canonical resource URI of a remote MCP server
// endpoint: mcpoauth's serialization (https only, lowercase host, default
// port dropped, "/" for an empty path, trailing slashes kept), bounded to
// what a target binding stores.
func CanonicalResource(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxTargetBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", credentialadapters.ErrInvalidTarget, maxTargetBytes)
	}
	canonical, err := mcpoauth.CanonicalResource(ProviderType, raw)
	if err != nil || len(canonical) > maxTargetBytes {
		return "", fmt.Errorf("%w: not an https MCP resource URI", credentialadapters.ErrInvalidTarget)
	}
	return canonical, nil
}

// Descriptor is the remote MCP server adapter. The class is fixture
// qualified: it is proven against an in-process MCP server and
// authorization server only (gate D6).
func Descriptor() credentialadapters.AdapterDescriptor {
	return credentialadapters.AdapterDescriptor{
		ProviderType:       ProviderType,
		AuthClasses:        []credentialgrant.CredentialClass{credentialgrant.ClassOAuthDelegated},
		TargetKind:         credentialadapters.TargetResourceURI,
		CanonicalizeTarget: CanonicalResource,
		Operations: []credentialadapters.OperationSpec{
			{ID: OperationToolCall, Class: credentialadapters.OperationWrite, ResourceKind: "tool", CostHint: 1},
		},
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			credentialgrant.ClassOAuthDelegated: {"access_token"},
		},
		// The user consents to the MCP server's authorization server; no
		// Entra exchange is involved.
		OAuthAuthority: credentialgrant.OAuthAuthorityProvider,
		Qualification: map[credentialgrant.CredentialClass]string{
			credentialgrant.ClassOAuthDelegated: credentialadapters.QualificationFixture,
		},
		RateLimitHint: "per MCP server; the server's authorization server meters token requests",
	}
}
