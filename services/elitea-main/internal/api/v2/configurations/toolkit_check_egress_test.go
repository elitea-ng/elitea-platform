package configurations_test

// SEC-13: the toolkit "test connection" probe dials through the egress guard.
// The host-name allowlist stays the outer bound; the dialled address is
// classified at dial time, a private address passes only when an entry of the
// same list names its range, no proxy is used, a redirect is never followed and
// the response is bounded. Every case resolves through a fake resolver, so no
// case depends on real DNS.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
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

// hostRefusal is the answer for host when the allowlist does not name it. It
// is the ONE answer every egress refusal of that host gets, whatever rule
// refused it: the tests compare outcomes to it whole.
func hostRefusal(t *testing.T, host string) handler.ToolkitCheckOutcome {
	t.Helper()
	outcome := jiraProbe(handler.NewToolkitConnectionChecker("only.example"), "https://"+host)
	if outcome.Reason != handler.ToolkitCheckReasonUnreachable || outcome.Message == "" {
		t.Fatalf("baseline refusal = %+v", outcome)
	}
	if !strings.Contains(outcome.Message, host) ||
		!strings.Contains(outcome.Message, handler.ToolkitCheckAllowlistEnv) {
		t.Fatalf("baseline refusal %q must name the host and %s", outcome.Message, handler.ToolkitCheckAllowlistEnv)
	}
	return outcome
}

// egressRefusalLog is one "toolkit_check: egress refused the destination"
// record: the operator half of a refusal, which the tenant never sees.
type egressRefusalLog struct {
	Reason    string `json:"reason"`
	Host      string `json:"host"`
	Addresses string `json:"addresses"`
	Remedy    string `json:"remedy"`
}

// captureEgressRefusalLogs routes slog to a buffer for the rest of the test
// and returns a function that reads the refusal records written so far. The
// test must not run in parallel: slog's default logger is global.
func captureEgressRefusalLogs(t *testing.T) func() []egressRefusalLog {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []egressRefusalLog {
		var records []egressRefusalLog
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var record struct {
				Msg string `json:"msg"`
				egressRefusalLog
			}
			if json.Unmarshal([]byte(line), &record) == nil && record.Msg == "toolkit_check: egress refused the destination" {
				records = append(records, record.egressRefusalLog)
			}
		}
		buf.Reset()
		return records
	}
}

// lastRefusalLog returns the one refusal record of the last probe.
func lastRefusalLog(t *testing.T, read func() []egressRefusalLog) egressRefusalLog {
	t.Helper()
	records := read()
	if len(records) != 1 {
		t.Fatalf("refusal log records = %+v, want exactly one", records)
	}
	return records[0]
}

// The tenant gets ONE answer for every egress refusal of a host: off the
// list, unresolvable, resolving to an allowlistable private address, or to an
// address class that is never dialled. Only the operator log tells them
// apart, so Test connection is no DNS or SSRF oracle for a credential author.
func TestToolkitCheckEgressRefusalIsOneAnswer(t *testing.T) {
	read := captureEgressRefusalLogs(t)
	want := hostRefusal(t, "provider.example")
	if got := lastRefusalLog(t, read); got.Reason != "host_not_allowlisted" ||
		!strings.Contains(got.Remedy, "add the host to "+handler.ToolkitCheckAllowlistEnv) {
		t.Fatalf("host-off-the-list log = %+v", got)
	}
	for _, leak := range []string{"10.0.0.5", "169.254", "private", "metadata", "CIDR", "resolve"} {
		if strings.Contains(want.Message, leak) {
			t.Fatalf("the tenant answer %q says %q", want.Message, leak)
		}
	}

	for _, tc := range []struct {
		name, allowlist string
		resolver        toolkitCheckResolver
		reason          string
		addresses       string
		remedy          string
	}{
		{"private address, allowlistable", "provider.example",
			toolkitCheckResolver{"provider.example": "10.0.0.5"},
			"address_refused", "10.0.0.5 (private)", "add the address or its range (CIDR)"},
		{"metadata, never reachable", "provider.example, 0.0.0.0/0",
			toolkitCheckResolver{"provider.example": "169.254.169.254"},
			"address_refused", "169.254.169.254 (forbidden)", "no allowlist entry permits it"},
		{"unresolvable", "provider.example",
			toolkitCheckResolver{},
			"destination_refused", "", "did not resolve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := handler.NewToolkitConnectionChecker(tc.allowlist, egress.WithResolver(tc.resolver))
			if got := jiraProbe(checker, "https://provider.example"); got != want {
				t.Fatalf("outcome = %+v, want the same answer as a host off the list %+v", got, want)
			}
			got := lastRefusalLog(t, read)
			if got.Reason != tc.reason || got.Host != "provider.example" || got.Addresses != tc.addresses ||
				!strings.Contains(got.Remedy, tc.remedy) {
				t.Fatalf("log = %+v, want reason %q, addresses %q and a remedy with %q", got, tc.reason, tc.addresses, tc.remedy)
			}
		})
	}
}

