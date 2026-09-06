package apps_test

// The registry is the one place that answers "which applications does this
// binary serve, and with which runners". These tests read every entry
// through Lookup — not through the package variables — because the failure
// the registry replaced was exactly a lookup that disagreed with the table.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps"
	deepwikirun "github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	inventoryrun "github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

func TestEveryRegisteredApplicationIsWholeAndDistinct(t *testing.T) {
	names := apps.Names()
	if len(names) == 0 {
		t.Fatal("the registry is empty")
	}
	prefixes := map[string]string{}
	for _, name := range names {
		app, err := apps.Lookup(name)
		if err != nil {
			t.Fatalf("%s is named and not registered: %v", name, err)
		}
		if app.Key != name || app.Name == "" || app.Version == "" || app.Descriptor == nil {
			t.Errorf("%s: %+v", name, app)
		}
		if !strings.HasPrefix(app.EnvPrefix, "ELITEA_") || !strings.HasSuffix(app.EnvPrefix, "_") {
			t.Errorf("%s: settings prefix %q", name, app.EnvPrefix)
		}
		if other, dup := prefixes[app.EnvPrefix]; dup {
			t.Errorf("%s and %s read the same settings namespace %s", other, name, app.EnvPrefix)
		}
		prefixes[app.EnvPrefix] = name
		// An admission table that does not validate refuses every
		// invocation at runtime instead of failing here.
		if err := app.Toolkits.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		// The descriptor is the application's own document, and the service
		// location it is asked for is the one it carries.
		raw, err := json.Marshal(app.Descriptor("https://host.example/subapp"))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		if document["service_location_url"] != "https://host.example/subapp" {
			t.Errorf("%s descriptor location: %v", name, document["service_location_url"])
		}
		if document["name"] == nil || document["provided_toolkits"] == nil {
			t.Errorf("%s descriptor is not a provider self-description: %v", name, document)
		}
	}
}

// The lookup answers with THAT application: the identity a host composes
// under one name must not be another's. Each row is checked against a value
// no other entry carries.
func TestLookupAnswersWithTheApplicationItWasAskedFor(t *testing.T) {
	for _, want := range []struct{ key, name, prefix, descriptorName, toolkit string }{
		{"deepwiki", "elitea-deepwiki", "ELITEA_DEEPWIKI_", "wikis", "Wikis"},
		{"echo", "elitea-echo", "ELITEA_ECHO_", "echo", "Echo"},
		{"inventory", "elitea-inventory", "ELITEA_INVENTORY_", "inventory", "inventory_search"},
	} {
		app, err := apps.Lookup(want.key)
		if err != nil {
			t.Fatalf("%s: %v", want.key, err)
		}
		if app.Name != want.name || app.EnvPrefix != want.prefix {
			t.Errorf("%s composed as %s / %s", want.key, app.Name, app.EnvPrefix)
		}
		var document map[string]any
		raw, _ := json.Marshal(app.Descriptor("http://127.0.0.1:8080"))
		_ = json.Unmarshal(raw, &document)
		if document["name"] != want.descriptorName {
			t.Errorf("%s served the descriptor of %v", want.key, document["name"])
		}
		if _, err := app.Toolkits.Resolve(want.toolkit); err != nil {
			t.Errorf("%s does not admit its own toolkit %s: %v", want.key, want.toolkit, err)
		}
		// The name is normalised the way the environment variable is read.
		if upper, err := apps.Lookup("  " + strings.ToUpper(want.key) + " "); err != nil || upper.Name != want.name {
			t.Errorf("%s is not found when spelled loudly: %v", want.key, err)
		}
	}
	if app, err := apps.Lookup(""); err != nil || app.Key != apps.DefaultKey {
		t.Errorf("the empty key is not the default: %v %v", app.Key, err)
	}
	unknown, err := apps.Lookup("not-an-application")
	if !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("an unknown application was composed as %+v (%v)", unknown, err)
	}
	for _, name := range apps.Names() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s: %v", name, err)
		}
	}
}

