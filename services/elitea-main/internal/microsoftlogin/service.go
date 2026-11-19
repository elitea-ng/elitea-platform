// Package microsoftlogin is the server side of a Microsoft sign-in (ADR-0057
// decision 6; epic 2254 story S1c): the start, the callback and the handoff, for
// Custom app and Default app, on web, desktop and mobile.
//
//	start     POST /api/v2/microsoft_login:start    authenticated, the starter
//	callback  GET  /api/v2/microsoft_login:callback the one https redirect URI
//	redeem    POST /api/v2/microsoft_login:redeem   authenticated, the starter
//
// The start builds the authorization URL (PKCE S256, a random state bound server
// side to the user, project, credential, app and client kind). The browser talks to
// Entra and never to a token endpoint; Main alone does the code exchange, with the
// client secret and the PKCE verifier, at a token endpoint computed from the
// validated authority and never read from a discovery document.
//
// THE CALLBACK ATTACHES NOTHING TO ANYONE. It runs in a browser that may belong to
// somebody else (a victim finishing a link an attacker started), and on desktop and
// mobile in the system browser, which holds no session at all. It exchanges the
// code, reads the identity, and parks the sealed access token as a pending handoff
// for 60 seconds. Only the person who started the sign-in can redeem it, over the
// authenticated API: with the session on web, with the verifier of the
// client_challenge on desktop and mobile. A login CSRF therefore ends in a handoff
// nobody can redeem.
//
// No refresh token reaches a client or the credential's data: the browser receives
// an access token and its expiry only, and the refresh token of the code exchange is
// discarded here (the opt-in stored login is story S1h). A token, code, verifier or
// client secret is never logged, put in a trace attribute or an error, or sent on
// NATS; an error carries the Entra error code and the correlation id only.
package microsoftlogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
)

// Lifetimes.
const (
	// StateTTL is how long a started sign-in stays redeemable at the callback.
	StateTTL = 10 * time.Minute
	// HandoffTTL is how long a finished sign-in waits for its starter.
	HandoffTTL = 60 * time.Second
	// MaxLiveSignIns bounds the sign-ins one person may have open at once.
	MaxLiveSignIns = 20
)

// Client kinds.
const (
	ClientWeb     = "web"
	ClientDesktop = "desktop"
	ClientMobile  = "mobile"
)

// DefaultMobileScheme is the custom scheme of the default edition (ADR-0052).
const DefaultMobileScheme = "elitea"

