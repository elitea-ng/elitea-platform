// Package mcp names the governed remote MCP server adapter (ADR-0039,
// issue #1123) for the packages that bind MCP tokens: governedmcp, the
// broker's tests and the composition root. The descriptor itself is
// credentialadapters.MCP, one of the built-in providers in the shared Go/Rust
// catalogue; this package only re-exports its names.
//
// A remote MCP toolkit's credential is oauth_delegated: the user consents to
// the MCP server's own authorization server, and the credential broker
// releases the access token for one bound operation. The target is the
// server's canonical resource URI (RFC 8707), the same string mcpoauth binds
// its tokens to, so a token issued for one server never serves another.
package mcp

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
)

// ProviderType is the adapter id stored on credential references.
const ProviderType = credentialadapters.MCPProviderType

// OperationToolsList lists a server's tools, once per governed toolkit
// build, with an empty resource.
const OperationToolsList = credentialadapters.MCPOperationToolsList

// OperationToolCall is a tools/call whose resource ref is the tool's name.
const OperationToolCall = credentialadapters.MCPOperationToolCall

// CanonicalResource is credentialadapters.CanonicalResourceURI.
func CanonicalResource(raw string) (string, error) {
	return credentialadapters.CanonicalResourceURI(raw)
}

// Descriptor is credentialadapters.MCP.
func Descriptor() credentialadapters.AdapterDescriptor { return credentialadapters.MCP() }