// Every application gets the two shared runners; an application's own
// runners belong to it alone, and asking another for one is a refusal, not
// a silently different runner.
func TestRunnersAreTheSharedOnesPlusTheApplicationsOwn(t *testing.T) {
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range apps.Names() {
		app, _ := apps.Lookup(name)
		for _, shared := range []string{"unavailable", "echo"} {
			runner, err := app.Runner(shared, settings, 0)
			if err != nil || runner.Name() != shared {
				t.Errorf("%s/%s: %v %v", name, shared, err, runner)
			}
		}
		if _, err := app.Runner("no-such-runner", settings, 0); !errors.Is(err, spi.ErrConfig) {
			t.Errorf("%s accepted an unknown runner: %v", name, err)
		}
		for _, offered := range app.RunnerNames() {
			if _, err := app.Runner(offered, settings, time.Millisecond); err != nil && offered != "legacy" {
				t.Errorf("%s offers %s and refuses it: %v", name, offered, err)
			}
		}
	}
	deepwiki, _ := apps.Lookup("deepwiki")
	if runner, err := deepwiki.Runner("fixture", settings, 0); err != nil || runner.Name() != "fixture" {
		t.Fatalf("deepwiki/fixture: %v %v", err, runner)
	}
	// The sidecar runner without a socket to reach: a host that came up
	// looking healthy here would answer /health UP and fail every call.
	if _, err := deepwiki.Runner("legacy", settings, 0); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("deepwiki/legacy with no socket: %v", err)
	}
	withSocket := settings
	withSocket.EngineSocket = "/run/deepwiki/engine.sock"
	if runner, err := deepwiki.Runner("legacy", withSocket, 0); err != nil || runner.Name() != "legacy" {
		t.Fatalf("deepwiki/legacy: %v %v", err, runner)
	}
	// Inventory has a legacy runner of its own since ADR-0023 H4c stage I3, and
	// it takes the same refusal without a socket.
	//
	// It is asserted SEPARATELY from the "another application's runner" loop
	// below, because until this stage that loop covered inventory/legacy and
	// passed — for the wrong reason. `legacy` was not offered, so it was
	// refused as unknown, with the same spi.ErrConfig an unreachable socket
	// produces. Adding the runner would have left that assertion green while it
	// stopped meaning anything.
	inventoryApp, _ := apps.Lookup("inventory")
	if _, err := inventoryApp.Runner("legacy", settings, 0); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("inventory/legacy with no socket: %v", err)
	}
	inventorySocket := settings
	inventorySocket.EngineSocket = "/run/inventory/engine.sock"
	runner, err := inventoryApp.Runner("legacy", inventorySocket, 0)
	if err != nil || runner.Name() != "legacy" {
		t.Fatalf("inventory/legacy: %v %v", err, runner)
	}

	// Inventory has a fixture runner of its own now, and it is ITS OWN — not
	// DeepWiki's under a shared name. The distinction is the whole assertion:
	// both are called "fixture", both answer Name() the same way, and a
	// registry that handed Inventory DeepWiki's factory would produce a host
	// that answers `generate_wiki` on an `inventory` toolkit. The concrete
	// type is what tells them apart, so that is what is compared.
	inventoryFixture, err := inventoryApp.Runner("fixture", settings, 0)
	if err != nil || inventoryFixture.Name() != "fixture" {
		t.Fatalf("inventory/fixture: %v %v", err, inventoryFixture)
	}
	deepwikiFixture, err := deepwiki.Runner("fixture", settings, 0)
	if err != nil {
		t.Fatalf("deepwiki/fixture: %v", err)
	}
	if _, isInventorys := inventoryFixture.(*inventoryrun.Runner); !isInventorys {
		t.Errorf("inventory/fixture is %T, not Inventory's own runner", inventoryFixture)
	}
	if _, isDeepWikis := deepwikiFixture.(*deepwikirun.Runner); !isDeepWikis {
		t.Errorf("deepwiki/fixture is %T, not DeepWiki's own runner", deepwikiFixture)
	}

	// echo has neither. A runner served by an application it was not written
	// for is how a deployment gets one provider's canned results out of
	// another's host.
	echoNoFixture, _ := apps.Lookup("echo")
	if _, err := echoNoFixture.Runner("fixture", inventorySocket, 0); !errors.Is(err, spi.ErrConfig) {
		t.Errorf("echo served a fixture runner it has none of: %v", err)
	}
	echoApp, _ := apps.Lookup("echo")
	if _, err := echoApp.Runner("legacy", inventorySocket, 0); !errors.Is(err, spi.ErrConfig) {
		t.Errorf("echo served a legacy runner it has no engine for: %v", err)
	}
}

// Compose carries the entry into the served application unchanged.
func TestComposeCarriesTheEntryOntoTheRunner(t *testing.T) {
	entry, _ := apps.Lookup("inventory")
	app := entry.Compose(spi.UnavailableRunner{})
	if app.Name != entry.Name || app.Version != entry.Version || app.Runner.Name() != "unavailable" {
		t.Fatalf("%+v", app)
	}
	if len(app.Toolkits.Families) != len(entry.Toolkits.Families) {
		t.Fatalf("toolkits: %d vs %d", len(app.Toolkits.Families), len(entry.Toolkits.Families))
	}
}
