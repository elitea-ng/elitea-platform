package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// blockingHTTPSource holds every admission until released, so the action
// keeps its pool slot the way a 30 s remote call would.
type blockingHTTPSource struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingHTTPSource) ReadHTTPActionInput(ctx context.Context, _ ContentClaim) (HTTPActionInput, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return HTTPActionInput{}, errors.New("released")
}

type nopHTTPDeps struct{}

func (nopHTTPDeps) Rule(context.Context, int64, string) (app.Rule, error) {
	return app.Rule{}, app.ErrUnauthorized
}
func (nopHTTPDeps) Lookup(context.Context, app.Admission, app.Invocation) (app.Receipt, bool, error) {
	return app.Receipt{}, false, nil
}
func (nopHTTPDeps) Begin(context.Context, app.Admission, app.Invocation, app.Intent) (app.Receipt, bool, error) {
	return app.Receipt{}, false, app.ErrUnauthorized
}
func (nopHTTPDeps) Commit(context.Context, app.Admission, app.Receipt) error { return nil }
func (nopHTTPDeps) Resolve(context.Context, app.Admission, int32) (app.Credential, error) {
	return app.Credential{}, app.ErrUnauthorized
}

func newPoolServer(t *testing.T, source HTTPActionSource) *ContentServer {
	t.Helper()
	data := []byte{0x0a, 0x00}
	digest := sha256.Sum256(data)
	server, err := NewContentServer(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{ResourceProjectID: "42", ActorID: "17", InputBundleID: "bundle-1", CapabilityID: executiondomain.AgentApplicationCapability, SemanticRole: executiondomain.AgentExecutionRequestRole, ExpectedMediaType: executiondomain.AgentExecutionInputMediaType, ExpectedDigest: digest, ExpectedLength: int64(len(data))}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}),
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := NewRuntimeHTTPActionService(source, nopHTTPDeps{}, nopHTTPDeps{}, nopHTTPDeps{}, nil, http.DefaultTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server.WithRuntimeHTTPActions(actions)
}

func validHTTPInvocationBody() []byte {
	inputs := []byte(`{"path":{"item":"i"}}`)
	binding := strings.Repeat("d", 64)
	inv := app.Invocation{SchemaVersion: app.Schema, NodeID: "fetch", ThreadID: "graph", BindingDigest: binding, InputsB64: base64.StdEncoding.EncodeToString(inputs), InputsDigest: app.Digest(inputs)}
	inv.ActivationID = app.VisitID(inv.ThreadID, inv.NodeID, 0, app.BindingBytes(binding))
	return []byte(`{"schema_version":"` + inv.SchemaVersion + `","node_id":"fetch","thread_id":"graph","step":0,"attempt":0,"activation_id":"` + inv.ActivationID + `","binding_digest":"` + binding + `","inputs_b64":"` + inv.InputsB64 + `","inputs_digest":"` + inv.InputsDigest + `"}`)
}

func runtimeRequest(method, path string, body []byte) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	request.Header.Set(claimIDHeader, "claim-1")
	request.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	return request
}

func postAction(routes http.Handler, execution string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, runtimeRequest(http.MethodPost, "/executions/"+execution+"/generations/1/runtime-context/http-actions", validHTTPInvocationBody()))
	return response
}

// Pool boundaries (D5): 8 in flight per execution, the 9th 503 + Retry-After;
// 32 in flight per replica, the 33rd 503.
func TestHTTPActionPoolBoundaries(t *testing.T) {
	source := &blockingHTTPSource{entered: make(chan struct{}, 64), release: make(chan struct{})}
	routes := newPoolServer(t, source).Routes()
	var wg sync.WaitGroup
	start := func(execution string) {
		wg.Add(1)
		go func() { defer wg.Done(); postAction(routes, execution) }()
		<-source.entered
	}
	for range MaxHTTPActionsPerExecution {
		start("exec-a")
	}
	if response := postAction(routes, "exec-a"); response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("9th in one execution: %d", response.Code)
	}
	for _, execution := range []string{"exec-b", "exec-c", "exec-d"} {
		for range MaxHTTPActionsPerExecution {
			start(execution)
		}
	}
	if response := postAction(routes, "exec-e"); response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("33rd on the replica: %d", response.Code)
	}
	close(source.release)
	wg.Wait()
	// Slots are returned: a new action is admitted (and refused by authority, not capacity).
	source.release = make(chan struct{})
	close(source.release)
	go func() { <-source.entered }()
	if response := postAction(routes, "exec-a"); response.Code == http.StatusServiceUnavailable {
		t.Fatalf("slots leaked: %d", response.Code)
	}
}

