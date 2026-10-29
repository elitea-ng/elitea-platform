package workeregress

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func sortedToolkitHosts(t *testing.T, toolkitType, settings string) []string {
	t.Helper()
	hosts, err := HostsForToolkit(toolkitType, []byte(settings))
	if err != nil {
		t.Fatalf("%s %s: %v", toolkitType, settings, err)
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// An OpenAPI toolkit's hosts are its spec URL and its operations' base: the
// override, else the spec's first server with its variables' defaults
// (families/openapi/source.rs load, spec.rs parse_base_url). Nothing else
// the spec names is admitted.
func TestOpenAPIToolkitHostsMatchTheWorker(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		want           []string
	}{
		{"spec by URL", `{"spec":"https://specs.example.com/petstore.yaml?rev=2"}`, []string{"specs.example.com"}},
		{"spec by URL under openapi_spec", `{"openapi_spec":" https://specs.example.com/a.json "}`, []string{"specs.example.com"}},
		{"an http spec URL is refused by the worker", `{"spec":"http://specs.example.com/a.json"}`, []string{}},
		{"spec URL with an override", `{"spec":"https://specs.example.com/a.json","base_url_override":"https://api.example.com/v2"}`,
			[]string{"api.example.com", "specs.example.com"}},
		{"inline YAML with a templated server", `{"spec":"openapi: 3.0.3\nservers:\n  - url: '{scheme}://{host}/v1'\n    variables:\n      scheme: {default: https}\n      host: {default: yaml.example.com}\n  - url: https://second.example.com\npaths: {}\ninfo: {contact: {url: 'https://contact.example.com'}}\n"}`,
			[]string{"yaml.example.com"}},
		{"inline JSON text", `{"schema_settings":"{\"openapi\":\"3.0.0\",\"servers\":[{\"url\":\"https://json.example.com/api\"}],\"paths\":{}}"}`,
			[]string{"json.example.com"}},
		{"object spec, first server only", `{"spec":{"openapi":"3.1.0","servers":[{"url":"https://{region}.example.com","variables":{"region":{"default":"eu"}}},{"url":"https://second.example.com"}],"externalDocs":{"url":"https://docs.example.com"},"paths":{}}}`,
			[]string{"eu.example.com"}},
		{"an override replaces the servers", `{"base_url":"https://override.example.com","spec":{"servers":[{"url":"https://server.example.com"}]}}`,
			[]string{"override.example.com"}},
		{"a relative inline server is refused by the worker", `{"spec":{"servers":[{"url":"/v1"}]}}`, []string{}},
		{"an http server is refused by the worker", `{"spec":{"servers":[{"url":"http://plain.example.com"}]}}`, []string{}},
		{"a variable without a default is refused by the worker", `{"spec":{"servers":[{"url":"https://{host}"}]}}`, []string{}},
		{"the auth settings are still read", `{"spec":{"servers":[{"url":"https://api.example.com"}]},"openapi_configuration":{"token_url":"https://login.example.com/token"}}`,
			[]string{"api.example.com", "login.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedToolkitHosts(t, "openapi", tc.settings)
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("hosts = %v, want %v", got, tc.want)
			}
		})
	}
	// An index ingest's typed toolkit is read from its settings.
	hosts, err := HostsFromTypedToolkit([]byte(`{"type":"openapi","settings":{"spec":"https://specs.example.com/a.json"}}`))
	if err != nil || !slices.Equal(hosts, []string{"specs.example.com"}) {
		t.Fatalf("typed: %v %v", hosts, err)
	}
	// Another type keeps reading every URL field of its settings.
	if got := sortedToolkitHosts(t, "", `{"spec":{"servers":[{"url":"https://a.example.com"},{"url":"https://b.example.com"}]}}`); !slices.Equal(got, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("untyped: %v", got)
	}
}

// A GitHub toolkit's workspace push goes to the forge's web origin, which
// the worker derives from the API base: api.github.com is github.com, and a
// GitHub Enterprise base's own host is its web host.
func TestGitHubToolkitAdmitsItsGitRemote(t *testing.T) {
	if got := sortedToolkitHosts(t, "github", `{"repository":"o/r","github_configuration":{"base_url":"https://api.github.com"}}`); !slices.Equal(got, []string{"api.github.com", "github.com"}) {
		t.Fatalf("github.com: %v", got)
	}
	if got := sortedToolkitHosts(t, "github", `{"repository":"o/r","github_configuration":{"base_url":"https://ghe.example.com:8443/api/v3"}}`); !slices.Equal(got, []string{"ghe.example.com"}) {
		t.Fatalf("GitHub Enterprise: %v", got)
	}
	if got := sortedToolkitHosts(t, "gitlab", `{"gitlab_configuration":{"url":"https://api.github.com"}}`); !slices.Equal(got, []string{"api.github.com"}) {
		t.Fatalf("another type: %v", got)
	}
	// Pinned against the worker's mapping: a change there fails here.
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "libs", "rust", "agent-runtime", "src", "toolkits", "workspace_pull_request.rs"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "fn github_web_origin(")
	if start < 0 {
		t.Fatal("workspace_pull_request.rs no longer declares github_web_origin: update derivedHosts")
	}
	body := text[start:min(len(text), start+800)]
	if !strings.Contains(body, `eq_ignore_ascii_case("`+githubAPIHost+`")`) || !strings.Contains(body, `set_host(Some("`+githubWebHost+`"))`) {
		t.Fatalf("github_web_origin no longer maps %s to %s: update derivedHosts", githubAPIHost, githubWebHost)
	}
}
