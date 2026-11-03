package runbrief

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestParseBriefRenumbersCleansAndBounds(t *testing.T) {
	long := strings.Repeat("x", 400)
	answer := "Here it is:\n```json\n" + `{
  "goal": "Add a\tCSV export\n to the report page",
  "done_when": [{"id": "z", "text": "The export button exists"}, {"id": "c9", "text": "` + long + `"}],
  "constraints": ["a", "b", "c", "d", "e", "f", "` + long + `"],
  "assumptions": ["  ", "UTF-8"],
  "ambiguity": "Medium"
}` + "\n```"
	brief, err := ParseBrief(answer)
	require.NoError(t, err)
	require.Equal(t, "Add a CSV export to the report page", brief.Goal)
	require.Equal(t, []Condition{{ID: "c1", Text: "The export button exists"}, {ID: "c2", Text: strings.Repeat("x", 300)}},
		brief.DoneWhen)
	require.Equal(t, []string{"a", "b", "c", "d", "e"}, brief.Constraints)
	require.Equal(t, []string{"UTF-8"}, brief.Assumptions)
	require.Equal(t, AmbiguityMedium, brief.Ambiguity)
	require.True(t, brief.Valid())
}

func TestParseBriefAcceptsNoConditions(t *testing.T) {
	brief, err := ParseBrief(`{"goal":"Say hello","done_when":[],"ambiguity":"low"}`)
	require.NoError(t, err)
	require.False(t, brief.HasConditions())
	require.Equal(t, []Condition{}, brief.DoneWhen)
	require.Equal(t, []string{}, brief.Constraints)
}

func TestParseBriefRefusesInvalidAnswers(t *testing.T) {
	for name, answer := range map[string]string{
		"prose":           "I cannot do that.",
		"no goal":         `{"done_when":[],"ambiguity":"low"}`,
		"empty goal":      `{"goal":"  \n ","ambiguity":"low"}`,
		"six conditions":  `{"goal":"g","ambiguity":"low","done_when":[{"text":"1"},{"text":"2"},{"text":"3"},{"text":"4"},{"text":"5"},{"text":"6"}]}`,
		"empty condition": `{"goal":"g","ambiguity":"low","done_when":[{"id":"c1","text":"\t"}]}`,
		"ambiguity":       `{"goal":"g","ambiguity":"unclear"}`,
		"wrong type":      `{"goal":3,"ambiguity":"low"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseBrief(answer)
			require.ErrorIs(t, err, ErrInvalidBrief)
		})
	}
}

func TestCleanCutsAtCharactersNotBytes(t *testing.T) {
	text := Clean(strings.Repeat("é", 301), MaxGoalChars)
	require.Equal(t, MaxGoalChars, utf8.RuneCountInString(text))
	require.Equal(t, "a [31m b", Clean("a\x00\x1b[31m\u0085b", 20))
}

func TestDigestIsStable(t *testing.T) {
	brief := Brief{Goal: "g", DoneWhen: []Condition{{ID: "c1", Text: "t"}}, Constraints: []string{}, Assumptions: []string{},
		Ambiguity: AmbiguityLow}
	raw, sum, err := Digest(brief)
	require.NoError(t, err)
	require.JSONEq(t, `{"goal":"g","done_when":[{"id":"c1","text":"t"}],"constraints":[],"assumptions":[],"ambiguity":"low"}`,
		string(raw))
	_, again, err := Digest(brief)
	require.NoError(t, err)
	require.Equal(t, sum, again)
	require.Len(t, sum, 64)
}

func TestBoundTranscriptKeepsTheNewestContent(t *testing.T) {
	text := strings.Repeat("old ", MaxTranscriptBytes) + "NEWEST"
	bounded := BoundTranscript(text)
	require.LessOrEqual(t, len(bounded), MaxTranscriptBytes)
	require.True(t, strings.HasSuffix(bounded, "NEWEST"))
	require.True(t, utf8.ValidString(BoundTranscript(strings.Repeat("ж", MaxTranscriptBytes))))
}
