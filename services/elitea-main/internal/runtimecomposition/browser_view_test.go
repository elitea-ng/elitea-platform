package runtimecomposition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
)

const browserViewFixture = `{"main_workload_identity":"dns:elitea-main",` +
	`"certificate_chain_path":"/run/elitea-runtime/browser-view-client.crt",` +
	`"private_key_path":"/run/elitea-runtime/browser-view-client.key",` +
	`"server_ca_path":"/run/elitea-runtime/runtime-ca.crt",` +
	`"supervisors":[{"audience":"dns:elitea-sandbox-browser","https_origin":"https://elitea-sandbox-browser:9471"}]}`

func TestBrowserViewConfiguration(t *testing.T) {
	env := map[string]string{}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	audiences := []string{"dns:elitea-sandbox-workspace", "dns:elitea-sandbox-browser"}
	if cfg, err := browserViewConfigFromEnv(lookup, true, true, audiences); err != nil || cfg != nil {
		t.Fatalf("unset: cfg=%v err=%v", cfg, err)
	}
	env[BrowserViewConfigEnv] = browserViewFixture
	cfg, err := browserViewConfigFromEnv(lookup, true, true, audiences)
	if err != nil || cfg == nil || cfg.MainWorkloadIdentity != "dns:elitea-main" || cfg.Supervisors[0].Audience != "dns:elitea-sandbox-browser" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := browserViewConfigFromEnv(lookup, true, false, audiences); err == nil {
		t.Fatal("accepted without agent dispatch")
	}
	if _, err := browserViewConfigFromEnv(lookup, true, true, []string{"dns:elitea-sandbox-workspace"}); err == nil {
		t.Fatal("accepted a supervisor Main does not sign for")
	}
	for name, raw := range map[string]string{
		"not JSON":         "{",
		"an unknown field": strings.Replace(browserViewFixture, `{"main_workload_identity"`, `{"extra":1,"main_workload_identity"`, 1),
		"too fast":         strings.Replace(browserViewFixture, `{"main_workload_identity"`, `{"max_fps":11,"main_workload_identity"`, 1),
		"a plain origin":   strings.Replace(browserViewFixture, "https://elitea-sandbox-browser", "http://elitea-sandbox-browser", 1),
		"a relative path":  strings.Replace(browserViewFixture, "/run/elitea-runtime/runtime-ca.crt", "runtime-ca.crt", 1),
		"no supervisors":   strings.Replace(browserViewFixture, `[{"audience":"dns:elitea-sandbox-browser","https_origin":"https://elitea-sandbox-browser:9471"}]`, `[]`, 1),
		"no origin":        strings.Replace(browserViewFixture, `,"https_origin":"https://elitea-sandbox-browser:9471"`, ``, 1),
		"an origin path":   strings.Replace(browserViewFixture, ":9471", ":9471/x", 1),
	} {
		t.Run(name, func(t *testing.T) {
			env[BrowserViewConfigEnv] = raw
			if _, err := browserViewConfigFromEnv(lookup, true, true, audiences); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestBrowserViewNotConfiguredComposesNothing(t *testing.T) {
	composition, err := configureBrowserView(Config{}, Dependencies{}, nil)
	if err != nil || composition != nil {
		t.Fatalf("composition=%v err=%v", composition, err)
	}
	if closer := browserViewCloser(nil); closer != nil {
		t.Fatal("a nil composition closes something (typed nil)")
	}
	if handler, err := newBrowserViewHandler(nil, nil, nil, nil); err != nil || handler != nil {
		t.Fatalf("handler=%v err=%v", handler, err)
	}
}

func TestBrowserSessionRecorderMapsARefusal(t *testing.T) {
	// The mapping only: the repository's own refusal becomes the control
	// server's, which answers AUTHORIZATION_FAILED rather than a retry.
	if !errors.Is(mapBrowserSessionError(repos.ErrBrowserSessionRefused), control.ErrBrowserSessionReportRefused) {
		t.Fatal("a refused report is not the control server's refusal")
	}
	other := errors.New("database down")
	if !errors.Is(mapBrowserSessionError(other), other) || errors.Is(mapBrowserSessionError(other), control.ErrBrowserSessionReportRefused) {
		t.Fatal("an outage reads as a refusal")
	}
	if mapBrowserSessionError(nil) != nil {
		t.Fatal("success reads as an error")
	}
}

type prunerStub struct{ calls, limit int }

func (p *prunerStub) Prune(_ context.Context, limit int) (int64, error) {
	p.calls++
	p.limit = limit
	return 0, nil
}

type replayStub struct{ calls int }

func (r *replayStub) PruneExpiredReplayProgress(context.Context) (int64, error) {
	r.calls++
	return 0, nil
}

// The session table is pruned on the replay retention pass, a bounded batch
// each time, and the replay pass still runs.
func TestBrowserSessionsArePrunedOnTheRetentionPass(t *testing.T) {
	replay := &replayStub{}
	janitor, err := newExecutionReplayRetentionJanitor(replay, time.Minute, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sessions := &prunerStub{}
	if err := attachBrowserSessionMaintenance(janitor, sessions); err != nil {
		t.Fatal(err)
	}
	if err := janitor.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if replay.calls != 1 || sessions.calls != 1 || sessions.limit != browserSessionPruneBatch {
		t.Fatalf("replay=%d sessions=%d limit=%d", replay.calls, sessions.calls, sessions.limit)
	}
	if attachBrowserSessionMaintenance(nil, sessions) == nil {
		t.Fatal("no janitor accepted")
	}
}

// The chart places Main's browser view client identity through MaterialFiles;
// a path the inventory leaves out is never mounted and the relay cannot start.
func TestBrowserViewMaterialIsListed(t *testing.T) {
	values := oneDirectoryEnvironment()
	values["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	values["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "ELITEA_RT_V1_AGENT"
	values["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] = "dns:elitea-sandbox-browser"
	values[BrowserViewConfigEnv] = browserViewFixture
	config, err := ConfigFromEnv(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]securefile.Permissions{}
	for _, file := range files {
		listed[file.Path] = file.Permissions
	}
	for path, want := range map[string]securefile.Permissions{
		"/run/elitea-runtime/browser-view-client.crt": securefile.PublicMaterial,
		"/run/elitea-runtime/browser-view-client.key": securefile.PrivateMaterial,
		"/run/elitea-runtime/runtime-ca.crt":          securefile.PublicMaterial,
	} {
		if got, found := listed[path]; !found || got != want {
			t.Fatalf("%s: listed=%v profile=%d, want %d", path, found, got, want)
		}
	}
	if directory, err := config.MaterialDirectory(); err != nil || directory != "/run/elitea-runtime" {
		t.Fatal(directory, err)
	}
}
