package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

// HTTP action credentials (decision D3): a rule's configuration_id names an
// `openapi` configuration. Supported: no auth, API key as Authorization:
// Bearer, Authorization: Basic, a custom header, and OAuth2 client
// credentials (token fetched by Main through the same egress guard). Delegated
// user OAuth (oauth_discovery_endpoint) and mTLS are refused until they ship.
const (
	// httpConfigurationPermission is the existing project permission that lets
	// an actor read a configuration (migration 0072).
	httpConfigurationPermission = "configurations.configuration.details"
	// MaxHTTPTokenResponseBytes bounds one OAuth2 token response.
	MaxHTTPTokenResponseBytes = 64 << 10
	// MaxHTTPTokenCacheEntries bounds the client-credentials token cache.
	MaxHTTPTokenCacheEntries = 256
	httpTokenTimeout         = 10 * time.Second
	httpTokenMaxLifetime     = time.Hour
	httpTokenEarlyExpiry     = 30 * time.Second
	maxHTTPConfigurationData = 256 << 10
)

// HTTPActionUnsecreter resolves {{secret.NAME}} placeholders of one
// configuration's data from the project vault (storage.CurrentVaultUnsecreter).
type HTTPActionUnsecreter interface {
	Unsecret(ctx context.Context, projectID int32, data map[string]any) (map[string]any, error)
}

// HTTPActionCredentials implements app.Credentials.
type HTTPActionCredentials struct {
	store      sharedStore
	unsecreter HTTPActionUnsecreter
	transport  app.Transport
	now        func() time.Time

	mu     sync.Mutex
	tokens map[httpTokenKey]httpToken
	flight singleflight.Group
}

type httpTokenKey struct {
	project  int64
	config   int32
	revision string
}

type httpToken struct {
	value   string
	expires time.Time
}

// NewHTTPActionCredentials needs the egress-guarded transport for the OAuth2
// token endpoint (the second egress path of an HTTP action).
func NewHTTPActionCredentials(pool *pgxpool.Pool, unsecreter HTTPActionUnsecreter, transport app.Transport) (*HTTPActionCredentials, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil || unsecreter == nil || transport == nil {
		return nil, app.ErrUnavailable
	}
	return &HTTPActionCredentials{store: store, unsecreter: unsecreter, transport: transport, now: time.Now, tokens: map[httpTokenKey]httpToken{}}, nil
}

type httpConfiguration struct {
	revision string
	data     []byte
}

// httpConfigurationAccess is the live authority check for one configuration:
// the original actor holds configurations.configuration.details in the
// project now, the row is an `openapi` configuration of that project visible
// to the actor (shared, unowned, or a private configuration owned by both the
// executing actor and the rule's last writer), and its revision is derived
// from identity, last write and data. It runs on the caller's transaction so
// Begin orders it against concurrent changes.
func httpConfigurationAccess(ctx context.Context, tx sqlExecutor, projectID, actorID, ruleUpdatedBy int64, configurationID int32, lock bool) (httpConfiguration, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || projectID <= 0 || projectID > 2147483647 || actorID <= 0 || configurationID <= 0 {
		return httpConfiguration{}, httpPolicyDenied(nil)
	}
	result, err := legacyrbac.NewTransactionResolver(executor.queryer).ResolvePermissions(ctx, auth.User{UserID: strconv.FormatInt(actorID, 10)}, auth.PermissionModeDefault, strconv.FormatInt(projectID, 10))
	if errors.Is(err, auth.ErrPermissionDenied) {
		return httpConfiguration{}, httpPolicyDenied(nil)
	}
	if err != nil {
		return httpConfiguration{}, app.ErrUnavailable
	}
	permitted := false
	for _, granted := range result.Permissions {
		if granted == httpConfigurationPermission {
			permitted = true
			break
		}
	}
	if result.UserID != actorID || !permitted {
		return httpConfiguration{}, httpPolicyDenied(nil)
	}
	// The schema name is built from Main's own integer project id, never from
	// request input; identifiers cannot be parameters.
	table := pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10), "configuration"}.Sanitize()
	statement := `SELECT id,uuid::text,type,data::text,shared,author_id,COALESCE(updated_at,created_at)::text FROM ` + table + ` WHERE id=$1 AND project_id=$2`
	if lock {
		statement += " FOR SHARE"
	}
	var (
		id        int32
		uuid      string
		kind      string
		data      string
		shared    bool
		author    *int64
		writtenAt string
	)
	err = tx.QueryRow(ctx, statement, configurationID, int32(projectID)).Scan(&id, &uuid, &kind, &data, &shared, &author, &writtenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpConfiguration{}, httpPolicyDenied(nil)
	}
	if err != nil {
		return httpConfiguration{}, app.ErrUnavailable
	}
	privateAllowed := author != nil && *author == actorID && ruleUpdatedBy > 0 && *author == ruleUpdatedBy
	if kind != "openapi" || (!shared && author != nil && !privateAllowed) || len(data) > maxHTTPConfigurationData {
		return httpConfiguration{}, httpPolicyDenied(nil)
	}
	h := sha256.New()
	h.Write([]byte("elitea.http.credential-revision.v1\x00"))
	for _, part := range []string{strconv.FormatInt(int64(id), 10), uuid, writtenAt, app.Digest([]byte(data))} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	return httpConfiguration{revision: hex.EncodeToString(h.Sum(nil)), data: []byte(data)}, nil
}

