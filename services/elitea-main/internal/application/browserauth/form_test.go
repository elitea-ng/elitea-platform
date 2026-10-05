package browserauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

func TestFormAssertionPreservesCurrentIdentitySemantics(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	configuration := []byte(`{
		"users": [
			{"login":"first","password":"first-secret"},
			{
				"login":"Roman.Mitusov",
				"password":"correct horse battery staple",
				"attributes":{
					"email":"ROMAN@example.test",
					"given_name":"Roman",
					"family_name":"Mitusov",
					"name":"Ignored when both component names exist",
					"groups":["editor","viewer"]
				}
			}
		]
	}`)
	provider, err := newFormProvider(configuration, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for index := range configuration {
		configuration[index] = 'x'
	}

	submission := FormSubmission{Login: "Roman.Mitusov", Password: "correct horse battery staple"}
	verifier := provider.AssertionVerifier(submission)
	if retained := fmt.Sprintf("%+v %+v", provider, verifier); strings.Contains(retained, submission.Password) ||
		strings.Contains(retained, "first-secret") {
		t.Fatalf("provider or verifier retained a raw password: %s", retained)
	}

	assertion, err := verifier.Verify(context.Background(), formVerification("origin-session-42"))
	if err != nil {
		t.Fatal(err)
	}
	if assertion.Provider != FormProviderName || assertion.ProviderReference != submission.Login ||
		assertion.Email != "ROMAN@example.test" || assertion.GivenName != "Roman" ||
		assertion.FamilyName != "Mitusov" || assertion.Name != "Ignored when both component names exist" {
		t.Fatalf("assertion identity = %+v", assertion)
	}
	if assertion.Expiration == nil || !assertion.Expiration.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("expiration = %v, want %v", assertion.Expiration, now.Add(24*time.Hour))
	}
	if assertion.ProtocolCorrelation != (browserflow.ProtocolCorrelation{}) {
		t.Fatalf("protocol correlation = %+v, want empty", assertion.ProtocolCorrelation)
	}

	var providerAttributes struct {
		NameID       string         `json:"nameid"`
		Attributes   map[string]any `json:"attributes"`
		SessionIndex string         `json:"sessionindex"`
	}
	if err := json.Unmarshal(assertion.ProviderAttributes, &providerAttributes); err != nil {
		t.Fatal(err)
	}
	if providerAttributes.NameID != submission.Login || providerAttributes.SessionIndex != "origin-session-42" {
		t.Fatalf("provider attributes = %+v", providerAttributes)
	}
	groups, ok := providerAttributes.Attributes["groups"].([]any)
	if !ok || len(groups) != 2 || groups[0] != "editor" || groups[1] != "viewer" {
		t.Fatalf("configured attributes were not preserved: %+v", providerAttributes.Attributes)
	}
	encoded, err := json.Marshal(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(submission.Password)) || bytes.Contains(encoded, []byte("first-secret")) {
		t.Fatalf("assertion contains a raw password: %s", encoded)
	}
}

// A Form user with no configured address used to sign in as
// `<login>@centry.user`: the adapter left the email empty and identity
// provisioning synthesized one in the system-identity domain, which hides the
// account from the Users page and from analytics. Now the user is a
// configuration error: the document still loads (one bad user must not lock
// every other user out), the load reports the login, and a sign-in with the
// RIGHT password is refused with the generic failure and a logged cause.
func TestFormUserWithoutEmailIsRefusedNotSynthesized(t *testing.T) {
	logs := captureDefaultLog(t)

	provider, err := NewFormProvider([]byte(`{"users":[
		{"login":"admin","password":"secret"},
		{"login":"system-ish","password":"secret","email":"someone@CENTRY.user"},
		{"login":"attr-reserved","password":"secret","attributes":{"email":"x@centry.user"}},
		{"login":"ok","password":"secret","email":"ok@example.test"}
	]}`))
	if err != nil {
		t.Fatalf("one misconfigured user must not reject the whole document: %v", err)
	}
	if got, want := provider.MisconfiguredLogins(), []string{"admin", "system-ish", "attr-reserved"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("misconfigured logins = %q, want %q", got, want)
	}
	if provider.UserCount() != 4 {
		t.Fatalf("user count = %d, want 4", provider.UserCount())
	}

	for _, login := range []string{"admin", "system-ish", "attr-reserved"} {
		assertion, err := provider.NewVerifier(login, "secret").Verify(context.Background(), formVerification("origin-session"))
		if !errors.Is(err, ErrUnauthenticated) || err.Error() != ErrUnauthenticated.Error() ||
			!reflect.DeepEqual(assertion, browserflow.VerifiedAssertion{}) {
			t.Fatalf("%s: assertion = %+v, error = %v; want the generic %v", login, assertion, err, ErrUnauthenticated)
		}
		if strings.Contains(err.Error(), "centry.user") {
			t.Fatalf("%s: the user-facing error names the configuration problem: %v", login, err)
		}
	}
	logged := logs.String()
	if !strings.Contains(logged, "no email address configured") || !strings.Contains(logged, "login=admin") {
		t.Fatalf("the configuration error was not logged with the login:\n%s", logged)
	}

	assertion, err := provider.NewVerifier("ok", "secret").Verify(context.Background(), formVerification("origin-session"))
	if err != nil || assertion.Email != "ok@example.test" {
		t.Fatalf("a configured user must still sign in: assertion = %+v, error = %v", assertion, err)
	}
}

