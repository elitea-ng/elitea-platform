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
	err          error
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
	return f.used, nil
}

func (f *fakeSources) TriggerOrigin(context.Context, string) (string, error) { return f.origin, nil }

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
	field, err := resolver(t, enabledSources(), Egress{ProxyURL: "http://egress.svc:3128/", Minter: minter}).
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

func TestGovernedRunForcesTheAllowlist(t *testing.T) {
	t.Parallel()
	minter, keys := testMinter(t)
	field, err := resolver(t, enabledSources(), Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
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

func TestFailures(t *testing.T) {
	t.Parallel()
	minter, _ := testMinter(t)
	many := make([]string, 129)
	for index := range many {
		many[index] = fmt.Sprintf("host%d.example.com", index)
	}
	for _, test := range []struct {
		name string
		edit func(*fakeSources)
		want error
	}{
		{name: "source error", edit: func(s *fakeSources) { s.err = errors.New("down") }, want: ErrUnavailable},
		{name: "over the cap", edit: func(s *fakeSources) {
			s.browser.Egress = platformconfig.BrowserEgressAllowlist
			s.setting.AllowDomains = many
		}, want: ErrAllowlistTooLarge},
		{name: "bad stored entry", edit: func(s *fakeSources) {
			s.browser.Egress = platformconfig.BrowserEgressAllowlist
			s.projectHosts = []string{"https://example.com/path"}
		}, want: ErrAllowlistInvalid},
		{name: "unknown engine", edit: func(s *fakeSources) { s.setting.Engine = "lite" }, want: ErrSettingInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources := enabledSources()
			test.edit(sources)
			_, err := resolver(t, sources, Egress{ProxyURL: "http://egress.svc:3128", Minter: minter}).
				ResolveBrowserContext(context.Background(), Claim{ExecutionID: "exec-1", ProjectID: 7})
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v, want %v", err, test.want)
			}
		})
	}
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
