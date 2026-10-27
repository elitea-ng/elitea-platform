package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

const (
	requestTimeout   = 10 * time.Second
	maxResponseBytes = 1 << 20
	maxTokenFileSize = 64 << 10
	// A token is refreshed once less than this fraction of its lease is left,
	// and never later than minRefreshMargin before expiry.
	refreshFraction  = 3
	minRefreshMargin = 10 * time.Second
)

func validScope(scope string) bool { return secretstore.ValidWorkloadScope(scope) }

// client is the HTTP half: TLS, login, per-role tokens, error mapping.
type client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	roles map[string]*roleToken
}

// roleToken is one role's in-memory token. lock is a one-slot channel so a
// waiter can give up on its context; holding it serialises logins for the
// role (the concurrent-login metadata race is hashicorp/vault#11798).
type roleToken struct {
	lock    chan struct{}
	token   string
	refresh time.Time
}

func newClient(cfg Config) (*client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.CAPEM) {
		return nil, errors.New("secretstore vault: the CA bundle holds no certificate")
	}
	transport := &http.Transport{
		// No proxy from the environment: the Vault token never leaves through
		// a proxy the operator did not name for it.
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: requestTimeout,
	}
	return &client{
		cfg: cfg,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			// A redirect could carry the token to another origin.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:   time.Now,
		roles: map[string]*roleToken{},
	}, nil
}

func (c *client) role(role string) *roleToken {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.roles[role]
	if entry == nil {
		entry = &roleToken{lock: make(chan struct{}, 1)}
		c.roles[role] = entry
	}
	return entry
}

// token returns a live token for role, logging in at most once at a time.
// stale, when set, is a token the caller saw rejected: it is discarded unless
// another caller already replaced it.
func (c *client) token(ctx context.Context, op secretstore.Op, role, stale string) (string, error) {
	entry := c.role(role)
	select {
	case entry.lock <- struct{}{}:
	case <-ctx.Done():
		return "", secretstore.Errorf(secretstore.ErrUnavailable, op, "cancelled waiting for login")
	}
	defer func() { <-entry.lock }()

	if stale != "" && entry.token == stale {
		entry.token = ""
	}
	if entry.token != "" && c.now().Before(entry.refresh) {
		return entry.token, nil
	}
	token, lease, err := c.login(ctx, op, role)
	if err != nil {
		entry.token = ""
		return "", err
	}
	margin := lease / refreshFraction
	if margin < minRefreshMargin {
		margin = minRefreshMargin
	}
	entry.token = token
	entry.refresh = c.now().Add(lease - margin)
	return token, nil
}

type loginResponse struct {
	Auth *struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int64  `json:"lease_duration"`
	} `json:"auth"`
}

func (c *client) login(ctx context.Context, op secretstore.Op, role string) (string, time.Duration, error) {
	jwt, err := readTokenFile(c.cfg.TokenFile)
	if err != nil {
		return "", 0, secretstore.Errorf(secretstore.ErrMisconfigured, op, "read the workload token file")
	}
	body, err := json.Marshal(map[string]string{"role": role, "jwt": jwt})
	if err != nil {
		return "", 0, secretstore.Errorf(secretstore.ErrMisconfigured, op, "encode the login request")
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/auth/"+c.cfg.AuthMount+"/login", "", body)
	clear(body)
	if err != nil {
		return "", 0, mapTransport(op, err)
	}
	if status != http.StatusOK {
		// A login refusal is a denial whatever the status: a missing role,
		// a bound-claim mismatch and a bad token all look alike to us.
		if status >= 500 {
			return "", 0, secretstore.Errorf(secretstore.ErrUnavailable, op, "login failed: backend error")
		}
		return "", 0, secretstore.Errorf(secretstore.ErrDenied, op, "login refused")
	}
	var decoded loginResponse
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Auth == nil || decoded.Auth.ClientToken == "" {
		return "", 0, secretstore.Errorf(secretstore.ErrUnavailable, op, "malformed login response")
	}
	lease := time.Duration(decoded.Auth.LeaseDuration) * time.Second
	if lease <= 0 {
		// A non-expiring token is not something this client should hold
		// forever; treat it as short-lived and log in again soon.
		lease = 5 * time.Minute
	}
	return decoded.Auth.ClientToken, lease, nil
}

func readTokenFile(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- operator-configured path.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxTokenFileSize+1))
	if err != nil || len(raw) > maxTokenFileSize {
		return "", errors.New("token file unreadable")
	}
	token := strings.TrimSpace(string(raw))
	clear(raw)
	if token == "" {
		return "", errors.New("token file empty")
	}
	return token, nil
}

