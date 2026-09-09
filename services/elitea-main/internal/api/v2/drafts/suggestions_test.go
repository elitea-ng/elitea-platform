package drafts

import (
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		text string
		want map[string]struct{}
	}{
		{
			name: "lowercases and splits on punctuation",
			text: "GitHub Issue-Tracker, v2!",
			want: map[string]struct{}{"github": {}, "issue": {}, "tracker": {}, "v2": {}},
		},
		{
			name: "drops stopwords",
			text: "an agent for the Jira project",
			want: map[string]struct{}{"agent": {}, "jira": {}, "project": {}},
		},
		{
			name: "drops single-character tokens",
			text: "a I x database",
			want: map[string]struct{}{"database": {}},
		},
		{
			name: "empty text tokenizes to nothing",
			text: "",
			want: map[string]struct{}{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenize(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("tokenize(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for tok := range tc.want {
				if _, ok := got[tok]; !ok {
					t.Errorf("tokenize(%q) missing token %q, got %v", tc.text, tok, got)
				}
			}
		})
	}
}

func TestOverlapScore(t *testing.T) {
	a := tokenize("triage incidents severity pagerduty")
	cases := []struct {
		name string
		b    string
		want int
	}{
		{name: "full overlap", b: "triage incidents severity pagerduty", want: 4},
		{name: "partial overlap", b: "incidents dashboard", want: 1},
		{name: "no overlap", b: "weather forecast", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlapScore(a, tokenize(tc.b)); got != tc.want {
				t.Errorf("overlapScore = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestScoreCandidates(t *testing.T) {
	candidates := []suggestionCandidate{
		{id: "1", name: "Jira", description: "Track issues in Jira"},
		{id: "2", name: "GitHub", description: "Manage pull requests and issues"},
		{id: "3", name: "Weather", description: "Look up the forecast"},
		{id: "4", name: "Confluence", description: "Read and write wiki pages"},
	}

	t.Run("ranks the strongest lexical match first", func(t *testing.T) {
		got := scoreCandidates("An agent that triages incidents by filing Jira issues", candidates, 5)
		if len(got) == 0 || got[0].id != "1" {
			t.Fatalf("expected Jira (id 1) to rank first, got %+v", got)
		}
	})

	t.Run("excludes candidates with zero overlap", func(t *testing.T) {
		got := scoreCandidates("An agent that triages incidents by filing Jira issues", candidates, 5)
		for _, c := range got {
			if c.id == "3" {
				t.Fatalf("Weather (id 3, zero overlap) must not be suggested, got %+v", got)
			}
		}
	})

	t.Run("respects the limit", func(t *testing.T) {
		// A query that touches every candidate's own name so all four score > 0.
		got := scoreCandidates("jira github weather confluence", candidates, 2)
		if len(got) != 2 {
			t.Fatalf("expected exactly 2 results (limit), got %d: %+v", len(got), got)
		}
	})

	t.Run("a candidate whose NAME echoes the query outranks one that only shares a description word", func(t *testing.T) {
		// "github" appears in both the query and candidate 2's NAME (counted
		// twice per scoreCandidates' own doc comment); "issues" appears only
		// in candidate 1's description.
		got := scoreCandidates("github issues", candidates, 5)
		if len(got) < 2 {
			t.Fatalf("expected at least 2 matches, got %+v", got)
		}
		if got[0].id != "2" {
			t.Errorf("expected GitHub (id 2, name-echo) to outrank Jira (id 1, description-only), got %+v", got)
		}
	})

	t.Run("empty query yields no suggestions", func(t *testing.T) {
		if got := scoreCandidates("", candidates, 5); got != nil {
			t.Errorf("expected nil for an empty query, got %+v", got)
		}
	})

	t.Run("empty candidate list yields no suggestions", func(t *testing.T) {
		if got := scoreCandidates("anything", nil, 5); got != nil {
			t.Errorf("expected nil for no candidates, got %+v", got)
		}
	})

	t.Run("a limit of zero returns every positively-scored candidate", func(t *testing.T) {
		got := scoreCandidates("jira github confluence", candidates, 0)
		if len(got) != 3 {
			t.Fatalf("expected 3 (all but Weather), got %d: %+v", len(got), got)
		}
	})

	t.Run("ties keep the caller's original ordering", func(t *testing.T) {
		tied := []suggestionCandidate{
			{id: "b", name: "Bravo tool", description: "shared word"},
			{id: "a", name: "Alpha tool", description: "shared word"},
		}
		got := scoreCandidates("shared word", tied, 5)
		if len(got) != 2 || got[0].id != "b" || got[1].id != "a" {
			t.Fatalf("expected stable order [b, a], got %+v", got)
		}
	})
}

func TestToSuggestedResources(t *testing.T) {
	t.Run("never returns nil, even for an empty input", func(t *testing.T) {
		got := toSuggestedResources(nil)
		if got == nil {
			t.Fatal("expected a non-nil empty slice, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("expected an empty slice, got %+v", got)
		}
	})

	t.Run("carries type and agent_type through, omits empty ones", func(t *testing.T) {
		got := toSuggestedResources([]suggestionCandidate{
			{id: "1", name: "GitHub", description: "desc", toolkitType: "github"},
			{id: "2", name: "Triager", agentType: "pipeline"},
		})
		if len(got) != 2 {
			t.Fatalf("expected 2 resources, got %d", len(got))
		}
		if got[0].Type != "github" || got[0].Description != "desc" {
			t.Errorf("resource 0 = %+v, want type=github description=desc", got[0])
		}
		if got[1].AgentType != "pipeline" {
			t.Errorf("resource 1 = %+v, want agent_type=pipeline", got[1])
		}
		if got[1].Type != "" {
			t.Errorf("resource 1 should not carry a toolkit type, got %+v", got[1])
		}
	})
}

func TestStringField(t *testing.T) {
	row := map[string]any{"name": "GitHub", "count": 3, "missing_is_nil": nil}
	if got := stringField(row, "name"); got != "GitHub" {
		t.Errorf("stringField(name) = %q, want GitHub", got)
	}
	if got := stringField(row, "count"); got != "" {
		t.Errorf("stringField(count) = %q, want empty (wrong type degrades safely)", got)
	}
	if got := stringField(row, "absent"); got != "" {
		t.Errorf("stringField(absent) = %q, want empty", got)
	}
}
