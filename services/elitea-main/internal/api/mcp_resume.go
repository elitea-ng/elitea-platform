package api

import (
	"os"

	v2mcp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// Deployments without a valid master key retain the JSON-only MCP transport.
// No process-local key can provide response recovery after Main replacement.
func mcpResponseCursorCodec() *v2mcp.ResumeCursorCodec {
	key, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil {
		return nil
	}
	defer clear(key)
	codec, err := v2mcp.NewResumeCursorCodec(key)
	if err != nil {
		return nil
	}
	return codec
}
