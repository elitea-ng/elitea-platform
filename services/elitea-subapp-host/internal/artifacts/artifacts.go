// Package artifacts is the host's client for the platform's artifact API:
// one object, into one bucket, over the transport a request carried.
//
// It was DeepWiki's (internal/apps/deepwiki/run/artifacts.go) until ADR-0023
// stage H4c, and nothing in it is DeepWiki's: the route, the bearer, the
// multipart shape and the refusal texts are the platform's, and any
// sub-application that produces artifact objects uploads them exactly this
// way. What stayed behind in DeepWiki's package is the part that is not
// generic — deriving these settings from the legacy llm_settings dict, and
// deciding when there is no transport at all.
//
// TODO(#1359 U5): this is still an HTTP callback into elitea-main's
// artifact API (cleartext in compose). The engines' model calls already moved
// to elitea.llm.v1.ModelService on the platform listener (U2); this hop moves
// to gRPC over mTLS with the artifacts unit.
package artifacts

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client puts one object into a bucket. The real one talks to the
// platform's artifact API; tests hand in a fake.
type Client interface {
	Upload(ctx context.Context, bucket, name string, data []byte) error
}

// Factory builds the client for one invocation from its llm_settings — the
// transport the facade hands over: the callback base URL and the bearer it
// minted. nil means "no transport", and nothing uploads.
type Factory func(llmSettings map[string]any) (Client, error)

// Settings is what extract_artifact_settings produced from the llm_settings
// dict: the platform base URL with the LLM suffix stripped, the bearer, and
// the project the bucket belongs to.
type Settings struct {
	BaseURL   string
	APIKey    string
	ProjectID string
	APIPath   string
	XSecret   string
}

// DefaultAPIPath is the platform API prefix the artifact routes live under.
const DefaultAPIPath = "/api/v2"

// HTTPClient uploads through the platform's artifact API:
// POST {base}{api_path}/artifacts/objects/{project}/{bucket}?overwrite=true
// as multipart with the object in the `file` part, bearer-authenticated.
type HTTPClient struct {
	Settings Settings
	Client   *http.Client
}

// CallbackTLS is the TLS the callback hop uses: the bundle it trusts and,
// when the hop is elitea-main's platform listener (#1359), the client
// certificate it presents there — that listener requires one signed by the
// runtime CA. All empty keeps the platform's default roots and no identity.
type CallbackTLS struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

// NewHTTPClient validates the settings the way the Python client did and
// builds the transport, trusting callback.CAFile and presenting
// callback.CertFile/KeyFile when given.
func NewHTTPClient(settings Settings, callback CallbackTLS) (*HTTPClient, error) {
	if settings.BaseURL == "" {
		return nil, fmt.Errorf("artifact_settings.base_url is required")
	}
	if settings.APIKey == "" {
		return nil, fmt.Errorf("artifact_settings.api_key is required")
	}
	if settings.APIPath == "" {
		settings.APIPath = DefaultAPIPath
	}
	if (callback.CertFile == "") != (callback.KeyFile == "") {
		return nil, fmt.Errorf("the callback client certificate and key must be set together")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if callback.CAFile != "" || callback.CertFile != "" {
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if callback.CAFile != "" {
			pem, err := os.ReadFile(callback.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read the callback CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s holds no certificate", callback.CAFile)
			}
			config.RootCAs = pool
		}
		if callback.CertFile != "" {
			pair, err := tls.LoadX509KeyPair(callback.CertFile, callback.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("load the callback client certificate: %w", err)
			}
			config.Certificates = []tls.Certificate{pair}
		}
		transport.TLSClientConfig = config
	}
	return &HTTPClient{Settings: settings, Client: &http.Client{Transport: transport, Timeout: 300 * time.Second}}, nil
}

// ObjectsURL is the collection route for a bucket; the bucket is lowercased
// and escaped as one segment.
func (c *HTTPClient) ObjectsURL(bucket string) string {
	return c.Settings.BaseURL + c.Settings.APIPath + "/artifacts/objects/" + c.Settings.ProjectID + "/" + url.PathEscape(strings.ToLower(bucket))
}

// Upload puts one object, overwriting an existing one. A 403 and every
// other non-2xx are errors carrying the platform's own text, as the Python
// client raised them — the runner reports them in band.
func (c *HTTPClient) Upload(ctx context.Context, bucket, name string, data []byte) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	// No Content-Type on the part, deliberately. The Python client sent
	// none and elitea-main then derives the object's type from the key's
	// extension (.json → application/json, .md → text/markdown), which is
	// what the wiki browser's manifest read depends on. multipart's
	// CreateFormFile would label every part application/octet-stream, the
	// server would keep that, and a manifest served as octet-stream is a
	// wiki the browser cannot see — the local E2E run that found this had
	// every page land and no title.
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(name)))
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ObjectsURL(bucket)+"?overwrite=true", &body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Settings.APIKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := c.Client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	text, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	// The texts are the Python client's, capitals included: they surface
	// verbatim in the in-band "⚠️ … could not be uploaded" message.
	switch {
	case response.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Not authorized to upload artifact (HTTP 403): %s", clip(string(text), 200)) //nolint:staticcheck // legacy message text
	case response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated:
		return fmt.Errorf("Failed to upload artifact: HTTP %d — %s", response.StatusCode, clip(string(text), 500)) //nolint:staticcheck // legacy message text
	}
	return nil
}

// escapeQuotes is multipart's own (unexported) quoting for a filename.
func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