// An IPv6 host is named as an allowlist entry or a URL writes it, in
// brackets: url.URL.Hostname drops them, and egressSubject puts them back.
func TestToolkitCheckRefusalBracketsAnIPv6Host(t *testing.T) {
	outcome := jiraProbe(handler.NewToolkitConnectionChecker("only.example"), "https://[fd12::7]:8443")
	if !strings.HasPrefix(outcome.Message, "The host [fd12::7] is refused") {
		t.Fatalf("message = %q, want the IPv6 host in brackets", outcome.Message)
	}
}

// Every blocked class, as the address an allowlisted NAME resolves to, is
// refused before any connection: with no private entry, and for the classes
// that stay forbidden, even with private ranges named. The tenant answer is
// the one a host off the list gets. The operator log says which: a private
// address that an entry naming its range would permit, or a class that no
// entry permits (never suggesting the allowlist for it).
func TestToolkitCheckRefusesEveryBlockedAddressClass(t *testing.T) {
	read := captureEgressRefusalLogs(t)
	refusal := hostRefusal(t, "provider.example")
	read()
	const (
		allowlistable = "add the address or its range (CIDR)"
		never         = "no allowlist entry permits it"
	)
	cases := []struct {
		ip      string
		entries string // beyond the host name
		remedy  string
	}{
		{"127.0.0.1", "", allowlistable},
		{"::1", "", allowlistable},
		{"::ffff:127.0.0.1", "", allowlistable},
		{"64:ff9b::7f00:1", "", allowlistable}, // NAT64 of loopback
		{"10.0.0.5", "", allowlistable},
		{"172.16.0.5", "", allowlistable},
		{"192.168.1.5", "", allowlistable},
		{"fc00::5", "", allowlistable},
		{"100.64.1.5", "", allowlistable},      // CGNAT
		{"198.18.0.5", "", allowlistable},      // benchmarking
		{"::ffff:10.0.0.5", "", allowlistable}, // IPv4-mapped RFC 1918
		{"2002:a00:5::1", "", allowlistable},   // 6to4 of RFC 1918
		{"0.0.0.0", "0.0.0.0/0", never},
		{"169.254.169.254", "0.0.0.0/0", never},                   // cloud metadata (link-local)
		{"::ffff:169.254.169.254", "0.0.0.0/0", never},            // IPv4-mapped metadata
		{"64:ff9b::a9fe:a9fe", "0.0.0.0/0", never},                // NAT64 metadata
		{"2002:a9fe:a9fe::1", "0.0.0.0/0", never},                 // 6to4 metadata
		{"100.100.100.200", "100.64.0.0/10", never},               // metadata inside CGNAT
		{"fd00:ec2::254", "fc00::/7", never},                      // metadata inside ULA
		{"168.63.129.16", "0.0.0.0/0", never},                     // platform endpoint
		{"224.0.0.1", "0.0.0.0/0", never},                         // multicast
		{"240.0.0.1", "0.0.0.0/0", never},                         // reserved
		{"fe80::1", "::/0", never},                                // IPv6 link-local
		{"127.0.0.1", "100.64.0.0/10, 10.0.0.0/8", allowlistable}, // other private ranges named, not loopback
	}
	for _, tc := range cases {
		t.Run(tc.ip+" with "+tc.entries, func(t *testing.T) {
			checker := handler.NewToolkitConnectionChecker("provider.example, "+tc.entries,
				egress.WithResolver(toolkitCheckResolver{"provider.example": tc.ip}))
			started := time.Now()
			outcome := jiraProbe(checker, "https://provider.example")
			if outcome != refusal {
				t.Fatalf("outcome = %+v, want the one refusal %+v", outcome, refusal)
			}
			if time.Since(started) > time.Second {
				t.Fatalf("refusal took %s; it must be decided before any connect", time.Since(started))
			}
			got := lastRefusalLog(t, read)
			if got.Reason != "address_refused" || !strings.Contains(got.Remedy, tc.remedy) {
				t.Fatalf("log = %+v, want address_refused with a remedy that says %q", got, tc.remedy)
			}
			if tc.remedy == never && strings.Contains(got.Remedy, "CIDR") {
				t.Fatalf("log remedy %q suggests the allowlist for a class no entry permits", got.Remedy)
			}
		})
	}
}

// A self-hosted provider on a private address passes when the same list names
// its range, and is refused otherwise, with the provider never reached.
func TestToolkitCheckPrivateProviderNeedsItsRangeNamed(t *testing.T) {
	refusal := hostRefusal(t, "gitlab.corp.example")
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
	refusal := hostRefusal(t, "metadata.example")
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
