package authsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type recordingValidator struct {
	name  string
	calls int
}

func (v *recordingValidator) ValidateToken(context.Context, string) (auth.User, error) {
	v.calls++
	return auth.User{ID: v.name}, nil
}

func TestNativeAwareValidatorRoutesByPrefix(t *testing.T) {
	pat := &recordingValidator{name: "pat"}
	native := &recordingValidator{name: "native"}
	validator := NewNativeAwareValidator(pat, native)

	user, err := validator.ValidateToken(context.Background(), "elnat_abc")
	if err != nil || user.ID != "native" {
		t.Fatalf("elnat_ = (%v, %v), want the native validator", user, err)
	}
	user, err = validator.ValidateToken(context.Background(), "eyJhbGciOiJIUzUxMiJ9.x.y")
	if err != nil || user.ID != "pat" {
		t.Fatalf("JWT = (%v, %v), want the PAT validator", user, err)
	}
	for _, token := range []string{"elnrt_abc", "elnac_abc"} {
		before := pat.calls + native.calls
		_, err := validator.ValidateToken(context.Background(), token)
		if !errors.Is(err, auth.ErrCredentialRejected) {
			t.Fatalf("%s: err = %v, want ErrCredentialRejected", token, err)
		}
		if pat.calls+native.calls != before {
			t.Fatalf("%s reached a store; a refresh token or code must be refused without a read", token)
		}
	}
}

func TestNativeAwareValidatorWithoutNativeIsThePATValidatorItself(t *testing.T) {
	pat := &recordingValidator{name: "pat"}
	got := NewNativeAwareValidator(pat, nil)
	if got != TokenValidator(pat) {
		t.Fatalf("nil native must return the PAT validator unchanged, got %T", got)
	}
}
