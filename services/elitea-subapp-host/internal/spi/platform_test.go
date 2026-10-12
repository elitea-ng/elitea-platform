package spi_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// The verified-peer rules, mirrored from elitea-main's workload
// authorisation: only the chain the TLS stack VERIFIED counts.

func certificate(cn string, dns ...string) *x509.Certificate {
	return &x509.Certificate{Raw: []byte("raw-" + cn), Subject: pkix.Name{CommonName: cn}, DNSNames: dns}
}

func peerContext(info credentials.AuthInfo) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: info})
}

func tlsInfo(chains [][]*x509.Certificate, presented ...*x509.Certificate) credentials.TLSInfo {
	return credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: chains, PeerCertificates: presented}}
}

func TestOnlyAVerifiedClientCertificateOnTheAllowlistIsAnIdentity(t *testing.T) {
	allowed := []string{"elitea-main"}
	main := certificate("elitea-main")
	bySAN := certificate("other", "elitea-main")
	other := certificate("elitea-facade")

	got, err := spi.VerifiedPeerIdentity(peerContext(tlsInfo([][]*x509.Certificate{{main}}, main)), allowed)
	if err != nil || got != "elitea-main" {
		t.Fatalf("%q %v", got, err)
	}
	// A pointer to the info, as some transports hand it over.
	info := tlsInfo([][]*x509.Certificate{{bySAN}}, bySAN)
	if got, err := spi.VerifiedPeerIdentity(peerContext(&info), allowed); err != nil || got != "elitea-main" {
		t.Fatalf("a DNS SAN: %q %v", got, err)
	}

	denied := map[string]context.Context{
		"no peer at all":      context.Background(),
		"no auth info":        peerContext(nil),
		"not TLS":             peerContext(insecureAuth{}),
		"no verified chain":   peerContext(tlsInfo(nil, main)),
		"an empty chain":      peerContext(tlsInfo([][]*x509.Certificate{{}}, main)),
		"no presented cert":   peerContext(tlsInfo([][]*x509.Certificate{{main}})),
		"leaf is not peer 0":  peerContext(tlsInfo([][]*x509.Certificate{{main}}, other)),
		"a nil pointer":       peerContext((*credentials.TLSInfo)(nil)),
		"nil verified leaf":   peerContext(tlsInfo([][]*x509.Certificate{{nil}}, main)),
		"presented, unproven": peerContext(tlsInfo(nil, main, main)),
	}
	for name, ctx := range denied {
		if _, err := spi.VerifiedPeerIdentity(ctx, allowed); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Verified but not allowed, or nobody allowed: PermissionDenied.
	for name, list := range map[string][]string{"another service": {"elitea-main"}, "an empty list": nil} {
		cert := other
		if name == "an empty list" {
			cert = main
		}
		if _, err := spi.VerifiedPeerIdentity(peerContext(tlsInfo([][]*x509.Certificate{{cert}}, cert)), list); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v", name, err)
		}
	}
}

type insecureAuth struct{}

func (insecureAuth) AuthType() string { return "insecure" }

func TestPlatformClientsAreParsedFromACommaList(t *testing.T) {
	got := spi.PlatformClients(" elitea-main , ,elitea-main.elitea.svc,")
	if len(got) != 2 || got[0] != "elitea-main" || got[1] != "elitea-main.elitea.svc" {
		t.Fatalf("%q", got)
	}
	s, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(map[string]string{
		"ELITEA_DEEPWIKI_PLATFORM_CLIENTS":   "elitea-main",
		"ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR": ":9555",
	}))
	if err != nil || len(s.PlatformClients) != 1 || s.PlatformGRPCAddr != ":9555" {
		t.Fatalf("%+v %v", s, err)
	}
	s, _ = spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(nil))
	if len(s.PlatformClients) != 0 || s.PlatformGRPCAddr != "" {
		t.Fatalf("the service must be off by default: %+v", s)
	}
}

// The platform operations are not an HTTP route any more: the SPI's
// listener has nothing at the old path, for any caller.
func TestTheHTTPPlatformRouteIsGone(t *testing.T) {
	server, err := spi.NewServer(spi.Settings{}, echo.App(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(method, "/internal/v1/projects/delete", strings.NewReader(`{"project_id": 17}`)))
		if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", method, recorder.Code)
		}
	}
}

func TestStopMatchingStopsOnlyTheMatchingRunsAndWaitsForThem(t *testing.T) {
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(t.Context())
	t.Cleanup(manager.Stop)

	var stubborn atomic.Bool
	stubborn.Store(true)
	run := func(label string, honour bool) *spi.Invocation {
		started := make(chan struct{})
		invocation, err := manager.Submit(t.Context(), "T", "tool", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
			tc.SetLabel("project", label)
			close(started)
			for {
				if honour {
					if err := tc.Checkpoint(); err != nil {
						return nil, err
					}
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(5 * time.Millisecond):
				}
				if !stubborn.Load() && !honour {
					return map[string]any{"status": "Completed"}, nil
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		<-started
		return invocation
	}
	a := run("1", true)
	b := run("2", true)
	c := run("1", false) // ignores the stop
	_ = a
	_ = b

	match := func(project string) func(spi.RunningInvocation) bool {
		return func(r spi.RunningInvocation) bool { return r.Labels["project"] == project }
	}
	// Project 2's run honours the stop and leaves; nothing else is touched.
	if left := manager.StopMatching(t.Context(), match("2"), 2*time.Second); left != 0 {
		t.Fatalf("%d left", left)
	}
	if manager.InFlight() != 2 {
		t.Fatalf("%d running, want project 1's two", manager.InFlight())
	}
	// Project 1 has one run that ignores the stop: the wait ends at its bound
	// and says one is left.
	started := time.Now()
	if left := manager.StopMatching(t.Context(), match("1"), 150*time.Millisecond); left != 1 {
		t.Fatalf("%d left, want the stubborn one", left)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("the wait took %v", elapsed)
	}
	// Nothing matches: nothing to wait for.
	if left := manager.StopMatching(t.Context(), match("none"), time.Hour); left != 0 {
		t.Fatalf("%d", left)
	}
	stubborn.Store(false)
	_ = c
}
