// Package echo is the trivial sub-application ADR-0023's verification names:
// one toolkit, one tool, a runner that echoes. It exists to prove the host
// is generic — the SPI conformance suite passes against it as it does
// against DeepWiki's table — and to give a stack an invoke → poll → cancel
// path with no engine behind it.
package echo

import (
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	Name      = "elitea-echo"
	Version   = "1.0.0"
	EnvPrefix = "ELITEA_ECHO_"
)

// Toolkits: one family, one advertised name, one tool.
var Toolkits = spi.Toolkits{
	Families:   []spi.Family{{Name: "main", Aliases: []string{"Echo", "echo"}, Tools: []string{"echo"}}},
	Advertised: []string{"Echo"},
}

// Descriptor is the minimal provider self-description in the legacy shape.
func Descriptor(serviceLocationURL string) any {
	return map[string]any{
		"name":                 "echo",
		"service_location_url": serviceLocationURL,
		"configuration":        map[string]any{},
		"provided_toolkits": []map[string]any{{
			"name": "Echo",
			"provided_tools": []map[string]any{{
				"name":                      "echo",
				"description":               "Answers with what it was asked.",
				"sync_invocation_supported": true,
				"args_schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"query": map[string]any{"type": "string"}},
				},
			}},
		}},
	}
}

// App assembles the application with the echo runner pacing its steps.
func App(step time.Duration) spi.App {
	return spi.App{Name: Name, Version: Version, Descriptor: Descriptor, Toolkits: Toolkits, Runner: spi.EchoRunner{Step: step}}
}
