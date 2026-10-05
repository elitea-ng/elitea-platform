package browserauth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

func TestSuccessMapperPreservesTrackedGrafanaProjection(t *testing.T) {
	mapper := newTrackedSuccessMapper(t)
	decision := validSuccessMapperDecision(
		`{"nameid":"admin","attributes":{"email":"admin@example.test"},"sessionindex":"must-not-escape"}`,
	)
	header, disposition := mapper.Header(decision)
	if disposition != MappingApplied || header.Name != "X-Webauth-User" || header.Value != "admin" {
		t.Fatalf("header = %+v, disposition=%v", header, disposition)
	}
	if strings.Contains(header.Value, "must-not-escape") || strings.Contains(header.Value, "session") {
		t.Fatalf("header leaked provider-session material: %+v", header)
	}
}

func TestSuccessMapperBindsProjectionToDecisionTargetAndScope(t *testing.T) {
	mapper := newTrackedSuccessMapper(t)
	base := validSuccessMapperDecision(`{"nameid":"admin"}`)
	tests := map[string]func(*forwardapp.Decision){
		"absent target":  func(value *forwardapp.Decision) { value.Source.TargetPresent = false; value.Source.Target = "" },
		"empty target":   func(value *forwardapp.Decision) { value.Source.Target = "" },
		"unknown target": func(value *forwardapp.Decision) { value.Source.Target = "json" },
		"absent scope":   func(value *forwardapp.Decision) { value.Source.ScopePresent = false; value.Source.Scope = "" },
		"empty scope":    func(value *forwardapp.Decision) { value.Source.Scope = "" },
		"unknown scope":  func(value *forwardapp.Decision) { value.Source.Scope = "custom" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			decision := base
			mutate(&decision)
			if header, disposition := mapper.Header(decision); disposition != MappingNotApplicable || header != (MappedHeader{}) {
				t.Fatalf("header = %+v, disposition=%v", header, disposition)
			}
		})
	}
}

func TestSuccessMapperFailsClosedForMalformedOrNonBrowserDecision(t *testing.T) {
	mapper := newTrackedSuccessMapper(t)
	tests := map[string]forwardapp.Decision{
		"token": {
			Kind:   forwardapp.DecisionAllow,
			Reason: forwardapp.ReasonCredentialAccepted,
			Source: mapperSource(),
			Authentication: forwardapp.Authentication{
				Type: forwardapp.AuthenticationToken,
				Principal: auth.User{
					ID: "7", UserID: "7", TokenID: "42", AuthType: "token",
				},
				Reference: "-",
			},
		},
		"public": {
			Kind:   forwardapp.DecisionAllow,
			Reason: forwardapp.ReasonPublicRuleMatched,
			Source: mapperSource(),
			Authentication: forwardapp.Authentication{
				Type: forwardapp.AuthenticationPublic, Reference: "-",
			},
		},
		"missing nameid":      validSuccessMapperDecision(`{"attributes":{}}`),
		"empty nameid":        validSuccessMapperDecision(`{"nameid":""}`),
		"blank nameid":        validSuccessMapperDecision(`{"nameid":"   "}`),
		"leading whitespace":  validSuccessMapperDecision(`{"nameid":" admin"}`),
		"trailing whitespace": validSuccessMapperDecision(`{"nameid":"admin "}`),
		"non-string nameid":   validSuccessMapperDecision(`{"nameid":42}`),
		"duplicate nameid":    validSuccessMapperDecision(`{"nameid":"first","nameid":"second"}`),
		"malformed JSON":      validSuccessMapperDecision(`{"nameid":`),
		"header injection":    validSuccessMapperDecision(`{"nameid":"admin\r\nX-Injected: true"}`),
		"oversized nameid": validSuccessMapperDecision(
			`{"nameid":"` + strings.Repeat("x", browserflow.MaxProviderReferenceBytes+1) + `"}`,
		),
	}
	mismatch := validSuccessMapperDecision(`{"nameid":"admin"}`)
	mismatch.Authentication.Principal.UserID = "8"
	tests["mismatched principal"] = mismatch
	for name, decision := range tests {
		t.Run(name, func(t *testing.T) {
			header, disposition := mapper.Header(decision)
			want := MappingInvalidAuthorizedData
			if name == "token" || name == "public" {
				want = MappingNotApplicable
			}
			if disposition != want || header != (MappedHeader{}) {
				t.Fatalf("header = %+v, disposition=%v, want %v", header, disposition, want)
			}
		})
	}
}

func TestNewSuccessMapperAcceptsOnlyVersionedTrackedContract(t *testing.T) {
	if _, err := NewSuccessMapper(MapperContractTrackedV1); err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{"", "elitea.auth_mappers.tracked.v2", "custom"} {
		if _, err := NewSuccessMapper(contract); !errors.Is(err, ErrInvalidMapperConfiguration) {
			t.Fatalf("contract %q error = %v", contract, err)
		}
	}
	var mapper *SuccessMapper
	if header, disposition := mapper.Header(validSuccessMapperDecision(`{"nameid":"admin"}`)); disposition != MappingNotApplicable || header != (MappedHeader{}) {
		t.Fatalf("typed nil header = %+v, disposition=%v", header, disposition)
	}
}

func FuzzProviderReferenceNeverPanics(f *testing.F) {
	f.Add([]byte(`{"nameid":"admin"}`))
	f.Add([]byte(`{"nameid":"first","nameid":"second"}`))
	f.Add([]byte(`{"attributes":{"groups":["users"]}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = providerReference(json.RawMessage(raw))
	})
}

func validSuccessMapperDecision(providerAttributes string) forwardapp.Decision {
	principal := auth.User{
		ID: "7", UserID: "7", Email: "admin@example.test", AuthType: "session",
	}
	authorization := browserapp.Authorization{
		Principal: principal, Provider: "form", ProviderAttributes: json.RawMessage(providerAttributes),
	}
	return forwardapp.Decision{
		Kind:   forwardapp.DecisionAllow,
		Reason: forwardapp.ReasonBrowserSessionAccepted,
		Source: mapperSource(),
		Authentication: forwardapp.Authentication{
			Type:                 forwardapp.AuthenticationUser,
			Principal:            principal,
			Reference:            "-",
			BrowserAuthorization: authorization,
		},
	}
}

func mapperSource() forwardapp.Source {
	return forwardapp.Source{
		Method: "GET", Proto: "https", Host: "elitea.example.test", URI: "/grafana", IP: "192.0.2.7",
		Target: "header", TargetPresent: true, Scope: "grafana", ScopePresent: true,
	}
}
