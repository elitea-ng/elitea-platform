package browserauth

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestBrowserAttemptValidateAcceptsExactStageShapes(t *testing.T) {
	digest := sha256.Sum256([]byte("admin"))
	for _, attempt := range []BrowserAttempt{
		{ClientKey: "192.0.2.7", Stage: BrowserAttemptFormBegin},
		{ClientKey: "2001:db8::7", Stage: BrowserAttemptFormBegin},
		{ClientKey: "192.0.2.7", Stage: BrowserAttemptFormCredential, LoginDigest: digest},
	} {
		if err := attempt.Validate(); err != nil {
			t.Fatalf("attempt %+v: %v", attempt, err)
		}
	}
}

func TestBrowserAttemptValidateRejectsAmbiguousOrUnboundedShapes(t *testing.T) {
	digest := sha256.Sum256([]byte("admin"))
	for name, attempt := range map[string]BrowserAttempt{
		"missing client":       {Stage: BrowserAttemptFormBegin},
		"spaced client":        {ClientKey: "192.0.2.7 proxy", Stage: BrowserAttemptFormBegin},
		"oversized client":     {ClientKey: strings.Repeat("a", MaxBrowserAttemptClientKeyBytes+1), Stage: BrowserAttemptFormBegin},
		"unknown stage":        {ClientKey: "192.0.2.7", Stage: "future"},
		"missing login digest": {ClientKey: "192.0.2.7", Stage: BrowserAttemptFormCredential},
		"unexpected digest":    {ClientKey: "192.0.2.7", Stage: BrowserAttemptFormBegin, LoginDigest: digest},
		// The OIDC stages served only a handler that nothing composed. They
		// are gone, so an attempt naming one is an unknown stage.
		"retired OIDC begin":    {ClientKey: "192.0.2.7", Stage: "oidc_begin"},
		"retired OIDC callback": {ClientKey: "192.0.2.7", Stage: "oidc_callback"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := attempt.Validate(); !errors.Is(err, ErrInvalidBrowserAttempt) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
