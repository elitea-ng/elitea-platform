package run_test

// ApplyExtraContext's own rules (#873). Unlike `context_paths`
// (contextpaths_test.go), there is no golden fixture shared with the Python
// wrapper here: this resolver never reads from a transport — the content
// arrives in the request already — so there is nothing a Python mirror could
// diverge on that a Go-only table cannot already pin. `wiki_context.py`'s own
// `extra_context` mirror carries its own unit test
// (tests/unit/test_wiki_context.py) asserting the SAME budgets and the SAME
// prepend shape, which is what keeps the two from drifting instead.

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
)

func extraFile(name, content string) map[string]any {
	return map[string]any{"name": name, "content": content}
}

func TestApplyExtraContextIsANoOpWithNoSelection(t *testing.T) {
	params := run.Params{"question": "hi"}
	resolved, err := run.ApplyExtraContext("ask", params)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["question"] != "hi" {
		t.Fatalf("question changed with no attachment: %v", resolved["question"])
	}
	if _, present := resolved[run.ExtraContextParam]; present {
		t.Fatal("extra_context key survived a no-op resolution")
	}
}

func TestApplyExtraContextPrependsWithoutACurrentQuestionTrailer(t *testing.T) {
	params := run.Params{
		"question":            "What does this do?",
		run.ExtraContextParam: []any{extraFile("notes.md", "the important bit")},
	}
	resolved, err := run.ApplyExtraContext("ask", params)
	if err != nil {
		t.Fatal(err)
	}
	question, _ := resolved["question"].(string)
	if !strings.HasPrefix(question, run.ExtraContextLeadIn) {
		t.Fatalf("block is not prepended: %q", question)
	}
	if !strings.Contains(question, "--- file: notes.md ---\nthe important bit") {
		t.Fatalf("file section missing: %q", question)
	}
	if !strings.HasSuffix(question, "What does this do?") {
		t.Fatalf("original question is not the trailing text: %q", question)
	}
	// The whole point of the no-trailer design (file header): stacking two
	// of context_paths' own markers would read as the model being asked
	// twice.
	if strings.Contains(question, "Current question:") {
		t.Fatalf("extra_context must not add its own Current question marker: %q", question)
	}
}

func TestApplyExtraContextRunsAfterContextPathsWithoutDoubleWrapping(t *testing.T) {
	// Simulates the runner.go pipeline: ApplyContextPaths already ran and
	// left its own prepend in `question`.
	alreadyPrepended := run.ContextLeadIn + "\n--- source: wiki_pages/a.md ---\nwiki text\n\n" +
		"Current question: What does this do?"
	params := run.Params{
		"question":            alreadyPrepended,
		run.ExtraContextParam: []any{extraFile("notes.md", "extra")},
	}
	resolved, err := run.ApplyExtraContext("ask", params)
	if err != nil {
		t.Fatal(err)
	}
	question, _ := resolved["question"].(string)
	// Exactly one "Current question:" — the wiki-page block's own — survives.
	if strings.Count(question, "Current question:") != 1 {
		t.Fatalf("expected exactly one Current question marker, got:\n%s", question)
	}
	if !strings.HasPrefix(question, run.ExtraContextLeadIn) {
		t.Fatalf("the files block must sit in FRONT of the wiki-page block: %q", question)
	}
}

func TestApplyExtraContextTruncatesEachFileAtItsOwnBudget(t *testing.T) {
	long := strings.Repeat("x", run.ExtraContextPerFileBudgetChars+500)
	params := run.Params{
		"question":            "q",
		run.ExtraContextParam: []any{extraFile("big.txt", long)},
	}
	resolved, err := run.ApplyExtraContext("ask", params)
	if err != nil {
		t.Fatal(err)
	}
	question, _ := resolved["question"].(string)
	if !strings.Contains(question, "truncated to") {
		t.Fatalf("no truncation marker in an over-budget file: %q", question)
	}
	prefix := strings.Repeat("x", run.ExtraContextPerFileBudgetChars)
	if !strings.Contains(question, prefix) {
		t.Fatal("truncation did not keep the per-file budget's worth of the PREFIX")
	}
	if strings.Contains(question, long) {
		t.Fatal("the untruncated body leaked through")
	}
}

func TestApplyExtraContextNamesFilesItCouldNotAffordAtAll(t *testing.T) {
	// The PER-FILE cap (6k) is well under the TOTAL budget (16k), so one
	// oversized file only ever spends its own cap, never the whole budget —
	// filling the total for real takes enough files that each one's capped
	// body, summed, reaches it.
	fill := strings.Repeat("y", run.ExtraContextPerFileBudgetChars)
	files := []any{}
	spent := 0
	name := 0
	for spent < run.ExtraContextTotalBudgetChars {
		name++
		files = append(files, extraFile("filler.txt", fill))
		spent += run.ExtraContextPerFileBudgetChars
	}
	files = append(files, extraFile("gets-nothing.txt", "leftover text"))

	params := run.Params{"question": "q", run.ExtraContextParam: files}
	resolved, err := run.ApplyExtraContext("ask", params)
	if err != nil {
		t.Fatal(err)
	}
	question, _ := resolved["question"].(string)
	if !strings.Contains(question, "gets-nothing.txt") {
		t.Fatalf("an omitted file must still be NAMED, not silently dropped: %q", question)
	}
	if strings.Contains(question, "leftover text") {
		t.Fatal("an omitted file's content must not appear at all")
	}
}

func TestApplyExtraContextRefusesMoreFilesThanTheCap(t *testing.T) {
	files := make([]any, 0, run.MaxExtraContextFiles+1)
	for i := 0; i <= run.MaxExtraContextFiles; i++ {
		files = append(files, extraFile("f.txt", "x"))
	}
	params := run.Params{"question": "q", run.ExtraContextParam: files}
	_, err := run.ApplyExtraContext("ask", params)
	if err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Fatalf("got %v", err)
	}
}

func TestApplyExtraContextRefusesAToolThatCannotHonourIt(t *testing.T) {
	// Silently ignoring it would answer ungrounded and look like the
	// feature working — the same rule context_paths enforces.
	params := run.Params{
		"question":            "q",
		run.ExtraContextParam: []any{extraFile("notes.md", "x")},
	}
	_, err := run.ApplyExtraContext("generate_wiki", params)
	if err == nil || !strings.Contains(err.Error(), "is not supported by generate_wiki") {
		t.Fatalf("got %v", err)
	}
}

func TestApplyExtraContextRefusesAMalformedEntry(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"not a list", "notes.md"},
		{"entry is not an object", []any{"notes.md"}},
		{"missing content", []any{map[string]any{"name": "a.txt"}}},
		{"missing name", []any{map[string]any{"content": "x"}}},
		{"empty name", []any{map[string]any{"name": "  ", "content": "x"}}},
		{"content is not a string", []any{map[string]any{"name": "a.txt", "content": 5}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			params := run.Params{"question": "q", run.ExtraContextParam: c.value}
			_, err := run.ApplyExtraContext("ask", params)
			if err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}

func TestApplyExtraContextConsumesItsKeyEvenWhenUnused(t *testing.T) {
	// A tool with no question (list_wikis, say) never carries extra_context
	// in practice, but the key must not leak downstream regardless of what
	// arrives — nothing here depends on `tool` before an empty selection is
	// confirmed.
	params := run.Params{run.ExtraContextParam: []any{}}
	resolved, err := run.ApplyExtraContext("list_wikis", params)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := resolved[run.ExtraContextParam]; present {
		t.Fatal("extra_context key survived an empty-list resolution")
	}
}
