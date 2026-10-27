package agentid

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entraoauth"
)

const (
	grantClientCredentials = "client_credentials"
	clientAssertionTypeJWT = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// tokenExchangeScope is what the blueprint asks for to mint T1.
	tokenExchangeScope  = "api://AzureADTokenExchange/.default"
	graphScope          = "https://graph.microsoft.com/.default"
	maxResponseBytes    = 1 << 20
	maxTokenBytes       = 16 << 10
	minTokenLifetime    = 30 * time.Second
	tokenRefreshSkew    = 5 * time.Minute
	defaultHTTPTimeout  = 10 * time.Second
	clientRequestHeader = "client-request-id"
)

// issued is one token endpoint answer. The token is held only inside this
// package; issued has no exported field and no formatting of its token.
type issued struct {
	token     string
	expiresAt time.Time
	// requestID and correlationID are Entra's x-ms-request-id and the
	// echoed client-request-id: the values an Entra support case needs.
	requestID     string
	correlationID string
}

func (i issued) String() string { return "agentid.issued{token=[redacted]}" }

// GoString keeps the token out of %#v.
func (i issued) GoString() string { return i.String() }

// tokenClaims is the part of an unverified access token the checks read.
type tokenClaims struct {
	TenantID  string `json:"tid"`
	AZP       string `json:"azp"`
	AppID     string `json:"appid"`
	ParentAZP string `json:"xms_par_app_azp"`
	IDType    string `json:"idtyp"`
	UTI       string `json:"uti"`
	ObjectID  string `json:"oid"`
}

func (c tokenClaims) clientID() string {
	if c.AZP != "" {
		return c.AZP
	}
	return c.AppID
}

func unverifiedClaims(token string) (tokenClaims, bool) {
	var claims tokenClaims
	ok := entraoauth.UnverifiedPayload(token, &claims)
	return claims, ok
}

// readFederatedToken re-reads the projected token on every call, so a
// rotated token is always the one sent.
func (p *Provider) readFederatedToken() (string, error) {
	raw, err := os.ReadFile(p.cfg.FederatedTokenFile)
	if err != nil {
		return "", ErrUnavailable
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || len(token) > maxTokenBytes || strings.ContainsFunc(token, unicode.IsControl) {
		return "", ErrUnavailable
	}
	return token, nil
}

// requestToken posts one client credentials request.
func (p *Provider) requestToken(ctx context.Context, form url.Values) (issued, error) {
	correlation := correlationFrom(ctx)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.authority()+"/"+p.cfg.TenantID+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return issued{}, ErrInvalidRequest
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set(clientRequestHeader, correlation)
	response, err := p.http.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return issued{}, ctxErr
		}
		return issued{}, ErrUnavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	_ = response.Body.Close()
	if readErr != nil || len(body) > maxResponseBytes {
		return issued{}, ErrUnavailable
	}
	out := issued{requestID: response.Header.Get("x-ms-request-id"), correlationID: correlation}
	if !entraoauth.JSONContentType(response.Header) {
		return out, ErrUnavailable
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		err := mapOAuthError(body)
		p.logger.LogAttrs(ctx, slog.LevelWarn, "entra agent id token request refused",
			slog.String("client_id", form.Get("client_id")), slog.String("request_id", out.requestID),
			slog.String("client_request_id", correlation), slog.String("reason", err.Error()))
		return out, err
	default:
		return out, ErrUnavailable
	}
	var answer struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&answer); err != nil {
		return out, ErrUnavailable
	}
	seconds, ok := entraoauth.ExpiresInSeconds(answer.ExpiresIn)
	if !ok || answer.AccessToken == "" || len(answer.AccessToken) > maxTokenBytes ||
		strings.ContainsFunc(answer.AccessToken, unicode.IsControl) || !strings.EqualFold(answer.TokenType, "Bearer") {
		return out, ErrUnavailable
	}
	lifetime := time.Duration(seconds) * time.Second
	if lifetime < minTokenLifetime {
		return out, ErrUnavailable
	}
	out.token, out.expiresAt = answer.AccessToken, p.now().Add(lifetime).UTC()
	return out, nil
}

// blueprintForm is a client credentials request of the blueprint,
// authenticated with the federated token.
func (p *Provider) blueprintForm(scope string) (url.Values, error) {
	assertion, err := p.readFederatedToken()
	if err != nil {
		return nil, err
	}
	return url.Values{
		"grant_type": {grantClientCredentials}, "client_id": {p.cfg.BlueprintAppID}, "scope": {scope},
		"client_assertion_type": {clientAssertionTypeJWT}, "client_assertion": {assertion},
	}, nil
}

// graphToken is the blueprint's Microsoft Graph token, cached until close to
// its expiry. The lock guards only the cached value: a refresh runs outside
// it, and concurrent callers share one refresh (singleflight) instead of
// queueing behind it or each minting their own.
func (p *Provider) graphToken(ctx context.Context) (string, error) {
	p.graphMu.Lock()
	cached := p.graph
	p.graphMu.Unlock()
	if cached.token != "" && p.now().Add(tokenRefreshSkew).Before(cached.expiresAt) {
		return cached.token, nil
	}
	// The shared refresh must not die with the first caller's context;
	// each caller still stops waiting when its own context ends.
	result := p.graphFlight.DoChan("graph", func() (any, error) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultHTTPTimeout)
		defer cancel()
		form, err := p.blueprintForm(graphScope)
		if err != nil {
			return "", err
		}
		token, err := p.requestToken(refreshCtx, form)
		if err != nil {
			return "", err
		}
		p.graphMu.Lock()
		p.graph = token
		p.graphMu.Unlock()
		return token.token, nil
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	}
}

