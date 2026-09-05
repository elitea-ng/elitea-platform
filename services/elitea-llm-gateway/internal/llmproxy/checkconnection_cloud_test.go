package llmproxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// checkconnection_cloud_test.go covers the amazon_bedrock and vertex_ai
// checkers.
//
// NO LIVE CLOUD PROVIDER IS CONTACTED, and none can be from here: this
// checkout has no AWS or GCP credentials. The tests therefore stand a FAKE
// DIALER in front of the probes — fakeCloudDialer below rewrites the
// destination of the real, production URL the probe built and hands it to a
// loopback httptest server, while keeping the original Host header. That lets
// every assertion be made against what the probe actually put on the wire (the
// derived host, the path, the SigV4 Authorization header, the bearer token),
// rather than against a mock of the probe itself.
//
// What that CANNOT prove is that AWS and Google accept the signature and the
// token. Those two facts are borrowed from bifrost/core, whose own bedrock and
// vertex providers sign and authenticate the same way against the same
// endpoints in production — see the doc comments on the probes.

// --- fake dialer ----------------------------------------------------------

// fakeCloudDialer sends every request to base, whatever host the probe asked
// for, and records what the probe built. It stands where the SSRF-guarded
// dialer stands in production (newCheckConnectionProbeClient), which is why it
// is wired as the client's Transport and not as a URL the probe is told to use:
// the probe must keep deriving its own production URL for the test to mean
// anything.
type fakeCloudDialer struct {
	base *url.URL
	next http.RoundTripper

	mu    sync.Mutex
	hosts []string
}

func (d *fakeCloudDialer) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.hosts = append(d.hosts, r.URL.Host)
	d.mu.Unlock()

	clone := r.Clone(r.Context())
	// Keep the Host header the probe derived so the fake server can assert it.
	clone.Host = r.URL.Host
	clone.URL.Scheme = d.base.Scheme
	clone.URL.Host = d.base.Host
	return d.next.RoundTrip(clone)
}

func (d *fakeCloudDialer) dialledHosts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.hosts...)
}

// fakeCloudProvider is a loopback stand-in for the AWS and Google endpoints.
// It answers the two Vertex hops (token exchange, publisher-model listing) and
// the single Bedrock hop, and records every request it received.
type fakeCloudProvider struct {
	*httptest.Server

	hits atomic.Int64

	mu       sync.Mutex
	requests []recordedRequest

	// status is the answer given to the provider API call (not the token
	// exchange), so a test can make the credential "rejected".
	status int
	// tokenStatus is the answer given to the OAuth token exchange.
	tokenStatus int
}

type recordedRequest struct {
	host   string
	path   string
	query  string
	method string
	auth   string
	amzSHA string
	amzDte string
}