// Budget: 16 concurrent long HTTP actions hold none of the shared content
// slots; token/version/input reads keep their latency (≤ 50 ms p95 added).
func TestHTTPActionPoolIsolationFromContentReads(t *testing.T) {
	source := &blockingHTTPSource{entered: make(chan struct{}, 64), release: make(chan struct{})}
	routes := newPoolServer(t, source).Routes()
	measure := func() time.Duration {
		durations := make([]time.Duration, 0, 200)
		for range 200 {
			started := time.Now()
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, runtimeRequest(http.MethodGet, "/executions/execution-1/generations/1/inputs/settings/versions/v1", nil))
			durations = append(durations, time.Since(started))
			if response.Code != http.StatusOK {
				t.Fatalf("content read refused while HTTP actions run: %d", response.Code)
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		return durations[190]
	}
	baseline := measure()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); postAction(routes, "exec-"+strconv.Itoa(i%4)) }()
		<-source.entered
	}
	loaded := measure()
	close(source.release)
	wg.Wait()
	t.Logf("content read p95: baseline %s, with 16 HTTP actions in flight %s", baseline, loaded)
	if loaded-baseline > 50*time.Millisecond {
		t.Fatalf("HTTP actions added %s to content reads", loaded-baseline)
	}
}

type failingHTTPSource struct{ err error }

func (s failingHTTPSource) ReadHTTPActionInput(context.Context, ContentClaim) (HTTPActionInput, error) {
	return HTTPActionInput{}, s.err
}

func TestHTTPActionRouteStatusMapping(t *testing.T) {
	routes := newPoolServer(t, failingHTTPSource{err: errors.New("no claim")}).Routes()
	for name, tc := range map[string]struct {
		body   []byte
		status int
		code   string
	}{
		"unknown field":       {[]byte(strings.Replace(string(validHTTPInvocationBody()), `"attempt":0`, `"attempt":0,"url":"https://x"`, 1)), http.StatusUnprocessableEntity, app.CodeInvalidInput},
		"missing attempt":     {[]byte(strings.Replace(string(validHTTPInvocationBody()), `"attempt":0,`, ``, 1)), http.StatusUnprocessableEntity, app.CodeInvalidInput},
		"over invocation cap": {append(validHTTPInvocationBody(), bytes.Repeat([]byte(" "), app.MaxInvocation)...), http.StatusRequestEntityTooLarge, app.CodeInputsTooBig},
		"claim refused":       {validHTTPInvocationBody(), http.StatusForbidden, app.CodePolicyDenied},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, runtimeRequest(http.MethodPost, "/executions/exec/generations/1/runtime-context/http-actions", tc.body))
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("%d %s", response.Code, response.Body)
			}
		})
	}
	unauthenticated := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/executions/exec/generations/1/runtime-context/http-actions", bytes.NewReader(validHTTPInvocationBody()))
	routes.ServeHTTP(unauthenticated, request)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("no mTLS claim answered %d", unauthenticated.Code)
	}
	for err, want := range map[error]int{
		&app.Failure{Code: app.CodeInputsTooBig}:  413,
		&app.Failure{Code: app.CodeNonDetermined}: 409,
		&app.Failure{Code: app.CodeInvalidInput}:  422,
		&app.Failure{Code: app.CodePolicyDenied}:  403,
		app.ErrDispatchInFlight:                   503,
		app.ErrUnavailable:                        503,
		errors.New("anything else"):               500,
	} {
		if status, _ := httpActionStatus(err); status != want {
			t.Errorf("%v → %d, want %d", err, status, want)
		}
	}
}

type flipResolver struct {
	mu      sync.Mutex
	answers map[string][][]net.IP
	calls   map[string]int
}