// dropGraphToken forgets a Graph token Graph refused.
func (p *Provider) dropGraphToken() {
	p.graphMu.Lock()
	p.graph = issued{}
	p.graphMu.Unlock()
}

// t1 returns the blueprint token for one agent identity: the client
// credentials grant with fmi_path = the agent's client id. It is cached per
// agent in the bounded LRU and never leaves the package except through
// AgentAssertionSource.
func (p *Provider) t1(ctx context.Context, agentAppID string) (string, error) {
	if cached, ok := p.agents.t1(agentAppID, p.now().Add(tokenRefreshSkew)); ok {
		return cached, nil
	}
	form, err := p.blueprintForm(tokenExchangeScope)
	if err != nil {
		return "", err
	}
	form.Set("fmi_path", agentAppID)
	token, err := p.requestToken(ctx, form)
	if err != nil {
		if errors.Is(err, ErrIdentityDisabled) {
			p.agents.evictApp(agentAppID)
		}
		return "", err
	}
	claims, ok := unverifiedClaims(token.token)
	if !ok || claims.TenantID != p.cfg.TenantID || claims.clientID() != p.cfg.BlueprintAppID {
		return "", ErrTokenValidation
	}
	p.logger.LogAttrs(ctx, slog.LevelDebug, "entra agent id agent assertion issued", slog.String("agent_app_id", agentAppID),
		slog.String("uti", claims.UTI), slog.String("request_id", token.requestID), slog.String("client_request_id", token.correlationID))
	p.agents.putT1(agentAppID, token.token, token.expiresAt)
	return token.token, nil
}

// agentToken is the agent identity's own client credentials grant with T1
// as its client assertion: an app-only resource token.
func (p *Provider) agentToken(ctx context.Context, agentAppID string, scopes []string) (issued, error) {
	assertion, err := p.t1(ctx, agentAppID)
	if err != nil {
		return issued{}, err
	}
	token, err := p.requestToken(ctx, url.Values{
		"grant_type": {grantClientCredentials}, "client_id": {agentAppID}, "scope": {strings.Join(scopes, " ")},
		"client_assertion_type": {clientAssertionTypeJWT}, "client_assertion": {assertion},
	})
	if errors.Is(err, ErrIdentityDisabled) {
		p.agents.evictApp(agentAppID)
	}
	return token, err
}

type correlationKey struct{}

// withCorrelation puts the client-request-id every token request of one
// call sends.
func withCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

func correlationFrom(ctx context.Context) string {
	if id, ok := ctx.Value(correlationKey{}).(string); ok && id != "" {
		return id
	}
	return uuid.NewString()
}

type t1ErrorKey struct{}

// t1Failure records why the assertion source failed: the obo package maps
// every source error to one value, and a disabled identity must stay one.
type t1Failure struct{ err error }

// agentCache is the bounded LRU of the agent identities this process may
// mint tokens for: object id → client id and the cached T1. Only an
// identity last seen owned by this blueprint and enabled is put here;
// every disable (Suspend, Entra refusing a disabled client, a Resolve that
// reads it disabled or not owned) evicts it.
type agentCache struct {
	mu      sync.Mutex
	size    int
	order   *list.List
	objects map[string]*list.Element
	apps    map[string]*list.Element
}

type agentEntry struct {
	objectID string
	appID    string
	t1       string
	t1Expiry time.Time
}

func newAgentCache(size int) *agentCache {
	return &agentCache{size: size, order: list.New(), objects: map[string]*list.Element{}, apps: map[string]*list.Element{}}
}

func (c *agentCache) appID(objectID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.objects[objectID]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(element)
	return element.Value.(*agentEntry).appID, true
}

func (c *agentCache) put(objectID, appID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.objects[objectID]; ok {
		entry := element.Value.(*agentEntry)
		if entry.appID == appID {
			c.order.MoveToFront(element)
			return
		}
		c.removeLocked(element)
	}
	if element, ok := c.apps[appID]; ok {
		c.removeLocked(element)
	}
	element := c.order.PushFront(&agentEntry{objectID: objectID, appID: appID})
	c.objects[objectID], c.apps[appID] = element, element
	for c.order.Len() > c.size {
		c.removeLocked(c.order.Back())
	}
}

func (c *agentCache) removeLocked(element *list.Element) {
	entry := element.Value.(*agentEntry)
	c.order.Remove(element)
	delete(c.objects, entry.objectID)
	delete(c.apps, entry.appID)
}

func (c *agentCache) knownApp(appID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.apps[appID]
	return ok
}

func (c *agentCache) t1(appID string, validAt time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.apps[appID]
	if !ok {
		return "", false
	}
	entry := element.Value.(*agentEntry)
	if entry.t1 == "" || !validAt.Before(entry.t1Expiry) {
		return "", false
	}
	return entry.t1, true
}

func (c *agentCache) putT1(appID, t1 string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.apps[appID]; ok {
		entry := element.Value.(*agentEntry)
		entry.t1, entry.t1Expiry = t1, expiry
	}
}

func (c *agentCache) evict(objectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.objects[objectID]; ok {
		c.removeLocked(element)
	}
}

func (c *agentCache) evictApp(appID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.apps[appID]; ok {
		c.removeLocked(element)
	}
}

func (c *agentCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