// The Admin schema writes the address at the TOP level, and that field used to
// be accepted and then thrown away. It is now the primary source; the
// attribute is the fallback.
func TestFormProviderReadsTopLevelAdminSchemaEmail(t *testing.T) {
	t.Parallel()

	provider, err := NewFormProvider([]byte(`{"users":[
		{"login":"top","password":"secret","email":"Top@Example.test"},
		{"login":"both","password":"secret","email":"top@example.test","attributes":{"email":"attr@example.test"}},
		{"login":"attr","password":"secret","attributes":{"email":"attr@example.test"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if logins := provider.MisconfiguredLogins(); len(logins) != 0 {
		t.Fatalf("misconfigured logins = %q, want none", logins)
	}
	for login, want := range map[string]string{
		"top":  "Top@Example.test",
		"both": "top@example.test",
		"attr": "attr@example.test",
	} {
		assertion, err := provider.NewVerifier(login, "secret").Verify(context.Background(), formVerification("origin-session"))
		if err != nil {
			t.Fatalf("%s: %v", login, err)
		}
		if assertion.Email != want {
			t.Fatalf("%s: email = %q, want %q", login, assertion.Email, want)
		}
	}
}

func TestFormProviderRejectsInvalidOrUnboundedConfiguration(t *testing.T) {
	t.Parallel()

	invalidUTF8 := append(
		[]byte(`{"users":[{"login":"admin","password":"`),
		append([]byte{0xff}, []byte(`"}]}`)...)...,
	)
	tooManyUsers := make([]string, 0, MaxFormUsers+1)
	for index := range MaxFormUsers + 1 {
		tooManyUsers = append(tooManyUsers, fmt.Sprintf(`{"login":"user-%d","password":"secret"}`, index))
	}
	deepAttributes := "{}"
	for range maxFormConfigurationNesting {
		deepAttributes = `{"next":` + deepAttributes + `}`
	}

	tests := []struct {
		name          string
		configuration []byte
	}{
		{name: "empty", configuration: nil},
		{name: "oversized document", configuration: bytes.Repeat([]byte("x"), MaxFormConfigurationBytes+1)},
		{name: "invalid UTF-8", configuration: invalidUTF8},
		{name: "malformed", configuration: []byte(`{"users":[`)},
		{name: "trailing value", configuration: []byte(`{"users":[]} {}`)},
		{name: "root array", configuration: []byte(`[]`)},
		{name: "missing users", configuration: []byte(`{}`)},
		{name: "null users", configuration: []byte(`{"users":null}`)},
		{name: "unknown root field", configuration: []byte(`{"users":[],"mode":"form"}`)},
		{name: "unknown user field", configuration: []byte(`{"users":[{"login":"admin","password":"secret","mode":"local"}]}`)},
		{name: "duplicate root member", configuration: []byte(`{"users":[],"users":[]}`)},
		{name: "duplicate user member", configuration: []byte(`{"users":[{"login":"admin","login":"other","password":"secret"}]}`)},
		{name: "duplicate attribute member", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":{"email":"one@example.test","email":"two@example.test"}}]}`)},
		{name: "duplicate login", configuration: []byte(`{"users":[{"login":"admin","password":"one"},{"login":"admin","password":"two"}]}`)},
		{name: "too many users", configuration: []byte(`{"users":[` + strings.Join(tooManyUsers, ",") + `]}`)},
		{name: "missing login", configuration: []byte(`{"users":[{"password":"secret"}]}`)},
		{name: "blank login", configuration: []byte(`{"users":[{"login":"  ","password":"secret"}]}`)},
		{name: "login control", configuration: []byte(`{"users":[{"login":"admin\nother","password":"secret"}]}`)},
		{name: "oversized login", configuration: formConfigurationJSON(strings.Repeat("l", browserflow.MaxProviderReferenceBytes+1), "secret", nil)},
		{name: "missing password", configuration: []byte(`{"users":[{"login":"admin"}]}`)},
		{name: "blank password", configuration: []byte(`{"users":[{"login":"admin","password":"  "}]}`)},
		{name: "password control", configuration: []byte(`{"users":[{"login":"admin","password":"secret\nother"}]}`)},
		{name: "oversized password", configuration: formConfigurationJSON("admin", strings.Repeat("p", MaxFormPasswordBytes+1), nil)},
		{name: "null attributes", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":null}]}`)},
		{name: "array attributes", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":[]}]}`)},
		{name: "non-string email", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":{"email":["admin@example.test"]}}]}`)},
		{name: "top-level email whitespace", configuration: []byte(`{"users":[{"login":"admin","password":"secret","email":"admin @example.test"}]}`)},
		{name: "top-level email control", configuration: []byte(`{"users":[{"login":"admin","password":"secret","email":"admin\u0007@example.test"}]}`)},
		{name: "email whitespace", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":{"email":"admin @example.test"}}]}`)},
		{name: "name control", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":{"name":"Admin\nUser"}}]}`)},
		{name: "excessive nesting", configuration: []byte(`{"users":[{"login":"admin","password":"secret","attributes":` + deepAttributes + `}]}`)},
		{name: "provider attributes too large", configuration: formConfigurationJSON("admin", "secret", map[string]any{
			"value": strings.Repeat("a", MaxFormConfigurationBytes/2),
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewFormProvider(test.configuration)
			if provider != nil || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("provider = %v, error = %v; want nil, %v", provider, err, ErrInvalidConfiguration)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("configuration error leaked a password: %v", err)
			}
		})
	}
}

func TestFormVerifierUsesOneGenericFailureForInvalidCredentialsAndContext(t *testing.T) {
	t.Parallel()

	provider, err := NewFormProvider([]byte(`{"users":[
		{"login":"first","password":"first-secret"},
		{"login":"second","password":"second-secret"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	validContext := formVerification("origin-session")
	tests := []struct {
		name       string
		provider   *FormProvider
		submission FormSubmission
		context    browserflow.VerificationContext
	}{
		{name: "unknown login", provider: provider, submission: FormSubmission{Login: "missing", Password: "second-secret"}, context: validContext},
		{name: "wrong password", provider: provider, submission: FormSubmission{Login: "second", Password: "wrong-password"}, context: validContext},
		{name: "empty login", provider: provider, submission: FormSubmission{Password: "second-secret"}, context: validContext},
		{name: "oversized password", provider: provider, submission: FormSubmission{Login: "second", Password: strings.Repeat("p", MaxFormPasswordBytes+1)}, context: validContext},
		{name: "password control", provider: provider, submission: FormSubmission{Login: "second", Password: "second-secret\n"}, context: validContext},
		{name: "wrong provider", provider: provider, submission: FormSubmission{Login: "second", Password: "second-secret"}, context: browserflow.VerificationContext{Provider: "oidc", OriginatingSessionID: "origin-session"}},
		{name: "missing originating session", provider: provider, submission: FormSubmission{Login: "second", Password: "second-secret"}, context: browserflow.VerificationContext{Provider: FormProviderName}},
		{name: "form correlation", provider: provider, submission: FormSubmission{Login: "second", Password: "second-secret"}, context: browserflow.VerificationContext{Provider: FormProviderName, OriginatingSessionID: "origin-session", Correlation: browserflow.ProtocolCorrelation{Nonce: "unexpected"}}},
		{name: "form provider state", provider: provider, submission: FormSubmission{Login: "second", Password: "second-secret"}, context: browserflow.VerificationContext{Provider: FormProviderName, OriginatingSessionID: "origin-session", ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)}}},
		{name: "nil provider", provider: nil, submission: FormSubmission{Login: "second", Password: "second-secret"}, context: validContext},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertion, err := test.provider.AssertionVerifier(test.submission).Verify(context.Background(), test.context)
			if !reflect.DeepEqual(assertion, browserflow.VerifiedAssertion{}) || !errors.Is(err, ErrUnauthenticated) ||
				err.Error() != ErrUnauthenticated.Error() {
				t.Fatalf("assertion = %+v, error = %v; want zero assertion and %v", assertion, err, ErrUnauthenticated)
			}
			if test.submission.Login != "" && strings.Contains(err.Error(), test.submission.Login) ||
				test.submission.Password != "" && strings.Contains(err.Error(), test.submission.Password) {
				t.Fatalf("authentication error leaked submitted credentials: %v", err)
			}
		})
	}
}

func TestFormProviderFactoryDoesNotDiscloseSubmissionValidity(t *testing.T) {
	t.Parallel()

	provider, err := NewFormProvider([]byte(`{"users":[{"login":"admin","password":"secret"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, submission := range []FormSubmission{
		{Login: "admin", Password: "secret"},
		{Login: "admin", Password: "wrong"},
		{Login: "missing", Password: "secret"},
		{},
	} {
		verifier := provider.NewVerifier(submission.Login, submission.Password)
		if verifier == nil {
			t.Fatalf("NewVerifier(%q, <redacted>) returned nil", submission.Login)
		}
	}
}

func TestFormProviderUsesPerSnapshotCredentialDigests(t *testing.T) {
	t.Parallel()

	configuration := []byte(`{"users":[{"login":"admin","password":"secret"}]}`)
	first, err := NewFormProvider(configuration)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFormProvider(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if first.digestKey == second.digestKey || first.users[0].passwordDigest == second.users[0].passwordDigest {
		t.Fatal("separate Form snapshots reused deterministic credential digests")
	}
}

func TestFormVerifierPreservesCancellationBeforeAuthentication(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var verifier *FormAssertionVerifier
	if _, err := verifier.Verify(ctx, browserflow.VerificationContext{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestVerificationContextDoesNotSerializeServerOnlyState(t *testing.T) {
	t.Parallel()

	verification := formVerification("origin-session")
	verification.ProviderState.PKCEVerifier = strings.Repeat("v", browserflow.MinPKCEVerifierBytes)
	encoded, err := json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("serialized verification context = %s, want {}", encoded)
	}
}

func TestFormProviderSnapshotSupportsConcurrentVerification(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	provider, err := newFormProvider([]byte(`{"users":[
		{"login":"first","password":"first-secret","email":"first@example.test"},
		{"login":"second","password":"second-secret","email":"second@example.test"}
	]}`), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier := provider.AssertionVerifier(FormSubmission{Login: "second", Password: "second-secret"})

	var wait sync.WaitGroup
	errorsFromVerification := make(chan error, 32)
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			assertion, verifyErr := verifier.Verify(context.Background(), formVerification("origin-session"))
			if verifyErr != nil {
				errorsFromVerification <- verifyErr
				return
			}
			if assertion.ProviderReference != "second" || assertion.Expiration == nil ||
				!assertion.Expiration.Equal(now.Add(FormAuthenticationLifetime)) {
				errorsFromVerification <- fmt.Errorf("unexpected assertion: %+v", assertion)
			}
		}()
	}
	wait.Wait()
	close(errorsFromVerification)
	for err := range errorsFromVerification {
		t.Error(err)
	}
}

func formVerification(originatingSessionID string) browserflow.VerificationContext {
	return browserflow.VerificationContext{
		Provider:             FormProviderName,
		OriginatingSessionID: originatingSessionID,
	}
}

func formConfigurationJSON(login string, password string, attributes any) []byte {
	user := map[string]any{"login": login, "password": password}
	if attributes != nil {
		user["attributes"] = attributes
	}
	encoded, err := json.Marshal(map[string]any{"users": []any{user}})
	if err != nil {
		panic(err)
	}
	return encoded
}
