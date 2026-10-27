package securityaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func sampleEvent() SecurityEvent {
	return SecurityEvent{
		ID:                   "0192f0c1-7b6e-7c3a-8f00-000000000001",
		Type:                 TypeDecision,
		Source:               "elitea-main/test",
		Time:                 time.Date(2026, 10, 26, 12, 0, 0, 123456789, time.UTC),
		TenantProjectID:      42,
		Action:               "credential.use",
		Effect:               EffectDeny,
		Reasons:              []string{"grant.missing", "epoch.stale"},
		PrincipalRef:         "user:7",
		ActorChain:           []string{"agent:binding-1", "user:7"},
		ActorFidelity:        FidelityConfiguredAct,
		CallingApplicationID: "client-abc",
		AuthenticationRef:    "pat:991",
		AgentBindingID:       "binding-1",
		MandateRef:           "mandate-3",
		GrantRef:             "sha256:abcd",
		WorkloadID:           "spiffe://elitea/ns/default/sa/worker",
		ExecutionID:          "exec-1",
		InvocationID:         "inv-1",
		DecisionID:           "dec-1",
		ResourceType:         "toolkit",
		ResourceID:           "17",
		PolicyRevisionID:     "2026-10-r1",
		AuthorizationEpoch:   4,
		RevocationEpoch:      2,
		Arrangement:          ArrangementC,
		CorrelationID:        "trace-1",
	}
}

// secretLikeName is the owner rule: an event carries references and digests
// only, so no field may be named like a credential value.
var secretLikeName = regexp.MustCompile(`(?i)token|secret|password|material|assertion`)

func TestSecurityEventHasNoSecretLikeFields(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(SecurityEvent{}),
		reflect.TypeOf(envelope{}),
		reflect.TypeOf(eventData{}),
		reflect.TypeOf(Record{}),
		reflect.TypeOf(StoredRow{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if secretLikeName.MatchString(field.Name) {
				t.Errorf("%s.%s: a security audit field must not be named like a credential value", typ.Name(), field.Name)
			}
			if tag := field.Tag.Get("json"); secretLikeName.MatchString(tag) {
				t.Errorf("%s.%s: JSON name %q is named like a credential value", typ.Name(), field.Name, tag)
			}
		}
	}
}

// The struct test must itself catch the names it exists for.
func TestSecretLikeNamePatternCatchesTheForbiddenNames(t *testing.T) {
	for _, name := range []string{"AccessToken", "ClientSecret", "Password", "KeyMaterial", "Assertion", "id_token"} {
		if !secretLikeName.MatchString(name) {
			t.Errorf("pattern misses %q", name)
		}
	}
}

func TestEveryEventTypeHasAnOCSFClass(t *testing.T) {
	for _, typ := range []EventType{TypeDecision, TypeCredentialRedeem, TypeGrantChanged, TypeBindingChanged,
		TypeMandateChanged, TypeDelegationChanged, TypeEpochBumped, TypeProbe} {
		class, ok := OCSFClassUID(typ)
		if !ok || class < 1000 {
			t.Errorf("%s has no OCSF class", typ)
		}
		if !regexp.MustCompile(`^ai\.elitea\.security\.v1\.[a-z.]{1,64}$`).MatchString(string(typ)) {
			t.Errorf("%s does not match the migration's event_type check", typ)
		}
	}
	if EventType("ai.elitea.security.v1.unknown").Valid() {
		t.Error("an unknown type is valid")
	}
}

func TestCanonicalIsDeterministicCloudEvent(t *testing.T) {
	event := sampleEvent()
	first, err := event.Canonical(7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := event.Canonical(7)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical bytes differ between two encodings")
	}
	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"specversion": "1.0", "type": string(TypeDecision), "partitionkey": "42",
		"sequence": "00000000000000000007", "ocsfclassuid": float64(OCSFClassAuthorizeSession),
		"ocsfcategoryuid": float64(OCSFCategoryIAM), "subject": "toolkit/17",
		"time": "2026-10-26T12:00:00.123456789Z", "datacontenttype": DataContentType,
	} {
		if decoded[key] != want {
			t.Errorf("%s = %v, want %v", key, decoded[key], want)
		}
	}
	other, err := event.Canonical(8)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, other) {
		t.Error("the chain position is not part of the canonical bytes")
	}
	if _, err := event.Canonical(0); err == nil {
		t.Error("partition sequence 0 was accepted")
	}
}

