package vectorintrospection

import (
	"strings"
	"testing"
	"time"

	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
)

const testPlatformSecret = "0123456789abcdef0123456789abcdef"

func newTestPlatformTokens(t *testing.T) *PlatformTokens {
	t.Helper()
	tokens, err := NewPlatformTokens(testPlatformSecret)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestAPlatformTokenNamesOneProjectAndOneSource(t *testing.T) {
	tokens := newTestPlatformTokens(t)
	token, expires, err := tokens.Mint(7, "agent_memory", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, PlatformTokenPrefix) || time.Until(expires) <= 0 || time.Until(expires) > 11*time.Minute {
		t.Fatalf("token %q expires %v", token, expires)
	}
	response := tokens.verify(token)
	if !response.GetActive() || response.GetProjectId() != 7 || response.GetPrincipal() != platformPrincipal ||
		response.GetKind() != vectorv1.TokenKind_TOKEN_KIND_ENGINE_CALLBACK || response.GetExpiresAtUnix() != expires.Unix() ||
		len(response.GetAllowedSources()) != 1 || response.GetAllowedSources()[0] != vectorv1.Source_SOURCE_AGENT_MEMORY {
		t.Fatalf("response %v", response)
	}
}

func TestAnotherReplicaWithTheSameSecretVerifiesTheToken(t *testing.T) {
	token, _, err := newTestPlatformTokens(t).Mint(7, "agent_memory", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !newTestPlatformTokens(t).verify(token).GetActive() {
		t.Fatal("a replica with the same secret refused the token")
	}
	other, err := NewPlatformTokens("another-secret-of-enough-length")
	if err != nil {
		t.Fatal(err)
	}
	if other.verify(token).GetActive() {
		t.Fatal("a replica with another secret accepted the token")
	}
}

func TestAPlatformTokenIsRefusedWhenAltered(t *testing.T) {
	tokens := newTestPlatformTokens(t)
	token, _, err := tokens.Mint(7, "agent_memory", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(token, PlatformTokenPrefix)
	encoded, signature, _ := strings.Cut(body, ".")
	// Another project's payload under this MAC.
	other, _, _ := tokens.Mint(8, "agent_memory", time.Minute)
	otherEncoded, _, _ := strings.Cut(strings.TrimPrefix(other, PlatformTokenPrefix), ".")
	for name, bad := range map[string]string{
		"swapped payload": PlatformTokenPrefix + otherEncoded + "." + signature,
		"no signature":    PlatformTokenPrefix + encoded,
		"empty signature": PlatformTokenPrefix + encoded + ".",
		"garbage":         PlatformTokenPrefix + "x.y",
		"other prefix":    "elvc_" + body,
		"empty":           "",
	} {
		if tokens.verify(bad).GetActive() {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAPlatformTokenExpires(t *testing.T) {
	tokens := newTestPlatformTokens(t)
	token, _, err := tokens.Mint(7, "agent_memory", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tokens.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if tokens.verify(token).GetActive() {
		t.Fatal("an expired token was accepted")
	}
}

func TestMintRefusesAnUnknownSourceAProjectlessTokenAndMissingSecret(t *testing.T) {
	tokens := newTestPlatformTokens(t)
	if _, _, err := tokens.Mint(7, "toolkit_index", time.Minute); err == nil {
		t.Fatal("a claim-token source was minted")
	}
	if _, _, err := tokens.Mint(0, "agent_memory", time.Minute); err == nil {
		t.Fatal("a projectless token was minted")
	}
	if _, err := NewPlatformTokens("short"); err == nil {
		t.Fatal("a short secret was accepted")
	}
	// An over-long lifetime is capped, not honoured.
	_, expires, err := tokens.Mint(7, "agent_memory", 24*time.Hour)
	if err != nil || time.Until(expires) > MaxPlatformTokenLifetime+time.Second {
		t.Fatalf("lifetime %v %v", time.Until(expires), err)
	}
}

func TestTheServerAnswersPlatformTokensOnlyWhenItHasTheKey(t *testing.T) {
	server := newTestServer(t)
	token, _, err := newTestPlatformTokens(t).Mint(7, "agent_memory", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := &vectorv1.IntrospectTokenRequest{Token: token}
	response, err := server.IntrospectToken(peerContext("elitea-vector"), request)
	if err != nil || response.GetActive() {
		t.Fatalf("without the key: %v %v", response, err)
	}
	server.WithPlatformTokens(newTestPlatformTokens(t))
	response, err = server.IntrospectToken(peerContext("elitea-vector"), request)
	if err != nil || !response.GetActive() || response.GetProjectId() != 7 {
		t.Fatalf("with the key: %v %v", response, err)
	}
}
