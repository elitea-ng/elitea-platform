package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host names the TLS listener answers. The compose overlay gives the
// governed-mocks service each of them as a network alias, and mint-tls puts
// each in the leaf's SANs (deploy/governed-mocks/Containerfile).
const (
	hostJira       = "jira.governed.test"
	hostSharePoint = "sharepoint.governed.test"
	hostGraph      = "graph.microsoft.com"
	hostEntra      = "login.microsoftonline.com"
	hostIssuer     = "entra.governed.test"
)

// Sentinels a test finds in a tool result: they can only come from this
// process, so a turn that quotes one proves the provider call happened.
const (
	jiraSentinel       = "GOVJIRASENTINEL"
	jiraWriteSentinel  = "GOVJIRACOMMENTED"
	sharePointSentinel = "GOVSHAREPOINTSENTINEL"
)

const maxJournal = 2000

// entry is one journalled request. It never holds a credential: the
// credential the request presented is recorded as its SHA-256 only.
type entry struct {
	Seq              int64     `json:"seq"`
	At               time.Time `json:"at"`
	Host             string    `json:"host"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	Scheme           string    `json:"scheme"`
	CredentialSHA256 string    `json:"credential_sha256,omitempty"`
	Status           int       `json:"status"`
}

type journal struct {
	mu      sync.Mutex
	seq     int64
	entries []entry
}

func (j *journal) add(e entry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	e.Seq = j.seq
	j.entries = append(j.entries, e)
	if len(j.entries) > maxJournal {
		j.entries = j.entries[len(j.entries)-maxJournal:]
	}
}

func (j *journal) list(host string) []entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]entry, 0, len(j.entries))
	for _, e := range j.entries {
		if host == "" || e.Host == host {
			out = append(out, e)
		}
	}
	return out
}

func (j *journal) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = nil
}

// keys are the signing keys minted per process. Nothing outside this
// process can sign for them, so a token that verifies came from here.
type keys struct {
	entra       *rsa.PrivateKey
	issuer      *ecdsa.PrivateKey
	untrusted   *ecdsa.PrivateKey
	entraKID    string
	issuerKID   string
	codesMu     sync.Mutex
	codes       map[string]map[string]any
	defaultUser string
}

type server struct {
	journal *journal
	keys    *keys
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	tlsAddr := flags.String("tls-addr", ":443", "provider listener")
	adminAddr := flags.String("admin-addr", ":8080", "admin listener (journal, minting, health)")
	tlsDir := flags.String("tls-dir", "/opt/governed-mocks/tls", "directory with server-chain.crt and server.key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(*tlsDir, "server-chain.crt"), filepath.Join(*tlsDir, "server.key"))
	if err != nil {
		return err
	}
	entra, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	untrusted, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	s := &server{journal: &journal{}, keys: &keys{
		entra: entra, issuer: issuer, untrusted: untrusted,
		entraKID: "governed-entra-1", issuerKID: "governed-mcp-issuer-1",
		codes:       map[string]map[string]any{},
		defaultUser: envOr("GOVERNED_ENTRA_DEFAULT_USER", "e2e-chat@autotest.local"),
	}}
	provider := &http.Server{
		Addr:              *tlsAddr,
		Handler:           http.HandlerFunc(s.provider),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	admin := &http.Server{Addr: *adminAddr, Handler: s.adminMux(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- provider.ListenAndServeTLS("", "") }()
	go func() { errs <- admin.ListenAndServe() }()
	log.Printf("governed-mocks: providers on %s (TLS), admin on %s", *tlsAddr, *adminAddr)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errs:
		return err
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(ctx)
		_ = admin.Shutdown(ctx)
		return nil
	}
}

func healthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := flags.String("url", "http://127.0.0.1:8080/healthz", "health URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(*url)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health %d", response.StatusCode)
	}
	return nil
}

// ── admin listener ───────────────────────────────────────────────────────────

func (s *server) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /__journal", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"entries": s.journal.list(r.URL.Query().Get("host"))})
	})
	mux.HandleFunc("DELETE /__journal", func(w http.ResponseWriter, _ *http.Request) {
		s.journal.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	// An inbound MCP access token for the governed MCP endpoint. The test
	// names the audience, the user and the scopes; `untrusted_key` signs with
	// a key the JWKS does not publish, for the negative leg.
	mux.HandleFunc("POST /__mint/mcp-inbound", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Audience     string `json:"audience"`
			Email        string `json:"email"`
			Scope        string `json:"scope"`
			ExpiresIn    int64  `json:"expires_in"`
			UntrustedKey bool   `json:"untrusted_key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Audience == "" || body.Email == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audience and email are required"})
			return
		}
		if body.ExpiresIn == 0 {
			body.ExpiresIn = 600
		}
		now := time.Now()
		key := s.keys.issuer
		if body.UntrustedKey {
			key = s.keys.untrusted
		}
		token, err := signES256(key, s.keys.issuerKID, map[string]any{
			"iss": "https://" + hostIssuer + "/mcp-issuer", "sub": "mcp-client:" + body.Email,
			"aud": body.Audience, "email": body.Email, "scope": body.Scope,
			"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Duration(body.ExpiresIn) * time.Second).Unix(),
			"jti": randomID(),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "Bearer"})
	})
	return mux
}