func TestCanonicalNilAndEmptyListsAgree(t *testing.T) {
	a := sampleEvent()
	a.Reasons, a.ActorChain = nil, nil
	b := sampleEvent()
	b.Reasons, b.ActorChain = []string{}, []string{}
	ca, _ := a.Canonical(1)
	cb, _ := b.Canonical(1)
	if !bytes.Equal(ca, cb) {
		t.Fatalf("nil and empty lists encode differently:\n%s\n%s", ca, cb)
	}
}

// jsonb reorders keys and changes whitespace; recanonicalize must give the
// original bytes back.
func TestRecanonicalizeUndoesAJSONBReformat(t *testing.T) {
	event := sampleEvent()
	event.ResourceID = `<a&b>` // HTML escaping must survive the round trip
	canonical, err := event.Canonical(3)
	if err != nil {
		t.Fatal(err)
	}
	reformatted := jsonbLike(t, canonical)
	if bytes.Equal(reformatted, canonical) {
		t.Fatal("test reformat did not change the bytes")
	}
	_, again, err := recanonicalize(reformatted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, canonical) {
		t.Fatalf("round trip changed the bytes:\n%s\n%s", canonical, again)
	}
}

func TestRecanonicalizeRefusesAnAddedField(t *testing.T) {
	canonical, _ := sampleEvent().Canonical(1)
	var generic map[string]any
	_ = json.Unmarshal(canonical, &generic)
	generic["extra"] = "x"
	tampered, _ := json.Marshal(generic)
	if _, _, err := recanonicalize(tampered); err == nil {
		t.Fatal("an added field decoded")
	}
}

func TestValidateRefusesCredentialShapedValues(t *testing.T) {
	cases := map[string]func(*SecurityEvent){
		"jwt":            func(e *SecurityEvent) { e.AuthenticationRef = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig" },
		"bearer":         func(e *SecurityEvent) { e.PrincipalRef = "Bearer abc" },
		"control":        func(e *SecurityEvent) { e.ResourceID = "a\x00b" },
		"invalid utf8":   func(e *SecurityEvent) { e.ResourceID = "a\xffb" },
		"too long":       func(e *SecurityEvent) { e.ResourceID = strings.Repeat("a", maxStringBytes+1) },
		"free reason":    func(e *SecurityEvent) { e.Reasons = []string{"Provider said: no"} },
		"bad id":         func(e *SecurityEvent) { e.ID = "not-a-uuid" },
		"upper id":       func(e *SecurityEvent) { e.ID = strings.ToUpper(e.ID) },
		"unknown type":   func(e *SecurityEvent) { e.Type = "x" },
		"no effect":      func(e *SecurityEvent) { e.Effect = "" },
		"bad effect":     func(e *SecurityEvent) { e.Effect = "maybe" },
		"bad arrange":    func(e *SecurityEvent) { e.Arrangement = "D" },
		"bad fidelity":   func(e *SecurityEvent) { e.ActorFidelity = "x" },
		"negative":       func(e *SecurityEvent) { e.TenantProjectID = -1 },
		"negative epoch": func(e *SecurityEvent) { e.RevocationEpoch = -1 },
		"no action":      func(e *SecurityEvent) { e.Action = "" },
		"no time":        func(e *SecurityEvent) { e.Time = time.Time{} },
		"actor chain":    func(e *SecurityEvent) { e.ActorChain = []string{"Bearer x"} },
		"empty actor":    func(e *SecurityEvent) { e.ActorChain = []string{""} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			event := sampleEvent()
			mutate(&event)
			err := event.Validate()
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("Validate = %v, want ErrInvalidEvent", err)
			}
			if strings.Contains(err.Error(), "eyJ") || strings.Contains(err.Error(), "Bearer abc") {
				t.Fatalf("the error echoes the value: %v", err)
			}
		})
	}
	if err := sampleEvent().Validate(); err != nil {
		t.Fatalf("sample event: %v", err)
	}
	grant := sampleEvent()
	grant.Type, grant.Effect = TypeGrantChanged, ""
	if err := grant.Validate(); err != nil {
		t.Fatalf("a non-decision event without an effect: %v", err)
	}
}

// jsonbLike imitates PostgreSQL's jsonb output: keys sorted by length then
// bytes, ", " and ": " separators, no HTML escaping.
func jsonbLike(t *testing.T, canonical []byte) []byte {
	t.Helper()
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", " ")
	if err := encoder.Encode(generic); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(out.Bytes())
}
