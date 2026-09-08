// Package deepwiki is the DeepWiki sub-application as the host sees it: its
// descriptor, its toolkit admission table, and whichever runner a
// deployment wires. The descriptor is the legacy-v1 document — a copy of
// conformance/provider/fixtures/deepwiki/descriptor/legacy-v1/
// provider_descriptor.json, pinned byte for byte by a test — with the
// service location the host is configured with written into it.
//
// REVISION legacy-v1 adds three arguments to `ask` and `deep_research` and
// changes nothing else (a test asserts the "nothing else"):
//
//	context_paths             wiki pages a reader attached to the question
//	context_wiki_version_id   the version that selection was made in
//	chat_history              sent on every wiki-chat turn since the port,
//	                          and never declared by the legacy plugin
//
// It is generated, not hand-edited: services/elitea-deepwiki/tools/
// build_descriptor_v1.py derives it from legacy-v0 and writes both this
// copy and the fixture, so the two cannot disagree. legacy-v0 stays in the
// fixtures as the record of what the legacy plugin actually declared.
//
// The engine stays where it is (services/elitea-deepwiki, Python); reaching
// it from this host is ADR-0023 stage H2. Until then a host serving this
// application runs the unavailable runner, or the echo runner on a stack
// that needs the invoke → poll → cancel path with no engine.
package deepwiki

import (
	_ "embed"
	"encoding/json"
	"regexp"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

//go:embed descriptor.json
var descriptorJSON []byte

// Name and Version are what /health reports.
const (
	Name    = "elitea-deepwiki"
	Version = "1.0.0"
)

// EnvPrefix is the settings namespace this application reads.
const EnvPrefix = "ELITEA_DEEPWIKI_"

// locationField is the descriptor's second key; only its value changes.
var locationField = regexp.MustCompile(`"service_location_url":\s*"[^"]*"`)

// Descriptor returns the descriptor for a service location. The document is
// emitted verbatim — key order included, which the conformance test pins —
// with the location substituted; nothing else is parsed or re-encoded.
func Descriptor(serviceLocationURL string) any {
	quoted, _ := json.Marshal(serviceLocationURL)
	return json.RawMessage(locationField.ReplaceAll(descriptorJSON, []byte(`"service_location_url": `+string(quoted))))
}

// Toolkits is the admission table: three families, the aliases user data
// created before renames still carries, and the two refusal shapes the
// legacy plugin raised — see conformance/provider/fixtures/deepwiki/spi/
// toolkit_aliases.json, which a test compares this against.
var Toolkits = spi.Toolkits{
	Families: []spi.Family{
		{
			Name:    "main",
			Aliases: []string{"WikiBuilderToolkit", "deepwiki", "Deepwiki", "wiki", "DeepWikiToolkit", "DeepWiki", "Wiki", "wikis", "Wikis"},
			Tools:   []string{"generate_wiki", "ask", "deep_research"},
		},
		{
			Name:                      "query",
			Aliases:                   []string{"wikis_query", "deepwiki_query", "DeepwikiQuery", "deepwiki-query"},
			Tools:                     []string{"ask", "deep_research"},
			UnknownToolIsInvalidInput: true,
			Label:                     "deepwiki_query",
		},
		{
			Name:                      "wiki_query",
			Aliases:                   []string{"wiki_query", "WikiQuery", "wiki-query"},
			Tools:                     []string{"list_wikis", "resolve_and_ask", "resolve_and_deep_research", "delete_wiki"},
			UnknownToolIsInvalidInput: true,
			Label:                     "wiki_query",
		},
	},
	Advertised: []string{"Wikis", "wikis_query", "wiki_query"},
}

// App assembles the application over a runner.
func App(runner spi.Runner) spi.App {
	return spi.App{Name: Name, Version: Version, Descriptor: Descriptor, Toolkits: Toolkits, Runner: runner}
}