// ── provider listener ────────────────────────────────────────────────────────

// recorder captures the status a handler wrote, for the journal.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *server) provider(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	scheme, digest := credentialOf(r)
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.journal.add(entry{At: time.Now().UTC(), Host: host, Method: r.Method, Path: r.URL.Path,
			Scheme: scheme, CredentialSHA256: digest, Status: rec.status})
	}()
	switch host {
	case hostJira:
		s.jira(rec, r, scheme)
	case hostSharePoint, hostGraph:
		s.graph(rec, r, scheme)
	case hostEntra:
		s.entra(rec, r)
	case hostIssuer:
		s.issuer(rec, r)
	default:
		writeJSON(rec, http.StatusNotFound, map[string]string{"error": "unknown host " + host})
	}
}

// credentialOf names the scheme a request authenticated with and the
// SHA-256 of its secret part: a bearer token, or a Basic password (the API
// token Jira takes as the password).
func credentialOf(r *http.Request) (string, string) {
	header := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(header, "Bearer "):
		return "bearer", sha(strings.TrimPrefix(header, "Bearer "))
	case strings.HasPrefix(header, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
		if err != nil {
			return "basic", ""
		}
		_, password, _ := strings.Cut(string(raw), ":")
		return "basic", sha(password)
	default:
		return "none", ""
	}
}

func (s *server) jira(w http.ResponseWriter, r *http.Request, scheme string) {
	if scheme == "none" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"errorMessages": []string{"authentication required"}})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// rest / api / <version> / <resource> ...
	if len(parts) < 4 || parts[0] != "rest" || parts[1] != "api" {
		writeJSON(w, http.StatusOK, map[string]string{"sentinel": jiraSentinel})
		return
	}
	project := map[string]any{"id": "10000", "key": "GOV", "name": jiraSentinel + " project", "projectTypeKey": "software"}
	switch {
	case parts[3] == "project" && len(parts) == 4:
		writeJSON(w, http.StatusOK, []any{project})
	case parts[3] == "project" && len(parts) == 5 && parts[4] == "search":
		writeJSON(w, http.StatusOK, map[string]any{"values": []any{project}, "isLast": true, "total": 1})
	case parts[3] == "issue" && len(parts) == 6 && parts[5] == "comment" && r.Method == http.MethodPost:
		writeJSON(w, http.StatusCreated, map[string]any{"id": "10001", "body": jiraWriteSentinel})
	case parts[3] == "issue" && len(parts) == 6 && parts[5] == "comment":
		writeJSON(w, http.StatusOK, map[string]any{"comments": []any{}, "total": 0})
	case parts[3] == "issue" && len(parts) == 5:
		writeJSON(w, http.StatusOK, map[string]any{"id": "10002", "key": parts[4],
			"fields": map[string]any{"summary": jiraSentinel + " issue", "project": project}})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"sentinel": jiraSentinel})
	}
}

func (s *server) graph(w http.ResponseWriter, r *http.Request, scheme string) {
	if scheme != "bearer" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "InvalidAuthenticationToken"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.0")
	site := map[string]any{"id": "sharepoint.governed.test,site-1,web-1", "displayName": sharePointSentinel,
		"webUrl": "https://" + hostSharePoint + "/sites/governed"}
	switch {
	case strings.HasSuffix(path, "/lists"):
		writeJSON(w, http.StatusOK, map[string]any{"value": []any{map[string]any{"id": "list-1", "displayName": sharePointSentinel + " list"}}})
	case strings.HasSuffix(path, "/drives"):
		writeJSON(w, http.StatusOK, map[string]any{"value": []any{map[string]any{"id": "drive-1", "name": sharePointSentinel + " drive"}}})
	case strings.HasPrefix(path, "/sites/"):
		writeJSON(w, http.StatusOK, site)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"value": []any{}})
	}
}

