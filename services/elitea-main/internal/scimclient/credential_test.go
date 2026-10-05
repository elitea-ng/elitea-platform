package scimclient

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMintedCredentialsHaveTheirPrefixAndLength(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		bytes  int
	}{
		{PrefixBearerSecret, secretBytes},
		{PrefixClientSecret, secretBytes},
		{PrefixAccessToken, secretBytes},
		{PrefixClientID, clientIDBytes},
	} {
		value, err := randomToken(tc.prefix, tc.bytes)
		require.NoError(t, err)
		require.True(t, wellFormed(value, tc.prefix, tc.bytes), value)
		// 32 bytes is at least 43 base64url characters: 256 bits.
		if tc.bytes == secretBytes {
			require.Len(t, value, len(tc.prefix)+43)
		}
	}
}

// The bearer prefix must not admit the other three: a client secret or an
// access token presented as a bearer secret is a different credential.
func TestTheBearerPrefixDoesNotMatchTheOtherCredentials(t *testing.T) {
	for _, prefix := range []string{PrefixClientSecret, PrefixAccessToken, PrefixClientID} {
		value, err := randomToken(prefix, secretBytes)
		require.NoError(t, err)
		require.False(t, wellFormed(value, PrefixBearerSecret, secretBytes), value)
	}
}

func TestAPersonalAccessTokenIsNotWellFormed(t *testing.T) {
	for _, token := range []string{
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig",
		"",
		"scim_",
		"scim_" + strings.Repeat("A", 42),
		"scim_" + strings.Repeat("*", 43),
	} {
		require.False(t, wellFormed(token, PrefixBearerSecret, secretBytes), token)
		require.False(t, wellFormed(token, PrefixAccessToken, secretBytes), token)
	}
}

func TestHashAndHint(t *testing.T) {
	require.Len(t, HashSecret("scim_x"), 64)
	require.NotEqual(t, HashSecret("a"), HashSecret("b"))
	require.Equal(t, "wxyz", secretHint("scim_abcwxyz"))
	require.True(t, hashesEqual(HashSecret("a"), HashSecret("a")))
	require.False(t, hashesEqual(HashSecret("a"), HashSecret("b")))
}

func TestNamesAreTrimmedAndBounded(t *testing.T) {
	name, err := normalizeName("  Entra ID  ")
	require.NoError(t, err)
	require.Equal(t, "Entra ID", name)
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxNameLength+1), "line\nbreak"} {
		_, err := normalizeName(bad)
		require.ErrorIs(t, err, ErrInvalidName, bad)
	}
}

func TestAccessTokenTTLFromEnv(t *testing.T) {
	env := func(value string) func(string) string {
		return func(name string) string {
			if name == AccessTokenTTLEnv {
				return value
			}
			return ""
		}
	}
	ttl, err := AccessTokenTTLFromEnv(env(""))
	require.NoError(t, err)
	require.Equal(t, time.Hour, ttl)

	ttl, err = AccessTokenTTLFromEnv(env("30m"))
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, ttl)

	for _, bad := range []string{"soon", "1m", "48h", "-1h"} {
		_, err := AccessTokenTTLFromEnv(env(bad))
		require.Error(t, err, bad)
		require.Contains(t, err.Error(), AccessTokenTTLEnv)
	}
}

func TestPrincipalActorLabel(t *testing.T) {
	require.Equal(t, "scim:Entra ID", Principal{Name: "Entra ID"}.ActorLabel())
}
