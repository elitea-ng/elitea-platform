package browserauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

const (
	FormProviderName            = "form"
	FormAuthenticationLifetime  = 24 * time.Hour
	MaxFormConfigurationBytes   = 1 << 20
	MaxFormUsers                = 256
	MaxFormPasswordBytes        = 4096
	maxFormConfigurationNesting = sessionstate.MaxProviderAttributesNesting
	formLoginDigestDomain       = "form-login\x00"
	formPasswordDigestDomain    = "form-password\x00"
)

type formConfiguration struct {
	Users []formUserConfiguration `json:"users"`
}

type formUserConfiguration struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	// Email is the Admin schema's field for the account address, and it is
	// the PRIMARY source of it. attributes.email is the fallback. A user with
	// neither is a configuration error (see formUser.misconfigured); no
	// address is ever synthesized from the login.
	Email      string          `json:"email,omitempty"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

type formUser struct {
	login          string
	loginDigest    [sha256.Size]byte
	passwordDigest [sha256.Size]byte
	attributes     json.RawMessage
	email          string
	givenName      string
	familyName     string
	name           string
	// misconfigured marks a user whose configuration names no usable address:
	// no top-level email, no attributes.email, or an address in the reserved
	// system-identity domain. The user's credentials still verify (so a
	// mismatch and a misconfiguration take the same time and give the same
	// answer to anyone who does not hold the password), but a MATCH is
	// refused with a logged configuration error instead of signing in.
	misconfigured bool
}

// ReservedEmailDomain is the system-identity domain. Addresses in it belong
// to platform accounts (system@, system_user_<n>@); the admin surface, SCIM and
// analytics all treat `%@centry.user` as "not a person". A Form user must never
// be given one, neither by synthesis nor by configuration.
const ReservedEmailDomain = "@centry.user"

// FormProvider is an immutable, concurrency-safe snapshot of configured Form
// users. It retains password digests rather than raw configured passwords.
type FormProvider struct {
	users     []formUser
	digestKey [sha256.Size]byte
	now       func() time.Time
}

// FormSubmission is request-owned input. AssertionVerifier hashes its fields
// immediately and does not retain either raw value.
type FormSubmission struct {
	Login    string
	Password string
}

// FormAssertionVerifier owns only fixed-size submitted credential digests and
// a reference to the immutable provider snapshot.
type FormAssertionVerifier struct {
	provider       *FormProvider
	loginDigest    [sha256.Size]byte
	passwordDigest [sha256.Size]byte
	invalid        bool
}

// NewFormProvider parses one complete, bounded Form-provider configuration.
// Unknown fields, duplicate JSON member names, duplicate logins, and values
// that cannot produce the bounded browser-session assertion are rejected.
func NewFormProvider(rawConfiguration []byte) (*FormProvider, error) {
	return newFormProvider(rawConfiguration, time.Now)
}

func newFormProvider(rawConfiguration []byte, now func() time.Time) (*FormProvider, error) {
	if now == nil || len(rawConfiguration) == 0 || len(rawConfiguration) > MaxFormConfigurationBytes ||
		!utf8.Valid(rawConfiguration) || validateUniqueJSON(rawConfiguration) != nil {
		return nil, ErrInvalidConfiguration
	}

	decoder := json.NewDecoder(bytes.NewReader(rawConfiguration))
	decoder.DisallowUnknownFields()
	var configuration formConfiguration
	if err := decoder.Decode(&configuration); err != nil {
		return nil, ErrInvalidConfiguration
	}
	if err := requireJSONEOF(decoder); err != nil || configuration.Users == nil ||
		len(configuration.Users) > MaxFormUsers {
		return nil, ErrInvalidConfiguration
	}
	var digestKey [sha256.Size]byte
	if _, err := rand.Read(digestKey[:]); err != nil {
		return nil, ErrDependencyUnavailable
	}

	users := make([]formUser, 0, len(configuration.Users))
	logins := make(map[string]struct{}, len(configuration.Users))
	for _, configured := range configuration.Users {
		user, err := formUserFromConfiguration(configured, digestKey)
		if err != nil {
			return nil, ErrInvalidConfiguration
		}
		if _, duplicate := logins[user.login]; duplicate {
			return nil, ErrInvalidConfiguration
		}
		logins[user.login] = struct{}{}
		users = append(users, user)
	}

	return &FormProvider{users: users, digestKey: digestKey, now: now}, nil
}

// UserCount reports how many Form users the configuration names.
func (p *FormProvider) UserCount() int {
	if p == nil {
		return 0
	}
	return len(p.users)
}

// MisconfiguredLogins lists the logins whose configuration names no usable
// email address, in configuration order. They are reported at load and
// refused at sign-in; a login is an operator-chosen name, not a secret.
func (p *FormProvider) MisconfiguredLogins() []string {
	if p == nil {
		return nil
	}
	var logins []string
	for index := range p.users {
		if p.users[index].misconfigured {
			logins = append(logins, p.users[index].login)
		}
	}
	return logins
}

// AssertionVerifier snapshots the submitted credentials as digests. Invalid
// submissions deliberately produce the same verifier error as a mismatch.
func (p *FormProvider) AssertionVerifier(submission FormSubmission) *FormAssertionVerifier {
	verifier := &FormAssertionVerifier{provider: p}
	if p == nil || !validFormLogin(submission.Login) || !validFormPassword(submission.Password) {
		verifier.invalid = true
		return verifier
	}
	verifier.loginDigest = credentialDigest(p.digestKey, formLoginDigestDomain, submission.Login)
	verifier.passwordDigest = credentialDigest(p.digestKey, formPasswordDigestDomain, submission.Password)
	return verifier
}

// NewVerifier adapts the immutable Form provider to the browser HTTP boundary.
// Submission validity is deliberately not exposed here: malformed and
// mismatched credentials are resolved by Verify as the same generic
// ErrUnauthenticated result after the bound transaction is consumed.
func (p *FormProvider) NewVerifier(login string, password string) AssertionVerifier {
	return p.AssertionVerifier(FormSubmission{Login: login, Password: password})
}

// Verify compares every configured user before deciding whether the
// credentials match. Its only non-context failure is ErrUnauthenticated.
func (v *FormAssertionVerifier) Verify(
	ctx context.Context,
	verification browserflow.VerificationContext,
) (browserflow.VerifiedAssertion, error) {
	if err := ctx.Err(); err != nil {
		return browserflow.VerifiedAssertion{}, err
	}
	if v == nil || v.provider == nil || v.provider.now == nil ||
		verification.Provider != FormProviderName ||
		browserflow.ValidateOpaqueID(verification.OriginatingSessionID) != nil ||
		verification.Correlation != (browserflow.ProtocolCorrelation{}) ||
		verification.ProviderState != (browserflow.ProviderState{}) {
		return browserflow.VerifiedAssertion{}, ErrUnauthenticated
	}

	matchedIndex := 0
	matches := 0
	for index := range v.provider.users {
		user := &v.provider.users[index]
		loginMatch := subtle.ConstantTimeCompare(v.loginDigest[:], user.loginDigest[:])
		passwordMatch := subtle.ConstantTimeCompare(v.passwordDigest[:], user.passwordDigest[:])
		match := loginMatch & passwordMatch
		matchedIndex = subtle.ConstantTimeSelect(match, index, matchedIndex)
		matches += match
	}
	if v.invalid || matches != 1 {
		return browserflow.VerifiedAssertion{}, ErrUnauthenticated
	}

	user := v.provider.users[matchedIndex]
	if user.misconfigured {
		// The password was right; the CONFIGURATION is wrong. The browser
		// gets the generic sign-in failure, and the operator gets the cause.
		slog.ErrorContext(ctx, "Form sign-in refused: the Form user has no email address configured",
			"login", user.login,
			"fix", "set the user's top-level \"email\" (or attributes.email) in the Form users JSON to the person's real address; "+
				"addresses in the reserved "+ReservedEmailDomain+" domain are refused")
		return browserflow.VerifiedAssertion{}, ErrUnauthenticated
	}
	providerAttributes, err := json.Marshal(struct {
		NameID       string          `json:"nameid"`
		Attributes   json.RawMessage `json:"attributes"`
		SessionIndex string          `json:"sessionindex"`
	}{
		NameID:     user.login,
		Attributes: user.attributes,
		// The unmounted target keeps the HTTP cookie's version prefix out of
		// application state. Current Form stores the raw pre-regeneration Flask
		// cookie here, so cutover must explicitly disposition this observable
		// representation difference.
		SessionIndex: verification.OriginatingSessionID,
	})
	if err != nil || len(providerAttributes) > sessionstate.MaxProviderAttributesBytes {
		return browserflow.VerifiedAssertion{}, ErrUnauthenticated
	}

	now := v.provider.now().UTC()
	if now.IsZero() {
		return browserflow.VerifiedAssertion{}, ErrUnauthenticated
	}
	expiration := now.Add(FormAuthenticationLifetime)
	assertion := browserflow.VerifiedAssertion{
		Provider:            FormProviderName,
		ProviderReference:   user.login,
		Email:               user.email,
		GivenName:           user.givenName,
		FamilyName:          user.familyName,
		Name:                user.name,
		ProviderAttributes:  providerAttributes,
		Expiration:          &expiration,
		ProtocolCorrelation: verification.Correlation,
	}
	if assertion.Validate() != nil || authenticatedState(assertion, 1).Validate() != nil {
		return browserflow.VerifiedAssertion{}, ErrUnauthenticated
	}
	return assertion, nil
}

func formUserFromConfiguration(configured formUserConfiguration, digestKey [sha256.Size]byte) (formUser, error) {
	if !validFormLogin(configured.Login) || !validFormPassword(configured.Password) {
		return formUser{}, ErrInvalidConfiguration
	}
	attributes := configured.Attributes
	if len(attributes) == 0 {
		attributes = json.RawMessage("{}")
	}
	if len(attributes) > sessionstate.MaxProviderAttributesBytes ||
		!jsonObject(attributes) {
		return formUser{}, ErrInvalidConfiguration
	}

	claims, err := formIdentityClaims(attributes)
	if err != nil {
		return formUser{}, ErrInvalidConfiguration
	}
	// The top-level field wins: it is what the Admin schema writes. Validate
	// it with the same rule as an attribute-supplied address.
	if configured.Email != "" {
		if !validFormEmail(configured.Email) {
			return formUser{}, ErrInvalidConfiguration
		}
		claims.Email = configured.Email
	}
	misconfigured := strings.TrimSpace(claims.Email) == "" ||
		strings.HasSuffix(strings.ToLower(claims.Email), ReservedEmailDomain)
	worstCaseAttributes, err := json.Marshal(struct {
		NameID       string          `json:"nameid"`
		Attributes   json.RawMessage `json:"attributes"`
		SessionIndex string          `json:"sessionindex"`
	}{
		NameID:       configured.Login,
		Attributes:   attributes,
		SessionIndex: strings.Repeat(`\`, browserflow.MaxOpaqueIDBytes),
	})
	if err != nil {
		return formUser{}, ErrInvalidConfiguration
	}
	provider := FormProviderName
	if (sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Provider:           &provider,
		ProviderAttributes: worstCaseAttributes,
	}).Validate() != nil {
		return formUser{}, ErrInvalidConfiguration
	}

	return formUser{
		login:          strings.Clone(configured.Login),
		loginDigest:    credentialDigest(digestKey, formLoginDigestDomain, configured.Login),
		passwordDigest: credentialDigest(digestKey, formPasswordDigestDomain, configured.Password),
		attributes:     append(json.RawMessage(nil), attributes...),
		email:          claims.Email,
		givenName:      claims.GivenName,
		familyName:     claims.FamilyName,
		name:           claims.Name,
		misconfigured:  misconfigured,
	}, nil
}

func validFormEmail(value string) bool {
	return (browserflow.VerifiedAssertion{
		Provider:          FormProviderName,
		ProviderReference: "probe",
		Email:             value,
	}).Validate() == nil && !strings.ContainsFunc(value, unicode.IsSpace)
}

func credentialDigest(key [sha256.Size]byte, domain string, value string) [sha256.Size]byte {
	digest := hmac.New(sha256.New, key[:])
	_, _ = digest.Write([]byte(domain))
	_, _ = digest.Write([]byte(value))
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

type formClaims struct {
	Email      string
	GivenName  string
	FamilyName string
	Name       string
}

func formIdentityClaims(attributes json.RawMessage) (formClaims, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(attributes, &values); err != nil {
		return formClaims{}, err
	}
	claims := formClaims{}
	for key, target := range map[string]*string{
		"email":       &claims.Email,
		"given_name":  &claims.GivenName,
		"family_name": &claims.FamilyName,
		"name":        &claims.Name,
	} {
		value, present := values[key]
		if !present {
			continue
		}
		if err := json.Unmarshal(value, target); err != nil {
			return formClaims{}, err
		}
	}
	probe := browserflow.VerifiedAssertion{
		Provider:          FormProviderName,
		ProviderReference: "probe",
		Email:             claims.Email,
		GivenName:         claims.GivenName,
		FamilyName:        claims.FamilyName,
		Name:              claims.Name,
	}
	if probe.Validate() != nil {
		return formClaims{}, ErrInvalidConfiguration
	}
	return claims, nil
}

func validFormLogin(value string) bool {
	return validFormText(value, browserflow.MaxProviderReferenceBytes)
}

func validFormPassword(value string) bool {
	return validFormText(value, MaxFormPasswordBytes)
}

func validFormText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) && strings.TrimSpace(value) != "" &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func jsonObject(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	return object != nil
}

func validateUniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder, 1); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func consumeUniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxFormConfigurationNesting {
		return ErrInvalidConfiguration
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalidConfiguration
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrInvalidConfiguration
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrInvalidConfiguration
		}
		return nil
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrInvalidConfiguration
		}
		return nil
	default:
		return ErrInvalidConfiguration
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrInvalidConfiguration
		}
		return err
	}
	return nil
}
