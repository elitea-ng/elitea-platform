package agentid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	agentCollectionPath = "/v1.0/servicePrincipals/microsoft.graph.agentIdentity"
	graphPageSize       = 100
	// tagManaged marks every agent identity Elitea created or adopted;
	// tagRecordPrefix + the ExternalAgentIdentity id is the back-reference.
	tagManaged       = "elitea:managed"
	tagRecordPrefix  = "elitea:record:"
	tagProjectPrefix = "elitea:project:"
)

// errGraphNotFound is a Graph 404.
var errGraphNotFound = errors.New("entra agent id: graph object not found")

// agentObject is the part of a Graph agentIdentity the adapter reads.
type agentObject struct {
	ID             string   `json:"id"`
	AppID          string   `json:"appId"`
	DisplayName    string   `json:"displayName"`
	BlueprintID    string   `json:"agentIdentityBlueprintId"`
	AccountEnabled bool     `json:"accountEnabled"`
	Tags           []string `json:"tags"`
}

func (o agentObject) recordTag() string {
	for _, tag := range o.Tags {
		if strings.HasPrefix(tag, tagRecordPrefix) {
			return tag
		}
	}
	return ""
}

func (o agentObject) managed() bool { return slices.Contains(o.Tags, tagManaged) }

func (o agentObject) clientID() string {
	if o.AppID != "" {
		return o.AppID
	}
	return o.ID
}

type agentPage struct {
	Value    []agentObject `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
	Count    *int          `json:"@odata.count"`
}

// graphDo sends one Graph request. out, when set, receives a 2xx JSON body.
func (p *Provider) graphDo(ctx context.Context, method, target string, body, out any, eventual bool) error {
	token, err := p.graphToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return ErrInvalidRequest
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return ErrInvalidRequest
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set(clientRequestHeader, correlationFrom(ctx))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if eventual {
		// Advanced queries ($count, tags/any) need eventual consistency.
		request.Header.Set("ConsistencyLevel", "eventual")
	}
	response, err := p.http.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrUnavailable
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	_ = response.Body.Close()
	if readErr != nil || len(raw) > maxResponseBytes {
		return ErrUnavailable
	}
	p.logger.LogAttrs(ctx, slog.LevelDebug, "entra agent id graph call", slog.String("method", method),
		slog.Int("status", response.StatusCode), slog.String("request_id", response.Header.Get("request-id")),
		slog.String("client_request_id", request.Header.Get(clientRequestHeader)))
	switch {
	case response.StatusCode == http.StatusNotFound:
		return errGraphNotFound
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= 500:
		return ErrUnavailable
	case response.StatusCode == http.StatusUnauthorized:
		// A revoked or expired Graph token: drop it so the next call mints one.
		p.dropGraphToken()
		return ErrUnavailable
	case response.StatusCode < 200 || response.StatusCode > 299:
		return ErrRejected
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (p *Provider) collectionURL(query url.Values) string {
	return p.cfg.graphBaseURL() + agentCollectionPath + "?" + query.Encode()
}

func (p *Provider) objectURL(id string) string {
	return p.cfg.graphBaseURL() + "/v1.0/servicePrincipals/" + url.PathEscape(id) + "/microsoft.graph.agentIdentity"
}

func odataString(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func (p *Provider) blueprintFilter(clauses ...string) string {
	return strings.Join(append([]string{"agentIdentityBlueprintId eq " + odataString(p.cfg.BlueprintAppID)}, clauses...), " and ")
}

// findAgents lists the blueprint's agent identities matching clauses (one
// page of at most top).
func (p *Provider) findAgents(ctx context.Context, top int, clauses ...string) ([]agentObject, error) {
	query := url.Values{"$filter": {p.blueprintFilter(clauses...)}, "$top": {strconv.Itoa(top)}, "$count": {"true"}}
	var page agentPage
	if err := p.graphDo(ctx, http.MethodGet, p.collectionURL(query), nil, &page, true); err != nil {
		return nil, err
	}
	return page.Value, nil
}

// countAgents is the number of agent identities of the blueprint.
func (p *Provider) countAgents(ctx context.Context) (int, error) {
	query := url.Values{"$filter": {p.blueprintFilter()}, "$top": {"1"}, "$count": {"true"}}
	var page agentPage
	if err := p.graphDo(ctx, http.MethodGet, p.collectionURL(query), nil, &page, true); err != nil {
		return 0, err
	}
	if page.Count == nil {
		return 0, ErrUnavailable
	}
	return *page.Count, nil
}

func (p *Provider) getAgent(ctx context.Context, id string) (agentObject, error) {
	var object agentObject
	err := p.graphDo(ctx, http.MethodGet, p.objectURL(id), nil, &object, false)
	return object, err
}

func (p *Provider) getDeletedAgent(ctx context.Context, id string) (agentObject, error) {
	var object agentObject
	err := p.graphDo(ctx, http.MethodGet, p.cfg.graphBaseURL()+"/v1.0/directory/deletedItems/"+url.PathEscape(id), nil, &object, false)
	return object, err
}

func (p *Provider) setAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return p.graphDo(ctx, http.MethodPatch, p.objectURL(id), map[string]any{"accountEnabled": enabled}, nil, false)
}

// adoptPatch disables an adopted object and gives it the managed tag in one
// PATCH (tags replace the whole collection, so the existing ones are kept).
func (p *Provider) adoptPatch(ctx context.Context, object agentObject) error {
	tags := slices.Clone(object.Tags)
	if !object.managed() {
		tags = append(tags, tagManaged)
	}
	return p.graphDo(ctx, http.MethodPatch, p.objectURL(object.ID), map[string]any{"accountEnabled": false, "tags": tags}, nil, false)
}

func (p *Provider) deleteAgent(ctx context.Context, id string) error {
	return p.graphDo(ctx, http.MethodDelete, p.cfg.graphBaseURL()+"/v1.0/servicePrincipals/"+url.PathEscape(id), nil, nil, false)
}

func (p *Provider) createAgent(ctx context.Context, displayName, sponsorGroup string, tags []string) (agentObject, error) {
	body := map[string]any{
		"displayName":              displayName,
		"agentIdentityBlueprintId": p.cfg.BlueprintAppID,
		"accountEnabled":           false,
		"tags":                     tags,
		"sponsors@odata.bind":      []string{p.cfg.graphBaseURL() + "/v1.0/groups/" + sponsorGroup},
	}
	var object agentObject
	err := p.graphDo(ctx, http.MethodPost, p.cfg.graphBaseURL()+agentCollectionPath, body, &object, false)
	return object, err
}

// listPage reads one page of the blueprint's agent identities. token is
// empty or a nextLink Graph returned, which must point at the collection.
func (p *Provider) listPage(ctx context.Context, token string, size int) (agentPage, error) {
	target := token
	if target == "" {
		target = p.collectionURL(url.Values{"$filter": {p.blueprintFilter()}, "$top": {strconv.Itoa(size)}})
	} else if !strings.HasPrefix(target, p.cfg.graphBaseURL()+agentCollectionPath+"?") {
		// A page token is only ever a nextLink this adapter handed out; any
		// other value would send the Graph token elsewhere.
		return agentPage{}, ErrInvalidRequest
	}
	var page agentPage
	err := p.graphDo(ctx, http.MethodGet, target, nil, &page, false)
	return page, err
}
