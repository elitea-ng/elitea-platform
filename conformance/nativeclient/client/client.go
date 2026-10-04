// Package client is a black-box native client for the ADR-0025 `client`
// contract. It knows only what a mobile or desktop app can know: the
// deployment origin, the discovery document and the published operations.
// Nothing here imports services/elitea-main.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientVersionHeader is the header every native request carries (ADR-0025
// coordinator decision 13).
const ClientVersionHeader = "X-Client-Version"

// Client talks to one deployment.
type Client struct {
	// Origin is the deployment origin, e.g. https://elitea.example.com. Every
	// absolute path the server hands out (events_url, endpoints) is resolved
	// against it.
	Origin string
	// HTTP is the transport. It must not follow redirects for the token
	// endpoints to be observable; New sets that.
	HTTP *http.Client
	// Version is sent as X-Client-Version when non-empty.
	Version string
	// Token is the bearer access token; empty sends no Authorization header.
	Token string
}

// New builds a client for an origin.
func New(origin string) *Client {
	return &Client{
		Origin: strings.TrimRight(origin, "/"),
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// WithToken returns a copy that authenticates with token.
func (c *Client) WithToken(token string) *Client {
	copied := *c
	copied.Token = token
	return &copied
}

// WithVersion returns a copy that states version in X-Client-Version.
func (c *Client) WithVersion(version string) *Client {
	copied := *c
	copied.Version = version
	return &copied
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into target.
func (r *Response) JSON(target any) error {
	if err := json.Unmarshal(r.Body, target); err != nil {
		return fmt.Errorf("decode %d response %q: %w", r.Status, truncate(r.Body), err)
	}
	return nil
}

// Map decodes the body as a JSON object.
func (r *Response) Map() (map[string]any, error) {
	var out map[string]any
	return out, r.JSON(&out)
}

func (r *Response) String() string {
	return fmt.Sprintf("HTTP %d %s", r.Status, truncate(r.Body))
}

func truncate(body []byte) string {
	const limit = 600
	if len(body) > limit {
		return string(body[:limit]) + "…"
	}
	return string(body)
}

// Resolve turns an absolute path (or a full URL) into a URL on this origin.
func (c *Client) Resolve(pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	return c.Origin + pathOrURL
}

// NewRequest builds a request with the client's headers. body may be nil, a
// url.Values (form), []byte (sent as is) or any JSON-marshalable value.
func (c *Client) NewRequest(ctx context.Context, method, pathOrURL string, body any) (*http.Request, error) {
	var reader io.Reader
	contentType := ""
	switch value := body.(type) {
	case nil:
	case url.Values:
		reader = strings.NewReader(value.Encode())
		contentType = "application/x-www-form-urlencoded"
	case []byte:
		reader = bytes.NewReader(value)
		contentType = "application/json"
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	request, err := http.NewRequestWithContext(ctx, method, c.Resolve(pathOrURL), reader)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Version != "" {
		request.Header.Set(ClientVersionHeader, c.Version)
	}
	return request, nil
}

// Do sends a request and reads the whole response.
func (c *Client) Do(ctx context.Context, method, pathOrURL string, body any, header http.Header) (*Response, error) {
	request, err := c.NewRequest(ctx, method, pathOrURL, body)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, pathOrURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, pathOrURL, err)
	}
	return &Response{Status: response.StatusCode, Header: response.Header, Body: raw}, nil
}

// Get is Do with GET and no body.
func (c *Client) Get(ctx context.Context, pathOrURL string) (*Response, error) {
	return c.Do(ctx, http.MethodGet, pathOrURL, nil, nil)
}
