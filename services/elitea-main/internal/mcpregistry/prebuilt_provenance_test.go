package mcpregistry

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveReportSaysWhatTheCatalogueFilled(t *testing.T) {
	entry := &PrebuiltServer{Enabled: true, ClientID: "cat-client", ClientSecretRef: "ref"}
	lookup := func(string) (string, error) { return "cat-secret", nil }

	// Blank and whitespace-only values are the catalogue's to fill.
	for _, blank := range []any{nil, "", "   ", "\t\n"} {
		resolved, filled := ResolveReport(map[string]any{"client_secret": blank}, "mcp_x", entry, lookup)
		require.True(t, filled.ClientSecret, "%#v", blank)
		require.Equal(t, "cat-secret", resolved["client_secret"])
	}

	// A value the toolkit holds is its own: nothing is reported as filled.
	resolved, filled := ResolveReport(map[string]any{"client_secret": "mine", "client_id": "me"}, "mcp_x", entry, lookup)
	require.False(t, filled.ClientSecret)
	require.False(t, filled.ClientID)
	require.Equal(t, "mine", resolved["client_secret"])

	// A vault miss fills nothing, and says so.
	_, filled = ResolveReport(map[string]any{}, "mcp_x", entry,
		func(string) (string, error) { return "", ErrNoPool })
	require.False(t, filled.ClientSecret)

	// Disabled entries and non-prebuilt types fill nothing.
	_, filled = ResolveReport(map[string]any{}, "mcp_x", &PrebuiltServer{ClientSecretRef: "ref"}, lookup)
	require.False(t, filled.ClientSecret)
	_, filled = ResolveReport(map[string]any{}, "openapi", entry, lookup)
	require.False(t, filled.ClientSecret)
}

func TestPinnedTokenURLMustBeAnExactHTTPSAddress(t *testing.T) {
	for _, ok := range []string{"", "https://idp.example/oauth/token"} {
		require.NoError(t, ValidatePrebuiltTokenURL(ok), ok)
	}
	for _, bad := range []string{
		"http://idp.example/token", "https://{tenant}.example/token", "https://user:pw@idp.example/token",
		"https://idp.example/token#frag", "/relative", "https://idp.example/to ken",
	} {
		require.Error(t, ValidatePrebuiltTokenURL(bad), bad)
	}
}

type fakeRows struct {
	rows    []func(dest ...any) error
	current int
	err     error
}

func (f *fakeRows) Next() bool { f.current++; return f.current <= len(f.rows) }
func (f *fakeRows) Err() error { return f.err }
func (f *fakeRows) Scan(dest ...any) error {
	return f.rows[f.current-1](dest...)
}

func goodRow(key, serverURL string) func(dest ...any) error {
	return func(dest ...any) error {
		*(dest[0].(*string)) = key
		*(dest[1].(*string)) = key
		*(dest[2].(*string)) = serverURL
		*(dest[9].(*bool)) = true
		return nil
	}
}

func TestOneBadCatalogueRowDoesNotTakeDownTheRest(t *testing.T) {
	badSchema := func(dest ...any) error {
		*(dest[0].(*string)) = "broken"
		*(dest[8].(*[]byte)) = []byte("{not json")
		return nil
	}
	badPin := func(dest ...any) error {
		*(dest[0].(*string)) = "badpin"
		*(dest[10].(*string)) = "https://{x}.example/token"
		return nil
	}
	rows := &fakeRows{rows: []func(dest ...any) error{
		goodRow("first", "https://a.example/mcp"), badSchema, badPin, goodRow("last", "https://b.example/mcp"),
	}}
	entries, err := readPrebuiltRows(rows)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "first", entries[0].Key)
	require.Equal(t, "last", entries[1].Key)

	// A failure of the result set itself is still an error.
	_, err = readPrebuiltRows(&fakeRows{err: ErrNoPool})
	require.ErrorIs(t, err, ErrNoPool)
}

func TestTokenURLUsesTheSharedEndpointRules(t *testing.T) {
	require.NoError(t, ValidatePrebuiltTokenURL("http://localhost:9000/token"), "loopback http is allowed")
	require.NoError(t, ValidatePrebuiltTokenURL("http://127.0.0.1/token"))
	for _, bad := range []string{"https://idp.example/a/../token", "https://idp.example/./token", "http://idp.example/token"} {
		require.ErrorIs(t, ValidatePrebuiltTokenURL(bad), ErrInvalidPrebuiltTokenURL, bad)
	}
}

func TestTokenURLRequiredMarksTemplatedSecretsWithoutAPin(t *testing.T) {
	require.True(t, PrebuiltServer{ClientSecretRef: "r", ServerURL: "https://{w}.example/mcp"}.TokenURLRequired())
	require.False(t, PrebuiltServer{ClientSecretRef: "r", ServerURL: "https://{w}.example/mcp", TokenURL: "https://i.example/t"}.TokenURLRequired())
	require.False(t, PrebuiltServer{ServerURL: "https://{w}.example/mcp"}.TokenURLRequired(), "no secret, nothing to release")
	require.False(t, PrebuiltServer{ClientSecretRef: "r", ServerURL: "https://a.example/mcp"}.TokenURLRequired())
}
