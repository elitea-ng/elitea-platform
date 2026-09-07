package auth

// The browser SAML 2.0 login: metadata, authentication request, and the
// assertion consumer service.
//
// # What this replaces
//
// `legacy/plugins/auth_saml` — a pylon plugin that registers a provider with
// `auth_core` and spreads issuer, destination, NameID, ACS, signing,
// certificate and expiry behaviour through `routes/login.py`. There is no such
// plugin here, and until now no SAML at all: the type existed in
// `internal/auth/browserflow` as a provider string and nothing implemented it.
//
// The configuration is ONE typed provider revision
// (`elitea_auth.identity_providers`, shared migration 0095), authored on the
// admin Configuration page's Authentication section, and read here.
//
// # What is delegated, and what is emphatically not
//
// Verifying an XML digital signature is delegated to `gosaml2` — see
// saml_providers.go for why writing that here would be a bad idea. What is NOT
// delegated is the part of the contract that library leaves to its caller, and
// it is the part that matters most:
//
//	`RetrieveAssertionInfo` returns an EXPIRED assertion and one addressed to a
//	DIFFERENT AUDIENCE with `err == nil`. Both are reported on
//	`AssertionInfo.WarningInfo`, as `InvalidTime` and `NotInAudience`. A caller
//	that checks only the error accepts an assertion minted for another service
//	provider, and one minted last year.
//
// `assertionAcceptable` below is that check, and it is the reason this file
// does not simply hand `RetrieveAssertionInfo`'s output to the provisioning
// path.
//
// Three more checks are this file's own because the library performs none of
// them:
//
//   - **InResponseTo.** The library validates the response's `Destination`
//     against the ACS URL, and nothing about which login the response answers.
//     Without the check, an assertion captured from ANY login at this identity
//     provider can be posted to this endpoint. The identifier of the request
//     this browser started is held in a MAC-signed cookie and compared here.
//   - **Recipient.** `SubjectConfirmationData/@Recipient` names the endpoint
//     the assertion was minted for. The library checks the response-level
//     `Destination` only, and an identity provider that omits `Destination`
//     leaves nothing checked.
//   - **Clock skew.** The library has no tolerance: it flags `InvalidTime` the
//     moment `NotOnOrAfter` passes. The authored `clock_skew_seconds` is
//     applied by `conditionsAcceptable`, which re-reads the same two attributes
//     with the tolerance the operator authored.
//
// # The session this produces
//
// The same one the OIDC path produces: `resolveProvisionedUser` resolves the
// account from the provider subject, and `makeSessionToken` mints the
// `elitea_session` cookie. The subject is namespaced `saml:` so a NameID can
// never collide with an OIDC subject in the link table — the two are different
// identity spaces.
//
// The prefix alone does NOT stop an adoption, and reading it that way is what
// made this path unsafe on review. `joinAccountByEmail` also runs, and it
// decides by the email claim; its guard is what refuses an account another
// federated subject already holds. See FederatedRefPrefixes in oidc.go.

import (
	"context"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	saml2 "github.com/russellhaering/gosaml2"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/identityproviders"
)

const (
	SAMLMetadataPath = "/forward-auth/auth_saml/metadata"
	SAMLLoginPath    = "/forward-auth/auth_saml/login"
	SAMLACSPath      = "/forward-auth/auth_saml/acs"

	// samlRequestCookie holds the identifier of ONE authentication request,
	// with the redirect target, MAC-signed. See the InResponseTo note above:
	// this cookie is what binds an assertion to the login that asked for it.
	samlRequestCookie = "saml_request"

	// samlRequestLifetime bounds a login attempt. It is the same five minutes
	// the OIDC state and nonce cookies use.
	samlRequestLifetime = 300

	// maxSAMLResponseBytes bounds the posted form. A SAML response is a few
	// kilobytes; the bound is generous and exists so an unauthenticated POST
	// cannot ask this process to buffer an arbitrary body.
	maxSAMLResponseBytes = 1 << 20
)