var (
	challengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	schemePattern    = regexp.MustCompile(`^[a-z][a-z0-9+.-]{1,31}$`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// deniedSchemes can never carry a handoff: each runs code or leaves the app.
	deniedSchemes = map[string]bool{
		"http": true, "https": true, "javascript": true, "data": true, "file": true, "blob": true,
		"about": true, "ftp": true, "ws": true, "wss": true, "mailto": true, "intent": true,
		"content": true, "vbscript": true,
		// Schemes that dial, message or install: never a place for a handoff code.
		"tel": true, "sms": true, "facetime": true, "facetime-audio": true, "itms-services": true,
		"itms-apps": true, "market": true, "ssh": true, "sftp": true, "smb": true, "afp": true,
		"vnc": true, "ldap": true, "ldaps": true, "chrome": true, "chrome-extension": true,
		"moz-extension": true, "view-source": true, "ms-msdt": true, "search-ms": true,
	}
)

// ErrHandoffInvalid is every refusal of a redemption: unknown, expired, already
// redeemed, another person's, the wrong verifier. One error, so a caller learns
// nothing about which.
var ErrHandoffInvalid = errors.New("microsoftlogin: the handoff is not valid")

// ErrStateInvalid is a callback whose state is unknown, expired or used.
var ErrStateInvalid = errors.New("microsoftlogin: the sign-in state is not valid")

// Doer sends one request to Microsoft. The composition root supplies the egress
// guard's client (no redirects, private addresses refused).
type Doer func(*http.Request) (*http.Response, error)

// Config is the service's dependencies.
type Config struct {
	Pool        *pgxpool.Pool
	MasterKey   []byte
	Credentials Credentials
	// Apps is the Default app registry; nil refuses a Default app credential.
	Apps Apps
	Do   Doer
	// Origin is the deployment's public origin; the redirect URI is derived from it.
	Origin string
	// Logger defaults to slog.Default.
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service runs the sign-in.
type Service struct {
	pool        *pgxpool.Pool
	sealer      *sealer
	credentials Credentials
	apps        Apps
	do          Doer
	redirectURI string
	logger      *slog.Logger
	now         func() time.Time
	random      func([]byte) error
}

// New builds the service. The redirect URI must be one Entra accepts (https, or
// localhost for development).
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil || cfg.Credentials == nil || cfg.Do == nil {
		return nil, errors.New("microsoftlogin: a pool, a credential reader and an HTTP client are required")
	}
	sealer, err := newSealer(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	redirect := microsoftapps.RedirectURI(cfg.Origin)
	if strings.TrimSpace(cfg.Origin) == "" || microsoftapps.RedirectURIIssue(redirect) != "" {
		return nil, errors.New("microsoftlogin: the public origin must be https (or localhost) so Entra accepts the redirect URI")
	}
	s := &Service{
		pool: cfg.Pool, sealer: sealer, credentials: cfg.Credentials, apps: cfg.Apps, do: cfg.Do,
		redirectURI: redirect, logger: cfg.Logger, now: cfg.Now,
		random: func(b []byte) error { _, err := rand.Read(b); return err },
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// RedirectURI is the one https redirect URI an Entra app registers.
func (s *Service) RedirectURI() string { return s.redirectURI }

func (s *Service) token(n int) (string, error) {
	b := make([]byte, n)
	defer clear(b)
	if err := s.random(b); err != nil {
		return "", fmt.Errorf("microsoftlogin: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// StartRequest is one sign-in start. UserID is the authenticated caller.
type StartRequest struct {
	UserID       int64
	ProjectID    int64
	CredentialID string
	ClientKind   string
	// ClientChallenge is base64url(SHA-256(verifier)) for desktop and mobile; the
	// client keeps the verifier and sends it only to redeem.
	ClientChallenge string
	// LoopbackPort is the port of the desktop's 127.0.0.1 listener.
	LoopbackPort int
	// ReturnScheme is the mobile custom scheme; empty means DefaultMobileScheme.
	ReturnScheme string
}

// StartResult is where to send the person.
type StartResult struct {
	AuthorizationURL string
	State            string
	ExpiresAt        time.Time
	// App is "custom" or the Default app's id.
	App string
}

type clientShape struct {
	challenge string
	port      int
	scheme    string
}

func validateClient(r StartRequest) (clientShape, error) {
	switch r.ClientKind {
	case ClientWeb:
		if r.ClientChallenge != "" || r.LoopbackPort != 0 || r.ReturnScheme != "" {
			return clientShape{}, refuse(CodeClientKindInvalid, "A web sign-in redeems with the session and sends no challenge, port or scheme.")
		}
		return clientShape{}, nil
	case ClientDesktop:
		if !challengePattern.MatchString(r.ClientChallenge) {
			return clientShape{}, refuse(CodeClientChallenge, "client_challenge must be the base64url SHA-256 of the client's verifier.")
		}
		if r.LoopbackPort < 1024 || r.LoopbackPort > 65535 || r.ReturnScheme != "" {
			return clientShape{}, refuse(CodeClientReturnInvalid, "A desktop sign-in names its loopback port (1024 to 65535) and no scheme.")
		}
		return clientShape{challenge: r.ClientChallenge, port: r.LoopbackPort}, nil
	case ClientMobile:
		if !challengePattern.MatchString(r.ClientChallenge) {
			return clientShape{}, refuse(CodeClientChallenge, "client_challenge must be the base64url SHA-256 of the client's verifier.")
		}
		scheme := strings.ToLower(strings.TrimSpace(r.ReturnScheme))
		if scheme == "" {
			scheme = DefaultMobileScheme
		}
		if !schemePattern.MatchString(scheme) || deniedSchemes[scheme] || r.LoopbackPort != 0 {
			return clientShape{}, refuse(CodeClientReturnInvalid, "A mobile sign-in names the app's custom scheme and no port.")
		}
		return clientShape{challenge: r.ClientChallenge, scheme: scheme}, nil
	}
	return clientShape{}, refuse(CodeClientKindInvalid, "client_kind must be web, desktop or mobile.")
}

// Start checks the credential, picks the client, stores the state server side and
// answers the Entra authorization URL.
func (s *Service) Start(ctx context.Context, r StartRequest) (StartResult, error) {
	if r.UserID <= 0 || r.ProjectID <= 0 {
		return StartResult{}, refuse(CodeCredentialNotFound, "No such credential.")
	}
	credentialID := strings.ToLower(strings.TrimSpace(r.CredentialID))
	if !uuidPattern.MatchString(credentialID) {
		return StartResult{}, refuse(CodeCredentialIDInvalid, "credential_id must be the credential's uuid.")
	}
	shape, err := validateClient(r)
	if err != nil {
		return StartResult{}, err
	}
	credential, err := s.credentials.Load(ctx, r.ProjectID, credentialID)
	if errors.Is(err, ErrCredentialNotFound) {
		return StartResult{}, refuse(CodeCredentialNotFound, "No such credential in this project.")
	}
	if err != nil {
		return StartResult{}, fmt.Errorf("microsoftlogin: read the credential: %w", err)
	}
	c, err := s.resolveClient(ctx, r.ProjectID, credential)
	if err != nil {
		return StartResult{}, err
	}
	now := s.now()
	state, err := s.token(32)
	if err != nil {
		return StartResult{}, err
	}
	verifier, err := s.token(32)
	if err != nil {
		return StartResult{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	expires := now.Add(StateTTL).UTC().Truncate(time.Microsecond)
	digest := digestOf("state", state)
	b := binding{
		userID: r.UserID, projectID: r.ProjectID, credentialID: credentialID, credType: credential.Type,
		appRef: c.appRef, clientKind: r.ClientKind, challenge: shape.challenge,
		fingerprint: fingerprint(c.clientID), authority: c.authority, scope: strings.Join(c.scopes, " "),
	}
	sealed := s.sealer.seal(verifier, stateAAD{digest: digest, redirectURI: s.redirectURI, expiresAt: expires, binding: b})
	if err := s.insertState(ctx, digest, b, shape, s.redirectURI, sealed, expires, now); err != nil {
		return StartResult{}, err
	}
	query := url.Values{
		"client_id":             {c.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {s.redirectURI},
		"response_mode":         {"query"},
		"scope":                 {b.scope},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return StartResult{
		AuthorizationURL: microsoftauth.AuthorizeEndpoint(c.authority) + "?" + query.Encode(),
		State:            state, ExpiresAt: expires, App: c.appRef,
	}, nil
}

// RedeemRequest is a handoff redemption. UserID is the authenticated caller.
type RedeemRequest struct {
	UserID    int64
	ProjectID int64
	Handoff   string
	State     string
	// ClientVerifier proves a desktop or mobile client is the starter; a web
	// sign-in sends none.
	ClientVerifier string
}

// Redeemed is what the starter receives: an access token and its expiry. No refresh
// token, no client secret.
type Redeemed struct {
	AccessToken    string
	ExpiresAt      time.Time
	Scope          []string
	CredentialID   string
	CredentialType string
	App            string
	TenantID       string
	AccountName    string
}

// Redeem hands the starter the access token of a finished sign-in. A handoff is
// single use: the redemption deletes it. Every refusal is the same error.
func (s *Service) Redeem(ctx context.Context, r RedeemRequest) (Redeemed, error) {
	if r.UserID <= 0 || r.ProjectID <= 0 || r.Handoff == "" || len(r.Handoff) > 128 || r.State == "" || len(r.State) > 128 ||
		len(r.ClientVerifier) > 128 {
		return Redeemed{}, ErrHandoffInvalid
	}
	handoffDigest := digestOf("handoff", r.Handoff)
	stateDigest := digestOf("state", r.State)
	// One statement checks everything and deletes the row: the person who started it,
	// in this project, with this state and the verifier of its challenge, before it
	// expires. A caller who is not all of those matches nothing and deletes nothing,
	// so a stranger holding a handoff code cannot destroy the starter's sign-in; the
	// 256-bit handoff code and verifier make guessing pointless.
	row, err := s.takeHandoff(ctx, handoffDigest, stateDigest, r.UserID, r.ProjectID, challengeOf(r.ClientVerifier), s.now())
	if err != nil {
		return Redeemed{}, err
	}
	token, err := s.sealer.open(row.ciphertext, handoffAAD{
		handoffDigest: handoffDigest, stateDigest: row.stateDigest, tid: row.tid, oid: row.oid,
		expiresAt: row.expiresAt, binding: row.binding(),
	})
	if err != nil {
		return Redeemed{}, ErrHandoffInvalid
	}
	return Redeemed{
		AccessToken: token, ExpiresAt: row.accessExpiresAt, Scope: strings.Fields(row.scope),
		CredentialID: row.credentialID, CredentialType: row.credType, App: row.appRef,
		TenantID: row.tid, AccountName: row.accountName,
	}, nil
}

// challengeOf is the challenge a verifier answers: base64url(SHA-256(verifier)), or
// the empty string for no verifier (a web sign-in has none).
func challengeOf(verifier string) string {
	if verifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// errNoRow keeps pgx out of the callers' signatures.
var errNoRow = pgx.ErrNoRows
