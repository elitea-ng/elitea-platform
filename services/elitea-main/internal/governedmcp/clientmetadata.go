package governedmcp

import (
	"encoding/json"
	"net/http"
)

// ClientMetadataPath is where Main serves its Client ID Metadata Document.
// The document's URL is the client_id a CIMD-capable authorization server
// sees, so it is public: a server fetches it with no credential.
const ClientMetadataPath = "/.well-known/elitea-mcp-client.json"

// ServeClientMetadata serves the Client ID Metadata Document: a public
// client (no secret) for the authorization code grant with the exact
// redirect URI. It holds no tenant data.
func (s *Service) ServeClientMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  s.cfg.ClientMetadataURL,
		"client_name":                s.cfg.ClientName,
		"redirect_uris":              []string{s.cfg.RedirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}
