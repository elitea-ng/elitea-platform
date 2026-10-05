package storage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodePlatformStepStrictBodyPins(t *testing.T) {
	valid := `{"schema":"elitea.runtime.code-platform-step-request.v1","revision":1,"dispatch_activation":"` + strings.Repeat("1", 64) + `","prepared_request_fingerprint":"` + strings.Repeat("2", 64) + `"}`
	if _, err := ParseCodePlatformStep([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unknown":   strings.Replace(valid, `"revision":1`, `"revision":1,"actor_id":2`, 1),
		"duplicate": strings.Replace(valid, `"revision":1`, `"revision":1,"revision":1`, 1),
		"missing":   strings.Replace(valid, `"revision":1,`, ``, 1),
		"zero":      strings.Replace(valid, strings.Repeat("1", 64), strings.Repeat("0", 64), 1),
		"null":      strings.Replace(valid, `"revision":1`, `"revision":null`, 1),
		"trailing":  valid + `{}`,
		"bound":     strings.Repeat(" ", 513),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCodePlatformStep([]byte(raw)); err == nil {
				t.Fatal("invalid authority frame accepted")
			}
		})
	}
}
func TestCodePlatformStepDefaultDisabledAndClaimBeforeOwner(t *testing.T) {
	route := "/runtime/content/executions/0123456789abcdef0123456789abcdef/generations/1/code-platform/step"
	s := &ContentServer{}
	r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.PostCodePlatformStep(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled step status %d", w.Code)
	}
	s.codePlatformPump = &CodePlatformPump{}
	w = httptest.NewRecorder()
	s.PostCodePlatformStep(w, httptest.NewRequest(http.MethodPost, route, strings.NewReader(strings.Repeat("x", 513))))
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing claim reached body/owner: %d", w.Code)
	}
}
func TestCodeBrokerNoEffectFactsRequireRegistrationAndNoObservation(t *testing.T) {
	if (CodeBrokerEffectFacts{}).NoObservedEffect() {
		t.Fatal("missing job proved no effect")
	}
	clean := CodeBrokerEffectFacts{Registered: true}
	if !clean.NoObservedEffect() {
		t.Fatal("registered empty fact refused")
	}
	for _, facts := range []CodeBrokerEffectFacts{
		{Registered: true, HasObservedCalls: true}, {Registered: true, HasDispatchedEffects: true},
		{Registered: true, HasUncertainEffects: true}, {Registered: true, HasPendingToolkitChildren: true},
	} {
		if facts.NoObservedEffect() {
			t.Fatal("partial or uncertain call proved whole-Code no effect")
		}
	}
	if _, err := (*CodeBrokerPolicies)(nil).ReadOriginalCodeBrokerEffects(nil, nil, "", 0, "", "", ""); err == nil {
		t.Fatal("missing reader proved no effect")
	}
}
