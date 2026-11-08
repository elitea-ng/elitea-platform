package browserruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egresstoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakeSources struct {
	browser      platformconfig.BrowserSettings
	workerEgress platformconfig.WorkerEgressSettings
	setting      ProjectSetting
	projectHosts []string
	used         int64
	since        time.Time
	origin       string
	granted      bool
	err          error
	usageErr     error
	originErr    error
	grantErr     error
}

func (f *fakeSources) BrowserSettings(context.Context) (platformconfig.BrowserSettings, error) {
	return f.browser, f.err
}

func (f *fakeSources) WorkerEgressSettings(context.Context) (platformconfig.WorkerEgressSettings, error) {
	return f.workerEgress, nil
}

func (f *fakeSources) BrowserProjectSetting(context.Context, int64) (ProjectSetting, error) {
	return f.setting, nil
}

func (f *fakeSources) WorkerEgressProjectEntries(context.Context, int64) ([]string, error) {
	return f.projectHosts, nil
}

func (f *fakeSources) BrowserSecondsSince(_ context.Context, _ int64, since time.Time) (int64, error) {
	f.since = since
	return f.used, f.usageErr
}

func (f *fakeSources) TriggerOrigin(context.Context, string) (string, error) {
	return f.origin, f.originErr
}

func (f *fakeSources) UnattendedGrant(context.Context, string) (bool, error) {
	return f.granted, f.grantErr
}

var testNow = time.Date(2026, time.November, 5, 14, 30, 0, 0, time.UTC)

func enabledSources() *fakeSources {
	return &fakeSources{
		browser:      platformconfig.DefaultBrowserSettings(),
		workerEgress: platformconfig.WorkerEgressSettings{Mode: platformconfig.WorkerEgressOpen, Allowlist: []string{"api.github.com"}},
		setting:      ProjectSetting{Enabled: true, Engine: "full", AllowDomains: []string{"example.com"}},
		projectHosts: []string{"*.atlassian.net", "API.GitHub.com"},
		origin:       "manual",
	}
}

func testMinter(t *testing.T) (*egresstoken.Minter, *runtoken.KeySet) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	minter, err := egresstoken.New(priv, egresstoken.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := runtoken.NewKeySet(pub)
	if err != nil {
		t.Fatal(err)
	}
	return minter, keys
}

func resolver(t *testing.T, sources *fakeSources, egress Egress) *Resolver {
	t.Helper()
	return NewResolver(sources, sources, sources, sources, egress, nil).WithClock(func() time.Time { return testNow })
}

func verify(t *testing.T, keys *runtoken.KeySet, token string) runtoken.Claims {
	t.Helper()
	verified, err := keys.Verify(token, testNow)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return verified.Claims
}

func TestOpenEgressMintsAnOpenToken(t *testing.T) {
	t.Parallel()
	minter, keys := testMinter(t)
	sources := enabledSources()
	sources.browser.Egress = platformconfig.BrowserEgressOpen
	field, err := resolver(t, sources, Egress{ProxyURL: "http://egress.svc:3128/", Minter: minter}).
		ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
	if err != nil || field == nil {
		t.Fatalf("field=%v err=%v", field, err)
	}
	if field.Egress.Mode != "proxy" || field.Egress.ProxyURL != "http://egress.svc:3128" || field.Egress.Reason != "" {
		t.Fatalf("egress=%v", field.Egress)
	}
	claims := verify(t, keys, field.Egress.Token)
	if !claims.Open || len(claims.Allowlist) != 0 || claims.ProjectID != 7 || claims.ExecutionID != "exec-1" {
		t.Fatalf("claims=%+v", claims)
	}
	// 1800 s plus the 2 min grace.
	if got := claims.ExpiresAt.Sub(claims.NotBefore); got != 32*time.Minute {
		t.Fatalf("ttl=%s", got)
	}
}

