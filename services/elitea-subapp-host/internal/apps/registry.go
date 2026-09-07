// Package apps is the registry of the sub-applications this host binary can
// serve: one entry per application, contributing its name, its settings
// prefix, its descriptor, its toolkit admission table and the runners it
// offers beyond the two every application gets.
//
// It exists so that adding an application is one entry rather than an edit
// to the composition root. Before ADR-0023 stage H4c the binary carried a
// switch on ELITEA_SUBAPP and, next to it, a second switch on the runner
// name with per-application guards inside it ("fixture is DeepWiki's runner,
// not echo's"). Two switches that must agree is how a third application gets
// half-wired: composed here, unreachable there. The table below is the one
// place both questions are answered.
package apps

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	deepwikirun "github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	inventoryrun "github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// RunnerFactory builds one runner for an application from its settings.
// step is the pause a paced runner (echo, fixture) puts between progress
// events, read from <PREFIX>FIXTURE_STEP_SECONDS.
type RunnerFactory func(settings spi.Settings, step time.Duration) (spi.Runner, error)

// App is one registry entry: everything the host needs to serve an
// application, and nothing about how it is deployed.
type App struct {
	// Key is the ELITEA_SUBAPP value that selects this application.
	Key string
	// Name and Version are what /health reports.
	Name    string
	Version string
	// EnvPrefix is the settings namespace, <PREFIX>RUNNER included.
	EnvPrefix string
	// Descriptor is the provider self-description for a service location.
	Descriptor func(serviceLocationURL string) any
	// Toolkits is the admission table.
	Toolkits spi.Toolkits
	// Runners are this application's own runners, beyond the shared ones
	// (unavailable, echo) every application offers.
	Runners map[string]RunnerFactory
}

// sharedRunners are offered by every application: the default that refuses
// every tool in band, and the echo runner that walks invoke → poll → cancel
// with no engine behind it.
var sharedRunners = map[string]RunnerFactory{
	"unavailable": func(spi.Settings, time.Duration) (spi.Runner, error) { return spi.UnavailableRunner{}, nil },
	"echo":        func(_ spi.Settings, step time.Duration) (spi.Runner, error) { return spi.EchoRunner{Step: step}, nil },
}

// DefaultKey is the application a host serves when ELITEA_SUBAPP is unset.
const DefaultKey = "deepwiki"

// registry is the table. Order is the order Names reports.
var registry = []App{
	{
		Key:        "deepwiki",
		Name:       deepwiki.Name,
		Version:    deepwiki.Version,
		EnvPrefix:  deepwiki.EnvPrefix,
		Descriptor: deepwiki.Descriptor,
		Toolkits:   deepwiki.Toolkits,
		Runners: map[string]RunnerFactory{
			// The DeepWiki composition-and-upload path over canned engine
			// results (the Python shell's fixture runner, ported): what the
			// browser journeys run against.
			"fixture": func(settings spi.Settings, step time.Duration) (spi.Runner, error) {
				return deepwikirun.NewFixtureRunner(settings, step), nil
			},
			// The analysis engine, reached as a sidecar over a local socket
			// (ADR-0023 H2): the engine's dependency closure stays in Python;
			// composition, upload and the SPI are this host's. A host asked
			// for the engine with no socket to reach it must not come up
			// looking healthy.
			"legacy": func(settings spi.Settings, _ time.Duration) (spi.Runner, error) {
				if settings.EngineSocket == "" {
					return nil, fmt.Errorf("%w: %sRUNNER=legacy needs %sENGINE_SOCKET, the engine sidecar's Unix socket",
						spi.ErrConfig, settings.Prefix, settings.Prefix)
				}
				return deepwikirun.NewEngineRunner(settings), nil
			},
		},
	},
	{
		Key:        "echo",
		Name:       echo.Name,
		Version:    echo.Version,
		EnvPrefix:  echo.EnvPrefix,
		Descriptor: echo.Descriptor,
		Toolkits:   echo.Toolkits,
	},
	{
		Key:        "inventory",
		Name:       inventory.Name,
		Version:    inventory.Version,
		EnvPrefix:  inventory.EnvPrefix,
		Descriptor: inventory.Descriptor,
		Toolkits:   inventory.Toolkits,
		Runners: map[string]RunnerFactory{
			// The ingest-and-read path over a canned knowledge graph: what a
			// browser journey runs against on a stack with no engine image.
			// The merge, the source check, composition and the artifact
			// upload are the production code; only the graph is canned.
			"fixture": func(settings spi.Settings, step time.Duration) (spi.Runner, error) {
				return inventoryrun.NewFixtureRunner(settings, step), nil
			},
			// The knowledge-graph engine, reached as a sidecar over a local
			// socket (ADR-0023 H4c stage I3): the engine's dependency closure
			// stays in Python; composition, upload and the SPI are this
			// host's. A host asked for the engine with no socket to reach it
			// must not come up looking healthy — the same rule, and the same
			// refusal, as DeepWiki's.
			"legacy": func(settings spi.Settings, _ time.Duration) (spi.Runner, error) {
				if settings.EngineSocket == "" {
					return nil, fmt.Errorf("%w: %sRUNNER=legacy needs %sENGINE_SOCKET, the engine sidecar's Unix socket",
						spi.ErrConfig, settings.Prefix, settings.Prefix)
				}
				return inventoryrun.NewEngineRunner(settings), nil
			},
		},
	},
}

// Names lists the registered keys, in table order — the text a refusal
// names, so it can never disagree with what is served.
func Names() []string {
	names := make([]string, 0, len(registry))
	for _, app := range registry {
		names = append(names, app.Key)
	}
	return names
}

// Lookup returns the entry for a key, matched case-insensitively after
// trimming, as the environment variable was read before this registry.
func Lookup(key string) (App, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		key = DefaultKey
	}
	for _, app := range registry {
		if app.Key == key {
			return app, nil
		}
	}
	return App{}, fmt.Errorf("%w: ELITEA_SUBAPP=%q is not a known sub-application (%s)",
		spi.ErrConfig, key, strings.Join(Names(), ", "))
}

// RunnerNames lists the runners this application serves, sorted.
func (a App) RunnerNames() []string {
	names := make([]string, 0, len(sharedRunners)+len(a.Runners))
	for name := range sharedRunners {
		names = append(names, name)
	}
	for name := range a.Runners {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Runner builds the named runner, or refuses with the set this application
// actually serves. A runner another application offers is refused here for
// the same reason an unknown name is: this one does not serve it.
func (a App) Runner(name string, settings spi.Settings, step time.Duration) (spi.Runner, error) {
	if factory, ok := a.Runners[name]; ok {
		return factory(settings, step)
	}
	if factory, ok := sharedRunners[name]; ok {
		return factory(settings, step)
	}
	return nil, fmt.Errorf("%w: %sRUNNER=%q is not served by this host for %s (%s)",
		spi.ErrConfig, a.EnvPrefix, name, a.Key, strings.Join(a.RunnerNames(), ", "))
}

// Compose assembles the served application over a runner.
func (a App) Compose(runner spi.Runner) spi.App {
	return spi.App{Name: a.Name, Version: a.Version, Descriptor: a.Descriptor, Toolkits: a.Toolkits, Runner: runner}
}