// Resolve returns the auth headers of the configuration for this admission.
func (c *HTTPActionCredentials) Resolve(ctx context.Context, admitted app.Admission, configurationID int32) (app.Credential, error) {
	var configuration httpConfiguration
	err := c.store.WithinTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx sqlExecutor) error {
		var err error
		configuration, err = httpConfigurationAccess(ctx, tx, admitted.ProjectID, admitted.ActorID, admitted.RuleUpdatedBy, configurationID, false)
		return err
	})
	if err != nil {
		return app.Credential{}, err
	}
	var data map[string]any
	if json.Unmarshal(configuration.data, &data) != nil || data == nil {
		return app.Credential{}, httpPolicyDenied(nil)
	}
	resolved, err := c.unsecreter.Unsecret(ctx, int32(admitted.ProjectID), data)
	if err != nil {
		return app.Credential{}, app.ErrUnavailable
	}
	fields := map[string]string{}
	for _, name := range []string{"api_key", "auth_type", "custom_header_name", "client_id", "client_secret", "token_url", "scope", "method", "oauth_discovery_endpoint"} {
		switch value := resolved[name].(type) {
		case nil:
		case string:
			if strings.Contains(value, "{{secret.") || strings.ContainsAny(value, "\r\n\x00") {
				// An unresolved vault reference is never sent.
				return app.Credential{}, httpPolicyDenied(nil)
			}
			fields[name] = strings.TrimSpace(value)
		default:
			return app.Credential{}, httpPolicyDenied(nil)
		}
	}
	headers, err := c.headers(ctx, admitted, configurationID, configuration.revision, fields)
	if err != nil {
		return app.Credential{}, err
	}
	return app.Credential{Headers: headers, Revision: configuration.revision}, nil
}

func (c *HTTPActionCredentials) headers(ctx context.Context, admitted app.Admission, configurationID int32, revision string, fields map[string]string) (http.Header, error) {
	headers := http.Header{}
	switch {
	case fields["oauth_discovery_endpoint"] != "":
		return nil, httpPolicyDenied(nil) // delegated user OAuth: later (D3)
	case fields["client_id"] != "" || fields["client_secret"] != "" || fields["token_url"] != "":
		if fields["client_id"] == "" || fields["client_secret"] == "" || fields["token_url"] == "" {
			return nil, httpPolicyDenied(nil)
		}
		token, err := c.clientCredentialsToken(ctx, httpTokenKey{admitted.ProjectID, configurationID, revision}, fields)
		if err != nil {
			return nil, err
		}
		headers.Set("Authorization", "Bearer "+token)
	case fields["api_key"] != "":
		switch strings.ToLower(fields["auth_type"]) {
		case "", "bearer":
			headers.Set("Authorization", "Bearer "+fields["api_key"])
		case "basic":
			// The openapi configuration sends the key as given (SDK parity).
			headers.Set("Authorization", "Basic "+fields["api_key"])
		case "custom":
			name := strings.ToLower(fields["custom_header_name"])
			if !validHTTPCredentialHeader(name) {
				return nil, httpPolicyDenied(nil)
			}
			headers.Set(name, fields["api_key"])
		default:
			return nil, httpPolicyDenied(nil)
		}
	}
	return headers, nil
}

