package inventory_test

// SourcesFromEnv, and the refusals NewSources makes.
//
// These four variables decide whether Inventory may open a vault at all, and
// every one of them is fail-closed or fail-loud in a different way. Until this
// file they were read by nothing but the composition root, which no test
// exercises — the survey measured SourcesFromEnv at 0%.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

func env(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := pairs[key]
		return value, ok
	}
}

// Nothing set is a deployment that expands no source: the allowlist refuses
// every host, the callback origin is empty (which the composition root reads
// as "mount without expansion"), and the defaults stand.
func TestAnUnsetEnvironmentIsFailClosedRatherThanPermissive(t *testing.T) {
	cfg, err := inventory.SourcesFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CallbackBaseURL != "" {
		t.Errorf("callback origin %q", cfg.CallbackBaseURL)
	}
	if cfg.CallbackTokenTTL != 2*time.Hour {
		t.Errorf("token lifetime %v, want the two-hour default", cfg.CallbackTokenTTL)
	}
	if len(cfg.SourceTypes) != len(inventory.DefaultSourceTypes) {
		t.Errorf("source types %v, want the default %v", cfg.SourceTypes, inventory.DefaultSourceTypes)
	}
	// The decisive one: an unset allowlist must refuse, not permit. A policy
	// that admitted every host while nobody had written one down is how a
	// credential reaches a repository the deployment never approved.
	if err := cfg.GitEgress.Allow("github.com"); err == nil {
		t.Error("an unset ELITEA_INVENTORY_GIT_ALLOWLIST admitted a host")
	}
}

// A nil lookup reads the process environment. It is the production call — the
// composition root passes os.LookupEnv — and a reader that panicked on nil
// would take the boot down on a path no test covers.
func TestANilLookupReadsTheProcessEnvironment(t *testing.T) {
	cfg, err := inventory.SourcesFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CallbackTokenTTL != 2*time.Hour {
		t.Fatalf("token lifetime %v", cfg.CallbackTokenTTL)
	}
}

