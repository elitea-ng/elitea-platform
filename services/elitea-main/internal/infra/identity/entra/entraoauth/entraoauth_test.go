package entraoauth_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entraoauth"
)

func TestClassifyReadsCodesNotText(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want entraoauth.Kind
	}{
		"disabled":         {`{"error":"unauthorized_client","error_codes":[7000112]}`, entraoauth.KindApplicationDisabled},
		"agent consent":    {`{"error":"invalid_grant","error_codes":[82014,65001]}`, entraoauth.KindAgentConsent},
		"claims":           {`{"error":"interaction_required","claims":"{}"}`, entraoauth.KindClaimsChallenge},
		"claims on other":  {`{"error":"invalid_client","claims":"{}"}`, entraoauth.KindRejected},
		"audience":         {`{"error":"invalid_grant","error_codes":[50013]}`, entraoauth.KindAudienceMismatch},
		"interaction":      {`{"error":"interaction_required"}`, entraoauth.KindInteractionRequired},
		"mfa code":         {`{"error":"invalid_grant","error_codes":[50076]}`, entraoauth.KindInteractionRequired},
		"suberror":         {`{"error":"invalid_grant","suberror":"token_expired"}`, entraoauth.KindInteractionRequired},
		"plain invalid":    {`{"error":"invalid_grant"}`, entraoauth.KindRejected},
		"server error":     {`{"error":"server_error"}`, entraoauth.KindUnavailable},
		"not json":         {`<html>`, entraoauth.KindUnavailable},
		"description lies": {`{"error":"invalid_client","error_description":"AADSTS7000112 interaction_required"}`, entraoauth.KindRejected},
		"oversized claims": {`{"error":"interaction_required","claims":"` + strings.Repeat("x", entraoauth.MaxClaimsBytes+1) + `"}`, entraoauth.KindRejected},
	} {
		if got := entraoauth.Classify([]byte(tc.body)).Kind; got != tc.want {
			t.Errorf("%s: kind %d, want %d", name, got, tc.want)
		}
	}
	if c := entraoauth.Classify([]byte(`{"error":"interaction_required","claims":"abc"}`)); c.Claims != "abc" {
		t.Fatalf("claims %q", c.Claims)
	}
}

func TestExpiresInAndPayload(t *testing.T) {
	for raw, want := range map[string]int64{`3599`: 3599, `"3599"`: 3599, `0`: 0, `"x"`: 0, ``: 0} {
		got, _ := entraoauth.ExpiresInSeconds(json.RawMessage(raw))
		if got != want {
			t.Errorf("%q: %d", raw, got)
		}
	}
	var payload struct {
		TID string `json:"tid"`
	}
	if !entraoauth.UnverifiedPayload("e30.eyJ0aWQiOiJ0In0.sig", &payload) || payload.TID != "t" {
		t.Fatalf("payload %+v", payload)
	}
	if entraoauth.UnverifiedPayload("not-a-jwt", &payload) {
		t.Fatal("decoded a non-JWT")
	}
}
