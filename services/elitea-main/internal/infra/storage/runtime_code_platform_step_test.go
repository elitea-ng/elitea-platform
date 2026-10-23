package storage

import (
	"context"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
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
	scope := contentScope("0123456789abcdef0123456789abcdef", 1)
	s := &ContentServer{requests: make(chan struct{}, 1)}
	_, err := s.StepCodePlatform(context.Background(), &runtimev1.StepCodePlatformRequestV1{Scope: scope, RequestJson: []byte(`{}`)})
	requireContentCode(t, err, codes.Unimplemented)
	s.codePlatformPump = &CodePlatformPump{}
	_, err = s.StepCodePlatform(contentPeerContext(context.Background(), nil), &runtimev1.StepCodePlatformRequestV1{Scope: scope, RequestJson: []byte(strings.Repeat("x", 513))})
	requireContentCode(t, err, codes.PermissionDenied)
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
	if _, err := (*CodeBrokerPolicies)(nil).ReadOriginalCodeBrokerEffects(context.Background(), nil, "", 0, "", "", ""); err == nil {
		t.Fatal("missing reader proved no effect")
	}
}
