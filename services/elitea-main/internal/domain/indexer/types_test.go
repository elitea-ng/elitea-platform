package indexer_test

// The indexer domain package, which had no test file at all.
//
// READ THIS BEFORE ADDING TO IT — the same caveat as internal/domain/toolkits.
// Nothing outside internal/domain/indexer imports this package
// (`grep -r "domain/indexer"` finds only its own two files); `Service` is an
// empty struct with a TODO, and the types are DTOs whose only behaviour is
// their JSON encoding. These tests pin the wire names and the `json:"-"` tags,
// which are the only things here that can change silently.
//
// If this package is still unimported when it is next read, delete it and this
// file. Do not grow either.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/indexer"
)

func TestNewServiceIsConstructible(t *testing.T) {
	t.Parallel()

	if indexer.New() == nil {
		t.Fatal("New() returned nil")
	}
}

func TestIndexerTypesEncodeTheNamesTheirTagsDeclare(t *testing.T) {
	t.Parallel()

	moment := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	for name, testCase := range map[string]struct {
		value any
		want  []string
	}{
		"Task": {
			value: indexer.Task{
				ID: "1", ProjectID: "2", Type: "index_data", Status: "running",
				Progress: 0.5, Error: "e", CreatedAt: moment, UpdatedAt: moment,
			},
			want: []string{
				"id", "project_id", "type", "status", "progress", "error",
				"created_at", "updated_at",
			},
		},
		"IndexMeta": {
			value: indexer.IndexMeta{
				ID: "1", ToolkitID: "2", Name: "n", Type: "confluence", Status: "ok",
				Config: map[string]any{"space": "DOCS"}, CreatedAt: moment, UpdatedAt: moment,
			},
			want: []string{
				"id", "toolkit_id", "name", "type", "status", "config",
				"created_at", "updated_at",
			},
		},
		"IndexType": {
			value: indexer.IndexType{Type: "confluence", Name: "Confluence", Description: "d", Category: "c"},
			want:  []string{"type", "name", "description", "category"},
		},
		"ProjectContext": {
			value: indexer.ProjectContext{ProjectID: "1", Config: map[string]any{"k": "v"}},
			want:  []string{"project_id", "config"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertEncodesExactly(t, testCase.value, testCase.want)
		})
	}
}

// TestCancelTaskRequestCarriesNothingOnTheWire pins all four `json:"-"` tags.
//
// Every field of this request comes from the URL. A tag that let one be decoded
// from the body would make a cancel able to name a task in another project, and
// no other test in this repository would move.
func TestCancelTaskRequestCarriesNothingOnTheWire(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(indexer.CancelTaskRequest{
		ProjectID: "1", ToolkitID: "2", IndexName: "docs", TaskID: "t-1",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(encoded) != "{}" {
		t.Errorf("CancelTaskRequest encoded as %s, want {} — every field is the router's", encoded)
	}
}

func TestOptionalIndexerFieldsAreOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		value    any
		required []string
		omitted  []string
	}{
		"an empty Task keeps its identity and drops progress and error": {
			value:    indexer.Task{},
			required: []string{"id", "project_id", "type", "status", "created_at", "updated_at"},
			omitted:  []string{"progress", "error"},
		},
		"an empty IndexMeta drops its config": {
			value:    indexer.IndexMeta{},
			required: []string{"id", "toolkit_id", "name", "type", "status"},
			omitted:  []string{"config"},
		},
		"an IndexType with no description or category sends neither": {
			value:    indexer.IndexType{Type: "confluence", Name: "Confluence"},
			required: []string{"type", "name"},
			omitted:  []string{"description", "category"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			encoded := encodeToMap(t, testCase.value)
			for _, key := range testCase.required {
				if _, present := encoded[key]; !present {
					t.Errorf("the empty value dropped %q, which carries no omitempty", key)
				}
			}
			for _, key := range testCase.omitted {
				if _, present := encoded[key]; present {
					t.Errorf("the empty value encodes %q; it is declared omitempty", key)
				}
			}
		})
	}
}

// TestTaskProgressSurvivesAJSONRoundTrip guards the one non-string field.
//
// Progress is a float64 with omitempty, so a task that is genuinely 0% complete
// encodes without the key. That is a real behaviour a client has to handle and
// it is pinned here rather than discovered by one.
func TestTaskProgressSurvivesAJSONRoundTrip(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(indexer.Task{ID: "1", Progress: 0.25})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var received indexer.Task
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if received.Progress != 0.25 {
		t.Errorf("progress round-tripped as %v", received.Progress)
	}

	zero := encodeToMap(t, indexer.Task{ID: "1"})
	if _, present := zero["progress"]; present {
		t.Error("a task at 0% encodes a progress key; omitempty says it must not," +
			" and a client that reads absence as unknown depends on that")
	}
}

func encodeToMap(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode %T: %v (%s)", value, err, encoded)
	}
	return decoded
}

func assertEncodesExactly(t *testing.T, value any, want []string) {
	t.Helper()
	encoded := encodeToMap(t, value)
	wanted := make(map[string]struct{}, len(want))
	for _, key := range want {
		wanted[key] = struct{}{}
		if _, present := encoded[key]; !present {
			t.Errorf("%T does not encode %q", value, key)
		}
	}
	for key := range encoded {
		if _, expected := wanted[key]; !expected {
			t.Errorf("%T encodes an undeclared field %q; add it to this table"+
				" once its wire name is deliberate", value, key)
		}
	}
}
