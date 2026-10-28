package modelruntoken

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func testBinding() Binding {
	return Binding{
		ExecutionID: "exec-1",
		Generation:  3,
		ClaimID:     "claim-1",
		FenceToken:  bytes.Repeat([]byte{7}, 32),
		ProjectID:   42,
		UserID:      9,
		TokenID:     11,
	}
}

func testSigner(t *testing.T, secret string, now time.Time) *Signer {
	t.Helper()
	signer, err := NewSigner(DeriveKey([]byte(secret)))
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return now }
	return signer
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := testSigner(t, "pat-signing-key", now)
	token, err := signer.Issue(testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if !HasPrefix(token) || strings.ContainsAny(token, "\r\n\x00 ") {
		t.Fatalf("token %q is not a header-safe model run token", token)
	}
	got, err := signer.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	want := testBinding()
	if got.ExecutionID != want.ExecutionID || got.Generation != want.Generation || got.ClaimID != want.ClaimID ||
		!bytes.Equal(got.FenceToken, want.FenceToken) || got.ProjectID != want.ProjectID ||
		got.UserID != want.UserID || got.TokenID != want.TokenID {
		t.Fatalf("binding = %+v, want %+v", got, want)
	}
}

func TestVerifyRefusesForgedTamperedAndExpiredTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := testSigner(t, "pat-signing-key", now)
	token, err := signer.Issue(testBinding())
	if err != nil {
		t.Fatal(err)
	}
	other := testSigner(t, "another-deployment", now)
	forged, err := other.Issue(testBinding())
	if err != nil {
		t.Fatal(err)
	}
	dot := strings.LastIndexByte(token, '.')
	tampered := token[:dot-2] + "AA" + token[dot:]
	cases := map[string]string{
		"another key":     forged,
		"tampered claims": tampered,
		"no signature":    token[:dot],
		"not prefixed":    strings.TrimPrefix(token, Prefix),
		"a PAT JWT":       "eyJhbGciOiJIUzUxMiJ9.e30.sig",
	}
	for name, candidate := range cases {
		if _, err := signer.Verify(candidate); !errors.Is(err, auth.ErrCredentialRejected) {
			t.Fatalf("%s: err = %v, want ErrCredentialRejected", name, err)
		}
	}
	later := testSigner(t, "pat-signing-key", now.Add(MaxLifetime+time.Second))
	if _, err := later.Verify(token); !errors.Is(err, auth.ErrCredentialRejected) {
		t.Fatalf("past the ceiling: err = %v, want ErrCredentialRejected", err)
	}
}

func TestIssueRefusesAnIncompleteBinding(t *testing.T) {
	signer := testSigner(t, "pat-signing-key", time.Now())
	binding := testBinding()
	binding.FenceToken = nil
	if _, err := signer.Issue(binding); err == nil {
		t.Fatal("a binding without a fence was signed")
	}
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatal("a short key was accepted")
	}
	if DeriveKey(nil) != nil {
		t.Fatal("an empty secret derived a key")
	}
}

// fakeLeases models elitea_runtime.execution_claims for one claim: a lease
// expiry that a renewal moves forward, and a release.
type fakeLeases struct {
	now      time.Time
	expires  time.Time
	released bool
	err      error
	claim    Binding
}

func (f *fakeLeases) renew(ttl time.Duration) { f.expires = f.now.Add(ttl) }

func (f *fakeLeases) ClaimLive(_ context.Context, b Binding) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return !f.released && f.expires.After(f.now) &&
		b.ClaimID == f.claim.ClaimID && bytes.Equal(b.FenceToken, f.claim.FenceToken) &&
		b.ExecutionID == f.claim.ExecutionID && b.Generation == f.claim.Generation, nil
}

func TestValidatorFollowsTheLease(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	signer := testSigner(t, "pat-signing-key", start)
	leases := &fakeLeases{now: start, claim: testBinding()}
	leases.renew(30 * time.Second)
	validator, err := NewValidator(signer, leases)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(testBinding())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	user, binding, err := validator.ValidateModelRunToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "9" || user.UserID != "9" || user.TokenID != "11" || user.AuthType != "token" || binding.ProjectID != 42 {
		t.Fatalf("principal = %+v binding = %+v, want the actor's token principal", user, binding)
	}

	// A long run: the worker keeps the one token while the lease is renewed
	// every few seconds, well past the lease it was issued under.
	for i := 0; i < 20; i++ {
		leases.now = leases.now.Add(10 * time.Second)
		signer.now = func() time.Time { return leases.now }
		leases.renew(30 * time.Second)
		if _, _, err := validator.ValidateModelRunToken(ctx, token); err != nil {
			t.Fatalf("renewal %d: a renewed lease lost model access: %v", i, err)
		}
	}

	// The lease lapses: the token expires with it.
	leases.now = leases.now.Add(31 * time.Second)
	if _, _, err := validator.ValidateModelRunToken(ctx, token); !errors.Is(err, auth.ErrCredentialRejected) {
		t.Fatalf("expired lease: err = %v, want ErrCredentialRejected", err)
	}

	// A released claim ends it even with lease time left.
	leases.renew(30 * time.Second)
	leases.released = true
	if _, _, err := validator.ValidateModelRunToken(ctx, token); !errors.Is(err, auth.ErrCredentialRejected) {
		t.Fatalf("released claim: err = %v, want ErrCredentialRejected", err)
	}

	// A store fault is not a refusal.
	leases.released = false
	leases.err = errors.New("pool timeout")
	if _, _, err := validator.ValidateModelRunToken(ctx, token); !errors.Is(err, auth.ErrCredentialValidationUnavailable) {
		t.Fatalf("store fault: err = %v, want ErrCredentialValidationUnavailable", err)
	}
}
