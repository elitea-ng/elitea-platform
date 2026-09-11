package mcpregistry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthorizationRequiredResourceMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"bearer", []string{`Bearer resource_metadata="https://example.test/resource"`}, "https://example.test/resource"},
		{"quoted comma", []string{`Basic realm="private", Bearer realm="tools, public", resource_metadata="https://example.test/resource"`}, "https://example.test/resource"},
		{"case insensitive", []string{`bearer RESOURCE_METADATA="https://example.test/resource"`}, "https://example.test/resource"},
		{"not bearer", []string{`Basic resource_metadata="https://example.test/resource"`}, ""},
		{"duplicates", []string{`Bearer resource_metadata="https://one.test", resource_metadata="https://two.test"`}, ""},
		{"multiple challenges", []string{`Bearer resource_metadata="https://one.test"`, `Bearer resource_metadata="https://two.test"`}, ""},
		{"invalid quoting", []string{`Bearer resource_metadata="https://example.test/resource`}, ""},
		{"oversized", []string{`Bearer realm="` + strings.Repeat("x", 8192) + `", resource_metadata="https://example.test"`}, ""},
		{"too many", make([]string, 17), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizationRequired(tc.headers)
			require.Equal(t, tc.want, err.ResourceMetadataURL())
			require.Equal(t, "MCP authorization required", err.Error())
		})
	}
}