// Everything set is read as written, and the allowlist admits exactly what it
// names.
func TestEachSettingIsReadAsWritten(t *testing.T) {
	cfg, err := inventory.SourcesFromEnv(env(map[string]string{
		inventory.GitAllowlistEnv:     "github.com, *.github.com",
		inventory.CallbackBaseURLEnv:  "  http://elitea-main:8080  ",
		inventory.CallbackTokenTTLEnv: "30",
		inventory.SourceTypesEnv:      "github, ado_repos",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CallbackBaseURL != "http://elitea-main:8080" {
		t.Errorf("callback origin %q — the surrounding space was not trimmed", cfg.CallbackBaseURL)
	}
	if cfg.CallbackTokenTTL != 30*time.Minute {
		t.Errorf("token lifetime %v, want 30 minutes", cfg.CallbackTokenTTL)
	}
	if strings.Join(cfg.SourceTypes, ",") != "github,ado_repos" {
		t.Errorf("source types %v", cfg.SourceTypes)
	}
	if err := cfg.GitEgress.Allow("api.github.com"); err != nil {
		t.Errorf("a listed wildcard host was refused: %v", err)
	}
	if err := cfg.GitEgress.Allow("gitlab.example"); err == nil {
		t.Error("a host nobody listed was admitted")
	}
}

// A lifetime that cannot be parsed is an ERROR, not a default. The two are
// indistinguishable at run time otherwise: a mistyped TTL would silently
// become two hours and nobody would learn which value is in force.
func TestALifetimeThatCannotBeParsedIsRefused(t *testing.T) {
	for _, raw := range []string{"soon", "0", "-5", "2.5"} {
		cfg, err := inventory.SourcesFromEnv(env(map[string]string{
			inventory.CallbackTokenTTLEnv: raw,
		}))
		if err == nil {
			t.Errorf("%q was accepted as %v", raw, cfg.CallbackTokenTTL)
			continue
		}
		if !errors.Is(err, inventory.ErrInvalidRoute) {
			t.Errorf("%q: %v", raw, err)
		}
		if !strings.Contains(err.Error(), inventory.CallbackTokenTTLEnv) {
			t.Errorf("%q: the refusal does not name the variable: %v", raw, err)
		}
	}
}

// The source-type list can only SUBTRACT. A type with no field projection
// cannot be admitted by configuration, because admitting it would mean sending
// fields nobody enumerated — the list here is what an operator writes, and
// the Expander is what refuses an unprojectable one.
func TestTheSourceTypeListIsWhateverTheOperatorNames(t *testing.T) {
	cfg, err := inventory.SourcesFromEnv(env(map[string]string{
		inventory.SourceTypesEnv: "github",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.SourceTypes, ",") != "github" {
		t.Fatalf("source types %v", cfg.SourceTypes)
	}
	// Every type this facade knows how to project has an entry in SourceKinds,
	// and every DEFAULT type must be one of them: a default naming a type with
	// no projection would promise an expansion nobody wrote.
	for _, kind := range inventory.DefaultSourceTypes {
		if _, projected := inventory.SourceKinds[kind]; !projected {
			t.Errorf("%s is a default source type with no field projection", kind)
		}
	}
	// And the three tools that name a source are the three the descriptor's
	// args_schema declares one for. A fourth here would rewrite a body no
	// provider expects; a missing one would forward an unexpanded id.
	if strings.Join(inventory.ExpandingTools, ",") !=
		"run_ingestion,delta_update,remove_source_entities" {
		t.Errorf("expanding tools %v", inventory.ExpandingTools)
	}
}

// A half-wired expander is refused. It would serve perfectly well and simply
// not expand, or not check — the failure with no symptom this composition root
// exists to prevent.
func TestAHalfWiredExpanderIsRefused(t *testing.T) {
	toolkits := toolkits()
	settings := &countingSettings{}
	minter := &recordingMinter{}
	whole := config()

	cases := map[string]func() (*inventory.Sources, error){
		"no toolkit reader": func() (*inventory.Sources, error) {
			return inventory.NewSources(nil, settings, minter, whole)
		},
		"no settings resolver": func() (*inventory.Sources, error) {
			return inventory.NewSources(toolkits, nil, minter, whole)
		},
		"no callback minter": func() (*inventory.Sources, error) {
			return inventory.NewSources(toolkits, settings, nil, whole)
		},
		"no callback origin": func() (*inventory.Sources, error) {
			without := whole
			without.CallbackBaseURL = "   "
			return inventory.NewSources(toolkits, settings, minter, without)
		},
	}
	for name, compose := range cases {
		t.Run(name, func(t *testing.T) {
			built, err := compose()
			if err == nil || built != nil {
				t.Fatalf("composed anyway: %v %v", built, err)
			}
			if !errors.Is(err, material.ErrSourceUnavailable) {
				t.Fatalf("%v", err)
			}
			if !strings.Contains(err.Error(), inventory.CallbackBaseURLEnv) {
				t.Fatalf("the refusal does not name the variable an operator must set: %v", err)
			}
		})
	}

	// The whole thing composes, and the callback origin loses its trailing
	// slash — the provider joins a path onto it, and two slashes is a URL the
	// artifact routes do not answer.
	trailing := whole
	trailing.CallbackBaseURL = "http://elitea-main:8080/"
	built, err := inventory.NewSources(toolkits, settings, minter, trailing)
	if err != nil || built == nil {
		t.Fatalf("%v %v", built, err)
	}
	if built.CallbackBase != "http://elitea-main:8080" {
		t.Fatalf("callback base %q", built.CallbackBase)
	}
}

// Every refusal the expander can make maps to a status a caller can act on,
// and the default arm is a 503 rather than a 500: "could not be resolved" is
// a deployment state, not a bug in the request.
func TestEveryExpansionRefusalHasAStatusACallerCanActOn(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want int
	}{
		{"a source the toolkit does not list", material.ErrSourceNotAdmitted, 403},
		{"a host the deployment refuses", material.ErrEgressRefused, 403},
		{"a source this facade cannot expand", material.ErrSourceRefused, 400},
		{"a body the rewriter rejected", material.ErrRejected, 400},
		{"anything else", errors.New("the vault is unreachable"), 503},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, message := inventory.StatusForSourceError(testCase.err)
			if status != testCase.want {
				t.Fatalf("status %d, want %d", status, testCase.want)
			}
			if strings.TrimSpace(message) == "" {
				t.Fatal("no message: a refusal a caller cannot read is a 403 with nothing to do about it")
			}
			// The message is the facade's own text, never the underlying
			// error. A vault path or a toolkit id in a 403 body tells a
			// caller about rows they may not see.
			if strings.Contains(message, testCase.err.Error()) {
				t.Fatalf("the internal error text reached the caller: %q", message)
			}
		})
	}
}
