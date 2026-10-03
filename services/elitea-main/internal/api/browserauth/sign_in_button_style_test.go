package browserauth

// #6628: the "Sign in" button on the form login page and the "Continue"
// button on the SSO chooser are 40px tall with 14px text. Both pages share
// templates/auth.css.

import (
	"regexp"
	"strings"
	"testing"
)

// signInButtonRule returns the body of the first `.sign-in-button { ... }`
// rule in a stylesheet. The hover and focus rules have their own selectors
// and are not matched.
func signInButtonRule(t *testing.T, css string) string {
	t.Helper()
	match := regexp.MustCompile(`(?s)\n\.sign-in-button \{(.*?)\n\}`).FindStringSubmatch(css)
	if match == nil {
		t.Fatal("the stylesheet has no .sign-in-button rule")
	}
	return match[1]
}

func TestSignInButtonsAre40PixelsTallWith14PixelText(t *testing.T) {
	rule := signInButtonRule(t, authStyleSource)
	for _, want := range []string{"height: 40px;", "font-size: 14px;", "padding: 0 16px;"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".sign-in-button lacks %q; rule body:\n%s", want, rule)
		}
	}
	// A min-height or a vertical padding would let the button grow past 40px
	// again.
	if strings.Contains(rule, "min-height") {
		t.Errorf(".sign-in-button still sets min-height; rule body:\n%s", rule)
	}
}
