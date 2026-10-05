package repos

// The Users tab row order (legacy issue 6764): calls, then tokens, then display
// name, then user id. Rows with equal call counts used to come back in user-id
// order, which is on no column of the table, so the order looked random.

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

func TestSortUserActivityAppliesTheDocumentedTieBreak(t *testing.T) {
	users := []analytics.UserActivity{
		{UserID: "10", RunCount: 5, TotalTokens: 100, Name: "zoe"},
		{UserID: "9", RunCount: 5, TotalTokens: 100, Name: ""},
		{UserID: "3", RunCount: 5, TotalTokens: 100, Name: "Adam"},
		{UserID: "2", RunCount: 5, TotalTokens: 900, Name: "mike"},
		{UserID: "8", RunCount: 7, TotalTokens: 1, Name: "kim"},
		{UserID: "11", RunCount: 5, TotalTokens: 100, Name: "", Email: "bob@example.com"},
		{UserID: "12", RunCount: 5, TotalTokens: 100, Name: "adam"},
	}
	sortUserActivity(users)

	want := []string{
		"8",  // most calls
		"2",  // same calls, more tokens
		"3",  // same calls and tokens: "adam" by name, then id 3 before 12
		"12", //
		"11", // "bob@example.com" — the email stands in for a missing name
		"10", // "zoe"
		"9",  // no name and no email sorts after every named row
	}
	for index, id := range want {
		if users[index].UserID != id {
			got := make([]string, len(users))
			for i, user := range users {
				got[i] = user.UserID
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Ids compare as numbers, so "10" follows "9" when everything else ties.
func TestSortUserActivityComparesIDsNumerically(t *testing.T) {
	users := []analytics.UserActivity{
		{UserID: "10", RunCount: 1},
		{UserID: "9", RunCount: 1},
	}
	sortUserActivity(users)
	if users[0].UserID != "9" {
		t.Fatalf("first = %s, want 9: ids must compare numerically", users[0].UserID)
	}
}
