package secretstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func governedContext() StoreContext {
	return StoreContext{
		TenantProjectID:  7,
		CredentialRefID:  "cref-1",
		DecisionID:       "dec-1",
		WorkloadScope:    "agent-runtime",
		PolicyRevisionID: "2026-10-r1",
	}
}

func TestStoreContextValidateRefusesMissingFields(t *testing.T) {
	cases := map[string]func(*StoreContext){
		"tenant":          func(sc *StoreContext) { sc.TenantProjectID = 0 },
		"credential ref":  func(sc *StoreContext) { sc.CredentialRefID = "" },
		"workload scope":  func(sc *StoreContext) { sc.WorkloadScope = "" },
		"scope shape":     func(sc *StoreContext) { sc.WorkloadScope = "Agent/../x" },
		"policy revision": func(sc *StoreContext) { sc.PolicyRevisionID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			sc := governedContext()
			mutate(&sc)
			for _, op := range []Op{OpRead, OpWrite, OpMetadata, OpRetire} {
				if err := sc.Validate(op); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("%s: Validate = %v, want ErrInvalidRequest", op, err)
				}
			}
		})
	}
}

func TestStoreContextReadNeedsADecision(t *testing.T) {
	sc := governedContext()
	sc.DecisionID = ""
	if err := sc.Validate(OpRead); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("read without decision = %v", err)
	}
	for _, op := range []Op{OpWrite, OpMetadata, OpRetire} {
		if err := sc.Validate(op); err != nil {
			t.Fatalf("%s without decision = %v", op, err)
		}
	}
}

func TestValidateReadVersionRefusesLatest(t *testing.T) {
	for _, version := range []int64{0, -1} {
		if err := ValidateReadVersion(version); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("version %d = %v", version, err)
		}
	}
	if err := ValidateReadVersion(1); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBackendHasNoFallback(t *testing.T) {
	err := ValidateBackend(OpRead, SecretRef{Backend: BackendVault, Path: "x"}, BackendPGVault)
	if !errors.Is(err, ErrMisconfigured) {
		t.Fatalf("cross-backend ref = %v", err)
	}
}

func TestRuntimeSecretNeverFormats(t *testing.T) {
	const canary = "canary-secret-value-123"
	secret := NewRuntimeSecret([]byte(canary))
	wrapper := struct{ S RuntimeSecret }{secret}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		if got := fmt.Sprintf(verb, secret); strings.Contains(got, canary) || strings.Contains(got, "63616e617279") {
			t.Fatalf("%s leaked: %s", verb, got)
		}
		if got := fmt.Sprintf(verb, wrapper); strings.Contains(got, canary) {
			t.Fatalf("%s in a struct leaked: %s", verb, got)
		}
	}
	if _, err := json.Marshal(wrapper); err == nil {
		t.Fatal("JSON marshalling must refuse")
	}
	if _, err := secret.MarshalText(); err == nil {
		t.Fatal("text marshalling must refuse")
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "secret", secret)
	if strings.Contains(logged.String(), canary) {
		t.Fatalf("slog leaked: %s", logged.String())
	}
	if string(secret.UnsafeBytes()) != canary {
		t.Fatal("UnsafeBytes must return the value")
	}
	copyOf := secret
	secret.Destroy()
	if !bytes.Equal(copyOf.UnsafeBytes(), make([]byte, len(canary))) {
		t.Fatal("Destroy must zero every copy")
	}
}

func TestNewRuntimeSecretCopies(t *testing.T) {
	source := []byte("abc")
	secret := NewRuntimeSecret(source)
	source[0] = 'x'
	if string(secret.UnsafeBytes()) != "abc" {
		t.Fatal("NewRuntimeSecret must copy its input")
	}
}

func TestDefinitiveRefusalClassifiesOutcomes(t *testing.T) {
	for _, kind := range []error{ErrVersionConflict, ErrDenied, ErrMisconfigured, ErrNotFound, ErrInvalidRequest} {
		if !definitiveRefusal(Errorf(kind, OpWrite, "x")) {
			t.Fatalf("%v must be definitive", kind)
		}
	}
	if definitiveRefusal(Errorf(ErrUnavailable, OpWrite, "x")) {
		t.Fatal("an unavailable backend is an unknown outcome")
	}
}