func TestAllowlistEgressMintsTheAssembledAllowlist(t *testing.T) {
	t.Parallel()
	minter, keys := testMinter(t)
	sources := enabledSources()
	sources.browser.Egress = platformconfig.BrowserEgressAllowlist
	sources.browser.MaxRunSeconds = 0
	field, err := resolver(t, sources, Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
		ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
	if err != nil {
		t.Fatal(err)
	}
	claims := verify(t, keys, field.Egress.Token)
	want := []string{"api.github.com", "*.atlassian.net", "example.com"}
	if claims.Open || !reflect.DeepEqual(claims.Allowlist, want) {
		t.Fatalf("claims=%+v, want allowlist %v", claims, want)
	}
	// No run cap: 1 h plus the grace.
	if got := claims.ExpiresAt.Sub(claims.NotBefore); got != 62*time.Minute {
		t.Fatalf("ttl=%s", got)
	}
}

// A deployment that never saved the section gets the allowlist: open
// egress is an administrator's explicit choice (ADR-0049 decision 5).
func TestTheDefaultEgressIsTheAllowlist(t *testing.T) {
	t.Parallel()
	minter, keys := testMinter(t)
	field, err := resolver(t, enabledSources(), Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
		ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if claims := verify(t, keys, field.Egress.Token); claims.Open || len(claims.Allowlist) != 3 {
		t.Fatalf("claims=%+v, want the allowlist", claims)
	}
}

func TestGovernedRunForcesTheAllowlist(t *testing.T) {
	t.Parallel()
	minter, keys := testMinter(t)
	open := enabledSources()
	open.browser.Egress = platformconfig.BrowserEgressOpen
	field, err := resolver(t, open, Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
		ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7, Governed: true})
	if err != nil {
		t.Fatal(err)
	}
	if claims := verify(t, keys, field.Egress.Token); claims.Open || len(claims.Allowlist) != 3 {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestEgressUnavailable(t *testing.T) {
	t.Parallel()
	minter, _ := testMinter(t)
	for _, test := range []struct {
		name   string
		egress Egress
		edit   func(*fakeSources)
		reason string
	}{
		{name: "no proxy", egress: Egress{Minter: minter}, reason: "egress_proxy_not_enabled"},
		{name: "no key", egress: Egress{ProxyURL: "http://egress.svc:3128"}, reason: "egress_token_signing_key_not_configured"},
		{name: "empty allowlist", egress: Egress{ProxyURL: "http://egress.svc:3128", Minter: minter},
			edit: func(s *fakeSources) {
				s.browser.Egress = platformconfig.BrowserEgressAllowlist
				s.workerEgress.Allowlist, s.projectHosts, s.setting.AllowDomains = nil, nil, nil
			}, reason: "egress_token_mint_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources := enabledSources()
			if test.edit != nil {
				test.edit(sources)
			}
			field, err := resolver(t, sources, test.egress).
				ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
			if err != nil {
				t.Fatal(err)
			}
			if field.Egress.Mode != "unavailable" || field.Egress.Reason != test.reason ||
				field.Egress.ProxyURL != "" || field.Egress.Token != "" {
				t.Fatalf("egress=%v", field.Egress)
			}
		})
	}
}

func TestQuotaUnattendedAndShape(t *testing.T) {
	t.Parallel()
	minter, _ := testMinter(t)
	sources := enabledSources()
	sources.browser.MonthlyQuotaSeconds = 36000
	sources.used = 1234
	sources.origin = "schedule"
	field, err := resolver(t, sources, Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
		ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !sources.since.Equal(time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("since=%s", sources.since)
	}
	if field.MaxRunSeconds != 1800 || field.MonthlyQuotaSeconds != 36000 || field.MonthUsedSeconds != 1234 ||
		!field.Unattended || !reflect.DeepEqual(field.AllowDomains, []string{"example.com"}) || field.Engine != "full" {
		t.Fatalf("field=%+v", field)
	}
	encoded, err := json.Marshal(field)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{"allow_domains", "egress", "engine", "max_run_seconds", "month_used_seconds", "monthly_quota_seconds", "unattended"}
	got := make([]string, 0, len(keys))
	for key := range keys {
		got = append(got, key)
	}
	if fmt.Sprint(sortStrings(got)) != fmt.Sprint(want) {
		t.Fatalf("keys=%v, want %v", got, want)
	}
	var egress map[string]any
	if err := json.Unmarshal(keys["egress"], &egress); err != nil || len(egress) != 4 {
		t.Fatalf("egress=%v err=%v", egress, err)
	}
	// An empty domain list is [] on the wire, never null.
	sources.setting.AllowDomains = nil
	field, _ = resolver(t, sources, Egress{}).ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
	if encoded, _ := json.Marshal(field); !strings.Contains(string(encoded), `"allow_domains":[]`) {
		t.Fatalf("encoded=%s", encoded)
	}
}

func TestDisabledProjectServesNoField(t *testing.T) {
	t.Parallel()
	sources := enabledSources()
	sources.setting.Enabled = false
	field, err := resolver(t, sources, Egress{}).ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
	if err != nil || field != nil {
		t.Fatalf("field=%v err=%v", field, err)
	}
}

// A source that fails after the project setting degrades the field to
// egress `unavailable` with a fixed reason: the redemption (and the run)
// goes on, and only the browser tools refuse.
func TestFailuresDegradeTheField(t *testing.T) {
	t.Parallel()
	minter, _ := testMinter(t)
	many := make([]string, MaxAllowlistEntries+1)
	for index := range many {
		many[index] = fmt.Sprintf("host%d.example.com", index)
	}
	for _, test := range []struct {
		name       string
		edit       func(*fakeSources)
		reason     string
		unattended bool
	}{
		{name: "platform section error", edit: func(s *fakeSources) { s.err = errors.New("down") },
			reason: EgressUnavailableSourceFailed, unattended: true},
		{name: "usage error", edit: func(s *fakeSources) { s.usageErr = errors.New("ledger down") },
			reason: EgressUnavailableSourceFailed},
		{name: "trigger origin error", edit: func(s *fakeSources) { s.originErr = errors.New("jobs down") },
			reason: EgressUnavailableSourceFailed, unattended: true},
		{name: "grant read error", edit: func(s *fakeSources) { s.grantErr = errors.New("chains down") },
			reason: EgressUnavailableSourceFailed, unattended: true},
		{name: "over the cap", edit: func(s *fakeSources) { s.setting.AllowDomains = many },
			reason: EgressUnavailableAllowlistTooLarge},
		{name: "bad stored entry", edit: func(s *fakeSources) { s.projectHosts = []string{"https://example.com/path"} },
			reason: EgressUnavailableAllowlistInvalid},
		{name: "unknown engine", edit: func(s *fakeSources) { s.setting.Engine = "lite" },
			reason: EgressUnavailableSourceFailed, unattended: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources := enabledSources()
			test.edit(sources)
			field, err := resolver(t, sources, Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
				ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
			if err != nil || field == nil {
				t.Fatalf("field=%v err=%v, want a degraded field", field, err)
			}
			if field.Egress.Mode != EgressModeUnavailable || field.Egress.Reason != test.reason ||
				field.Egress.Token != "" || field.Egress.ProxyURL != "" {
				t.Fatalf("egress=%+v, want unavailable %q", field.Egress, test.reason)
			}
			if field.Unattended != test.unattended || field.Engine != EngineFull {
				t.Fatalf("field=%+v", field)
			}
			if encoded, _ := json.Marshal(field); strings.Contains(string(encoded), "down") {
				t.Fatalf("a source error reached the field: %s", encoded)
			}
		})
	}
	// Once the platform admits Moli the stored engine is served as is.
	for _, engine := range []string{"lite", "auto"} {
		sources := enabledSources()
		sources.setting.Engine = engine
		field, err := resolver(t, sources, Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
			WithEngines(platformconfig.BrowserEngines(true)).
			ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
		if err != nil || field == nil || field.Engine != engine || field.Egress.Mode != EgressModeProxy {
			t.Fatalf("%s: field=%+v err=%v", engine, field, err)
		}
	}
	// The project setting itself is the one source whose failure is an
	// error: without it Main cannot tell whether the browser is on.
	failing := &failingProject{fakeSources: enabledSources()}
	if _, err := NewResolver(failing, failing, failing, failing, Egress{}, nil).
		ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

type failingProject struct{ *fakeSources }

func (f *failingProject) BrowserProjectSetting(context.Context, int64) (ProjectSetting, error) {
	return ProjectSetting{}, errors.New("tenant down")
}

// A manual or api run under an ADR-0045 unattended grant has nobody to
// answer, like a schedule or webhook run.
func TestAGrantedRunIsUnattended(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		origin  string
		granted bool
		want    bool
	}{
		{origin: "manual", want: false},
		{origin: "api", want: false},
		{origin: "manual", granted: true, want: true},
		{origin: "api", granted: true, want: true},
		{origin: "schedule", want: true},
		{origin: "webhook", want: true},
	} {
		sources := enabledSources()
		sources.origin, sources.granted = test.origin, test.granted
		field, err := resolver(t, sources, Egress{}).
			ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
		if err != nil || field.Unattended != test.want {
			t.Fatalf("%s granted=%v: field=%+v err=%v, want unattended %v", test.origin, test.granted, field, err, test.want)
		}
	}
}

func TestInvalidClaims(t *testing.T) {
	t.Parallel()
	if _, err := resolver(t, enabledSources(), Egress{}).ResolveBrowserContext(context.Background(), Claim{}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("err=%v", err)
	}
	if NewResolver(nil, nil, nil, nil, Egress{}, nil) != nil {
		t.Fatal("a resolver without readers was built")
	}
}

func sortStrings(values []string) []string {
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if values[j] < values[i] {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
	return values
}