// SAMLHandler serves the browser SAML login.
//
// Its CONFIGURATION is resolved per request rather than held here, so an
// operator authoring a provider on the admin page takes effect without a
// restart. See saml_providers.go.
type SAMLHandler struct {
	pool          *pgxpool.Pool
	secretKey     string
	secureCookies bool

	// firstLogin carries the same operator configuration the OIDC handler
	// holds, for the same reason: both planes provision, so both must apply it.
	firstLogin FirstLoginPolicy

	providers    IdentityProviderSource
	secretSource IdentitySecretSource

	// sessions and personalProjects are the two things router.go attaches
	// after construction. See OIDCHandler.WithSessionManager for why they are
	// setters.
	sessions         *browsersession.Manager
	personalProjects personalproject.AsyncEnsurer

	runtimeMu    sync.Mutex
	runtimeCache map[string]*samlRuntime
}

// WithSessionManager gives this plane the server-side session store.
//
// SAML is the plane that needs it most: a LogoutRequest names the assertion's
// SessionIndex, and until the row existed there was nowhere to keep one. The
// row now carries it, which is what makes single logout buildable at all —
// router.go still mounts `/auth_saml/logout` as a local clear, and that is the
// remaining half of the work.
func (h *SAMLHandler) WithSessionManager(manager *browsersession.Manager) *SAMLHandler {
	if h == nil || manager == nil {
		return h
	}
	h.sessions = manager
	return h
}

// WithPersonalProjectEnsurer is the sign-in half of the personal-project fix.
func (h *SAMLHandler) WithPersonalProjectEnsurer(ensurer personalproject.AsyncEnsurer) *SAMLHandler {
	if h == nil || ensurer == nil {
		return h
	}
	h.personalProjects = ensurer
	return h
}

// NewSAMLHandler builds the handler.
//
// There is no environment configuration to accept: SAML never had one, so a
// deployment federates SAML only through an authored provider.
func NewSAMLHandler(
	pool *pgxpool.Pool,
	secretKey string,
	providers IdentityProviderSource,
	secretSource IdentitySecretSource,
	secureCookies bool,
) *SAMLHandler {
	return &SAMLHandler{
		pool:          pool,
		secretKey:     secretKey,
		secureCookies: secureCookies,
		providers:     providers,
		secretSource:  secretSource,
		runtimeCache:  map[string]*samlRuntime{},
	}
}

// resolve answers the request itself when no provider is usable.
//
// The cause is logged and never returned: it names the identity provider, the
// vault entry or the database, and these responses go to an unauthenticated
// browser.
func (h *SAMLHandler) resolve(w http.ResponseWriter, r *http.Request) (*samlRuntime, bool) {
	runtime, err := h.runtime(r.Context())
	if err != nil {
		slog.Error("SAML: no usable identity provider", "err", err)
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return nil, false
	}
	return runtime, true
}