func newFakeCloudProvider(status, tokenStatus int) *fakeCloudProvider {
	fp := &fakeCloudProvider{status: status, tokenStatus: tokenStatus}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.hits.Add(1)
		fp.mu.Lock()
		fp.requests = append(fp.requests, recordedRequest{
			host:   r.Host,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			method: r.Method,
			auth:   r.Header.Get("Authorization"),
			amzSHA: r.Header.Get("X-Amz-Content-Sha256"),
			amzDte: r.Header.Get("X-Amz-Date"),
		})
		fp.mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/token") {
			if fp.tokenStatus != http.StatusOK {
				w.WriteHeader(fp.tokenStatus)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"ya29.fake-access-token","token_type":"Bearer","expires_in":3600}`))
			return
		}

		w.WriteHeader(fp.status)
		_, _ = w.Write([]byte(`{}`))
	}))
	return fp
}

func (fp *fakeCloudProvider) Hits() int64 { return fp.hits.Load() }

func (fp *fakeCloudProvider) recorded() []recordedRequest {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]recordedRequest(nil), fp.requests...)
}

// find returns the recorded request whose path contains want.
func (fp *fakeCloudProvider) find(t *testing.T, want string) recordedRequest {
	t.Helper()
	for _, rec := range fp.recorded() {
		if strings.Contains(rec.path, want) {
			return rec
		}
	}
	t.Fatalf("no request with path containing %q; got %+v", want, fp.recorded())
	return recordedRequest{}
}

// fakeCloudClient builds the http.Client the probes are given: a real client
// whose transport is the fake dialer above.
func fakeCloudClient(t *testing.T, fp *fakeCloudProvider) (*http.Client, *fakeCloudDialer) {
	t.Helper()
	base, err := url.Parse(fp.URL)
	if err != nil {
		t.Fatalf("parse fake provider url: %v", err)
	}
	dialer := &fakeCloudDialer{base: base, next: fp.Client().Transport}
	return &http.Client{Transport: dialer}, dialer
}

// --- test service-account document ---------------------------------------

// newTestServiceAccountJSON builds a real, structurally valid Google
// service-account key document around a freshly generated RSA key, so
// x/oauth2/google actually parses it and actually signs a JWT assertion with
// it. tokenURI points the exchange at the fake provider.
func newTestServiceAccountJSON(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	doc := map[string]string{
		"type":           "service_account",
		"project_id":     "elitea-test-project",
		"private_key_id": "test-key-id",
		"private_key":    pemKey,
		"client_email":   "elitea-test@elitea-test-project.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      tokenURI,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	return string(raw)
}

// --- amazon_bedrock -------------------------------------------------------

// TestProbeBedrock_SignsRealListFoundationModelsCall proves the probe builds
// the real Bedrock control-plane URL for the credential's region and signs it
// with SigV4 over the tenant's own keys. A stub that answered without calling
// anything would leave fp.Hits() at zero.
func TestProbeBedrock_SignsRealListFoundationModelsCall(t *testing.T) {
	fp := newFakeCloudProvider(http.StatusOK, http.StatusOK)
	defer fp.Close()
	client, dialer := fakeCloudClient(t, fp)

	err := probeBedrockFoundationModels(context.Background(), client, checkConnectionRequest{
		Type:               "amazon_bedrock",
		AWSAccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		AWSSecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		AWSRegionName:      "us-east-1",
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if fp.Hits() != 1 {
		t.Fatalf("expected exactly one real round trip, got %d", fp.Hits())
	}

	if got := dialer.dialledHosts(); len(got) != 1 || got[0] != "bedrock.us-east-1.amazonaws.com" {
		t.Fatalf("probe dialled %v, want [bedrock.us-east-1.amazonaws.com]", got)
	}

	rec := fp.find(t, "/foundation-models")
	if rec.method != http.MethodGet {
		t.Fatalf("method = %q, want GET (a check must never invoke a model)", rec.method)
	}
	if rec.host != "bedrock.us-east-1.amazonaws.com" {
		t.Fatalf("Host header = %q, want bedrock.us-east-1.amazonaws.com", rec.host)
	}
	// The empty-body payload hash SigV4 requires.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if rec.amzSHA != emptySHA256 {
		t.Fatalf("x-amz-content-sha256 = %q, want the empty-body hash", rec.amzSHA)
	}
	if rec.amzDte == "" {
		t.Fatal("X-Amz-Date must be set by the signer")
	}
	if !strings.HasPrefix(rec.auth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want an AWS4-HMAC-SHA256 signature", rec.auth)
	}
	if !strings.Contains(rec.auth, "/us-east-1/bedrock/aws4_request") {
		t.Fatalf("Authorization scope = %q, want the us-east-1 bedrock scope", rec.auth)
	}
	if !strings.Contains(rec.auth, "Credential=AKIAIOSFODNN7EXAMPLE/") {
		t.Fatalf("Authorization = %q, want the supplied access key id in the credential scope", rec.auth)
	}
	// The signature must never carry the secret key itself.
	if strings.Contains(rec.auth, "wJalrXUtnFEMI") {
		t.Fatalf("Authorization leaked the secret access key: %q", rec.auth)
	}
}

// TestProbeBedrock_RejectedCredentialIsUnauthorized proves an AWS refusal is
// reported as a rejected credential and that the verdict came from a real
// round trip, not a canned answer.
func TestProbeBedrock_RejectedCredentialIsUnauthorized(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantReason string
	}{
		{"forbidden", http.StatusForbidden, checkConnectionReasonUnauth},
		{"unauthorized", http.StatusUnauthorized, checkConnectionReasonUnauth},
		{"server error", http.StatusBadGateway, checkConnectionReasonUpstream},
		{"not found", http.StatusNotFound, checkConnectionReasonUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakeCloudProvider(tc.status, http.StatusOK)
			defer fp.Close()
			client, _ := fakeCloudClient(t, fp)

			err := probeBedrockFoundationModels(context.Background(), client, checkConnectionRequest{
				Type:               "amazon_bedrock",
				AWSAccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
				AWSSecretAccessKey: "not-the-real-secret",
				AWSRegionName:      "eu-central-1",
			})
			if err == nil {
				t.Fatal("expected a failure for a rejected credential")
			}
			reason, detail := classifyCheckConnectionProbeError(err)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if strings.Contains(detail, "not-the-real-secret") {
				t.Fatalf("detail leaked the credential: %q", detail)
			}
			if fp.Hits() != 1 {
				t.Fatalf("expected exactly one real round trip, got %d", fp.Hits())
			}
		})
	}
}

// --- vertex_ai ------------------------------------------------------------

// TestProbeVertex_MintsTokenThenListsPublisherModels proves the whole two-hop
// probe runs for real: the service-account assertion is exchanged at the
// document's token_uri, and the minted token is then presented to the Vertex
// host derived from the credential's location.
func TestProbeVertex_MintsTokenThenListsPublisherModels(t *testing.T) {
	fp := newFakeCloudProvider(http.StatusOK, http.StatusOK)
	defer fp.Close()
	client, dialer := fakeCloudClient(t, fp)

	sa := newTestServiceAccountJSON(t, "https://oauth2.googleapis.com/token")
	err := probeVertexPublisherModels(context.Background(), client, checkConnectionRequest{
		Type:              "vertex_ai",
		VertexProject:     "elitea-test-project",
		VertexLocation:    "us-central1",
		VertexCredentials: jsonTextField(sa),
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if fp.Hits() != 2 {
		t.Fatalf("expected two real round trips (token + list), got %d", fp.Hits())
	}

	hosts := dialer.dialledHosts()
	if len(hosts) != 2 {
		t.Fatalf("dialled hosts = %v, want two", hosts)
	}
	if hosts[0] != "oauth2.googleapis.com" {
		t.Fatalf("first hop host = %q, want oauth2.googleapis.com", hosts[0])
	}
	if hosts[1] != "us-central1-aiplatform.googleapis.com" {
		t.Fatalf("second hop host = %q, want us-central1-aiplatform.googleapis.com", hosts[1])
	}

	list := fp.find(t, "/publishers/google/models")
	if list.method != http.MethodGet {
		t.Fatalf("method = %q, want GET (a check must never invoke a model)", list.method)
	}
	if list.path != "/v1beta1/publishers/google/models" {
		t.Fatalf("path = %q, want /v1beta1/publishers/google/models", list.path)
	}
	if list.query != "pageSize=1" {
		t.Fatalf("query = %q, want pageSize=1", list.query)
	}
	if list.auth != "Bearer ya29.fake-access-token" {
		t.Fatalf("Authorization = %q, want the minted bearer token", list.auth)
	}
}

// TestProbeVertex_GlobalLocationUsesThePlainHost pins the location→host rule
// against bifrost's own getVertexModelListingAPIHost: the multi-region pools
// and "global" list publisher models on the unprefixed host.
func TestProbeVertex_GlobalLocationUsesThePlainHost(t *testing.T) {
	cases := []struct {
		location string
		wantHost string
	}{
		{"global", "aiplatform.googleapis.com"},
		{"us", "aiplatform.googleapis.com"},
		{"eu", "aiplatform.googleapis.com"},
		{"us-central1", "us-central1-aiplatform.googleapis.com"},
		{"europe-west4", "europe-west4-aiplatform.googleapis.com"},
		{"northamerica-northeast1", "northamerica-northeast1-aiplatform.googleapis.com"},
	}
	for _, c := range cases {
		if got := vertexCheckAPIHost(c.location); got != c.wantHost {
			t.Errorf("vertexCheckAPIHost(%q) = %q, want %q", c.location, got, c.wantHost)
		}
	}
}

// TestProbeVertex_TokenExchangeRefusalIsUnauthorized proves a service-account
// key Google refuses is reported as a rejected credential — Google answers the
// JWT-assertion exchange with 400 invalid_grant, not 401, so a naive status
// passthrough would have reported "upstream_error" for a plainly bad key.
func TestProbeVertex_TokenExchangeRefusalIsUnauthorized(t *testing.T) {
	for _, tokenStatus := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(fmt.Sprintf("token status %d", tokenStatus), func(t *testing.T) {
			fp := newFakeCloudProvider(http.StatusOK, tokenStatus)
			defer fp.Close()
			client, _ := fakeCloudClient(t, fp)

			sa := newTestServiceAccountJSON(t, "https://oauth2.googleapis.com/token")
			err := probeVertexPublisherModels(context.Background(), client, checkConnectionRequest{
				Type:              "vertex_ai",
				VertexProject:     "elitea-test-project",
				VertexLocation:    "us-central1",
				VertexCredentials: jsonTextField(sa),
			})
			if err == nil {
				t.Fatal("expected a failure when google refuses the assertion")
			}
			reason, _ := classifyCheckConnectionProbeError(err)
			if reason != checkConnectionReasonUnauth {
				t.Fatalf("reason = %q, want %q", reason, checkConnectionReasonUnauth)
			}
			// Exactly one hop: the Vertex API must never be called with no token.
			if fp.Hits() != 1 {
				t.Fatalf("expected only the token exchange, got %d requests", fp.Hits())
			}
		})
	}
}

// TestProbeVertex_ListRefusalIsUnauthorized proves a token Vertex itself
// rejects is reported as a rejected credential, and that the response never
// echoes the provider body.
func TestProbeVertex_ListRefusalIsUnauthorized(t *testing.T) {
	fp := newFakeCloudProvider(http.StatusForbidden, http.StatusOK)
	defer fp.Close()
	client, _ := fakeCloudClient(t, fp)

	sa := newTestServiceAccountJSON(t, "https://oauth2.googleapis.com/token")
	err := probeVertexPublisherModels(context.Background(), client, checkConnectionRequest{
		Type:              "vertex_ai",
		VertexProject:     "elitea-test-project",
		VertexLocation:    "us-central1",
		VertexCredentials: jsonTextField(sa),
	})
	if err == nil {
		t.Fatal("expected a failure when vertex rejects the token")
	}
	reason, detail := classifyCheckConnectionProbeError(err)
	if reason != checkConnectionReasonUnauth {
		t.Fatalf("reason = %q, want %q", reason, checkConnectionReasonUnauth)
	}
	if detail == "{}" || strings.Contains(detail, "invalid_grant") {
		t.Fatalf("detail must not echo the provider body, got %q", detail)
	}
	if fp.Hits() != 2 {
		t.Fatalf("expected the token exchange and the list call, got %d", fp.Hits())
	}
}

// --- target resolution (the allowlist gate's input) -----------------------

// TestCheckConnectionCloudTargets is the table that pins what
// CheckConnection puts through the egress allowlist, and which payloads are
// refused before anything is dialled at all. Every host the probes contact
// must appear here — a host missing from this list is a host the allowlist
// never sees.
func TestCheckConnectionCloudTargets(t *testing.T) {
	goodSA := `{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token"}`

	cases := []struct {
		name        string
		req         checkConnectionRequest
		wantTargets []string
		wantReason  string
		wantDetail  string
	}{
		{
			name: "bedrock derives the control-plane host from the region",
			req: checkConnectionRequest{
				Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s", AWSRegionName: "eu-central-1",
			},
			wantTargets: []string{"https://bedrock.eu-central-1.amazonaws.com"},
		},
		{
			name:       "bedrock without any field names every missing one",
			req:        checkConnectionRequest{Type: "amazon_bedrock"},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "this credential is missing: aws_access_key_id, aws_secret_access_key, aws_region_name",
		},
		{
			name: "bedrock without a region is refused before any dial",
			req: checkConnectionRequest{
				Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s",
			},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "this credential is missing: aws_region_name",
		},
		{
			// The region is interpolated into the host name, so a value that
			// carries a path or an authority must never reach the URL builder.
			name: "bedrock refuses a region that would re-point the host",
			req: checkConnectionRequest{
				Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s",
				AWSRegionName: "us-east-1.attacker.example/",
			},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "aws_region_name is not a valid AWS region name",
		},
		{
			name: "bedrock refuses a region carrying userinfo",
			req: checkConnectionRequest{
				Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s",
				AWSRegionName: "us-east-1@attacker.example",
			},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "aws_region_name is not a valid AWS region name",
		},
		{
			name: "vertex gates both the api host and the token endpoint",
			req: checkConnectionRequest{
				Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1",
				VertexCredentials: jsonTextField(goodSA),
			},
			wantTargets: []string{
				"https://us-central1-aiplatform.googleapis.com",
				"https://oauth2.googleapis.com/token",
			},
		},
		{
			name: "vertex falls back to google's own token endpoint",
			req: checkConnectionRequest{
				Type: "vertex_ai", VertexProject: "p", VertexLocation: "global",
				VertexCredentials: jsonTextField(`{"type":"service_account"}`),
			},
			wantTargets: []string{
				"https://aiplatform.googleapis.com",
				vertexDefaultTokenURI,
			},
		},
		{
			// A tenant-authored token_uri is a destination the allowlist has
			// to see; this asserts it is the one returned, not the default.
			name: "vertex returns the document's own token endpoint",
			req:  closureVertexReq(`{"type":"service_account","token_uri":"https://sts.example.com/token"}`),
			wantTargets: []string{
				"https://us-central1-aiplatform.googleapis.com",
				"https://sts.example.com/token",
			},
		},
		{
			name:       "vertex without any field names every missing one",
			req:        checkConnectionRequest{Type: "vertex_ai"},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "this credential is missing: vertex_project, vertex_location, vertex_credentials",
		},
		{
			name: "vertex refuses a location that would re-point the host",
			req: checkConnectionRequest{
				Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1/../..",
				VertexCredentials: jsonTextField(goodSA),
			},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "vertex_location is not a valid Vertex AI location",
		},
		{
			name: "vertex refuses a credential document that is not JSON",
			req: checkConnectionRequest{
				Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1",
				VertexCredentials: jsonTextField("not json at all"),
			},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "vertex_credentials is not a JSON document",
		},
		{
			// external_account documents name their own credential source and
			// impersonation URLs, which this file cannot enumerate for the
			// allowlist, so they are refused rather than fetched ungated.
			name: "vertex refuses a non service-account credential type",
			req: checkConnectionRequest{
				Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1",
				VertexCredentials: jsonTextField(`{"type":"external_account","token_url":"https://sts.example.com"}`),
			},
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "vertex_credentials must be a Google service-account key document",
		},
		{
			name: "vertex refuses a plaintext token endpoint",
			req:  closureVertexReq(`{"type":"service_account","token_uri":"http://oauth2.googleapis.com/token"}`),
			// A signed assertion for the tenant's own service account must
			// never go out over http.
			wantReason: checkConnectionReasonMissingFields,
			wantDetail: "vertex_credentials names a token_uri that is not an https URL",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider, ok := checkConnectionProviders[c.req.Type]
			if !ok {
				t.Fatalf("type %q is not registered as checkable", c.req.Type)
			}
			targets, err := provider.dialTargets(c.req)
			if c.wantReason != "" {
				var fe *checkConnectionFieldError
				if !errorAsField(err, &fe) {
					t.Fatalf("err = %v, want a checkConnectionFieldError", err)
				}
				if fe.reason != c.wantReason {
					t.Fatalf("reason = %q, want %q", fe.reason, c.wantReason)
				}
				if fe.detail != c.wantDetail {
					t.Fatalf("detail = %q, want %q", fe.detail, c.wantDetail)
				}
				if len(targets) != 0 {
					t.Fatalf("a refused payload must yield no dial target, got %v", targets)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(targets) != len(c.wantTargets) {
				t.Fatalf("targets = %v, want %v", targets, c.wantTargets)
			}
			for i, want := range c.wantTargets {
				if targets[i] != want {
					t.Fatalf("target[%d] = %q, want %q", i, targets[i], want)
				}
			}
		})
	}
}

// closureVertexReq is a small helper so a table row can vary only the
// credential document.
func closureVertexReq(sa string) checkConnectionRequest {
	return checkConnectionRequest{
		Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1",
		VertexCredentials: jsonTextField(sa),
	}
}

func errorAsField(err error, target **checkConnectionFieldError) bool {
	return errors.As(err, target)
}

// --- handler-level: the egress allowlist gate ----------------------------

// TestCheckConnection_CloudTypesAreCheckable proves both types have left the
// "not supported yet" set — the pre-existing test asserted amazon_bedrock
// answered unsupported_type, and that is now wrong.
func TestCheckConnection_CloudTypesAreCheckable(t *testing.T) {
	for _, configType := range []string{"amazon_bedrock", "vertex_ai"} {
		if _, ok := checkConnectionProviders[configType]; !ok {
			t.Errorf("%s must be a checkable type", configType)
		}
	}
}

// TestCheckConnection_BedrockEgressDeniedNeverDials is the SSRF gate for the
// derived Bedrock host: the region is tenant-authored, so the host it produces
// is put through the operator's allowlist like any other destination, and a
// refusal happens before any dial.
func TestCheckConnection_BedrockEgressDeniedNeverDials(t *testing.T) {
	fp := newFakeCloudProvider(http.StatusOK, http.StatusOK)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false, configured: true})
	rec := doCheckConnection(t, h, checkConnectionRequest{
		Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s", AWSRegionName: "us-east-1",
	})

	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("got success=%v reason=%q, want success=false reason=%q", resp.Success, resp.Reason, checkConnectionReasonEgress)
	}
	if fp.Hits() != 0 {
		t.Fatalf("nothing may be dialled when egress denies the host, got %d hits", fp.Hits())
	}
}

