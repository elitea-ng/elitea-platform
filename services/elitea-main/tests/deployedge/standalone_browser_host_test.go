// This file gates the standalone stack's browser host (STANDALONE_HOST).
//
// Browsers scope cookies by host and never by port. Two standalone stacks
// browsed on `localhost:<port>` from one browser profile therefore share one
// `elitea_session` cookie: each sign-in replaces the other stack's, and that
// stack's elitea-main refuses the browser with `session_unknown` while its own
// session row is live. Post-merge verification of 1ab920dde read that as a Main
// regression. STANDALONE_HOST=<name>.localhost gives a concurrent stack its own
// cookie jar; this gate keeps the two halves that make that work joined:
//
//  1. the compose file builds OIDC_REDIRECT_URI from STANDALONE_HOST, with
//     `localhost` as the default, so the callback that sets the cookie lands
//     on the host the browser uses and an unset variable changes nothing;
//  2. standalone-stack.sh accepts only `localhost` or one `<label>.localhost`,
//     so the redirect URI can never name a host off this machine.
//
// RUN IT WITH -count=1: the files it reads live in deploy/, outside this module.
package deployedge_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStandaloneOIDCRedirectFollowsTheBrowserHost(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "docker-compose.standalone-full.yml"))
	if err != nil {
		t.Fatalf("read the standalone compose file: %v", err)
	}
	var compose struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatalf("parse the standalone compose file: %v", err)
	}
	main, ok := compose.Services["elitea-main"]
	if !ok {
		t.Fatal("the standalone compose file has no elitea-main service")
	}
	const want = "http://${STANDALONE_HOST:-localhost}:${STANDALONE_PORT:-8084}/auth/oidc/callback"
	if got := main.Environment["OIDC_REDIRECT_URI"]; got != want {
		t.Fatalf("elitea-main OIDC_REDIRECT_URI = %q, want %q: the callback must be on the host "+
			"the browser uses, or a second stack on localhost signs this one's browser out", got, want)
	}
}

func TestStandaloneStackAcceptsOnlyLoopbackBrowserHosts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "scripts", "standalone-stack.sh"))
	if err != nil {
		t.Fatalf("read standalone-stack.sh: %v", err)
	}
	// The block runs exactly as the script runs it, so a weaker pattern fails
	// here and not in a browser.
	const first, last = `HOST="${STANDALONE_HOST:-localhost}"`, `export STANDALONE_HOST="$HOST"`
	text := string(script)
	start, end := strings.Index(text, first), strings.Index(text, last)
	if start < 0 || end < start {
		t.Fatalf("standalone-stack.sh no longer has the STANDALONE_HOST block (%q ... %q)", first, last)
	}
	block := text[start:end+len(last)] + "\nprintf '%s' \"$STANDALONE_HOST\"\n"

	for _, tc := range []struct {
		host, want string
		ok         bool
	}{
		{host: "", want: "localhost", ok: true},
		{host: "localhost", want: "localhost", ok: true},
		{host: "sessfix.localhost", want: "sessfix.localhost", ok: true},
		{host: "stack-2.localhost", want: "stack-2.localhost", ok: true},
		{host: "example.com"},
		{host: "localhost.example.com"},
		{host: "a.b.localhost"},
		{host: "-a.localhost"},
		{host: "A.localhost"},
		{host: "127.0.0.1"},
		{host: "x.localhost:1/evil"},
		{host: "x.localhost\n.evil"},
	} {
		command := exec.Command("bash", "-c", block) //nolint:gosec // the block is read from this repository
		command.Env = append(os.Environ(), "STANDALONE_HOST="+tc.host)
		out, err := command.Output()
		if tc.ok {
			if err != nil || string(out) != tc.want {
				t.Errorf("STANDALONE_HOST=%q: got %q, err %v; want it accepted as %q", tc.host, out, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("STANDALONE_HOST=%q was accepted (%q); only localhost and <label>.localhost may be", tc.host, out)
		}
	}
}