// call runs one authenticated request for scope. A 403 re-logs in once and
// retries once; a second 403 is ErrDenied.
func (c *client) call(ctx context.Context, op secretstore.Op, scope, method, path string, body []byte) (int, []byte, error) {
	role := c.cfg.Role(scope)
	token, err := c.token(ctx, op, role, "")
	if err != nil {
		return 0, nil, err
	}
	status, raw, err := c.do(ctx, method, path, token, body)
	if err != nil {
		return 0, nil, mapTransport(op, err)
	}
	if status != http.StatusForbidden {
		return status, raw, nil
	}
	token, err = c.token(ctx, op, role, token)
	if err != nil {
		return 0, nil, err
	}
	status, raw, err = c.do(ctx, method, path, token, body)
	if err != nil {
		return 0, nil, mapTransport(op, err)
	}
	return status, raw, nil
}

func (c *client) do(ctx context.Context, method, path, token string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.cfg.Addr.String()+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("X-Vault-Token", token)
	}
	if c.cfg.Namespace != "" {
		request.Header.Set("X-Vault-Namespace", c.cfg.Namespace)
	}
	request.Header.Set("X-Vault-Request", "true")
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(raw) > maxResponseBytes {
		return 0, nil, errors.New("response too large")
	}
	return response.StatusCode, raw, nil
}

// mapTransport turns a transport failure into ErrUnavailable. The cause is
// dropped on purpose: a *url.Error carries the request URL, which holds the
// secret path.
func mapTransport(op secretstore.Op, err error) error {
	if errors.Is(err, context.Canceled) {
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "request cancelled")
	}
	var timeout interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "request timed out")
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return secretstore.Errorf(secretstore.ErrMisconfigured, op, "TLS verification failed")
	}
	return secretstore.Errorf(secretstore.ErrUnavailable, op, "transport failure")
}

// vaultErrors is the error body shape. Its text is only matched, never
// returned: it can echo a path.
type vaultErrors struct {
	Errors []string `json:"errors"`
}

// mapStatus maps a non-success status to a typed error.
func mapStatus(op secretstore.Op, status int, raw []byte) error {
	switch {
	case status == http.StatusNotFound:
		return secretstore.Errorf(secretstore.ErrNotFound, op, "no such secret or version")
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return secretstore.Errorf(secretstore.ErrDenied, op, "permission denied")
	case status == http.StatusBadRequest && casMismatch(raw):
		return secretstore.Errorf(secretstore.ErrVersionConflict, op, "check-and-set version mismatch")
	case status == http.StatusBadRequest:
		return secretstore.Errorf(secretstore.ErrMisconfigured, op, "request refused by the backend")
	case status == http.StatusTooManyRequests || status >= 500:
		return secretstore.Errorf(secretstore.ErrUnavailable, op, "backend error")
	case status >= 300 && status < 400:
		return secretstore.Errorf(secretstore.ErrMisconfigured, op, "unexpected redirect")
	}
	return secretstore.Errorf(secretstore.ErrUnavailable, op, "unexpected backend status")
}

// casMismatch recognises KV v2's check-and-set failure: "check-and-set
// parameter did not match the current version". The "required" variant
// (cas missing on a cas_required mount) is a misconfiguration instead.
func casMismatch(raw []byte) bool {
	var decoded vaultErrors
	if json.Unmarshal(raw, &decoded) != nil {
		return false
	}
	for _, message := range decoded.Errors {
		lower := strings.ToLower(message)
		if strings.Contains(lower, "check-and-set") && strings.Contains(lower, "did not match") {
			return true
		}
	}
	return false
}
