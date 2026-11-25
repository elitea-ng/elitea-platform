package adminaccess

// The sealed credential format and its Argon2id digest, without a database
// (ADR-0059 R2.6a, issue 2520).

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseSealedCredentialAcceptsOnlyTheFormat(t *testing.T) {
	selector, verifier := strings.Repeat("a1", 12), strings.Repeat("0f", 32)
	good := FormatSealedCredential(selector, verifier)
	for _, presented := range []string{good, "  " + good + "\n", strings.ToUpper(good)} {
		gotSelector, gotVerifier, err := ParseSealedCredential(presented)
		if err != nil || gotSelector != selector || gotVerifier != verifier {
			t.Fatalf("parse %q = %q %q %v", presented, gotSelector, gotVerifier, err)
		}
	}
	for _, presented := range []string{
		"", "bg_", selector + "." + verifier, "bg_" + selector, "bg_" + selector + ".",
		"bg_" + selector[1:] + "." + verifier, "bg_" + selector + "." + verifier[1:],
		"bg_" + selector + "." + verifier + "0", "bg_" + selector + "." + strings.Repeat("g", 64),
		"bg_" + selector + ".." + verifier,
	} {
		if _, _, err := ParseSealedCredential(presented); !errors.Is(err, ErrSealedCredentialRefused) ||
			SealedRefusalOf(err) != SealedRefusalMalformed {
			t.Errorf("parse %q = %v, want a malformed refusal", presented, err)
		}
	}
}

func TestSealedDigestVerifiesOnlyItsVerifier(t *testing.T) {
	verifier, err := randomHex(sealedVerifierBytes)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := sealedDigest(verifier)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(digest, verifier) || !strings.HasPrefix(digest, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("digest = %q", digest)
	}
	if ok, err := sealedDigestMatches(digest, verifier); err != nil || !ok {
		t.Fatalf("the digest does not verify its verifier: %v %v", ok, err)
	}
	other := verifier[:63] + string("0123456789abcdef"[(strings.IndexByte("0123456789abcdef", verifier[63])+1)%16])
	if ok, err := sealedDigestMatches(digest, other); err != nil || ok {
		t.Fatalf("the digest verifies another verifier: %v %v", ok, err)
	}
	again, _ := sealedDigest(verifier)
	if again == digest {
		t.Fatal("two digests of one verifier share a salt")
	}
}

// A stored digest whose parameters would make one comparison expensive, or
// that is not Argon2id, is refused before any hash runs.
func TestSealedDigestRefusesParametersOutsideTheBounds(t *testing.T) {
	for _, digest := range []string{
		"$argon2id$v=19$m=1048576,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=100,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=64$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=16$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"plain",
	} {
		started := time.Now()
		if _, err := sealedDigestMatches(digest, strings.Repeat("0", 64)); err == nil {
			t.Errorf("%q was accepted", digest)
		}
		if time.Since(started) > 50*time.Millisecond {
			t.Errorf("%q ran a hash before it was refused", digest)
		}
	}
}

func TestSealedIssueValidate(t *testing.T) {
	base := SealedIssue{UserID: 7, At: time.Unix(1, 0)}
	if err := base.Validate(); err != nil {
		t.Fatalf("a valid issue: %v", err)
	}
	if base.level() != ElevationLevelAdmin || base.lifetime() != 365*24*time.Hour {
		t.Fatalf("defaults: level %q, lifetime %s", base.level(), base.lifetime())
	}
	for name, mutate := range map[string]func(*SealedIssue){
		"no person":         func(s *SealedIssue) { s.UserID = 0 },
		"an unknown level":  func(s *SealedIssue) { s.Level = "owner" },
		"no time":           func(s *SealedIssue) { s.At = time.Time{} },
		"366 days":          func(s *SealedIssue) { s.Lifetime = 366 * 24 * time.Hour },
		"a negative life":   func(s *SealedIssue) { s.Lifetime = -time.Hour },
		"a long label":      func(s *SealedIssue) { s.Label = strings.Repeat("x", 201) },
		"a control":         func(s *SealedIssue) { s.Label = "tab\there" },
		"an unknown source": func(s *SealedIssue) { s.Source = "web" },
	} {
		issue := base
		mutate(&issue)
		if err := issue.Validate(); !errors.Is(err, ErrSealedCredentialInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSealedCredentialState(t *testing.T) {
	at := time.Unix(1000, 0)
	c := SealedCredential{IssuedAt: at, ExpiresAt: at.Add(time.Hour)}
	for _, tc := range []struct {
		mutate func(*SealedCredential)
		now    time.Time
		want   string
	}{
		{func(*SealedCredential) {}, at, SealedStateActive},
		{func(*SealedCredential) {}, at.Add(time.Hour), SealedStateExpired},
		{func(c *SealedCredential) { c.UsedAt = at }, at, SealedStateUsed},
		{func(c *SealedCredential) { c.RevokedAt = at; c.UsedAt = at }, at, SealedStateRevoked},
	} {
		credential := c
		tc.mutate(&credential)
		if got := credential.State(tc.now); got != tc.want {
			t.Errorf("state = %q, want %q", got, tc.want)
		}
	}
}