// TestCheckConnection_VertexEgressDeniedNeverDials proves the same for
// vertex_ai — and it is the case that matters most, because the vertex probe's
// FIRST hop is the token endpoint named inside the tenant's own credential
// document.
func TestCheckConnection_VertexEgressDeniedNeverDials(t *testing.T) {
	fp := newFakeCloudProvider(http.StatusOK, http.StatusOK)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false, configured: true})
	rec := doCheckConnection(t, h, checkConnectionRequest{
		Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1",
		VertexCredentials: jsonTextField(`{"type":"service_account","token_uri":"https://sts.example.com/token"}`),
	})

	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("got success=%v reason=%q, want success=false reason=%q", resp.Success, resp.Reason, checkConnectionReasonEgress)
	}
	if fp.Hits() != 0 {
		t.Fatalf("nothing may be dialled when egress denies the host, got %d hits", fp.Hits())
	}
}

// TestCheckConnection_CloudNilPolicyFailsClosed proves a Handler with no
// egress policy refuses both new types rather than skipping the gate, exactly
// as it already did for the api_base types.
func TestCheckConnection_CloudNilPolicyFailsClosed(t *testing.T) {
	for _, req := range []checkConnectionRequest{
		{Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s", AWSRegionName: "us-east-1"},
		{Type: "vertex_ai", VertexProject: "p", VertexLocation: "us-central1",
			VertexCredentials: jsonTextField(`{"type":"service_account"}`)},
	} {
		h := NewHandler(nil, nil, nil) // no WithEgressPolicy
		rec := doCheckConnection(t, h, req)
		resp := decodeCheckConnectionResponse(t, rec)
		if resp.Success {
			t.Fatalf("%s: an unwired egress policy must fail closed", req.Type)
		}
		if resp.Reason != checkConnectionReasonEgress {
			t.Fatalf("%s: reason = %q, want %q", req.Type, resp.Reason, checkConnectionReasonEgress)
		}
	}
}

// TestCheckConnection_CloudMissingFieldsNeverDials proves an incomplete cloud
// credential is refused with the FIELD names and no network call — the answer
// must never be a fabricated success, and it must never be produced by falling
// back to the platform's own ambient AWS/GCP identity.
func TestCheckConnection_CloudMissingFieldsNeverDials(t *testing.T) {
	fp := newFakeCloudProvider(http.StatusOK, http.StatusOK)
	defer fp.Close()

	cases := []struct {
		name string
		req  checkConnectionRequest
		want string
	}{
		{"bedrock with no fields", checkConnectionRequest{Type: "amazon_bedrock"},
			"this credential is missing: aws_access_key_id, aws_secret_access_key, aws_region_name"},
		{"vertex with no fields", checkConnectionRequest{Type: "vertex_ai"},
			"this credential is missing: vertex_project, vertex_location, vertex_credentials"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
			rec := doCheckConnection(t, h, c.req)
			resp := decodeCheckConnectionResponse(t, rec)
			if resp.Success {
				t.Fatal("an incomplete credential must never report success")
			}
			if resp.Reason != checkConnectionReasonMissingFields {
				t.Fatalf("reason = %q, want %q", resp.Reason, checkConnectionReasonMissingFields)
			}
			if resp.Detail != c.want {
				t.Fatalf("detail = %q, want %q", resp.Detail, c.want)
			}
			if fp.Hits() != 0 {
				t.Fatalf("no provider call may happen, got %d hits", fp.Hits())
			}
		})
	}
}

