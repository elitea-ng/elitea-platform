package repos

import "testing"

// #946: the stored document's shapes, held against the SAME meanings the
// settings screen's own read route gives them
// (internal/api/v2/promptcontextreads/handler.go). A row this reader and that
// route disagree about is a project whose screen says ON while its prompts
// carry nothing — the silent half of the bug this replaced.
func TestDecodeProjectContextData(t *testing.T) {
	cases := []struct {
		name        string
		data        string
		wantContent string
		wantEnabled bool
	}{
		{"the settings panel's own write", `{"content":"House style: terse.","enabled":true}`, "House style: terse.", true},
		{"the toggle off", `{"content":"House style: terse.","enabled":false}`, "House style: terse.", false},
		// An absent `enabled` means ENABLED — the legacy writer omitted the
		// key, the read route defaults it to true, and the admission gate this
		// replaced spelled it out as COALESCE(data ->> 'enabled', 'true').
		{"no enabled key defaults to enabled", `{"content":"House style: terse."}`, "House style: terse.", true},
		{"a pydantic string boolean", `{"content":"c","enabled":"no"}`, "c", false},
		{"a pydantic numeric boolean", `{"content":"c","enabled":1}`, "c", true},
		{"an unreadable boolean keeps the default", `{"content":"c","enabled":"maybe"}`, "c", true},
		{"an empty document is no context at all", ``, "", false},
		{"a corrupt document is no context at all", `{`, "", false},
		{"a non-string content is dropped, not fatal", `{"content":{"x":1},"enabled":true}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeProjectContextData([]byte(tc.data))
			if got.Content != tc.wantContent || got.Enabled != tc.wantEnabled {
				t.Errorf("decodeProjectContextData(%s) = %+v, want {%q %v}",
					tc.data, got, tc.wantContent, tc.wantEnabled)
			}
		})
	}
}

func TestProjectContextRepoRefusesAnInvalidProject(t *testing.T) {
	repo := NewProjectContextRepo(nil)
	if _, err := repo.ResolveCurrentProjectContext(t.Context(), 1); err == nil {
		t.Error("a repo with no pool must answer an error, so the caller's fail-open path is the one that runs")
	}
}