// validHTTPCredentialHeader allows a token-syntax name that is not a restricted
// header, except the two credential headers a custom key may use.
func validHTTPCredentialHeader(name string) bool {
	if name == "authorization" || name == "x-api-key" {
		return true
	}
	if name == "" || len(name) > 128 || app.RestrictedHeader(name) {
		return false
	}
	for _, c := range []byte(name) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// clientCredentialsToken fetches (or reuses) an OAuth2 client-credentials
// token. The cache key includes the configuration revision, so an edited
// configuration never reuses an old token, and an entry never outlives the
// token's own expiry or one hour. Concurrent uncached fetches for the same
// revision are coalesced via singleflight.
func (c *HTTPActionCredentials) clientCredentialsToken(ctx context.Context, key httpTokenKey, fields map[string]string) (string, error) {
	if cached, ok := c.cachedToken(key); ok {
		return cached, nil
	}
	flightKey := fmt.Sprintf("%d:%d:%s", key.project, key.config, key.revision)
	ch := c.flight.DoChan(flightKey, func() (any, error) {
		if cached, ok := c.cachedToken(key); ok {
			return cached, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpTokenTimeout)
		defer cancel()
		return c.fetchClientCredentialsToken(fetchCtx, key, fields)
	})
	select {
	case <-ctx.Done():
		return "", app.ErrUnavailable
	case result := <-ch:
		if result.Err != nil {
			return "", result.Err
		}
		return result.Val.(string), nil
	}
}

func (c *HTTPActionCredentials) cachedToken(key httpTokenKey) (string, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.tokens[key]; ok && now.Before(cached.expires) {
		return cached.value, true
	}
	return "", false
}

func (c *HTTPActionCredentials) fetchClientCredentialsToken(ctx context.Context, key httpTokenKey, fields map[string]string) (string, error) {
	now := c.now()
	endpoint, err := url.Parse(fields["token_url"])
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || len(fields["token_url"]) > 2048 {
		return "", httpPolicyDenied(nil)
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if fields["scope"] != "" {
		form.Set("scope", fields["scope"])
	}
	basic := strings.EqualFold(fields["method"], "basic")
	if !basic {
		form.Set("client_id", fields["client_id"])
		form.Set("client_secret", fields["client_secret"])
	}
	body := form.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(body))
	if err != nil {
		return "", httpPolicyDenied(nil)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 §2.3.1: form-urlencode each part before Basic encoding.
		pair := url.QueryEscape(fields["client_id"]) + ":" + url.QueryEscape(fields["client_secret"])
		request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pair)))
	}
	// RoundTrip, not a client: a redirect from the token endpoint is refused.
	response, err := c.transport.RoundTrip(request)
	if err != nil {
		if errors.Is(err, egress.ErrDestinationRefused) || errors.Is(err, app.ErrEgressRefused) {
			return "", httpPolicyDenied(nil)
		}
		return "", app.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxHTTPTokenResponseBytes+1))
	if err != nil || len(raw) > MaxHTTPTokenResponseBytes {
		return "", app.ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 400 && response.StatusCode <= 499 && response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusRequestTimeout {
			return "", httpPolicyDenied(nil)
		}
		return "", app.ErrUnavailable
	}
	var token struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&token) != nil || token.AccessToken == "" || len(token.AccessToken) > 8192 || strings.ContainsAny(token.AccessToken, "\r\n\x00 ") || token.TokenType != "" && !strings.EqualFold(token.TokenType, "bearer") {
		return "", app.ErrUnavailable
	}
	lifetime := httpTokenMaxLifetime
	if seconds, err := strconv.ParseInt(strings.Trim(string(token.ExpiresIn), `"`), 10, 64); err == nil && seconds > 0 && seconds < int64(lifetime/time.Second) {
		lifetime = time.Duration(seconds) * time.Second
	}
	lifetime -= httpTokenEarlyExpiry
	if lifetime > 0 {
		c.mu.Lock()
		if len(c.tokens) >= MaxHTTPTokenCacheEntries {
			for existing, cached := range c.tokens {
				if !now.Before(cached.expires) || len(c.tokens) >= MaxHTTPTokenCacheEntries {
					delete(c.tokens, existing)
				}
			}
		}
		c.tokens[key] = httpToken{value: token.AccessToken, expires: now.Add(lifetime)}
		c.mu.Unlock()
	}
	return token.AccessToken, nil
}