func (r *flipResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	answers := r.answers[host]
	if len(answers) == 0 {
		return nil, errors.New("no such host")
	}
	index := r.calls[host]
	if index >= len(answers) {
		index = len(answers) - 1
	}
	r.calls[host]++
	out := []net.IPAddr{}
	for _, ip := range answers[index] {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

func countingTLSServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func httpActionTransportFor(t *testing.T, allowlist string, resolver egress.Resolver, server *httptest.Server) app.Transport {
	t.Helper()
	guard, err := egress.NewHTTPActionGuard(allowlist, egress.WithResolver(resolver))
	if err != nil {
		t.Fatal(err)
	}
	transport := guard.Transport()
	if server != nil {
		transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	}
	return transport
}

// Egress (gate §3a): every forbidden and private class, including the
// IPv4-mapped and NAT64 forms, is refused at rule write (Validate) and at
// dial, with an empty operator allowlist.
func TestHTTPActionEgressRefusesForbiddenAndPrivateClasses(t *testing.T) {
	addresses := map[string]string{
		"rfc1918": "10.0.0.5", "loopback": "127.0.0.1", "link-local metadata": "169.254.169.254", "alibaba metadata": "100.100.100.200",
		"cgnat": "100.64.0.1", "this network": "0.0.0.1", "ietf": "192.0.0.1", "benchmarking": "198.18.0.1", "reserved": "240.0.0.1",
		"multicast": "224.0.0.1", "broadcast": "255.255.255.255", "ipv6 loopback": "::1", "ula": "fd00::1", "aws ipv6 metadata": "fd00:ec2::254",
		"ipv4-mapped private": "::ffff:10.0.0.5", "ipv4-mapped metadata": "::ffff:169.254.169.254", "nat64 private": "64:ff9b::a00:5",
		"nat64 metadata": "64:ff9b::a9fe:a9fe", "nat64 loopback": "64:ff9b::7f00:1", "6to4 private": "2002:a00:5::1", "link-local v6": "fe80::1",
	}
	resolver := &flipResolver{answers: map[string][][]net.IP{}, calls: map[string]int{}}
	for name, address := range addresses {
		resolver.answers[strings.ReplaceAll(name, " ", "-")+".example"] = [][]net.IP{{net.ParseIP(address)}}
	}
	guard, err := egress.NewHTTPActionGuard("", egress.WithResolver(resolver))
	if err != nil {
		t.Fatal(err)
	}
	transport := guard.RoundTripper()
	for name := range addresses {
		host := strings.ReplaceAll(name, " ", "-") + ".example"
		if err := guard.Validate(t.Context(), "https://"+host); !errors.Is(err, egress.ErrDestinationRefused) {
			t.Errorf("%s: rule write accepted (%v)", name, err)
		}
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host+"/", nil)
		if response, err := transport.RoundTrip(request); err == nil {
			_ = response.Body.Close()
			t.Errorf("%s: dialled", name)
		}
	}
	if err := guard.Validate(t.Context(), "http://public.example"); !errors.Is(err, egress.ErrDestinationRefused) {
		t.Fatal("http origin accepted")
	}
}

// The operator allowlist is the outer bound: a named private address (port
// pinned) becomes reachable, nothing else does.
func TestHTTPActionEgressAllowlistEntryIsTheOuterBound(t *testing.T) {
	server, hits := countingTLSServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	target, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(target.Host)
	resolver := &flipResolver{answers: map[string][][]net.IP{}, calls: map[string]int{}}
	transport := httpActionTransportFor(t, "127.0.0.1:"+port, resolver, server)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("allowlisted private target refused: %v", err)
	}
	_ = response.Body.Close()
	other := httpActionTransportFor(t, "127.0.0.1:1", resolver, server)
	request, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/", nil)
	if response, err := other.RoundTrip(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("an entry pinned to another port admitted the target")
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
	if _, err := egress.NewHTTPActionGuard("10.0.0.0/33"); err == nil {
		t.Fatal("malformed allowlist accepted (startup must fail closed)")
	}
}

// A 3xx is returned to the caller and never followed (counting server).
func TestHTTPActionEgressNeverFollowsRedirects(t *testing.T) {
	followed, followHits := countingTLSServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	redirect, redirectHits := countingTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, followed.URL+"/stolen", http.StatusFound)
	})
	transport := httpActionTransportFor(t, "127.0.0.1", &flipResolver{answers: map[string][][]net.IP{}, calls: map[string]int{}}, redirect)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, redirect.URL+"/", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound || redirectHits.Load() != 1 || followHits.Load() != 0 {
		t.Fatalf("status %d redirect=%d followed=%d", response.StatusCode, redirectHits.Load(), followHits.Load())
	}
}

// HTTPS_PROXY set: the target is dialled directly, the proxy sees nothing.
func TestHTTPActionEgressIgnoresProxyEnvironment(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	target, hits := countingTLSServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	transport := httpActionTransportFor(t, "127.0.0.1", &flipResolver{answers: map[string][][]net.IP{}, calls: map[string]int{}}, target)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target.URL+"/", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if hits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("target=%d proxy=%d", hits.Load(), proxyHits.Load())
	}
}

// DNS rebinding between rule write and dial: the write-time answer was
// public, the dial-time answer is loopback; the dial is refused.
func TestHTTPActionEgressRefusesDNSRebinding(t *testing.T) {
	resolver := &flipResolver{answers: map[string][][]net.IP{"rebind.example": {{net.ParseIP("93.184.216.34")}, {net.ParseIP("127.0.0.1")}}}, calls: map[string]int{}}
	guard, err := egress.NewHTTPActionGuard("", egress.WithResolver(resolver))
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Validate(t.Context(), "https://rebind.example"); err != nil {
		t.Fatalf("write-time validation: %v", err)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://rebind.example/", nil)
	if response, err := guard.RoundTripper().RoundTrip(request); err == nil || !errors.Is(err, egress.ErrDestinationRefused) {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("rebound dial not refused: %v", err)
	}
}