// Metadata answers `GET /forward-auth/auth_saml/metadata`.
//
// It is the document an operator uploads at the identity provider, and it is
// SERVED rather than pasted together by hand there: entity ID, ACS URL, NameID
// format and the signing certificate all come from the same authored row the
// login path reads, so the two cannot drift.
func (h *SAMLHandler) Metadata(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.resolve(w, r)
	if !ok {
		return
	}
	descriptor, err := runtime.provider.Metadata()
	if err != nil {
		slog.Error("SAML: metadata could not be built", "err", err)
		http.Error(w, "metadata is not available", http.StatusServiceUnavailable)
		return
	}
	document, err := xml.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		slog.Error("SAML: metadata could not be encoded", "err", err)
		http.Error(w, "metadata is not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(document)
}

// Login answers `GET /forward-auth/auth_saml/login`.
func (h *SAMLHandler) Login(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.resolve(w, r)
	if !ok {
		return
	}

	// The request document is built first so its ID can be remembered BEFORE
	// the browser leaves. Building the URL and then trying to recover the ID
	// from it would mean parsing back what was just encoded.
	//
	// NO EMBEDDED SIGNATURE, and the pairing below is not interchangeable. This
	// is the HTTP-Redirect binding, where SAML 2.0 puts the signature in the
	// QUERY STRING (SigAlg and Signature, over the deflated request) rather than
	// inside the XML — section 3.4.4.1 of the bindings specification. The
	// library's `BuildAuthRequestDocument` embeds an XML signature and its
	// `BuildAuthURLFromDocument` builds a POST-binding URL that does NOT
	// query-sign, so that pair produces a redirect whose signature most identity
	// providers ignore or reject. `NoSig` + `BuildAuthURLRedirect` is the pair
	// that signs the redirect the way the specification says.
	document, err := runtime.provider.BuildAuthRequestDocumentNoSig()
	if err != nil {
		slog.Error("SAML: authentication request could not be built", "err", err)
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return
	}
	requestID := document.Root().SelectAttrValue("ID", "")
	if requestID == "" {
		slog.Error("SAML: the authentication request carries no ID")
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return
	}

	target := safeRedirectTarget(r.URL.Query().Get("target_to"))
	http.SetCookie(w, &http.Cookie{
		Name:     samlRequestCookie,
		Value:    signBrowserValue(h.secretKey, requestID+"|"+target),
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		// Lax, not Strict: the identity provider POSTs the assertion back to
		// this origin from its own, and a Strict cookie is not sent on that
		// navigation — the login would fail with a missing request cookie on
		// every attempt.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   samlRequestLifetime,
	})

	// RelayState carries no meaning here. The redirect target is in the signed
	// cookie instead, because RelayState is echoed by the identity provider and
	// is therefore attacker-influenced — a target read from it would be an open
	// redirect through the login.
	authURL, err := runtime.provider.BuildAuthURLRedirect("", document)
	if err != nil {
		slog.Error("SAML: authentication request could not be encoded", "err", err)
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// ACS answers `POST /forward-auth/auth_saml/acs`.
func (h *SAMLHandler) ACS(w http.ResponseWriter, r *http.Request) {
	runtime, ok := h.resolve(w, r)
	if !ok {
		return
	}

	requestID, target, ok := h.consumeRequestCookie(w, r)
	if !ok {
		http.Error(w, "missing or invalid login request cookie", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSAMLResponseBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid assertion response", http.StatusBadRequest)
		return
	}
	encoded := r.PostFormValue("SAMLResponse")
	if encoded == "" {
		http.Error(w, "missing SAMLResponse", http.StatusBadRequest)
		return
	}

	assertion, err := runtime.provider.RetrieveAssertionInfo(encoded)
	if err != nil {
		// The library's error names the element or the signature that failed.
		// It is logged and not returned: the browser gets one sentence.
		slog.Error("SAML: the assertion was refused", "err", err, "provider", runtime.origin)
		http.Error(w, "assertion verification failed", http.StatusUnauthorized)
		return
	}
	if reason, acceptable := assertionAcceptable(assertion, runtime, requestID); !acceptable {
		slog.Error("SAML: the assertion is verified but not acceptable",
			"reason", reason, "provider", runtime.origin)
		http.Error(w, "assertion verification failed", http.StatusUnauthorized)
		return
	}

	email, name := samlIdentity(assertion, runtime.document)
	if email == "" {
		slog.Error("SAML: the assertion carries no email address", "provider", runtime.origin)
		http.Error(w, "email attribute required", http.StatusBadRequest)
		return
	}

	userID, err := h.provisionUser(r.Context(), assertion.NameID, email, name)
	if err != nil {
		h.writeProvisioningFailure(w, err, email)
		return
	}

	numericUserID, ok := sessionUserID(userID)
	if !ok {
		slog.Error("SAML: provisioning returned an unusable user id", "user_id", userID)
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}
	if !issueBrowserSession(w, h.sessions, h.secretKey, h.secureCookies, browsersession.NewSession{
		UserID:   numericUserID,
		Email:    email,
		Provider: browsersession.ProviderSAML,
		// The value a LogoutRequest must name. Empty when the identity
		// provider sent no SessionIndex, which is legal and only means single
		// logout cannot address this session.
		ProviderSessionIndex: assertion.SessionIndex,
	}, r) {
		return
	}
	ensurePersonalProject(h.personalProjects, userID)
	slog.Info("SAML login successful", "email", email, "user_id", userID, "provider", runtime.origin)
	http.Redirect(w, r, target, http.StatusFound)
}

// consumeRequestCookie clears the request cookie and returns what it held.
//
// It is cleared UNCONDITIONALLY and before the assertion is examined, which is
// what makes a login attempt single use: a second POST of the same assertion
// arrives with no cookie and is refused before any verification runs.
func (h *SAMLHandler) consumeRequestCookie(
	w http.ResponseWriter,
	r *http.Request,
) (requestID, target string, ok bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     samlRequestCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	cookie, err := r.Cookie(samlRequestCookie)
	if err != nil || cookie.Value == "" {
		return "", "", false
	}
	value, signed := verifyBrowserValue(h.secretKey, cookie.Value)
	if !signed {
		return "", "", false
	}
	separator := strings.Index(value, "|")
	if separator < 0 {
		return "", "", false
	}
	return value[:separator], safeRedirectTarget(value[separator+1:]), true
}

// assertionAcceptable applies every check `RetrieveAssertionInfo` leaves to its
// caller. It returns the reason for the log, never for the browser.
//
// READ THE HEADER OF THIS FILE BEFORE CHANGING IT. Two of these are warnings
// the library returns alongside `err == nil`, and dropping either accepts an
// assertion minted for a different service provider, or one that expired.
func assertionAcceptable(
	assertion *saml2.AssertionInfo,
	runtime *samlRuntime,
	requestID string,
) (string, bool) {
	if assertion.WarningInfo == nil {
		// The library always sets it. A nil is a shape this code does not
		// understand, and "no warnings" is the wrong thing to assume about a
		// structure that carries the audience and expiry verdicts.
		return "the assertion carries no verification result", false
	}
	if assertion.WarningInfo.NotInAudience {
		return "the assertion names a different audience", false
	}
	if assertion.WarningInfo.InvalidTime && !conditionsAcceptable(assertion, runtime.clockSkewSeconds) {
		return "the assertion is outside its validity window", false
	}
	if assertion.NameID == "" {
		return "the assertion carries no NameID", false
	}

	confirmation := subjectConfirmationData(assertion)
	if confirmation == nil {
		return "the assertion carries no subject confirmation data", false
	}
	// The assertion must answer THIS login. See the header.
	if confirmation.InResponseTo != requestID {
		return "the assertion answers a different authentication request", false
	}
	// And it must have been minted for THIS endpoint. The library checks the
	// response-level Destination, which an identity provider may omit.
	if confirmation.Recipient != "" && confirmation.Recipient != runtime.document.ACSURL {
		return "the assertion was minted for a different endpoint", false
	}
	return "", true
}

// conditionsAcceptable re-reads the assertion's time conditions with the
// authored tolerance.
//
// It runs ONLY when the library has already flagged the assertion as out of
// its window, so with a zero skew the two agree and this returns false. The
// tolerance is bounded at authoring time (identityproviders.MaxClockSkewSeconds),
// so it cannot be widened into an extension of the assertion's lifetime.
func conditionsAcceptable(assertion *saml2.AssertionInfo, skewSeconds int) bool {
	if skewSeconds <= 0 || len(assertion.Assertions) == 0 {
		return false
	}
	conditions := assertion.Assertions[0].Conditions
	if conditions == nil {
		return false
	}
	skew := time.Duration(skewSeconds) * time.Second
	now := time.Now()

	notBefore, err := time.Parse(time.RFC3339, conditions.NotBefore)
	if err != nil {
		return false
	}
	notOnOrAfter, err := time.Parse(time.RFC3339, conditions.NotOnOrAfter)
	if err != nil {
		return false
	}
	return !now.Before(notBefore.Add(-skew)) && !now.After(notOnOrAfter.Add(skew))
}

// subjectConfirmationData reaches the element carrying InResponseTo and
// Recipient, without assuming any of its ancestors exist.
func subjectConfirmationData(assertion *saml2.AssertionInfo) *samlConfirmation {
	if len(assertion.Assertions) == 0 {
		return nil
	}
	subject := assertion.Assertions[0].Subject
	if subject == nil || subject.SubjectConfirmation == nil ||
		subject.SubjectConfirmation.SubjectConfirmationData == nil {
		return nil
	}
	data := subject.SubjectConfirmation.SubjectConfirmationData
	return &samlConfirmation{InResponseTo: data.InResponseTo, Recipient: data.Recipient}
}

// samlConfirmation is the two attributes this file checks, lifted out of the
// library's type so the checks above read as checks rather than as five levels
// of field access.
type samlConfirmation struct {
	InResponseTo string
	Recipient    string
}

// defaultSAMLEmailAttributes are the attribute names an identity provider
// commonly uses for the address, in the order they are tried.
//
// The authored `email_attribute` wins when it is set. This list exists so a
// provider that uses the ordinary names works without the operator having to
// discover which one it sends.
var defaultSAMLEmailAttributes = []string{
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
	"urn:oid:0.9.2342.19200300.100.1.3",
	"email",
	"mail",
	"emailAddress",
}

var defaultSAMLNameAttributes = []string{
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
	"urn:oid:2.16.840.1.113730.3.1.241",
	"displayName",
	"name",
	"cn",
}

// samlIdentity resolves the address and display name from the assertion.
//
// The NameID is used as the address ONLY when it looks like one. A persistent
// or transient NameID is an opaque identifier, and treating one as an email
// address would create an account whose address is a random string — and, worse,
// would let it match nothing on a later login when the identifier rotates.
func samlIdentity(
	assertion *saml2.AssertionInfo,
	document identityproviders.SAMLDocument,
) (email, name string) {
	email = samlAttribute(assertion, document.EmailAttribute, defaultSAMLEmailAttributes)
	if email == "" && strings.Contains(assertion.NameID, "@") {
		email = assertion.NameID
	}
	name = samlAttribute(assertion, document.NameAttribute, defaultSAMLNameAttributes)
	return strings.TrimSpace(email), strings.TrimSpace(name)
}

func samlAttribute(assertion *saml2.AssertionInfo, authored string, fallbacks []string) string {
	names := fallbacks
	if authored != "" {
		// The authored name is used ALONE, not merged with the fallbacks. An
		// operator who names an attribute has told this service where to look,
		// and quietly reading a different one when theirs is absent would make
		// a misconfiguration look like a working login against the wrong field.
		names = []string{authored}
	}
	for _, candidate := range names {
		attribute, ok := assertion.Values[candidate]
		if !ok || len(attribute.Values) == 0 {
			continue
		}
		if value := strings.TrimSpace(attribute.Values[0].Value); value != "" {
			return value
		}
	}
	return ""
}

// provisionUser resolves the account this login belongs to.
//
// It is the OIDC path's resolution, with a `saml:` subject namespace. Sharing
// it is the point: the takeover and orphaning defects that
// `resolveProvisionedUser` documents are properties of the RESOLUTION ORDER,
// not of the protocol, and a second copy here would be a second place for them
// to come back.
//
// `requireVerifiedEmail` is FALSE, and that is not an oversight. SAML has no
// `email_verified` claim: an assertion's attributes are what the identity
// provider asserts, and there is no separate statement about the address to
// require. Passing true would refuse every SAML login.
//
// FOR THE SAME REASON THE VERIFIED ADDRESS PASSED BELOW IS "". An `email:`
// entry of `initial_global_admins` matches a STATED verification, and a SAML
// assertion states nothing about the address it carries. To treat the address
// as verified here would hand the administration role to whoever the identity
// provider says the person is, which is the takeover class this plane refuses
// by construction. A SAML deployment names its first administrator by
// `saml:<nameid>` — a value the operator can read off
// `GET /api/v2/social/author` after one sign-in, and one that is often the
// address already. If a future SAML document gains an explicit operator flag
// that marks its address attribute trusted, this is the ONE call site that
// reads it.
func (h *SAMLHandler) provisionUser(ctx context.Context, nameID, email, name string) (string, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	providerRef := SAMLProviderRefPrefix + nameID
	userID, err := resolveProvisionedUser(ctx, tx, providerRef, email, name, nil, false)
	if err != nil {
		return "", err
	}
	if err := applyFirstLoginGrants(ctx, tx, userID, providerRef, "", h.firstLogin); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return strconv.Itoa(userID), nil
}

// WithFirstLoginPolicy applies the deployment's initial-administrator list to
// this handler. See first_login.go.
func (h *SAMLHandler) WithFirstLoginPolicy(policy FirstLoginPolicy) *SAMLHandler {
	if h == nil {
		return h
	}
	h.firstLogin = FirstLoginPolicy{
		InitialGlobalAdmins: append([]string(nil), policy.InitialGlobalAdmins...),
	}
	return h
}

// writeProvisioningFailure maps the resolution's outcomes to responses, in the
// same words the OIDC path uses.
func (h *SAMLHandler) writeProvisioningFailure(w http.ResponseWriter, err error, email string) {
	switch {
	case errors.Is(err, errUserSuspended):
		slog.Warn("SAML: suspended user attempted login", "email", email)
		http.Error(w, "account suspended", http.StatusForbidden)
	case errors.Is(err, errIdentityConflict):
		// The subject and the address name two different accounts. Only an
		// operator can decide which one survives, so the login stops here
		// rather than picking one.
		slog.Warn("SAML: refused a login that names two accounts", "email", email)
		http.Error(w, "this identity is already linked to another account", http.StatusConflict)
	default:
		slog.Error("SAML: user provisioning failed", "err", err, "email", email)
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
	}
}
