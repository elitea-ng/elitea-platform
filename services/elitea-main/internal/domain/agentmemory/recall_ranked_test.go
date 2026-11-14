package agentmemory

import (
	"testing"
	"time"
)

func candidateIDs(entries []Candidate) []int64 {
	out := make([]int64, len(entries))
	for i, entry := range entries {
		out[i] = entry.ID
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSelectRankedOrdersBySimilarityAndKeepsTheRowsRules(t *testing.T) {
	now := time.Now()
	settings := Settings{Enabled: true, Write: WriteAuto, MaxEntries: 10, MaxTokens: 1000}
	candidates := []Candidate{
		{ID: 1, Kind: KindFact, Content: "the deploy runs on friday", UpdatedAt: now},
		{ID: 2, Kind: KindFact, Content: "release day is the last weekday", UpdatedAt: now.Add(-time.Hour)},
		{ID: 3, Kind: KindFact, Content: "unrelated note", UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: 4, Kind: KindFact, Content: "about bob", SubjectUserID: 9, UpdatedAt: now},
		{ID: 5, Kind: KindCursor, Content: "cursor", UpdatedAt: now},
		{ID: 6, Kind: KindFact, Content: "pinned note", Pinned: true, UpdatedAt: now.Add(-3 * time.Hour)},
	}
	// The index ranks 2 above 1; 3 is not embedded yet. 4 is about an absent
	// person and 5 is a cursor: the index ranking never lifts either.
	similarity := map[int64]float32{2: 0.9, 1: 0.4, 4: 0.99, 5: 0.99}

	got := candidateIDs(SelectRanked(candidates, map[int64]bool{}, "when is the deploy", settings, similarity))
	if want := []int64{6, 2, 1, 3}; !sameIDs(got, want) {
		t.Fatalf("selected %v, want %v", got, want)
	}

	// With the person present their entry is a candidate and ranks by score.
	got = candidateIDs(SelectRanked(candidates, map[int64]bool{9: true}, "when is the deploy", settings, similarity))
	if want := []int64{6, 4, 2, 1, 3}; !sameIDs(got, want) {
		t.Fatalf("with the person present: %v, want %v", got, want)
	}

	// No similarity (an outage, or nothing embedded) is exactly Select.
	for _, empty := range []map[int64]float32{nil, {}} {
		keyword := candidateIDs(Select(candidates, map[int64]bool{}, "when is the deploy", settings))
		ranked := candidateIDs(SelectRanked(candidates, map[int64]bool{}, "when is the deploy", settings, empty))
		if !sameIDs(keyword, ranked) {
			t.Fatalf("keyword %v, ranked %v", keyword, ranked)
		}
	}

	// The entry budget is the rows': one entry fits, the pinned one.
	one := settings
	one.MaxEntries = 1
	if got := candidateIDs(SelectRanked(candidates, map[int64]bool{}, "x", one, similarity)); !sameIDs(got, []int64{6}) {
		t.Fatalf("entry budget: %v", got)
	}

	// An entry the index holds but the rows do not is never added.
	got = candidateIDs(SelectRanked(candidates[:1], map[int64]bool{}, "deploy", settings, map[int64]float32{99: 1}))
	if !sameIDs(got, []int64{1}) {
		t.Fatalf("an index-only entry appeared: %v", got)
	}
}

func TestRankingMattersOnlyWhenABudgetLeavesSomeoneOut(t *testing.T) {
	settings := Settings{Enabled: true, Write: WriteAuto, MaxEntries: 2, MaxTokens: 1000}
	fit := []Candidate{
		{ID: 1, Kind: KindFact, Content: "a"},
		{ID: 2, Kind: KindFact, Content: "b"},
		{ID: 3, Kind: KindCursor, Content: "cursor"},
		{ID: 4, Kind: KindFact, Content: "about bob", SubjectUserID: 9},
	}
	// Two candidates are left (the cursor and the absent person's entry do
	// not count), and the entry budget is 2: every one is recalled.
	if RankingMatters(fit, map[int64]bool{}, settings) {
		t.Fatal("the order cannot change a recall that takes every candidate")
	}
	// With the person present there are three candidates for two places.
	if !RankingMatters(fit, map[int64]bool{9: true}, settings) {
		t.Fatal("the entry budget leaves one out")
	}
	// The byte budget leaves one out.
	tight := settings
	tight.MaxTokens = MinRecallTokens
	big := []Candidate{
		{ID: 1, Kind: KindFact, Content: string(make([]byte, 300))},
		{ID: 2, Kind: KindFact, Content: string(make([]byte, 300))},
	}
	if !RankingMatters(big, map[int64]bool{}, tight) {
		t.Fatal("the byte budget leaves one out")
	}
	if RankingMatters(fit, map[int64]bool{9: true}, Settings{}) {
		t.Fatal("memory off recalls nothing")
	}
}
