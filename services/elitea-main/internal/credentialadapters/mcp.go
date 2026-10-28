package credentialadapters

import (
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
)

// MCPProviderType is the remote MCP server adapter id (ADR-0039, #1123).
const MCPProviderType = "mcp"

// The two MCP operations. A governed run lists a server's tools once, when
// the toolkit is built, with an empty resource; each tools/call then binds
// the tool's name ("mcp:tool/<name>"). A tool may change anything, so a call
// is a write.
const (
	MCPOperationToolsList credentialgrant.OperationID = "mcp.tools.list"
	MCPOperationToolCall  credentialgrant.OperationID = "mcp.tool.call"
)

// maxResourceURIBytes is the target binding column's bound.
const maxResourceURIBytes = 512

// CanonicalResourceURI is the canonical resource URI of a remote MCP server
// endpoint: mcpoauth's serialization (https only, lowercase host, default
// port dropped, "/" for an empty path, trailing slashes kept, any `%`
// refused), bounded to what a target binding stores. It is NOT
// CanonicalBaseURL: the worker applies the same rule to a `resource_uri`
// target (shared vectors in targets.json, resource_uri_vectors).
func CanonicalResourceURI(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxResourceURIBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidTarget, maxResourceURIBytes)
	}
	canonical, err := mcpoauth.CanonicalResource(MCPProviderType, raw)
	if err != nil || len(canonical) > maxResourceURIBytes {
		return "", fmt.Errorf("%w: not an https MCP resource URI", ErrInvalidTarget)
	}
	return canonical, nil
}

// MCP is the remote MCP server adapter. Its credential is oauth_delegated:
// the user consents to the server's own authorization server (the MCP
// authorization specification) and the broker releases the access token
// for one bound operation. The class is fixture qualified (gate D6), and
// no probe runs: a listing needs the user's consent.
func MCP() AdapterDescriptor {
	return AdapterDescriptor{
		ProviderType:       MCPProviderType,
		AuthClasses:        []credentialgrant.CredentialClass{oauthDelegated},
		TargetKind:         TargetResourceURI,
		CanonicalizeTarget: CanonicalResourceURI,
		Operations: []OperationSpec{
			op(string(MCPOperationToolsList), OperationRead, "server", 1),
			op(string(MCPOperationToolCall), OperationWrite, "tool", 1),
		},
		MaterialKeys: map[credentialgrant.CredentialClass][]string{
			oauthDelegated: {"access_token"},
		},
		// No Entra exchange is involved.
		OAuthAuthority: credentialgrant.OAuthAuthorityProvider,
		Qualification: map[credentialgrant.CredentialClass]string{
			oauthDelegated: QualificationFixture,
		},
		RateLimitHint: "per MCP server; the server's authorization server meters token requests",
	}
}
