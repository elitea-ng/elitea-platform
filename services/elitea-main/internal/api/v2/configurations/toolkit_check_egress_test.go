package configurations_test

// SEC-13: the toolkit "test connection" probe dials through the egress guard.
// The host-name allowlist stays the outer bound; the dialled address is
// classified at dial time, a private address passes only when an entry of the
// same list names its range, no proxy is used, a redirect is never followed and
// the response is bounded. Every case resolves through a fake resolver, so no
// case depends on real DNS.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

type toolkitCheckResolver map[string]string

func (r toolkitCheckResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if ip, ok := r[host]; ok {
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// countingProvider answers 200 and counts the requests it saw.
func countingProvider(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func serverPort(t *testing.T, server *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func jiraProbe(checker handler.ToolkitConnectionChecker, baseURL string) handler.ToolkitCheckOutcome {
	return checker.CheckToolkit(context.Background(), "jira", map[string]any{
		"base_url": baseURL, "username": "someone@example.com", "api_key": "a-token",
	})
}

// egressRefusal is the answer for a host the configuration does not permit.
func egressRefusal(t *testing.T) handler.ToolkitCheckOutcome {
	t.Helper()
	outcome := jiraProbe(handler.NewToolkitConnectionChecker("only.example"), "https://elsewhere.example")
	if outcome.Reason != handler.ToolkitCheckReasonUnreachable || outcome.Message == "" {
		t.Fatalf("baseline refusal = %+v", outcome)
	}
	return outcome
}

// Every blocked class, as the address an allowlisted NAME resolves to, is
// refused before any connection: with no private entry, and for the classes
// that stay forbidden, even with private ranges named.
func TestToolkitCheckRefusesEveryBlockedAddressClass(t *testing.T) {
	refusal := egressRefusal(t)
	cases := []struct {
		ip      string
		entries string // beyond the host name
	}{
		{"127.0.0.1", ""},
		{"::1", ""},
		{"::ffff:127.0.0.1", ""},
		{"64:ff9b::7f00:1", ""}, // NAT64 of loopback
		{"10.0.0.5", ""},
		{"172.16.0.5", ""},
		{"192.168.1.5", ""},
		{"fc00::5", ""},
		{"100.64.1.5", ""},      // CGNAT
		{"198.18.0.5", ""},      // benchmarking
		{"::ffff:10.0.0.5", ""}, // IPv4-mapped RFC 1918
		{"2002:a00:5::1", ""},   // 6to4 of RFC 1918
		{"0.0.0.0", "0.0.0.0/0"},
		{"169.254.169.254", "0.0.0.0/0"},           // cloud metadata (link-local)
		{"::ffff:169.254.169.254", "0.0.0.0/0"},    // IPv4-mapped metadata
		{"64:ff9b::a9fe:a9fe", "0.0.0.0/0"},        // NAT64 metadata
		{"2002:a9fe:a9fe::1", "0.0.0.0/0"},         // 6to4 metadata
		{"100.100.100.200", "100.64.0.0/10"},       // metadata inside CGNAT
		{"fd00:ec2::254", "fc00::/7"},              // metadata inside ULA
		{"168.63.129.16", "0.0.0.0/0"},             // platform endpoint
		{"224.0.0.1", "0.0.0.0/0"},                 // multicast
		{"240.0.0.1", "0.0.0.0/0"},                 // reserved
		{"fe80::1", "::/0"},                        // IPv6 link-local
		{"127.0.0.1", "100.64.0.0/10, 10.0.0.0/8"}, // other private ranges named, not loopback
	}
	for _, tc := range cases {
		t.Run(tc.ip+" with "+tc.entries, func(t *testing.T) {
			checker := handler.NewToolkitConnectionChecker("provider.example, "+tc.entries,
				egress.WithResolver(toolkitCheckResolver{"provider.example": tc.ip}))
			started := time.Now()
			outcome := jiraProbe(checker, "https://provider.example")
			if outcome != refusal {
				t.Fatalf("outcome = %+v, want the configuration refusal %+v", outcome, refusal)
			}
			if time.Since(started) > time.Second {
				t.Fatalf("refusal took %s; it must be decided before any connect", time.Since(started))
			}
		})
	}
}

// A self-hosted provider on a private address passes when the same list names
// its range, and is refused otherwise, with the provider never reached.
func TestToolkitCheckPrivateProviderNeedsItsRangeNamed(t *testing.T) {
	refusal := egressRefusal(t)
	server, hits := countingProvider(t)
	port := serverPort(t, server)
	resolver := egress.WithResolver(toolkitCheckResolver{"gitlab.corp.example": "127.0.0.1"})
	for _, tc := range []struct {
		name, allowlist, base string
		allowed               bool
	}{
		{"name and its range", "gitlab.corp.example, 127.0.0.0/8", "http://gitlab.corp.example:" + port, true},
		{"name and its address", "gitlab.corp.example, 127.0.0.1", "http://gitlab.corp.example:" + port, true},
		{"name and its address on this port", "gitlab.corp.example, 127.0.0.1:" + port, "http://gitlab.corp.example:" + port, true},
		{"IP literal inside a named block", "127.0.0.0/8", "http://127.0.0.1:" + port, true},
		{"name only", "gitlab.corp.example", "http://gitlab.corp.example:" + port, false},
		{"name and another private range", "gitlab.corp.example, 10.0.0.0/8", "http://gitlab.corp.example:" + port, false},
		{"name and its address on another port", "gitlab.corp.example, 127.0.0.1:1", "http://gitlab.corp.example:" + port, false},
		{"range without the name", "127.0.0.0/8", "http://gitlab.corp.example:" + port, false},
		{"bare star is not accepted", "*", "http://gitlab.corp.example:" + port, false},
		{"bare star beside a range", "*, 127.0.0.0/8", "http://gitlab.corp.example:" + port, false},
		{"default list", "", "http://gitlab.corp.example:" + port, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			outcome := jiraProbe(handler.NewToolkitConnectionChecker(tc.allowlist, resolver), tc.base)
			if tc.allowed {
				if outcome.Reason != handler.ToolkitCheckReasonOK || hits.Load() != 1 {
					t.Fatalf("outcome = %+v, hits = %d; want ok and one request", outcome, hits.Load())
				}
				return
			}
			if outcome != refusal || hits.Load() != 0 {
				t.Fatalf("outcome = %+v, hits = %d; want the configuration refusal and no request", outcome, hits.Load())
			}
		})
	}
}

// Proxy variables are ignored: the probe dials the checked provider directly,
// and a target the guard refuses is not sent to a proxy instead.
func TestToolkitCheckIgnoresProxyEnvironment(t *testing.T) {
	refusal := egressRefusal(t)
	proxy, proxyHits := countingProvider(t)
	provider, providerHits := countingProvider(t)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	resolver := egress.WithResolver(toolkitCheckResolver{"provider.example": "127.0.0.1", "metadata.example": "169.254.169.254"})
	checker := handler.NewToolkitConnectionChecker("provider.example, metadata.example, 127.0.0.1", resolver)

	if outcome := jiraProbe(checker, "http://provider.example:"+serverPort(t, provider)); outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("direct probe = %+v", outcome)
	}
	if outcome := jiraProbe(checker, "http://metadata.example"); outcome != refusal {
		t.Fatalf("refused target = %+v, want the configuration refusal", outcome)
	}
	if providerHits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("provider hits = %d, proxy hits = %d; want 1 and 0", providerHits.Load(), proxyHits.Load())
	}
}

// A redirect is never followed: the 302 is the answer, and the Location host
// never sees the request or the credential.
func TestToolkitCheckDoesNotFollowRedirects(t *testing.T) {
	target, targetHits := countingProvider(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rest/api/2/myself", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	checker := handler.NewToolkitConnectionChecker("127.0.0.1")

	outcome := jiraProbe(checker, redirector.URL)
	if outcome.Reason != handler.ToolkitCheckReasonUnreachable || !strings.Contains(outcome.Message, "302") {
		t.Fatalf("outcome = %+v, want the 302 reported as unreachable", outcome)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("the redirect target was asked %d times", targetHits.Load())
	}
}

// The response is bounded: an oversized header block is refused, and a large
// body is never read, so the probe answers from the status line.
func TestToolkitCheckBoundsTheResponse(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Padding", strings.Repeat("a", egress.MaxResponseHeaderBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(huge.Close)
	if outcome := jiraProbe(handler.NewToolkitConnectionChecker("127.0.0.1"), huge.URL); outcome.Reason != handler.ToolkitCheckReasonUnreachable {
		t.Fatalf("oversized headers = %+v, want unreachable", outcome)
	}

	var written atomic.Int64
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := []byte(strings.Repeat("b", 64<<10))
		for range 1024 { // up to 64 MiB
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil || r.Context().Err() != nil {
				return
			}
		}
	}))
	t.Cleanup(stream.Close)
	started := time.Now()
	if outcome := jiraProbe(handler.NewToolkitConnectionChecker("127.0.0.1"), stream.URL); outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("large body = %+v, want ok from the status line", outcome)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the probe took %s; it must not read the body", elapsed)
	}
	stream.CloseClientConnections()
	if got := written.Load(); got >= 64<<20 {
		t.Fatalf("the provider wrote the whole %d-byte body; the probe read it", got)
	}
}