// entra is an Entra token endpoint: OIDC metadata, the JWKS, an
// auto-approving authorization endpoint, and the token endpoint with the
// authorization code, client credentials and on-behalf-of grants. A client
// assertion or secret is accepted as presented; this double proves the
// exchange shape, not Entra's client authentication.
func (s *server) entra(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	tenant := parts[0]
	base := "https://" + hostEntra + "/" + tenant
	rest := strings.Join(parts[1:], "/")
	switch {
	case rest == "v2.0/.well-known/openid-configuration":
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                base + "/v2.0",
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"jwks_uri":                              base + "/discovery/v2.0/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"pairwise"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	case rest == "discovery/v2.0/keys":
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{rsaJWK(&s.keys.entra.PublicKey, s.keys.entraKID)}})
	case rest == "oauth2/v2.0/authorize":
		s.entraAuthorize(w, r, tenant)
	case rest == "oauth2/v2.0/token" && r.Method == http.MethodPost:
		s.entraToken(w, r, tenant)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (s *server) entraAuthorize(w http.ResponseWriter, r *http.Request, tenant string) {
	query := r.URL.Query()
	redirect := query.Get("redirect_uri")
	if redirect == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	user := query.Get("login_hint")
	if user == "" {
		user = s.keys.defaultUser
	}
	code := randomID()
	s.keys.codesMu.Lock()
	s.keys.codes[code] = map[string]any{"email": user, "nonce": query.Get("nonce"), "tenant": tenant,
		"client_id": query.Get("client_id"), "scope": query.Get("scope")}
	s.keys.codesMu.Unlock()
	separator := "?"
	if strings.Contains(redirect, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirect+separator+"code="+code+"&state="+query.Get("state"), http.StatusFound)
}

func (s *server) entraToken(w http.ResponseWriter, r *http.Request, tenant string) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	now := time.Now()
	issuer := "https://" + hostEntra + "/" + tenant + "/v2.0"
	clientID := r.PostForm.Get("client_id")
	scope := r.PostForm.Get("scope")
	audience := strings.TrimSuffix(strings.Fields(scope + " x")[0], "/.default")
	base := map[string]any{"iss": issuer, "tid": tenant, "azp": clientID, "ver": "2.0",
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		"uti": randomID()}
	switch r.PostForm.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:jwt-bearer":
		// On-behalf-of: the assertion's user becomes the token's user.
		if r.PostForm.Get("requested_token_use") != "on_behalf_of" || r.PostForm.Get("assertion") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		claims := unverifiedClaims(r.PostForm.Get("assertion"))
		token := copyClaims(base)
		token["aud"], token["scp"], token["idtyp"] = audience, scopeNames(scope), "user"
		token["oid"], token["sub"] = claims["oid"], claims["sub"]
		s.issueToken(w, token, scope)
	case "client_credentials":
		token := copyClaims(base)
		token["aud"], token["idtyp"], token["roles"] = audience, "app", []string{"Sites.Selected"}
		token["oid"], token["sub"] = "app-"+clientID, "app-"+clientID
		s.issueToken(w, token, scope)
	case "authorization_code":
		s.keys.codesMu.Lock()
		grant, ok := s.keys.codes[r.PostForm.Get("code")]
		delete(s.keys.codes, r.PostForm.Get("code"))
		s.keys.codesMu.Unlock()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		email, _ := grant["email"].(string)
		oid := "oid-" + sha(email)[:16]
		id := copyClaims(base)
		id["aud"], id["sub"], id["oid"], id["email"], id["preferred_username"] = clientID, oid, oid, email, email
		if nonce, _ := grant["nonce"].(string); nonce != "" {
			id["nonce"] = nonce
		}
		idToken, err := s.signRS256(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		access := copyClaims(base)
		access["aud"], access["sub"], access["oid"], access["scp"], access["idtyp"] = "api://"+clientID, oid, oid, scopeNames(scope), "user"
		accessToken, err := s.signRS256(access)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3600,
			"access_token": accessToken, "id_token": idToken, "scope": scope})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (s *server) issueToken(w http.ResponseWriter, claims map[string]any, scope string) {
	token, err := s.signRS256(claims)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3600, "ext_expires_in": 3600,
		"access_token": token, "scope": scope})
}

func (s *server) issuer(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/mcp-issuer/.well-known/openid-configuration":
		writeJSON(w, http.StatusOK, map[string]any{"issuer": "https://" + hostIssuer + "/mcp-issuer",
			"jwks_uri": "https://" + hostIssuer + "/mcp-issuer/jwks"})
	case "/mcp-issuer/jwks":
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{ecJWK(&s.keys.issuer.PublicKey, s.keys.issuerKID)}})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (s *server) signRS256(claims map[string]any) (string, error) {
	signingInput, err := jwsInput(map[string]any{"alg": "RS256", "typ": "JWT", "kid": s.keys.entraKID}, claims)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.keys.entra, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64(signature), nil
}

func rsaJWK(key *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{"kty": "RSA", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		"kid": kid, "alg": "RS256", "use": "sig"}
}

// unverifiedClaims reads a JWT's claims without checking it: the on-behalf-of
// double needs only to carry the user over.
func unverifiedClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return map[string]any{}
	}
	return claims
}

func copyClaims(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+6)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func scopeNames(scope string) string {
	names := make([]string, 0, 4)
	for _, field := range strings.Fields(scope) {
		if i := strings.LastIndexByte(field, '/'); i >= 0 {
			field = field[i+1:]
		}
		if field != ".default" {
			names = append(names, field)
		}
	}
	return strings.Join(names, " ")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func sha(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func randomID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