// TestCheckConnection_CloudNeverDialsPrivateAddress is the address-level half
// of the SSRF policy for the two cloud types. Neither resolves to a
// self-hosted provider class, so even with the allowlist armed the probe
// client must refuse a private destination.
//
// The decision is read through checkConnectionAllowsPrivateNetwork, which is
// the predicate the handler itself uses. A private api_base is supplied as
// well, to prove the answer does not become true when the endpoint looks
// self-hosted.
func TestCheckConnection_CloudNeverDialsPrivateAddress(t *testing.T) {
	for _, configType := range []string{"amazon_bedrock", "vertex_ai"} {
		for _, apiBase := range []string{"", "http://10.0.0.5:8000/v1"} {
			req := checkConnectionRequest{Type: configType, APIBase: apiBase}
			if checkConnectionAllowsPrivateNetwork(req) {
				t.Errorf("%s (api_base %q) may not reach a private address", configType, apiBase)
			}
		}
	}
}

// TestCheckConnectionVertexCredentials_AcceptsStringOrObject proves the wire
// field survives BOTH storage shapes the platform uses for the Google
// service-account document. A plain string field would fail to decode the
// object form, and a failed decode refuses a credential that is in fact valid
// (the defect account/credentials.go's jsonType comment records).
func TestCheckConnectionVertexCredentials_AcceptsStringOrObject(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "escaped json string",
			body: `{"type":"vertex_ai","vertex_credentials":"{\"type\":\"service_account\"}"}`,
			want: `{"type":"service_account"}`,
		},
		{
			name: "nested json object",
			body: `{"type":"vertex_ai","vertex_credentials":{"type":"service_account"}}`,
			want: `{"type":"service_account"}`,
		},
		{
			name: "absent",
			body: `{"type":"vertex_ai"}`,
			want: "",
		},
		{
			name: "null",
			body: `{"type":"vertex_ai","vertex_credentials":null}`,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req checkConnectionRequest
			if err := json.Unmarshal([]byte(c.body), &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(req.VertexCredentials) != c.want {
				t.Fatalf("vertex_credentials = %q, want %q", string(req.VertexCredentials), c.want)
			}
		})
	}
}
