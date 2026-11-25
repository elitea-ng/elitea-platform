package auth

// CredentialKind names the credential that authenticated a request, in the
// words of ADR-0059 decision 7 (the credentials table). It decides the
// ADMINISTRATION set the permission resolver answers; the developer and
// default modes do not read it.
//
// Only the code that authenticated the credential sets it, from what it
// verified: the browser session readers, the personal access token and native
// token validators, and the run-token verifier. It is never serialised, so no
// forwarded, cached or decoded principal can claim one.
//
// THE ZERO VALUE RESOLVES NOTHING. A principal an internal handler builds by
// hand (auth.User{UserID: actor}) carries no kind, and it must not gain its
// owner's administration set by accident: the resolver answers the empty set
// for it (fail closed).
type CredentialKind string

const (
	// CredentialKindNone is the zero value: no authenticating code set a
	// kind. The administration set is empty.
	CredentialKindNone CredentialKind = ""
	// CredentialKindSession is a single sign-on browser session (OIDC,
	// SAML), or the legacy signed `elitea_session` cookie. The owner's set.
	CredentialKindSession CredentialKind = "session"
	// CredentialKindForm is a Form plane browser session
	// (elitea_auth.form_sessions). The owner's set.
	CredentialKindForm CredentialKind = "form"
	// CredentialKindSealed is a sealed break-glass session (R2.7). Its
	// standing set is empty: it holds only its elevation (R2.1). Declared
	// here, first produced by R2.7.
	CredentialKindSealed CredentialKind = "sealed"
	// CredentialKindToken is a personal access token. Its set follows the
	// token row's admin_access (none, scoped, inherit; shared 0265).
	CredentialKindToken CredentialKind = "token"
	// CredentialKindNative is a native device session (desktop, mobile;
	// ADR-0025). The owner's set intersected with the registry permissions
	// marked native_client.
	CredentialKindNative CredentialKind = "native"
	// CredentialKindRun is a governed run's run token. Always empty.
	CredentialKindRun CredentialKind = "run"
	// CredentialKindSystem is a project system token
	// (`system_user_<project>@centry.user`). Empty: it is not a person.
	CredentialKindSystem CredentialKind = "system"
)

// Credential is what the permission resolver receives about the credential
// behind a principal: its kind, the token row and the browser session.
type Credential struct {
	Kind CredentialKind
	// TokenID is the auth_core__token row, "" for a browser session.
	TokenID string
	// BrowserSessionRef is the non-secret reference of the server-side
	// browser session row, 0 when there is none (a token, a native session,
	// the legacy signed cookie).
	BrowserSessionRef int64
}

// Credential returns the credential behind this principal.
func (u User) Credential() Credential {
	return Credential{Kind: u.CredentialKind, TokenID: u.TokenID, BrowserSessionRef: u.BrowserSessionRef}
}

// ElevationSessionRef is the browser session an elevation (ADR-0059 decision
// 5, "Bound to one session") may be bound to, or 0 when this credential can
// hold none: every kind other than a browser session, and a browser session
// without a server-side row (the legacy signed cookie). The resolver exposes
// it on its answer; the elevation union (R2.1) reads it there.
func (c Credential) ElevationSessionRef() int64 {
	switch c.Kind {
	case CredentialKindSession, CredentialKindForm, CredentialKindSealed:
		if c.BrowserSessionRef > 0 {
			return c.BrowserSessionRef
		}
	}
	return 0
}

// IsSealed reports a sealed break-glass session (ADR-0059 decision 7), which
// holds its elevation and nothing else.
func (c Credential) IsSealed() bool { return c.Kind == CredentialKindSealed }

// IsBrowserSession reports a credential a person holds in a browser: an
// `elitea_session` session, a Form session or a sealed session.
func (c Credential) IsBrowserSession() bool {
	switch c.Kind {
	case CredentialKindSession, CredentialKindForm, CredentialKindSealed:
		return true
	}
	return false
}
