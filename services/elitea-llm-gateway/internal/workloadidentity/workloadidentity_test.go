package workloadidentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

func TestParseAuthMode(t *testing.T) {
	for raw, want := range map[string]string{
		"": AuthModeKey, "key": AuthModeKey, " key ": AuthModeKey, "workload_identity": AuthModeWorkloadIdentity,
	} {
		if got, ok := ParseAuthMode(raw); !ok || got != want {
			t.Errorf("ParseAuthMode(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	// An unknown value is refused, never read as a default.
	for _, raw := range []string{"ambient", "WORKLOAD_IDENTITY", "workload-identity", "none"} {
		if _, ok := ParseAuthMode(raw); ok {
			t.Errorf("ParseAuthMode(%q) was accepted", raw)
		}
	}
}

func TestAudience(t *testing.T) {
	cases := map[string]string{
		"https://dial.example.com/openai/v1": "https://dial.example.com",
		"https://Dial.Example.com.:443/":     "https://dial.example.com",
		"http://dial.internal:8080/x":        "http://dial.internal:8080",
	}
	for apiBase, want := range cases {
		if got, err := Audience(apiBase, ""); err != nil || got != want {
			t.Errorf("Audience(%q) = %q, %v; want %q", apiBase, got, err, want)
		}
	}
	if got, _ := Audience("https://dial.example.com", " client-id "); got != "client-id" {
		t.Errorf("token_audience override = %q, want client-id", got)
	}
	for _, bad := range []string{"", "dial.example.com", "ftp://dial.example.com", "https://user:pw@dial.example.com"} {
		if _, err := Audience(bad, ""); err == nil {
			t.Errorf("Audience(%q) was accepted", bad)
		}
	}
}

type countingSource struct{ calls atomic.Int32 }

func (s *countingSource) Token() (*oauth2.Token, error) {
	s.calls.Add(1)
	return &oauth2.Token{AccessToken: "tok"}, nil
}

// TestADCCachesPerAudienceAndRetriesAFailedBuild: one source per audience, and
// a failed build is not cached, so a metadata blip does not refuse every later
// request for that audience.
func TestADCCachesPerAudienceAndRetriesAFailedBuild(t *testing.T) {
	var builds atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	a := NewADC()
	a.newIDSource = func(context.Context, string) (oauth2.TokenSource, error) {
		builds.Add(1)
		if fail.Load() {
			return nil, errors.New("metadata: unavailable")
		}
		return &countingSource{}, nil
	}
	ctx := context.Background()
	if _, err := a.IDToken(ctx, "https://dial.example.com"); err == nil {
		t.Fatal("a failed build returned a token")
	}
	fail.Store(false)
	for range 3 {
		if tok, err := a.IDToken(ctx, "https://dial.example.com"); err != nil || tok != "tok" {
			t.Fatalf("IDToken = %q, %v", tok, err)
		}
	}
	if _, err := a.IDToken(ctx, "https://other.example.com"); err != nil {
		t.Fatalf("second audience: %v", err)
	}
	if got := builds.Load(); got != 3 {
		t.Errorf("builds = %d, want 3 (one failure, then one per audience)", got)
	}
	if _, err := a.IDToken(ctx, " "); err == nil {
		t.Error("an empty audience was accepted")
	}
}

func TestADCAccessTokenHonoursTheContext(t *testing.T) {
	a := NewADC()
	block := make(chan struct{})
	defer close(block)
	a.newAccessSource = func(context.Context) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(nil), nil
	}
	a.access = tokenSourceFunc(func() (*oauth2.Token, error) { <-block; return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.AccessToken(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's cancellation", err)
	}
}

// TestADCAccessTokenBuildHonoursTheContext: the first AccessToken builds its
// source through FindDefaultCredentials, which can block on the metadata
// server; a cancelled request must not wait for it.
func TestADCAccessTokenBuildHonoursTheContext(t *testing.T) {
	a := NewADC()
	block := make(chan struct{})
	defer close(block)
	a.newAccessSource = func(context.Context) (oauth2.TokenSource, error) {
		<-block
		return nil, errors.New("unreachable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.AccessToken(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's cancellation", err)
	}
}

type tokenSourceFunc func() (*oauth2.Token, error)

func (f tokenSourceFunc) Token() (*oauth2.Token, error) { return f() }
