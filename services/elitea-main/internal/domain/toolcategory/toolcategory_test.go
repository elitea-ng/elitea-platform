package toolcategory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func repoFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(append([]string{filepath.Dir(file), "..", "..", "..", "..", ".."}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

// The embedded copy is the shared contract, byte for byte: the worker and
// Main read one declaration.
func TestContractCopyIsCurrent(t *testing.T) {
	t.Parallel()
	shared := repoFile(t, "libs", "proto", "contracts", "tool-categories-v1.json")
	if !bytes.Equal(shared, contractJSON) {
		t.Fatal("internal/domain/toolcategory/tool-categories-v1.json differs from " +
			"libs/proto/contracts/tool-categories-v1.json; copy the contract over")
	}
}

// Every declared tool is a tool of the SDK catalogue the platform serves,
// so a declaration cannot name a tool that does not exist (a typo would
// leave the real tool `write`).
func TestEveryDeclarationNamesAKnownTool(t *testing.T) {
	t.Parallel()
	var snapshot struct {
		Entries []struct {
			Type        string                     `json:"type"`
			ArgsSchemas map[string]json.RawMessage `json:"args_schemas"`
		} `json:"entries"`
	}
	raw := repoFile(t, "services", "elitea-main", "internal", "runtimecomposition", "current_toolkit_schema_snapshot.json")
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	known := map[string]map[string]bool{}
	for _, entry := range snapshot.Entries {
		tools := map[string]bool{}
		for name := range entry.ArgsSchemas {
			tools[CanonicalKey(name)] = true
		}
		known[CanonicalKey(entry.Type)] = tools
	}
	for toolkit, tools := range declared {
		for tool := range tools {
			if !known[toolkit][tool] {
				t.Errorf("the contract declares %s.%s, which the toolkit catalogue does not serve", toolkit, tool)
			}
		}
	}
}

func TestOf(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		toolkit, tool string
		want          Category
		declared      bool
	}{
		{"github", "comment_on_issue", ExternalSend, true},
		{"GitHub", "Comment On Issue", ExternalSend, true},
		{"github", "github___comment_on_issue", ExternalSend, true},
		{"github", "delete_branch", Delete, true},
		{"github", "get_issue", Read, true},
		{"github", "create_issue", Write, false},
		{"yagmail", "send_gmail_message", ExternalSend, true},
		{"slack", "send_message", ExternalSend, true},
		{"sandbox", "pyodide_sandbox", CodeExecution, true},
		{"k8s", "kubernetes_integration_healthcheck", Read, true},
		{"mcp", "send", Write, false},
		{"unknown", "anything", Write, false},
		{"github", "", Write, false},
	} {
		if got := Of(test.toolkit, test.tool); got != test.want {
			t.Errorf("Of(%q, %q) = %q, want %q", test.toolkit, test.tool, got, test.want)
		}
		if got := Declared(test.toolkit, test.tool); got != test.declared {
			t.Errorf("Declared(%q, %q) = %t, want %t", test.toolkit, test.tool, got, test.declared)
		}
	}
}

func TestLoadRefusesAMalformedContract(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"not json":         `{`,
		"wrong contract":   `{"contract":"x","default_category":"write","toolkits":{}}`,
		"wrong default":    `{"contract":"elitea.tool-categories.v1","default_category":"read","toolkits":{}}`,
		"unknown category": `{"contract":"elitea.tool-categories.v1","default_category":"write","toolkits":{"a":{"b":"admin"}}}`,
		"twice":            `{"contract":"elitea.tool-categories.v1","default_category":"write","toolkits":{"a":{"b_c":"read","bc":"read"}}}`,
	} {
		if _, err := load([]byte(raw)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	if category, ok := Parse(" payment "); !ok || category != Payment {
		t.Fatalf("Parse(payment) = %q, %t", category, ok)
	}
	if _, ok := Parse("admin"); ok {
		t.Fatal("Parse(admin) is a category")
	}
	if len(Strings()) != 6 || Strings()[3] != "external_send" {
		t.Fatalf("Strings() = %v", Strings())
	}
}
