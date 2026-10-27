package delegationstore

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testSealer(t *testing.T) *sealer {
	t.Helper()
	s, err := newSealer(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	return s
}

func TestTheCustodyKeyNeedsThirtyTwoBytes(t *testing.T) {
	_, err := newSealer(make([]byte, 16))
	require.Error(t, err)
}

// Decrypt fails when any AAD field changes: one case per field.
func TestDelegationCiphertextDoesNotOpenWhenAnyAADFieldChanges(t *testing.T) {
	s := testSealer(t)
	base := DelegationAAD{
		TenantProjectID: 7,
		DelegationID:    "0192f5e2-7c3a-7b1e-8a9d-000000000001",
		TenantID:        "aaaaaaaa-0000-4000-8000-000000000001",
		ObjectID:        "cccccccc-0000-4000-8000-000000000003",
		AgentBindingID:  "0192f5e2-7c3a-7b1e-8a9d-000000000002",
		Audience:        "11111111-1111-1111-1111-111111111111",
		ExpiresAt:       time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	ciphertext := s.seal("user-assertion", base)
	opened, err := s.open(ciphertext, base)
	require.NoError(t, err)
	require.Equal(t, "user-assertion", opened)
	require.NotContains(t, string(ciphertext), "user-assertion")

	mutations := map[string]func(a *DelegationAAD){
		"tenant_project_id": func(a *DelegationAAD) { a.TenantProjectID = 8 },
		"delegation_id":     func(a *DelegationAAD) { a.DelegationID = "0192f5e2-7c3a-7b1e-8a9d-0000000000ff" },
		"tid":               func(a *DelegationAAD) { a.TenantID = "bbbbbbbb-0000-4000-8000-000000000002" },
		"oid":               func(a *DelegationAAD) { a.ObjectID = "dddddddd-0000-4000-8000-000000000004" },
		"agent_binding_id":  func(a *DelegationAAD) { a.AgentBindingID = "0192f5e2-7c3a-7b1e-8a9d-0000000000ee" },
		"audience":          func(a *DelegationAAD) { a.Audience = "api://22222222-2222-2222-2222-222222222222/access_agent" },
		"expires_at":        func(a *DelegationAAD) { a.ExpiresAt = a.ExpiresAt.Add(time.Microsecond) },
	}
	for field, mutate := range mutations {
		t.Run(field, func(t *testing.T) {
			changed := base
			mutate(&changed)
			_, err := s.open(ciphertext, changed)
			require.ErrorIs(t, err, ErrCustodyIntegrity)
		})
	}
}

func TestSignInCiphertextDoesNotOpenWhenAnyAADFieldChanges(t *testing.T) {
	s := testSealer(t)
	base := SignInAAD{
		AssertionID:   "0192f5e2-7c3a-7b1e-8a9d-000000000001",
		SessionDigest: SessionDigest("session-a"),
		UserID:        42,
		TenantID:      "aaaaaaaa-0000-4000-8000-000000000001",
		ObjectID:      "cccccccc-0000-4000-8000-000000000003",
		Audience:      "11111111-1111-1111-1111-111111111111",
		ExpiresAt:     time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	}
	ciphertext := s.seal("user-assertion", base)
	mutations := map[string]func(a *SignInAAD){
		"assertion_id": func(a *SignInAAD) { a.AssertionID = "0192f5e2-7c3a-7b1e-8a9d-0000000000ff" },
		"session":      func(a *SignInAAD) { a.SessionDigest = SessionDigest("session-b") },
		"user_id":      func(a *SignInAAD) { a.UserID = 43 },
		"tid":          func(a *SignInAAD) { a.TenantID = "bbbbbbbb-0000-4000-8000-000000000002" },
		"oid":          func(a *SignInAAD) { a.ObjectID = "dddddddd-0000-4000-8000-000000000004" },
		"audience":     func(a *SignInAAD) { a.Audience = "other" },
		"expires_at":   func(a *SignInAAD) { a.ExpiresAt = a.ExpiresAt.Add(time.Second) },
	}
	for field, mutate := range mutations {
		t.Run(field, func(t *testing.T) {
			changed := base
			mutate(&changed)
			_, err := s.open(ciphertext, changed)
			require.ErrorIs(t, err, ErrCustodyIntegrity)
		})
	}
	// A sign-in ciphertext never opens as a delegation, even with equal values.
	_, err := s.open(ciphertext, DelegationAAD{TenantID: base.TenantID, ObjectID: base.ObjectID, Audience: base.Audience, ExpiresAt: base.ExpiresAt})
	require.ErrorIs(t, err, ErrCustodyIntegrity)
}

func TestAnotherMasterKeyDoesNotOpenTheCiphertext(t *testing.T) {
	aad := DelegationAAD{TenantProjectID: 1, DelegationID: "d"}
	ciphertext := testSealer(t).seal("x", aad)
	other, err := newSealer(bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	_, err = other.open(ciphertext, aad)
	require.ErrorIs(t, err, ErrCustodyIntegrity)
}
